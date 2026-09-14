package plugin

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.uber.org/goleak"

	"github.com/recurser/bossalib/config"
	sharedplugin "github.com/recurser/bossalib/plugin"
	"github.com/recurser/bossd/internal/plugin/eventbus"
)

func testHost() *Host {
	bus := eventbus.New(zerolog.Nop())
	return New(bus, nil, zerolog.Nop())
}

func TestPluginEnvFromConfig(t *testing.T) {
	cfg := config.PluginConfig{
		Name: "claude",
		Config: map[string]string{
			"dangerously_skip_permissions": "true",
			"some_other_key":               "value",
		},
	}
	env := pluginEnvFromConfig(cfg)

	got := map[string]string{}
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("malformed env entry %q", kv)
		}
		got[k] = v
	}
	if got["BOSS_PLUGIN_dangerously_skip_permissions"] != "true" {
		t.Errorf("BOSS_PLUGIN_dangerously_skip_permissions = %q, want %q", got["BOSS_PLUGIN_dangerously_skip_permissions"], "true")
	}
	if got["BOSS_PLUGIN_some_other_key"] != "value" {
		t.Errorf("BOSS_PLUGIN_some_other_key = %q, want %q", got["BOSS_PLUGIN_some_other_key"], "value")
	}
}

func TestPluginEnvFromConfig_EmptyConfig(t *testing.T) {
	cfg := config.PluginConfig{Name: "claude"}
	if env := pluginEnvFromConfig(cfg); len(env) != 0 {
		t.Errorf("pluginEnvFromConfig with nil Config = %v, want empty", env)
	}
}

func TestInjectLoginShell_AgentPluginsOnly(t *testing.T) {
	settings := config.Settings{
		LoginShell:          "/opt/homebrew/bin/fish",
		KnownAgentProviders: []string{"opencode"},
	}

	base := map[string]string{"x": "1"}
	cfg := injectLoginShell("codex", base, settings)
	if cfg["login_shell"] != "/opt/homebrew/bin/fish" {
		t.Fatalf("codex should get login_shell, got %q", cfg["login_shell"])
	}
	if cfg["x"] != "1" {
		t.Fatalf("existing keys preserved")
	}
	if base["login_shell"] != "" {
		t.Fatalf("source config map must not be mutated")
	}
	if got := injectLoginShell("claude", nil, settings); got["login_shell"] != "/opt/homebrew/bin/fish" {
		t.Fatalf("claude should get login_shell, got %q", got["login_shell"])
	}
	if got := injectLoginShell("opencode", nil, settings); got["login_shell"] != "/opt/homebrew/bin/fish" {
		t.Fatalf("known agent provider should get login_shell, got %q", got["login_shell"])
	}
	if got := injectLoginShell("dependabot", map[string]string{}, settings); got["login_shell"] != "" {
		t.Fatalf("non-agent plugin must not get login_shell")
	}
	// empty login shell -> no key (daemon started from a full shell, passthrough)
	if got := injectLoginShell("codex", map[string]string{}, config.Settings{}); got["login_shell"] != "" {
		t.Fatalf("empty login shell must not inject the key")
	}
}

