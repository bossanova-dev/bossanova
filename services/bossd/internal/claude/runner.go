// Package claude manages Claude CLI subprocess lifecycle for coding sessions.
package claude

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// DefaultRingBufferSize is the number of output lines kept in memory per session.
const DefaultRingBufferSize = 1000

// OutputLine is a single line of output from a Claude process.
type OutputLine struct {
	Text      string
	Timestamp time.Time
}

// ClaudeRunner manages Claude CLI subprocesses.
type ClaudeRunner interface {
	// Start spawns a Claude CLI process in workDir with the given plan.
	// If resume is non-nil, it resumes an existing Claude session.
	// Returns the session ID assigned to this process.
	Start(ctx context.Context, workDir, plan string, resume *string) (sessionID string, err error)

	// Stop terminates the Claude process for the given session.
	Stop(sessionID string) error

	// IsRunning reports whether a Claude process is active for the session.
	IsRunning(sessionID string) bool

	// Subscribe returns a channel that receives output lines for the session.
	// The channel is closed when the process exits or the caller cancels ctx.
	Subscribe(ctx context.Context, sessionID string) (<-chan OutputLine, error)

	// History returns the buffered output lines for a session.
	History(sessionID string) []OutputLine
}

// CommandFactory creates exec.Cmd instances. Allows injection for testing.
type CommandFactory func(ctx context.Context, name string, args ...string) *exec.Cmd

// Runner is the default ClaudeRunner implementation.
type Runner struct {
	mu      sync.RWMutex
	procs   map[string]*process
	cmdFunc CommandFactory
	logger  zerolog.Logger
	bufSize int
	logDir  string // if set, write log files here; otherwise use workDir/.boss/
}

// process tracks a running Claude subprocess.
type process struct {
	cmd       *exec.Cmd
	cancel    context.CancelFunc
	sessionID string
	workDir   string
	ring      *ringBuffer
	subs      *subscribers
	logFile   *os.File
	done      chan struct{} // closed when process exits
	exitErr   error
}

// NewRunner creates a new Claude process runner.
func NewRunner(logger zerolog.Logger) *Runner {
	return &Runner{
		procs:   make(map[string]*process),
		cmdFunc: exec.CommandContext,
		logger:  logger,
		bufSize: DefaultRingBufferSize,
	}
}

// Start spawns a Claude CLI process. It runs `claude --print --output-format=stream-json`
// in the given workDir, piping the plan to stdin.
func (r *Runner) Start(ctx context.Context, workDir, plan string, resume *string) (string, error) {
	// Generate a session ID from timestamp + short random suffix.
	sessionID := fmt.Sprintf("claude-%d", time.Now().UnixNano())

	r.mu.Lock()
	if _, exists := r.procs[sessionID]; exists {
		r.mu.Unlock()
		return "", fmt.Errorf("session %s already exists", sessionID)
	}
	r.mu.Unlock()

	// Build command args.
	args := []string{"--print", "--output-format", "stream-json"}
	if resume != nil {
		args = append(args, "--resume", *resume)
	}

	procCtx, cancel := context.WithCancel(ctx)
	cmd := r.cmdFunc(procCtx, "claude", args...)
	cmd.Dir = workDir

	// Pipe plan to stdin.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return "", fmt.Errorf("create stdin pipe: %w", err)
	}

	// Capture stdout.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return "", fmt.Errorf("create stdout pipe: %w", err)
	}

	// Capture stderr (merged into same output stream).
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return "", fmt.Errorf("create stderr pipe: %w", err)
	}

	// Open log file.
	logPath := r.logPath(workDir, sessionID)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		cancel()
		return "", fmt.Errorf("create log dir: %w", err)
	}

	logFile, err := os.Create(logPath)
	if err != nil {
		cancel()
		return "", fmt.Errorf("create log file: %w", err)
	}

	p := &process{
		cmd:       cmd,
		cancel:    cancel,
		sessionID: sessionID,
		workDir:   workDir,
		ring:      newRingBuffer(r.bufSize),
		subs:      newSubscribers(),
		logFile:   logFile,
		done:      make(chan struct{}),
	}

	r.mu.Lock()
	r.procs[sessionID] = p
	r.mu.Unlock()

	r.logger.Info().
		Str("session", sessionID).
		Str("workDir", workDir).
		Msg("starting claude process")

	if err := cmd.Start(); err != nil {
		cancel()
		_ = logFile.Close()
		r.mu.Lock()
		delete(r.procs, sessionID)
		r.mu.Unlock()
		return "", fmt.Errorf("start claude: %w", err)
	}

	// Write plan to stdin, then close.
	go func() {
		defer func() { _ = stdin.Close() }()
		_, _ = io.WriteString(stdin, plan)
	}()

	// Stream stdout and stderr into ring buffer + log file.
	go r.captureOutput(p, stdout)
	go r.captureOutput(p, stderr)

	// Wait for process exit.
	go func() {
		p.exitErr = cmd.Wait()
		_ = p.logFile.Close()
		p.subs.close()
		close(p.done)
		r.logger.Info().
			Str("session", sessionID).
			Err(p.exitErr).
			Msg("claude process exited")
	}()

	return sessionID, nil
}

