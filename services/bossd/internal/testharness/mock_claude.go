package testharness

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/recurser/bossd/internal/claude"
)

var _ claude.ClaudeRunner = (*MockClaudeRunner)(nil)

// MockClaudeRunner is a mock ClaudeRunner that simulates Claude processes
// without spawning real subprocesses.
type MockClaudeRunner struct {
	mu       sync.RWMutex
	sessions map[string]*mockProcess
	counter  atomic.Int64

	// StartFunc overrides the default Start behavior when set.
	StartFunc func(ctx context.Context, workDir, plan string, resume *string) (string, error)
}

type mockProcess struct {
	sessionID string
	workDir   string
	plan      string
	running   bool
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

func (m *MockClaudeRunner) Start(ctx context.Context, workDir, plan string, resume *string) (string, error) {
	if m.StartFunc != nil {
		return m.StartFunc(ctx, workDir, plan, resume)
	}

	id := fmt.Sprintf("claude-mock-%d", m.counter.Add(1))

	m.mu.Lock()
	m.sessions[id] = &mockProcess{
		sessionID: id,
		workDir:   workDir,
		plan:      plan,
		running:   true,
	}
	m.mu.Unlock()

	return id, nil
}

func (m *MockClaudeRunner) Stop(sessionID string) error {
	m.mu.RLock()
	p, ok := m.sessions[sessionID]
	m.mu.RUnlock()
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

func (m *MockClaudeRunner) ExitError(_ string) error {
	return nil
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