func TestPreparePluginConfigForStartInjectsLoginShellBeforeEnvProjection(t *testing.T) {
	settings := config.Settings{
		LoginShell:          "/opt/homebrew/bin/fish",
		KnownAgentProviders: []string{"opencode"},
	}
	cfg := preparePluginConfigForStart(config.PluginConfig{
		Name: "opencode",
		Config: map[string]string{
			"x": "1",
		},
	}, settings)

	env := map[string]string{}
	for _, kv := range pluginEnvFromConfig(cfg) {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("malformed env entry %q", kv)
		}
		env[k] = v
	}
	if env["BOSS_PLUGIN_login_shell"] != "/opt/homebrew/bin/fish" {
		t.Fatalf("BOSS_PLUGIN_login_shell = %q, want %q", env["BOSS_PLUGIN_login_shell"], "/opt/homebrew/bin/fish")
	}
	if env["BOSS_PLUGIN_x"] != "1" {
		t.Fatalf("BOSS_PLUGIN_x = %q, want %q", env["BOSS_PLUGIN_x"], "1")
	}
	if env["BOSS_PLUGIN_"+config.SessionStartReadyDeadlinePluginKey] != "45" {
		t.Fatalf("BOSS_PLUGIN_%s = %q, want %q",
			config.SessionStartReadyDeadlinePluginKey,
			env["BOSS_PLUGIN_"+config.SessionStartReadyDeadlinePluginKey], "45")
	}

	nonAgent := preparePluginConfigForStart(config.PluginConfig{Name: "linear"}, settings)
	nonAgentEnv := map[string]string{}
	for _, kv := range pluginEnvFromConfig(nonAgent) {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("malformed env entry %q", kv)
		}
		nonAgentEnv[k] = v
		if strings.HasPrefix(kv, "BOSS_PLUGIN_login_shell=") {
			t.Fatalf("non-agent plugin must not project login_shell env, got %q", kv)
		}
	}
	if nonAgentEnv["BOSS_PLUGIN_"+config.SessionStartReadyDeadlinePluginKey] != "45" {
		t.Fatalf("non-agent plugin readiness env = %q, want %q",
			nonAgentEnv["BOSS_PLUGIN_"+config.SessionStartReadyDeadlinePluginKey], "45")
	}
}

func TestPreparePluginConfigForStartProjectsSessionStartReadiness(t *testing.T) {
	settings := config.Settings{
		TmuxDelivery: config.TmuxDeliveryConfig{SessionStartReadyDeadlineSeconds: 300},
	}
	sourceConfig := map[string]string{
		"x": config.SessionStartReadyDeadlinePluginKey,
		config.SessionStartReadyDeadlinePluginKey: "99",
	}
	cfg := preparePluginConfigForStart(config.PluginConfig{
		Name:   "repair",
		Config: sourceConfig,
	}, settings)

	if sourceConfig[config.SessionStartReadyDeadlinePluginKey] != "99" {
		t.Fatal("preparePluginConfigForStart mutated the caller's config map")
	}
	if cfg.Config["x"] != config.SessionStartReadyDeadlinePluginKey {
		t.Fatalf("unrelated config entry = %q, want preserved", cfg.Config["x"])
	}
	if cfg.Config[config.SessionStartReadyDeadlinePluginKey] != "300" {
		t.Fatalf("%s = %q, want host-resolved 300",
			config.SessionStartReadyDeadlinePluginKey, cfg.Config[config.SessionStartReadyDeadlinePluginKey])
	}

	env := map[string]string{}
	for _, kv := range pluginEnvFromConfig(cfg) {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("malformed env entry %q", kv)
		}
		env[k] = v
	}
	if env["BOSS_PLUGIN_"+config.SessionStartReadyDeadlinePluginKey] != "300" {
		t.Fatalf("projected env = %q, want 300", env["BOSS_PLUGIN_"+config.SessionStartReadyDeadlinePluginKey])
	}
}

