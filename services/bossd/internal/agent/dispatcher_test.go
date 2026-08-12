package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// labeledAgentRunner is a minimal AgentRunner that records the
// "<name>:<sessionID>" tag for each Start call so dispatcher tests can
// assert which underlying runner was routed to. Other AgentRunner methods
// are no-op stubs — the dispatcher only needs to forward them.
type labeledAgentRunner struct {
	name      string
	startSeen atomic.Pointer[string] // captures "<name>:<sessionID>" on Start
}

type preflightRecordingRunner struct {
	*labeledAgentRunner
	calls   int
	workDir string
	model   string
	env     map[string]string
	profile bossanovav1.HeadlessCapabilityProfile
	err     error
}

func (r *preflightRecordingRunner) PreflightHeadlessCapabilityProfile(_ context.Context, workDir, model string, extraEnv map[string]string, profile bossanovav1.HeadlessCapabilityProfile) error {
	r.calls++
	r.workDir = workDir
	r.model = model
	r.env = extraEnv
	r.profile = profile
	return r.err
}

func newLabeledAgentRunner(name string) *labeledAgentRunner {
	return &labeledAgentRunner{name: name}
}

func (r *labeledAgentRunner) Start(_ context.Context, _, _ string, _ *string, sessionID, _ string, _ map[string]string) (string, error) {
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

	if _, err := d.Start(context.Background(), "/w", "p", nil, "sid-1", "", nil); err != nil {
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

	if _, err := d.Start(context.Background(), "/w", "p", nil, "sid-2", "", nil); err != nil {
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

	if _, err := d.Start(context.Background(), "/w", "p", nil, "sid-3", "", nil); err != nil {
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

	if _, err := d.Start(context.Background(), "/w", "p", nil, "sid-4", "", nil); err != nil {
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

	_, err := d.Start(context.Background(), "/w", "p", nil, "sid-5", "", nil)
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

// TestResolveSingleLoadedAgentOverridesEmptyName proves the dispatcher
// auto-selects the only registered runner when the session has no
// configured agent — even when defaultAgent points to a different (and
// unloaded) name. This is the headline ergonomic for the codex plugin:
// install codex alone with no settings.toml and sessions still start
// instead of failing with "claude not loaded".
func TestResolveSingleLoadedAgentOverridesEmptyName(t *testing.T) {
	codexRunner := newLabeledAgentRunner("codex")
	registry := map[string]AgentRunner{"codex": codexRunner}

	lookup := func(_ string) (string, error) { return "", nil }
	d := NewDispatcher(registry, lookup, "claude", zerolog.Nop())

	if _, err := d.Start(context.Background(), "/w", "p", nil, "sid-solo", "", nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if seen := codexRunner.startSeen.Load(); seen == nil || *seen != "codex:sid-solo" {
		t.Errorf("solo codex runner did not see Start; got %v", seen)
	}
}

// TestResolveMultipleLoadedFallsBackToDefault proves the override only
// triggers when exactly one runner is registered. With two runners the
// default-agent fallback wins so admin intent (configured via settings)
// is preserved instead of randomly picking one.
func TestResolveMultipleLoadedFallsBackToDefault(t *testing.T) {
	claudeRunner := newLabeledAgentRunner("claude")
	codexRunner := newLabeledAgentRunner("codex")
	registry := map[string]AgentRunner{"claude": claudeRunner, "codex": codexRunner}

	lookup := func(_ string) (string, error) { return "", nil }
	d := NewDispatcher(registry, lookup, "claude", zerolog.Nop())

	if _, err := d.Start(context.Background(), "/w", "p", nil, "sid-multi", "", nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if seen := claudeRunner.startSeen.Load(); seen == nil || *seen != "claude:sid-multi" {
		t.Errorf("default (claude) runner did not see Start; got %v", seen)
	}
	if seen := codexRunner.startSeen.Load(); seen != nil {
		t.Errorf("codex runner unexpectedly saw Start: %q", *seen)
	}
}

func TestDispatcher_StartByAgent_RoutesToNamedAgent(t *testing.T) {
	claudeRunner := newLabeledAgentRunner("claude")
	codexRunner := newLabeledAgentRunner("codex")
	registry := map[string]AgentRunner{
		"claude": claudeRunner,
		"codex":  codexRunner,
	}
	d := NewDispatcher(registry, func(string) (string, error) {
		t.Fatalf("lookup must not be called for StartByAgent")
		return "", nil
	}, "claude", zerolog.Nop())

	if _, err := d.StartByAgent(context.Background(), "codex", "/w", "p", nil, "agent-sid-1", "", nil); err != nil {
		t.Fatalf("StartByAgent: %v", err)
	}
	if seen := codexRunner.startSeen.Load(); seen == nil || *seen != "codex:agent-sid-1" {
		t.Errorf("codex did not see Start; got %v", seen)
	}
	if seen := claudeRunner.startSeen.Load(); seen != nil {
		t.Errorf("claude unexpectedly saw Start: %q", *seen)
	}
}

func TestDispatcher_StartByAgent_EmptyNameWithSingleRunnerWins(t *testing.T) {
	codexRunner := newLabeledAgentRunner("codex")
	registry := map[string]AgentRunner{"codex": codexRunner}
	d := NewDispatcher(registry, func(string) (string, error) {
		t.Fatalf("lookup must not be called")
		return "", nil
	}, "claude", zerolog.Nop())

	if _, err := d.StartByAgent(context.Background(), "", "/w", "p", nil, "sid", "", nil); err != nil {
		t.Fatalf("StartByAgent: %v", err)
	}
	if seen := codexRunner.startSeen.Load(); seen == nil || *seen != "codex:sid" {
		t.Errorf("codex did not see Start; got %v", seen)
	}
}

func TestDispatcher_StartByAgent_EmptyNameMultipleRunnersFallsBackToDefault(t *testing.T) {
	claudeRunner := newLabeledAgentRunner("claude")
	codexRunner := newLabeledAgentRunner("codex")
	registry := map[string]AgentRunner{
		"claude": claudeRunner,
		"codex":  codexRunner,
	}
	d := NewDispatcher(registry, func(string) (string, error) {
		t.Fatalf("lookup must not be called")
		return "", nil
	}, "claude", zerolog.Nop())

	if _, err := d.StartByAgent(context.Background(), "", "/w", "p", nil, "sid", "", nil); err != nil {
		t.Fatalf("StartByAgent: %v", err)
	}
	if seen := claudeRunner.startSeen.Load(); seen == nil || *seen != "claude:sid" {
		t.Errorf("default (claude) did not see Start; got %v", seen)
	}
}

func TestDispatcher_StartByAgent_UnknownAgentReturnsError(t *testing.T) {
	registry := map[string]AgentRunner{"claude": newLabeledAgentRunner("claude")}
	d := NewDispatcher(registry, func(string) (string, error) { return "", nil }, "claude", zerolog.Nop())

	_, err := d.StartByAgent(context.Background(), "ghost", "/w", "p", nil, "sid", "", nil)
	if err == nil || !errors.Is(err, ErrAgentNotLoaded) {
		t.Fatalf("expected ErrAgentNotLoaded, got %v", err)
	}
}

func TestDispatcher_PreflightByAgentWithHeadlessCapabilityProfileRoutesToNamedAgent(t *testing.T) {
	codexRunner := &preflightRecordingRunner{labeledAgentRunner: newLabeledAgentRunner("codex")}
	claudeRunner := newLabeledAgentRunner("claude")
	d := NewDispatcher(map[string]AgentRunner{
		"claude": claudeRunner,
		"codex":  codexRunner,
	}, func(string) (string, error) {
		t.Fatalf("lookup must not be called for explicit preflight routing")
		return "", nil
	}, "claude", zerolog.Nop())
	env := map[string]string{"CODEX_HOME": "/managed/codex-home"}

	err := d.PreflightByAgentWithHeadlessCapabilityProfile(
		context.Background(), "codex", "/worktrees/codex-session", "gpt-5-codex", env,
		bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
	)
	if err != nil {
		t.Fatalf("PreflightByAgentWithHeadlessCapabilityProfile: %v", err)
	}
	if codexRunner.calls != 1 {
		t.Fatalf("codex preflight calls = %d, want 1", codexRunner.calls)
	}
	if codexRunner.model != "gpt-5-codex" || codexRunner.env["CODEX_HOME"] != "/managed/codex-home" {
		t.Fatalf("codex preflight target = model=%q env=%v", codexRunner.model, codexRunner.env)
	}
	// The work dir must survive routing: it is what lets the plugin profile the
	// repo-level agent config the gated run will load.
	if codexRunner.workDir != "/worktrees/codex-session" {
		t.Fatalf("codex preflight workDir = %q, want /worktrees/codex-session", codexRunner.workDir)
	}
	if codexRunner.profile != bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1 {
		t.Fatalf("codex profile = %s, want tracker-plan-attachment-v1", codexRunner.profile)
	}
}

func TestDispatcher_PreflightByAgentWithHeadlessCapabilityProfileRejectsUnsupportedRunner(t *testing.T) {
	d := NewDispatcher(map[string]AgentRunner{
		"claude": newLabeledAgentRunner("claude"),
	}, func(string) (string, error) { return "", nil }, "claude", zerolog.Nop())

	err := d.PreflightByAgentWithHeadlessCapabilityProfile(
		context.Background(), "claude", t.TempDir(), "model", nil,
		bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
	)
	if !errors.Is(err, ErrHeadlessCapabilityProfileUnsupported) {
		t.Fatalf("preflight error = %v, want ErrHeadlessCapabilityProfileUnsupported", err)
	}
}

func TestDispatcher_StopByAgent_RoutesToNamedAgent(t *testing.T) {
	stopSeen := make(map[string]string)
	makeRunner := func(name string) AgentRunner {
		return &stopRecordingRunner{name: name, onStop: func(sid string) { stopSeen[name] = sid }}
	}
	registry := map[string]AgentRunner{
		"claude": makeRunner("claude"),
		"codex":  makeRunner("codex"),
	}
	d := NewDispatcher(registry, func(string) (string, error) {
		t.Fatalf("lookup must not be called")
		return "", nil
	}, "claude", zerolog.Nop())

	if err := d.StopByAgent(context.Background(), "codex", "agent-sid-7"); err != nil {
		t.Fatalf("StopByAgent: %v", err)
	}
	if got := stopSeen["codex"]; got != "agent-sid-7" {
		t.Errorf("codex.Stop got %q want %q", got, "agent-sid-7")
	}
	if _, ok := stopSeen["claude"]; ok {
		t.Errorf("claude unexpectedly saw Stop")
	}
}

// TestDispatcher_StopByAgent_BoundsAContextualRunner is the guard on the wedge
// path: a stop issued from a caller that is WAITING on it must be bounded by
// that caller's deadline.
//
// AgentRunner.Stop takes no context, and PluginRunner used to issue its StopRun
// RPC on context.Background(), so an unresponsive plugin blocked the caller
// forever. That is fatal for the failed-bootstrap stop, which runs from a defer
// while StartSession holds the per-target lock.
//
// The runner here never returns until its context is done, so a dispatcher that
// dropped the context would hang this test rather than fail it — the deadline
// below is what turns that into a reported failure instead of a suite timeout.
func TestDispatcher_StopByAgent_BoundsAContextualRunner(t *testing.T) {
	runner := &blockingContextualRunner{}
	d := NewDispatcher(map[string]AgentRunner{"claude": runner}, func(string) (string, error) {
		t.Fatalf("lookup must not be called")
		return "", nil
	}, "claude", zerolog.Nop())

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- d.StopByAgent(ctx, "claude", "agent-sid-7") }()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("StopByAgent err = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StopByAgent did not honor the caller's deadline; an unresponsive plugin would wedge the create path")
	}
	if !runner.sawContext() {
		t.Error("the runner's contextual stop was never used; StopByAgent fell back to the unbounded Stop")
	}
}

// blockingContextualRunner stands in for a plugin that never answers StopRun.
// It implements ContextualStopper, so the dispatcher must route through
// StopWithContext and the caller's deadline must end the call.
type blockingContextualRunner struct {
	mu         sync.Mutex
	contextual bool
}

func (r *blockingContextualRunner) StopWithContext(ctx context.Context, _ string) error {
	r.mu.Lock()
	r.contextual = true
	r.mu.Unlock()
	<-ctx.Done()
	return ctx.Err()
}

func (r *blockingContextualRunner) sawContext() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.contextual
}

func (r *blockingContextualRunner) Start(_ context.Context, _, _ string, _ *string, sid, _ string, _ map[string]string) (string, error) {
	return sid, nil
}

// Stop is the unbounded legacy path. Reaching it is the failure this test
// detects, so make that unmistakable rather than merely slow.
func (r *blockingContextualRunner) Stop(_ string) error      { select {} }
func (r *blockingContextualRunner) IsRunning(_ string) bool  { return false }
func (r *blockingContextualRunner) ExitError(_ string) error { return nil }
func (r *blockingContextualRunner) Subscribe(_ context.Context, _ string) (<-chan OutputLine, error) {
	return nil, nil
}
func (r *blockingContextualRunner) History(_ string) []OutputLine { return nil }

// stopRecordingRunner is a minimal AgentRunner that records the sessionID
// passed to Stop. Other methods are no-ops.
type stopRecordingRunner struct {
	name   string
	onStop func(sessionID string)
}

func (r *stopRecordingRunner) Start(_ context.Context, _, _ string, _ *string, sid, _ string, _ map[string]string) (string, error) {
	return sid, nil
}
func (r *stopRecordingRunner) Stop(sid string) error    { r.onStop(sid); return nil }
func (r *stopRecordingRunner) IsRunning(_ string) bool  { return false }
func (r *stopRecordingRunner) ExitError(_ string) error { return nil }
func (r *stopRecordingRunner) Subscribe(_ context.Context, _ string) (<-chan OutputLine, error) {
	return nil, nil
}
func (r *stopRecordingRunner) History(_ string) []OutputLine { return nil }

func TestDispatcher_IsRunningByAgent_RoutesAndReturnsTrue(t *testing.T) {
	registry := map[string]AgentRunner{
		"claude": &alwaysRunningRunner{running: false},
		"codex":  &alwaysRunningRunner{running: true},
	}
	d := NewDispatcher(registry, func(string) (string, error) {
		t.Fatalf("lookup must not be called")
		return "", nil
	}, "claude", zerolog.Nop())

	if !d.IsRunningByAgent("codex", "any") {
		t.Errorf("expected IsRunningByAgent(codex)=true")
	}
	if d.IsRunningByAgent("claude", "any") {
		t.Errorf("expected IsRunningByAgent(claude)=false")
	}
}

type alwaysRunningRunner struct{ running bool }

func (r *alwaysRunningRunner) Start(_ context.Context, _, _ string, _ *string, sid, _ string, _ map[string]string) (string, error) {
	return sid, nil
}
func (r *alwaysRunningRunner) Stop(_ string) error      { return nil }
func (r *alwaysRunningRunner) IsRunning(_ string) bool  { return r.running }
func (r *alwaysRunningRunner) ExitError(_ string) error { return nil }
func (r *alwaysRunningRunner) Subscribe(_ context.Context, _ string) (<-chan OutputLine, error) {
	return nil, nil
}
func (r *alwaysRunningRunner) History(_ string) []OutputLine { return nil }

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
