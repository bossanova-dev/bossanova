package testharness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/recurser/bossd/internal/agent"
)

var (
	_ agent.AgentRunner     = (*MockAgentRunner)(nil)
	_ agent.AgentDispatcher = (*MockAgentRunner)(nil)
)

// MockAgentRunner is a mock AgentRunner that simulates the coding-agent subprocess
// without spawning real subprocesses.
type MockAgentRunner struct {
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
	output    []agent.OutputLine
	subs      []chan agent.OutputLine
	mu        sync.Mutex
}

// NewMockAgentRunner creates a mock Claude runner.
func NewMockAgentRunner() *MockAgentRunner {
	return &MockAgentRunner{
		sessions: make(map[string]*mockProcess),
	}
}

// WithChanges configures the runner so that the next Start call writes
// filename (relative to the workDir supplied to Start) with the given content
// before "exiting" cleanly. The session process is registered and marked
// running=false immediately so the lifecycle sees a clean exit without blocking.
//
// WithChanges wires into StartFunc, replacing any previous StartFunc value.
// Call NoChanges() to reset to the default (do-nothing) behavior.
func (m *MockAgentRunner) WithChanges(filename, content string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.StartFunc = func(ctx context.Context, workDir, plan string, resume *string, sessionID string) (string, error) {
		id := sessionID
		if id == "" {
			id = fmt.Sprintf("claude-mock-%d", m.counter.Add(1))
		}
		// Write the file into the worktree.
		dest := filepath.Join(workDir, filename)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return "", fmt.Errorf("mock claude: mkdir %s: %w", filepath.Dir(dest), err)
		}
		if err := os.WriteFile(dest, []byte(content), 0o644); err != nil {
			return "", fmt.Errorf("mock claude: write %s: %w", dest, err)
		}
		// Register the session in completed state (not running).
		m.mu.Lock()
		m.sessions[id] = &mockProcess{
			sessionID: id,
			workDir:   workDir,
			plan:      plan,
			running:   false,
		}
		m.mu.Unlock()
		return id, nil
	}
}

// WithRunningSession resets StartFunc to nil so the next Start call uses the
// default behavior: register the process as running=true and return
// immediately. This is useful in overlap tests where the first session must
// stay alive to block the second tick.
func (m *MockAgentRunner) WithRunningSession() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.StartFunc = nil
}

// NoChanges configures the runner so that Start exits cleanly without
// touching the worktree. This is the default when no StartFunc is set, but
// NoChanges makes the intent explicit and clears any previous WithChanges.
func (m *MockAgentRunner) NoChanges() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.StartFunc = func(ctx context.Context, workDir, plan string, resume *string, sessionID string) (string, error) {
		id := sessionID
		if id == "" {
			id = fmt.Sprintf("claude-mock-%d", m.counter.Add(1))
		}
		m.mu.Lock()
		m.sessions[id] = &mockProcess{
			sessionID: id,
			workDir:   workDir,
			plan:      plan,
			running:   false,
		}
		m.mu.Unlock()
		return id, nil
	}
}

func (m *MockAgentRunner) Start(ctx context.Context, workDir, plan string, resume *string, sessionID string) (string, error) {
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
func (m *MockAgentRunner) SetSpawnError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spawnError = err
}

// CrashSession simulates a Claude subprocess crash for the given session.
// The session's running flag is cleared, an exit error is recorded (so
// ExitError returns it), and all subscriber channels are closed. Returns
// an error if the session is unknown.
func (m *MockAgentRunner) CrashSession(sessionID string, exitErr error) error {
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

func (m *MockAgentRunner) Stop(sessionID string) error {
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

func (m *MockAgentRunner) IsRunning(sessionID string) bool {
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

func (m *MockAgentRunner) ExitError(sessionID string) error {
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

func (m *MockAgentRunner) Subscribe(ctx context.Context, sessionID string) (<-chan agent.OutputLine, error) {
	m.mu.RLock()
	p, ok := m.sessions[sessionID]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("session %s not found", sessionID)
	}

	ch := make(chan agent.OutputLine, 64)
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

// StartByAgent forwards to Start so existing assertions on Started/Stopped
// still fire. By-agent routing is exercised by the dispatcher tests; this
// fake doesn't need to inspect agentName.
func (m *MockAgentRunner) StartByAgent(ctx context.Context, _, workDir, plan string, resume *string, agentSessionID string) (string, error) {
	return m.Start(ctx, workDir, plan, resume, agentSessionID)
}

// StopByAgent forwards to Stop, ignoring agentName (see StartByAgent).
func (m *MockAgentRunner) StopByAgent(_, agentSessionID string) error {
	return m.Stop(agentSessionID)
}

// IsRunningByAgent forwards to IsRunning, ignoring agentName (see StartByAgent).
func (m *MockAgentRunner) IsRunningByAgent(_, agentSessionID string) bool {
	return m.IsRunning(agentSessionID)
}

func (m *MockAgentRunner) History(sessionID string) []agent.OutputLine {
	m.mu.RLock()
	p, ok := m.sessions[sessionID]
	m.mu.RUnlock()
	if !ok {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	result := make([]agent.OutputLine, len(p.output))
	copy(result, p.output)
	return result
}

// EmitOutputLine is a convenience wrapper around EmitOutput that constructs
// an OutputLine with the given text and a current timestamp.
func (m *MockAgentRunner) EmitOutputLine(sessionID, line string) error {
	return m.EmitOutput(sessionID, agent.OutputLine{
		Text:      line,
		Timestamp: time.Now(),
	})
}

// EmitOutput sends an output line to all subscribers of the given session.
// This is used in tests to simulate Claude producing output.
func (m *MockAgentRunner) EmitOutput(sessionID string, line agent.OutputLine) error {
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