func TestStartEmptyPlugins(t *testing.T) {
	h := testHost()

	if err := h.Start(t.Context(), nil, config.Settings{}); err != nil {
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

	if err := h.Start(t.Context(), []config.PluginConfig{}, config.Settings{}); err != nil {
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

	if err := h.Start(t.Context(), cfgs, config.Settings{}); err != nil {
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

	if err := h.Start(t.Context(), cfgs, config.Settings{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if n := len(h.Plugins()); n != 0 {
		t.Errorf("expected 0 plugins, got %d", n)
	}

	if err := h.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestAllPluginsSurfacesMissesAndLoaded verifies that AllPlugins (the
// `boss plugin list` data source) reports every configured plugin —
// disabled and failed alongside loaded — so an operator with a typo'd
// path can spot the problem without grepping daemon logs.
func TestAllPluginsSurfacesMissesAndLoaded(t *testing.T) {
	h := testHost()

	cfgs := []config.PluginConfig{
		{Name: "off-plugin", Path: "/whatever", Enabled: false},
		{Name: "broken-plugin", Path: "/nonexistent/binary", Enabled: true},
	}

	if err := h.Start(t.Context(), cfgs, config.Settings{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop() })

	all := h.AllPlugins()
	if len(all) != 2 {
		t.Fatalf("AllPlugins() = %d entries, want 2: %+v", len(all), all)
	}

	byName := map[string]PluginStatus{}
	for _, p := range all {
		byName[p.Name] = p
	}

	off, ok := byName["off-plugin"]
	if !ok {
		t.Fatalf("off-plugin missing from AllPlugins")
	}
	if off.Enabled || off.Loaded || off.Error != "" {
		t.Errorf("off-plugin: want disabled non-loaded no-error, got %+v", off)
	}

	bad, ok := byName["broken-plugin"]
	if !ok {
		t.Fatalf("broken-plugin missing from AllPlugins")
	}
	if !bad.Enabled || bad.Loaded || bad.Error == "" {
		t.Errorf("broken-plugin: want enabled non-loaded with error, got %+v", bad)
	}

	if got := h.Plugins(); len(got) != 0 {
		t.Errorf("Plugins() leaked misses: got %d entries, want 0", len(got))
	}
}

func TestStopIdempotent(t *testing.T) {
	h := testHost()

	if err := h.Start(t.Context(), nil, config.Settings{}); err != nil {
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

	if err := h.Start(t.Context(), cfgs, config.Settings{}); err != nil {
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

	if err := h.Start(context.Background(), cfgs, config.Settings{}); err != nil {
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

	if err := h.Start(t.Context(), nil, config.Settings{}); err != nil {
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

	if err := h.Start(t.Context(), cfgs, config.Settings{}); err != nil {
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
	if err := h.Start(t.Context(), nil, config.Settings{}); err != nil {
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

	const killTimeout = 50 * time.Millisecond
	start := time.Now()
	killWithTimeout(zerolog.Nop(), "test-plugin", stuckKill, pid, killTimeout)
	elapsed := time.Since(start)

	if elapsed < killTimeout {
		t.Fatalf("killWithTimeout returned too early: %v", elapsed)
	}
	// 40x the kill timeout, and well inside the subprocess's 60s lifetime: a fallback that
	// never fired would leave this waiting on the sleep itself.
	if elapsed > 40*killTimeout {
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
	err := h.Start(t.Context(), cfgs, config.Settings{})
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
	if err := h.Start(t.Context(), cfgs, config.Settings{}); err != nil {
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

	const killTimeout = time.Second
	start := time.Now()
	killWithTimeout(zerolog.Nop(), "test-plugin", quickKill, 0, killTimeout)
	elapsed := time.Since(start)

	select {
	case <-done:
	default:
		t.Fatal("kill func was not invoked")
	}
	// A tenth of the fallback deadline: waiting that deadline out is the failure this catches.
	if elapsed > killTimeout/10 {
		t.Fatalf("expected fast return, took %v", elapsed)
	}
}

// effectiveEnv collapses a raw environment slice the way os/exec does before
// exec: later entries win (dedupEnvCase builds its output in reverse "to
// preserve the last occurrence of each key").
func effectiveEnv(entries []string) map[string]string {
	out := map[string]string{}
	for _, kv := range entries {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}

// TestLaunchPluginEnvStampsHostPIDWithoutConfig covers the case the old code
// skipped outright: pluginEnvFromConfig returns nil for a plugin with no
// config, so cmd.Env was left unset and nothing was ever stamped. Most plugins
// carry no config, so this was the majority path.
func TestLaunchPluginEnvStampsHostPIDWithoutConfig(t *testing.T) {
	env := effectiveEnv(pluginSubprocessEnv(config.PluginConfig{Name: "linear"}, 4242))

	if got := env[sharedplugin.ParentPIDEnvVar]; got != "4242" {
		t.Fatalf("%s = %q, want %q", sharedplugin.ParentPIDEnvVar, got, "4242")
	}
}

// TestLaunchPluginEnvStampsHostPIDWithConfig pins that the stamp is additive:
// the BOSS_PLUGIN_* projection still reaches the child alongside it.
func TestLaunchPluginEnvStampsHostPIDWithConfig(t *testing.T) {
	cfg := config.PluginConfig{
		Name:   "claude",
		Config: map[string]string{"dangerously_skip_permissions": "true"},
	}
	env := effectiveEnv(pluginSubprocessEnv(cfg, 4242))

	if got := env[sharedplugin.ParentPIDEnvVar]; got != "4242" {
		t.Errorf("%s = %q, want %q", sharedplugin.ParentPIDEnvVar, got, "4242")
	}
	if got := env["BOSS_PLUGIN_dangerously_skip_permissions"]; got != "true" {
		t.Errorf("BOSS_PLUGIN_dangerously_skip_permissions = %q, want %q", got, "true")
	}
}

// TestLaunchPluginEnvUsesHostProcessPID pins that launchPlugin stamps the
// daemon's own PID — the value the plugin-side watchdog compares its
// os.Getppid against — rather than some other identifier.
func TestLaunchPluginEnvUsesHostProcessPID(t *testing.T) {
	env := effectiveEnv(pluginSubprocessEnv(config.PluginConfig{Name: "linear"}, os.Getpid()))

	want := strconv.Itoa(os.Getpid())
	if got := env[sharedplugin.ParentPIDEnvVar]; got != want {
		t.Fatalf("%s = %q, want the host PID %q", sharedplugin.ParentPIDEnvVar, got, want)
	}
}

// TestLaunchPluginEnvPreservesHostInheritance pins that stamping did not
// replace the host environment the child has always inherited.
func TestLaunchPluginEnvPreservesHostInheritance(t *testing.T) {
	t.Setenv("BOSS_TEST_INHERITED_MARKER", "inherited-value")

	env := effectiveEnv(pluginSubprocessEnv(config.PluginConfig{Name: "linear"}, 4242))

	if got := env["BOSS_TEST_INHERITED_MARKER"]; got != "inherited-value" {
		t.Fatalf("BOSS_TEST_INHERITED_MARKER = %q, want %q — host environment inheritance regressed",
			got, "inherited-value")
	}
}

// TestLaunchPluginEnvSurvivesGoPluginSecondEnvironAppend is the catastrophic
// case in env form. go-plugin appends os.Environ() to cmd.Env AFTER we build
// it (SkipHostEnv is false) and os/exec keeps the last duplicate, so a
// same-named variable already in bossd's own environment would otherwise win
// and hand every plugin a foreign parent identity. Simulate that second append
// exactly and assert our stamp is still what the child sees.
func TestLaunchPluginEnvSurvivesGoPluginSecondEnvironAppend(t *testing.T) {
	t.Setenv(sharedplugin.ParentPIDEnvVar, "999999")

	built := pluginSubprocessEnv(config.PluginConfig{Name: "linear"}, 4242)

	// The host must no longer carry the variable, or go-plugin's append would
	// reintroduce the stale value.
	if v, ok := os.LookupEnv(sharedplugin.ParentPIDEnvVar); ok {
		t.Fatalf("host environment still carries %s=%q; go-plugin's os.Environ() append would shadow the stamp",
			sharedplugin.ParentPIDEnvVar, v)
	}

	// Replay go-plugin client.go's `cmd.Env = append(cmd.Env, os.Environ()...)`.
	asGoPluginSeesIt := append(append([]string(nil), built...), os.Environ()...)
	if got := effectiveEnv(asGoPluginSeesIt)[sharedplugin.ParentPIDEnvVar]; got != "4242" {
		t.Fatalf("%s = %q after go-plugin's second os.Environ() append, want %q",
			sharedplugin.ParentPIDEnvVar, got, "4242")
	}

	// And exactly one entry survives dedup ambiguity in the raw slice we own.
	count := 0
	for _, kv := range built {
		if strings.HasPrefix(kv, sharedplugin.ParentPIDEnvVar+"=") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("built env carries %d %s entries, want exactly 1", count, sharedplugin.ParentPIDEnvVar)
	}
}
