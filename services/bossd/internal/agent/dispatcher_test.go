package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"
)

// labeledAgentRunner is a minimal AgentRunner that records the
// "<name>:<sessionID>" tag for each Start call so dispatcher tests can
// assert which underlying runner was routed to. Other AgentRunner methods
// are no-op stubs — the dispatcher only needs to forward them.
type labeledAgentRunner struct {
	name      string
	startSeen atomic.Pointer[string] // captures "<name>:<sessionID>" on Start
}

func newLabeledAgentRunner(name string) *labeledAgentRunner {
	return &labeledAgentRunner{name: name}
}

func (r *labeledAgentRunner) Start(_ context.Context, _, _ string, _ *string, sessionID string) (string, error) {
	tag := r.name + ":" + sessionID
	r.startSeen.Store(&tag)
	return sessionID, nil
}

func (r *labeledAgentRunner) Stop(_ string) error      { return nil }
func (r *labeledAgentRunner) IsRunning(_ string) bool  { return false }
func (r *labeledAgentRunner) ExitError(_ string) error { return nil }
func (r *labeledAgentRunner) Subscribe(_ context.Context, _ string) (<-chan OutputLine, error) {
	return nil, nil
}
func (r *labeledAgentRunner) History(_ string) []OutputLine { return nil }

func TestDispatcher_Start_RoutesToLookupResult(t *testing.T) {
	claudeRunner := newLabeledAgentRunner("claude")
	opencodeRunner := newLabeledAgentRunner("opencode")
	registry := map[string]AgentRunner{
		"claude":   claudeRunner,
		"opencode": opencodeRunner,
	}

	lookup := func(_ string) (string, error) { return "claude", nil }
	d := NewDispatcher(registry, lookup, "claude", zerolog.Nop())

	if _, err := d.Start(context.Background(), "/w", "p", nil, "sid-1"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if seen := claudeRunner.startSeen.Load(); seen == nil || *seen != "claude:sid-1" {
		t.Errorf("claude runner did not see Start; got %v", seen)
	}
	if seen := opencodeRunner.startSeen.Load(); seen != nil {
		t.Errorf("opencode runner unexpectedly saw Start: %q", *seen)
	}
}

func TestDispatcher_Start_RoutesToOpenCode(t *testing.T) {
	claudeRunner := newLabeledAgentRunner("claude")
	opencodeRunner := newLabeledAgentRunner("opencode")
	registry := map[string]AgentRunner{
		"claude":   claudeRunner,
		"opencode": opencodeRunner,
	}

	lookup := func(_ string) (string, error) { return "opencode", nil }
	d := NewDispatcher(registry, lookup, "claude", zerolog.Nop())

	if _, err := d.Start(context.Background(), "/w", "p", nil, "sid-2"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if seen := opencodeRunner.startSeen.Load(); seen == nil || *seen != "opencode:sid-2" {
		t.Errorf("opencode runner did not see Start; got %v", seen)
	}
	if seen := claudeRunner.startSeen.Load(); seen != nil {
		t.Errorf("claude runner unexpectedly saw Start: %q", *seen)
	}
}

func TestDispatcher_Start_FallsBackToDefaultOnEmptyLookup(t *testing.T) {
	claudeRunner := newLabeledAgentRunner("claude")
	opencodeRunner := newLabeledAgentRunner("opencode")
	registry := map[string]AgentRunner{
		"claude":   claudeRunner,
		"opencode": opencodeRunner,
	}

	lookup := func(_ string) (string, error) { return "", nil }
	d := NewDispatcher(registry, lookup, "claude", zerolog.Nop())

	if _, err := d.Start(context.Background(), "/w", "p", nil, "sid-3"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if seen := claudeRunner.startSeen.Load(); seen == nil || *seen != "claude:sid-3" {
		t.Errorf("default (claude) runner did not see Start; got %v", seen)
	}
	if seen := opencodeRunner.startSeen.Load(); seen != nil {
		t.Errorf("opencode runner unexpectedly saw Start: %q", *seen)
	}
}

func TestDispatcher_Start_FallsBackToDefaultOnLookupError(t *testing.T) {
	claudeRunner := newLabeledAgentRunner("claude")
	opencodeRunner := newLabeledAgentRunner("opencode")
	registry := map[string]AgentRunner{
		"claude":   claudeRunner,
		"opencode": opencodeRunner,
	}

	lookup := func(_ string) (string, error) { return "", errors.New("db down") }
	d := NewDispatcher(registry, lookup, "claude", zerolog.Nop())

	if _, err := d.Start(context.Background(), "/w", "p", nil, "sid-4"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if seen := claudeRunner.startSeen.Load(); seen == nil || *seen != "claude:sid-4" {
		t.Errorf("default (claude) runner did not see Start after lookup error; got %v", seen)
	}
	if seen := opencodeRunner.startSeen.Load(); seen != nil {
		t.Errorf("opencode runner unexpectedly saw Start: %q", *seen)
	}
}

func TestDispatcher_Start_UnknownAgentReturnsError(t *testing.T) {
	registry := map[string]AgentRunner{"claude": newLabeledAgentRunner("claude")}

	lookup := func(_ string) (string, error) { return "ghost", nil }
	d := NewDispatcher(registry, lookup, "claude", zerolog.Nop())

	_, err := d.Start(context.Background(), "/w", "p", nil, "sid-5")
	if err == nil {
		t.Fatal("expected Start to error for unknown agent")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error should mention agent name; got %v", err)
	}
	if !errors.Is(err, ErrAgentNotLoaded) {
		t.Errorf("error should wrap ErrAgentNotLoaded; got %v", err)
	}
}

func TestDispatcher_IsRunning_UnknownAgentReturnsFalse(t *testing.T) {
	registry := map[string]AgentRunner{"claude": newLabeledAgentRunner("claude")}

	lookup := func(_ string) (string, error) { return "ghost", nil }
	d := NewDispatcher(registry, lookup, "claude", zerolog.Nop())

	if d.IsRunning("sid-6") {
		t.Error("IsRunning for unknown agent should return false")
	}
}

func TestNewDispatcher_PanicsOnNilLookup(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("NewDispatcher with nil lookup should panic")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("expected panic value to be string, got %T: %v", r, r)
		}
		if !strings.Contains(msg, "lookup must not be nil") {
			t.Errorf("panic message should mention nil lookup; got %q", msg)
		}
	}()

	_ = NewDispatcher(map[string]AgentRunner{}, nil, "claude", zerolog.Nop())
}
