package plugin

import (
	"context"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.uber.org/goleak"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossd/internal/plugin/eventbus"
)

func testHost() *Host {
	bus := eventbus.New(zerolog.Nop())
	return New(bus, nil, zerolog.Nop())
}

func TestStartEmptyPlugins(t *testing.T) {
	h := testHost()

	if err := h.Start(t.Context(), nil); err != nil {
		t.Fatalf("Start with nil plugins: %v", err)
	}

	statuses := h.Plugins()
	if len(statuses) != 0 {
		t.Errorf("expected 0 plugins, got %d", len(statuses))
	}

	if err := h.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestStartEmptySlice(t *testing.T) {
	h := testHost()

	if err := h.Start(t.Context(), []config.PluginConfig{}); err != nil {
		t.Fatalf("Start with empty slice: %v", err)
	}

	statuses := h.Plugins()
	if len(statuses) != 0 {
		t.Errorf("expected 0 plugins, got %d", len(statuses))
	}

	if err := h.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestStartDisabledPluginSkipped(t *testing.T) {
	h := testHost()

	cfgs := []config.PluginConfig{
		{
			Name:    "disabled-plugin",
			Path:    "/nonexistent/binary",
			Enabled: false,
		},
	}

	if err := h.Start(t.Context(), cfgs); err != nil {
		t.Fatalf("Start with disabled plugin: %v", err)
	}

	statuses := h.Plugins()
	if len(statuses) != 0 {
		t.Errorf("expected 0 running plugins (disabled was skipped), got %d", len(statuses))
	}

	if err := h.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestStartMultipleDisabledPlugins(t *testing.T) {
	h := testHost()

	cfgs := []config.PluginConfig{
		{Name: "plugin-a", Path: "/nonexistent/a", Enabled: false},
		{Name: "plugin-b", Path: "/nonexistent/b", Enabled: false},
		{Name: "plugin-c", Path: "/nonexistent/c", Enabled: false},
	}

	if err := h.Start(t.Context(), cfgs); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if n := len(h.Plugins()); n != 0 {
		t.Errorf("expected 0 plugins, got %d", n)
	}

	if err := h.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestStopIdempotent(t *testing.T) {
	h := testHost()

	if err := h.Start(t.Context(), nil); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := h.Stop(); err != nil {
		t.Fatalf("first Stop: %v", err)
	}

	// Second stop should not panic or error.
	if err := h.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestStopWithoutStart(t *testing.T) {
	h := testHost()

	// Stop without Start should not panic or error.
	if err := h.Stop(); err != nil {
		t.Fatalf("Stop without Start: %v", err)
	}
}

func TestPluginsReturnsEmptyBeforeStart(t *testing.T) {
	h := testHost()

	statuses := h.Plugins()
	if len(statuses) != 0 {
		t.Errorf("expected 0 plugins before Start, got %d", len(statuses))
	}
}

// TestStartEnabledPluginInvalidPath pins the log-and-continue contract: a
// single plugin with an unreachable binary must not abort Start — the daemon
// is expected to log the failure and finish its startup sequence so one
// broken plugin on disk cannot DOS the whole host.
func TestStartEnabledPluginInvalidPath(t *testing.T) {
	h := testHost()

	cfgs := []config.PluginConfig{
		{
			Name:    "bad-plugin",
			Path:    "/nonexistent/plugin/binary",
			Enabled: true,
		},
	}

	if err := h.Start(t.Context(), cfgs); err != nil {
		t.Fatalf("Start should skip bad plugins, not fail: %v", err)
	}

	// Bad plugin is skipped, so nothing should be tracked as running.
	if n := len(h.Plugins()); n != 0 {
		t.Errorf("expected 0 running plugins (bad was skipped), got %d", n)
	}

	if err := h.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestStartMixedEnabledDisabled pairs an enabled-but-broken plugin with two
// disabled siblings. Start must log the broken plugin, skip it, and still
// return successfully — disabled plugins were already being skipped; this
// guards the full "skip and continue" path with multiple config entries.
func TestStartMixedEnabledDisabled(t *testing.T) {
	h := testHost()

	cfgs := []config.PluginConfig{
		{Name: "disabled-first", Path: "/nonexistent/a", Enabled: false},
		{Name: "bad-enabled", Path: "/nonexistent/b", Enabled: true},
		{Name: "disabled-last", Path: "/nonexistent/c", Enabled: false},
	}

	if err := h.Start(context.Background(), cfgs); err != nil {
		t.Fatalf("Start should skip bad plugin, not fail: %v", err)
	}

	if n := len(h.Plugins()); n != 0 {
		t.Errorf("expected 0 running plugins, got %d", n)
	}

	if err := h.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestGetTaskSourcesEmptyBeforeStart(t *testing.T) {
	h := testHost()

	sources := h.GetTaskSources()
	if len(sources) != 0 {
		t.Errorf("expected 0 task sources before start, got %d", len(sources))
	}
}

func TestGetTaskSourcesEmptyWithNoPlugins(t *testing.T) {
	h := testHost()

	if err := h.Start(t.Context(), nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = h.Stop() }()

	sources := h.GetTaskSources()
	if len(sources) != 0 {
		t.Errorf("expected 0 task sources with no plugins, got %d", len(sources))
	}
}

func TestGetTaskSourcesEmptyWithDisabledPlugins(t *testing.T) {
	h := testHost()

	cfgs := []config.PluginConfig{
		{Name: "disabled-a", Path: "/nonexistent/a", Enabled: false},
		{Name: "disabled-b", Path: "/nonexistent/b", Enabled: false},
	}

	if err := h.Start(t.Context(), cfgs); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = h.Stop() }()

	// Disabled plugins are never started, so no task sources.
	sources := h.GetTaskSources()
	if len(sources) != 0 {
		t.Errorf("expected 0 task sources with disabled plugins, got %d", len(sources))
	}
}

// TestStopNoGoroutineLeak asserts the health-check loop exits cleanly when
// Stop is called. Regression test for the shutdown wait-group work and the
// pingAll mutex fix: if pingAll held the lock across an RPC, Stop would
// deadlock and goleak would flag the orphaned health-check goroutine.
func TestStopNoGoroutineLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	h := testHost()
	if err := h.Start(t.Context(), nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := h.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestKillWithTimeoutForceKillsStuckProcess verifies the fallback path: when
// the supplied kill func hangs, the PID is SIGKILL'd directly so daemon
// shutdown cannot be held hostage by a stuck plugin.
func TestKillWithTimeoutForceKillsStuckProcess(t *testing.T) {
	// Spawn a long-lived subprocess. `sleep 60` will not return on its own
	// within the test's budget; if the fallback SIGKILL doesn't fire, the
	// test hangs.
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn sleep subprocess: %v", err)
	}
	pid := cmd.Process.Pid

	// Simulate a Kill that never returns — this is the pathological case
	// killWithTimeout must defend against.
	stuckKill := func() { select {} }

	start := time.Now()
	killWithTimeout(zerolog.Nop(), "test-plugin", stuckKill, pid, 50*time.Millisecond)
	elapsed := time.Since(start)

	if elapsed < 50*time.Millisecond {
		t.Fatalf("killWithTimeout returned too early: %v", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("killWithTimeout took too long to fall through to SIGKILL: %v", elapsed)
	}

	// Wait for the OS to reap the subprocess, then assert it was signalled.
	err := cmd.Wait()
	if err == nil {
		t.Fatal("expected Wait to return an error after SIGKILL")
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("Wait error is not *exec.ExitError: %T (%v)", err, err)
	}
	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("exit status is not syscall.WaitStatus: %T", exitErr.Sys())
	}
	if !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("expected SIGKILL, got signaled=%v signal=%v", ws.Signaled(), ws.Signal())
	}
}

// TestStartRejectsUnwiredHostService asserts that Start surfaces missing
// HostService deps as a startup error rather than deferring the failure to
// the first in-flight plugin RPC. This is the main guarantee of Validate().
func TestStartRejectsUnwiredHostService(t *testing.T) {
	// Using a non-nil provider forces New to construct a HostServiceServer
	// with every other dep still nil — the exact misconfiguration Validate
	// is meant to catch. We don't need a real VCS provider; the validation
	// runs before the provider is ever used.
	provider := &mockVCSProvider{}
	bus := eventbus.New(zerolog.Nop())
	h := New(bus, provider, zerolog.Nop())

	cfgs := []config.PluginConfig{
		{Name: "fake-plugin", Path: "/nonexistent", Enabled: true},
	}
	err := h.Start(t.Context(), cfgs)
	if err == nil {
		t.Fatal("expected Start to reject unwired HostService")
	}
	// Spot-check the error names at least one missing dep so future
	// regressions don't silently return a misleading message.
	if msg := err.Error(); msg == "" ||
		(!strings.Contains(msg, "missing dependencies") &&
			!strings.Contains(msg, "not configured")) {
		t.Fatalf("error does not describe missing deps: %q", msg)
	}
	if err := h.Stop(); err != nil {
		t.Fatalf("Stop after rejected Start: %v", err)
	}
}

// TestStartSkipsValidationWithoutEnabledPlugins documents the carve-out:
// if no enabled plugin could ever call back into HostService, we don't
// fail Start — otherwise unit tests that spin up a Host with no plugins
// would require wiring every dep.
func TestStartSkipsValidationWithoutEnabledPlugins(t *testing.T) {
	provider := &mockVCSProvider{}
	bus := eventbus.New(zerolog.Nop())
	h := New(bus, provider, zerolog.Nop())

	cfgs := []config.PluginConfig{
		{Name: "disabled", Path: "/nonexistent", Enabled: false},
	}
	if err := h.Start(t.Context(), cfgs); err != nil {
		t.Fatalf("Start with disabled plugin should skip validation: %v", err)
	}
	if err := h.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestKillWithTimeoutReturnsWhenKillCompletes asserts the fast path: if the
// supplied kill func finishes before the deadline, the fallback is not
// invoked and the function returns promptly.
func TestKillWithTimeoutReturnsWhenKillCompletes(t *testing.T) {
	done := make(chan struct{})
	quickKill := func() { close(done) }

	start := time.Now()
	killWithTimeout(zerolog.Nop(), "test-plugin", quickKill, 0, time.Second)
	elapsed := time.Since(start)

	select {
	case <-done:
	default:
		t.Fatal("kill func was not invoked")
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("expected fast return, took %v", elapsed)
	}
}
