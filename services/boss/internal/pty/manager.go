package pty

import (
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	creackpty "github.com/creack/pty/v2"
)

// activeThreshold is how long since last output before a session is considered idle.
const activeThreshold = 5 * time.Second

// Manager tracks running Claude processes across PTY sessions.
type Manager struct {
	mu        sync.Mutex
	processes map[string]*Process
	sessions  map[string]string // claudeID → sessionID
}

// NewManager creates a new process manager.
func NewManager() *Manager {
	return &Manager{
		processes: make(map[string]*Process),
		sessions:  make(map[string]string),
	}
}

// RegisterSession associates a Claude process ID with a session ID.
func (m *Manager) RegisterSession(claudeID, sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[claudeID] = sessionID
}

// Status constants returned by SessionStatus and ProcessStatus.
const (
	StatusWorking  = "working"  // process alive, recent output
	StatusIdle     = "idle"     // process alive, no recent output
	StatusStopped  = "stopped"  // process exited or never started
	StatusQuestion = "question" // process idle, question prompt detected in PTY
)

// SessionStatus returns the aggregate status for a session.
// It checks all claude processes registered to the session and returns the
// highest-priority status found: question > working > idle > stopped.
func (m *Manager) SessionStatus(sessionID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	best := StatusStopped
	for claudeID, sid := range m.sessions {
		if sid != sessionID {
			continue
		}
		p, ok := m.processes[claudeID]
		if !ok {
			continue
		}
		select {
		case <-p.done:
			continue
		default:
		}
		// Process is alive — check question first (overrides working/idle).
		if p.HasQuestionPrompt() {
			best = StatusQuestion
			continue
		}
		lw := p.LastWrite()
		if !lw.IsZero() && time.Since(lw) < activeThreshold {
			if best != StatusQuestion {
				best = StatusWorking
			}
			continue
		}
		if best == StatusStopped {
			best = StatusIdle
		}
	}
	return best
}

// GetOrStart returns an existing process for the given ID, or starts a new one.
//
// Concurrency: the whole "check entry → observe p.done → delete → insert new"
// sequence runs under m.mu. That makes check-then-delete atomic with respect
// to any other map-mutating call (Get, Cleanup, GetOrStart) so a concurrent
// Get cannot observe a dead entry that this call is about to replace.
// TestManagerConcurrentGetGetOrStartCleanup locks this invariant in under -race.
func (m *Manager) GetOrStart(id string, cmd *exec.Cmd) (*Process, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if p, ok := m.processes[id]; ok {
		select {
		case <-p.done:
			// Process has exited; clean up and start fresh.
			delete(m.processes, id)
		default:
			return p, nil
		}
	}

	ptmx, err := creackpty.Start(cmd)
	if err != nil {
		return nil, err
	}

	p := &Process{
		ptyFile: ptmx,
		cmd:     cmd,
		buf:     NewRingBuffer(defaultBufSize),
		done:    make(chan struct{}),
	}
	go p.readLoop()
	m.processes[id] = p
	return p, nil
}

// Get returns the process for the given ID, if any.
func (m *Manager) Get(id string) (*Process, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.processes[id]
	if !ok {
		return nil, false
	}
	select {
	case <-p.done:
		// Already dead; clean up.
		delete(m.processes, id)
		return nil, false
	default:
		return p, true
	}
}

// IsRunning reports whether a process for the given ID is still alive.
func (m *Manager) IsRunning(id string) bool {
	_, ok := m.Get(id)
	return ok
}

// ProcessStatus returns the status for a specific claude process.
func (m *Manager) ProcessStatus(claudeID string) string {
	m.mu.Lock()
	p, ok := m.processes[claudeID]
	m.mu.Unlock()
	if !ok {
		return StatusStopped
	}
	select {
	case <-p.done:
		return StatusStopped
	default:
	}
	if p.HasQuestionPrompt() {
		return StatusQuestion
	}
	lw := p.LastWrite()
	if !lw.IsZero() && time.Since(lw) < activeThreshold {
		return StatusWorking
	}
	return StatusIdle
}