// Stop terminates the Claude process for the given session.
func (r *Runner) Stop(sessionID string) error {
	r.mu.RLock()
	p, ok := r.procs[sessionID]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}

	p.cancel()

	// Wait for process to exit with a timeout.
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		// Force kill if graceful shutdown didn't work.
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
	}

	return nil
}

// IsRunning reports whether a Claude process is active for the session.
func (r *Runner) IsRunning(sessionID string) bool {
	r.mu.RLock()
	p, ok := r.procs[sessionID]
	r.mu.RUnlock()
	if !ok {
		return false
	}

	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// Subscribe returns a channel that receives output lines for the session.
func (r *Runner) Subscribe(ctx context.Context, sessionID string) (<-chan OutputLine, error) {
	r.mu.RLock()
	p, ok := r.procs[sessionID]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("session %s not found", sessionID)
	}

	ch := p.subs.add(ctx)
	return ch, nil
}

// History returns the buffered output lines for a session.
func (r *Runner) History(sessionID string) []OutputLine {
	r.mu.RLock()
	p, ok := r.procs[sessionID]
	r.mu.RUnlock()
	if !ok {
		return nil
	}

	return p.ring.lines()
}

// logPath returns the log file path for a session.
func (r *Runner) logPath(workDir, sessionID string) string {
	if r.logDir != "" {
		return filepath.Join(r.logDir, sessionID+".log")
	}
	return filepath.Join(workDir, ".boss", "claude.log")
}

// captureOutput reads from a reader line by line and feeds into the ring buffer,
// log file, and subscriber channels.
func (r *Runner) captureOutput(p *process, reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	// Allow up to 1MB per line for JSON output.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := OutputLine{
			Text:      scanner.Text(),
			Timestamp: time.Now(),
		}

		// Write to ring buffer.
		p.ring.add(line)

		// Write to log file.
		_, _ = fmt.Fprintf(p.logFile, "%s\n", line.Text)

		// Broadcast to subscribers.
		p.subs.broadcast(line)
	}
}

// --- Ring Buffer ---

// ringBuffer is a fixed-size circular buffer of OutputLine entries.
type ringBuffer struct {
	mu    sync.RWMutex
	buf   []OutputLine
	size  int
	head  int // next write position
	count int // total items written (for overflow detection)
}

func newRingBuffer(size int) *ringBuffer {
	return &ringBuffer{
		buf:  make([]OutputLine, size),
		size: size,
	}
}

// add appends a line to the ring buffer.
func (rb *ringBuffer) add(line OutputLine) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	rb.buf[rb.head] = line
	rb.head = (rb.head + 1) % rb.size
	rb.count++
}

// lines returns all stored lines in chronological order.
func (rb *ringBuffer) lines() []OutputLine {
	rb.mu.RLock()
	defer rb.mu.RUnlock()

	if rb.count == 0 {
		return nil
	}

	n := rb.count
	if n > rb.size {
		n = rb.size
	}

	result := make([]OutputLine, n)
	if rb.count <= rb.size {
		// Buffer hasn't wrapped yet.
		copy(result, rb.buf[:n])
	} else {
		// Buffer has wrapped: read from head (oldest) to end, then start to head.
		start := rb.head // oldest entry
		copied := copy(result, rb.buf[start:])
		copy(result[copied:], rb.buf[:start])
	}

	return result
}

// --- Subscribers ---

// subscribers manages broadcast channels for output streaming.
type subscribers struct {
	mu     sync.RWMutex
	chans  []chan OutputLine
	closed bool
}

func newSubscribers() *subscribers {
	return &subscribers{}
}

// add creates a new subscription channel. The channel is removed when ctx is cancelled.
func (s *subscribers) add(ctx context.Context) <-chan OutputLine {
	ch := make(chan OutputLine, 64)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		close(ch)
		return ch
	}
	s.chans = append(s.chans, ch)
	s.mu.Unlock()

	// Remove the channel when the context is cancelled.
	go func() {
		<-ctx.Done()
		s.remove(ch)
	}()

	return ch
}

// broadcast sends a line to all subscriber channels.
func (s *subscribers) broadcast(line OutputLine) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, ch := range s.chans {
		select {
		case ch <- line:
		default:
			// Slow consumer — drop the line rather than blocking.
		}
	}
}

// close closes all subscriber channels.
func (s *subscribers) close() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closed = true
	for _, ch := range s.chans {
		close(ch)
	}
	s.chans = nil
}

// remove removes a specific channel from subscribers.
func (s *subscribers) remove(ch chan OutputLine) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, c := range s.chans {
		if c == ch {
			s.chans = append(s.chans[:i], s.chans[i+1:]...)
			close(ch)
			return
		}
	}
}
