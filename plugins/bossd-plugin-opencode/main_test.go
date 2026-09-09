package main

import (
	"slices"
	"testing"
	"time"

	"github.com/recurser/bossalib/agentruntime"
	"github.com/rs/zerolog"

	sharedplugin "github.com/recurser/bossalib/plugin"
)

// applyOpts builds a Runner from options without wiring agentruntime, so the
// parsed fields can be inspected directly.
func applyOpts(opts ...Option) *Runner {
	r := &Runner{}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// TestRunnerOptsFromEnv proves the BOSS_PLUGIN_* env vars the bossd plugin host
// injects translate into the expected Runner configuration — including the
// permission escape hatch, which flips the always-present permission flag from
// the default --auto to --dangerously-skip-permissions.
func TestRunnerOptsFromEnv(t *testing.T) {
	t.Run("defaults: no env, no probed version → skip-permissions fallback", func(t *testing.T) {
		t.Setenv("BOSS_PLUGIN_model", "")
		t.Setenv("BOSS_PLUGIN_login_shell", "")
		t.Setenv("BOSS_PLUGIN_dangerously_skip_permissions", "")
		r := applyOpts(runnerOptsFromEnv()...)
		if r.model != "" || r.loginShell != "" || r.dangerouslySkipPermissions || r.cliVersion != "" {
			t.Fatalf("unexpected non-default runner: %+v", r)
		}
		// buildArgv never dereferences the embedded agentruntime.Runner, so the
		// field-only Runner from applyOpts is enough to assert the default flag.
		// runnerOptsFromEnv carries no CLI version (the version probe lives in
		// resolveRunnerOpts), so the version-unknown path selects the
		// universally-accepted --dangerously-skip-permissions. Production adds
		// --auto only after probing an opencode >= 1.18 binary.
		argv := r.buildArgv(agentruntime.BuildArgvInput{WorkDir: "/work"})
		if !slices.Contains(argv, "--dangerously-skip-permissions") || slices.Contains(argv, "--auto") {
			t.Errorf("version-unknown default argv should carry --dangerously-skip-permissions, got %v", argv)
		}
	})

	t.Run("all settings wired", func(t *testing.T) {
		t.Setenv("BOSS_PLUGIN_model", "anthropic/claude-sonnet")
		t.Setenv("BOSS_PLUGIN_login_shell", "/opt/homebrew/bin/fish")
		t.Setenv("BOSS_PLUGIN_dangerously_skip_permissions", "true")
		r := applyOpts(runnerOptsFromEnv()...)
		if r.model != "anthropic/claude-sonnet" {
			t.Errorf("model = %q", r.model)
		}
		if r.loginShell != "/opt/homebrew/bin/fish" {
			t.Errorf("loginShell = %q", r.loginShell)
		}
		if !r.dangerouslySkipPermissions {
			t.Error("dangerouslySkipPermissions not set from env")
		}
	})

	t.Run("escape hatch requires the literal string true", func(t *testing.T) {
		t.Setenv("BOSS_PLUGIN_dangerously_skip_permissions", "1")
		r := applyOpts(runnerOptsFromEnv()...)
		if r.dangerouslySkipPermissions {
			t.Error(`only "true" should enable the escape hatch, not "1"`)
		}
	})
}

// TestParentWatchFailsOpenOnStartup is the plan's negative control, run
// against the real production entry point with the real poll interval: a main
// whose parent identity is absent, or present but not describing this process,
// must reach goplugin.Serve rather than exiting during startup. If the
// arm-time guard were wired backwards this test binary would be terminated by
// the watchdog's os.Exit rather than failing an assertion.
func TestParentWatchFailsOpenOnStartup(t *testing.T) {
	t.Run("identity absent", func(t *testing.T) {
		t.Setenv(sharedplugin.ParentPIDEnvVar, "")
		sharedplugin.StartParentWatch(zerolog.Nop())()
	})

	t.Run("identity does not describe this process", func(t *testing.T) {
		t.Setenv(sharedplugin.ParentPIDEnvVar, "999999")
		stop := sharedplugin.StartParentWatch(zerolog.Nop())
		defer stop()
		// Outlive one real poll interval: an armed watcher would fire here.
		time.Sleep(sharedplugin.DefaultParentPollInterval + 500*time.Millisecond)
	})
}