// ProcessLastWrite returns the last output time for the given claude ID.
// Returns zero time if the process is not found or has no output.
func (m *Manager) ProcessLastWrite(id string) time.Time {
	m.mu.Lock()
	p, ok := m.processes[id]
	m.mu.Unlock()
	if !ok {
		return time.Time{}
	}
	return p.LastWrite()
}

// ProcessInfo holds a snapshot of a process's status for heartbeat reporting.
type ProcessInfo struct {
	Status    string
	LastWrite time.Time
}

// AllStatuses returns a snapshot of all tracked processes for heartbeat batch reporting.
func (m *Manager) AllStatuses() map[string]ProcessInfo {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := make(map[string]ProcessInfo, len(m.processes))
	for id, p := range m.processes {
		var status string
		select {
		case <-p.done:
			status = StatusStopped
		default:
			if p.HasQuestionPrompt() {
				status = StatusQuestion
			} else {
				lw := p.LastWrite()
				if !lw.IsZero() && time.Since(lw) < activeThreshold {
					status = StatusWorking
				} else {
					status = StatusIdle
				}
			}
		}
		result[id] = ProcessInfo{
			Status:    status,
			LastWrite: p.LastWrite(),
		}
	}
	return result
}

// Cleanup removes a dead process from the map.
func (m *Manager) Cleanup(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.processes, id)
}

// Process wraps a running command inside a PTY.
type Process struct {
	ptyFile   *os.File
	cmd       *exec.Cmd
	mu        sync.Mutex
	writer    io.Writer // attached output destination
	buf       *RingBuffer
	done      chan struct{}
	exitErr   error
	lastWrite time.Time
}

// readLoop continuously reads from the PTY and forwards output.
func (p *Process) readLoop() {
	defer close(p.done)
	tmp := make([]byte, 4096)
	for {
		n, err := p.ptyFile.Read(tmp)
		if n > 0 {
			data := tmp[:n]
			_, _ = p.buf.Write(data)
			p.mu.Lock()
			p.lastWrite = time.Now()
			w := p.writer
			p.mu.Unlock()
			if w != nil {
				_, _ = w.Write(data)
			}
		}
		if err != nil {
			break
		}
	}
	// Wait for the process to fully exit and capture its error.
	p.exitErr = p.cmd.Wait()
	_ = p.ptyFile.Close()
}

// Attach sets the writer that receives live PTY output.
func (p *Process) Attach(w io.Writer) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.writer = w
}

// Detach clears the attached writer. Output continues to be buffered.
func (p *Process) Detach() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.writer = nil
}

// LastWrite returns the time of the last PTY output.
func (p *Process) LastWrite() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastWrite
}

// questionTailSize is the number of trailing PTY bytes scanned for question
// prompts. Must be large enough to capture the full AskUserQuestion UI
// including wide-terminal border lines (each ─ is 3 UTF-8 bytes) AND the
// post-response TUI chrome (dividers, status bars, cursor positioning).
// Claude Code re-renders this chrome on each TUI update, so the raw byte
// cost scales with terminal width × number of re-renders.
// 16384 covers terminals up to ~500 columns with multiple re-renders.
const questionTailSize = 16384

// HasQuestionPrompt checks if the PTY ring buffer ends with a Claude Code
// question prompt (AskUserQuestion or permission UI).
func (p *Process) HasQuestionPrompt() bool {
	return hasQuestionPrompt(p.buf.Tail(questionTailSize))
}

// WriteInput sends user input to the PTY.
func (p *Process) WriteInput(data []byte) error {
	_, err := p.ptyFile.Write(data)
	return err
}

// Resize updates the PTY window size.
func (p *Process) Resize(rows, cols uint16) error {
	return creackpty.Setsize(p.ptyFile, &creackpty.Winsize{
		Rows: rows,
		Cols: cols,
	})
}

// ReplayBuffer writes the buffered output to the given writer.
func (p *Process) ReplayBuffer(w io.Writer) {
	data := p.buf.Bytes()
	if len(data) > 0 {
		_, _ = w.Write(data)
	}
}

// Done returns a channel that is closed when the process exits.
func (p *Process) Done() <-chan struct{} {
	return p.done
}

// ExitErr returns the process exit error (only valid after Done is closed).
func (p *Process) ExitErr() error {
	return p.exitErr
}
