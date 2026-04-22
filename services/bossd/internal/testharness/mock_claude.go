package testharness

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/recurser/bossd/internal/claude"
)

var _ claude.ClaudeRunner = (*MockClaudeRunner)(nil)

// MockClaudeRunner is a mock ClaudeRunner that simulates Claude processes
// without spawning real subprocesses.
type MockClaudeRunner struct {
	mu       sync.RWMutex
	sessions map[string]*mockProcess
	counter  atomic.Int64

	// Stopped records the Claude session IDs that have had Stop called on
	// them, in order. Tests assert against this to verify that Pause/Stop
	// RPCs propagate to the runner.
	Stopped []string

	// Started records the Claude session IDs that have been spawned via
	// Start (in order). Tests assert against this to detect leaked
	// subprocesses on negative-path runs.
	Started []string

	// StartFunc overrides the default Start behavior when set.
	StartFunc func(ctx context.Context, workDir, plan string, resume *string, sessionID string) (string, error)

	// SubscribedCh, when non-nil, receives the sessionID on each Subscribe
	// call. The send is non-blocking (select/default), so tests that never
	// drain the channel don't deadlock the server. Used by streaming tests
	// to synchronize with the server's subscribe step without polling.
	SubscribedCh chan string

	// spawnError is returned by the next Start call when set, then cleared.
	spawnError error
}

type mockProcess struct {
	sessionID string
	workDir   string
	plan      string
	running   bool
	exitErr   error
	output    []claude.OutputLine
	subs      []chan claude.OutputLine
	mu        sync.Mutex
}

// NewMockClaudeRunner creates a mock Claude runner.
func NewMockClaudeRunner() *MockClaudeRunner {
	return &MockClaudeRunner{
		sessions: make(map[string]*mockProcess),
	}
}

func (m *MockClaudeRunner) Start(ctx context.Context, workDir, plan string, resume *string, sessionID string) (string, error) {
	m.mu.Lock()
	injectedErr := m.spawnError
	m.spawnError = nil
	m.mu.Unlock()

	if injectedErr != nil {
		return "", injectedErr
	}

	if m.StartFunc != nil {
		id, err := m.StartFunc(ctx, workDir, plan, resume, sessionID)
		if err == nil {
			m.mu.Lock()
			m.Started = append(m.Started, id)
			m.mu.Unlock()
		}
		return id, err
	}

	id := sessionID
	if id == "" {
		id = fmt.Sprintf("claude-mock-%d", m.counter.Add(1))
	}

	m.mu.Lock()
	m.sessions[id] = &mockProcess{
		sessionID: id,
		workDir:   workDir,
		plan:      plan,
		running:   true,
	}
	m.Started = append(m.Started, id)
	m.mu.Unlock()

	return id, nil
}

// SetSpawnError causes the next Start call to return err. After firing
// once it is cleared.
func (m *MockClaudeRunner) SetSpawnError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spawnError = err
}

// CrashSession simulates a Claude subprocess crash for the given session.
// The session's running flag is cleared, an exit error is recorded (so
// ExitError returns it), and all subscriber channels are closed. Returns
// an error if the session is unknown.
func (m *MockClaudeRunner) CrashSession(sessionID string, exitErr error) error {
	m.mu.RLock()
	p, ok := m.sessions[sessionID]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}

	p.mu.Lock()
	p.running = false
	if exitErr == nil {
		exitErr = fmt.Errorf("claude subprocess crashed")
	}
	p.exitErr = exitErr
	for _, ch := range p.subs {
		close(ch)
	}
	p.subs = nil
	p.mu.Unlock()
	return nil
}

func (m *MockClaudeRunner) Stop(sessionID string) error {
	m.mu.Lock()
	p, ok := m.sessions[sessionID]
	if ok {
		m.Stopped = append(m.Stopped, sessionID)
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}

	p.mu.Lock()
	p.running = false
	for _, ch := range p.subs {
		close(ch)
	}
	p.subs = nil
	p.mu.Unlock()

	return nil
}

func (m *MockClaudeRunner) IsRunning(sessionID string) bool {
	m.mu.RLock()
	p, ok := m.sessions[sessionID]
	m.mu.RUnlock()
	if !ok {
		return false
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running
}

func (m *MockClaudeRunner) ExitError(sessionID string) error {
	m.mu.RLock()
	p, ok := m.sessions[sessionID]
	m.mu.RUnlock()
	if !ok {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitErr
}

func (m *MockClaudeRunner) Subscribe(ctx context.Context, sessionID string) (<-chan claude.OutputLine, error) {
	m.mu.RLock()
	p, ok := m.sessions[sessionID]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("session %s not found", sessionID)
	}

	ch := make(chan claude.OutputLine, 64)
	p.mu.Lock()
	p.subs = append(p.subs, ch)
	p.mu.Unlock()

	if m.SubscribedCh != nil {
		select {
		case m.SubscribedCh <- sessionID:
		default:
		}
	}

	return ch, nil
}

func (m *MockClaudeRunner) History(sessionID string) []claude.OutputLine {
	m.mu.RLock()
	p, ok := m.sessions[sessionID]
	m.mu.RUnlock()
	if !ok {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	result := make([]claude.OutputLine, len(p.output))
	copy(result, p.output)
	return result
}

// EmitOutputLine is a convenience wrapper around EmitOutput that constructs
// an OutputLine with the given text and a current timestamp.
func (m *MockClaudeRunner) EmitOutputLine(sessionID, line string) error {
	return m.EmitOutput(sessionID, claude.OutputLine{
		Text:      line,
		Timestamp: time.Now(),
	})
}

// EmitOutput sends an output line to all subscribers of the given session.
// This is used in tests to simulate Claude producing output.
func (m *MockClaudeRunner) EmitOutput(sessionID string, line claude.OutputLine) error {
	m.mu.RLock()
	p, ok := m.sessions[sessionID]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.output = append(p.output, line)
	for _, ch := range p.subs {
		select {
		case ch <- line:
		default:
		}
	}

	return nil
}
