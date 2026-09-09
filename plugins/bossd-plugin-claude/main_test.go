package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"

	sharedplugin "github.com/recurser/bossalib/plugin"
	libskillinstall "github.com/recurser/bossalib/skillinstall"
	"github.com/recurser/bossd-plugin-claude/skilldata"
)

func TestRunnerOptsFromEnv_SkipPermissionsTrue(t *testing.T) {
	t.Setenv("BOSS_PLUGIN_dangerously_skip_permissions", "true")
	r := NewRunner(zerolog.Nop(), runnerOptsFromEnv()...)
	if !r.dangerouslySkipPermissions {
		t.Errorf("dangerouslySkipPermissions = false, want true (env says skip)")
	}
}

func TestRunnerOptsFromEnv_LoginShell(t *testing.T) {
	t.Setenv("BOSS_PLUGIN_login_shell", "/bin/zsh")
	opts := runnerOptsFromEnv()
	t.Setenv("BOSS_PLUGIN_login_shell", "")

	r := NewRunner(zerolog.Nop(), opts...)
	if r.loginShell != "/bin/zsh" {
		t.Fatalf("loginShell = %q, want /bin/zsh", r.loginShell)
	}
}

func TestRunnerOptsFromEnv_SkipPermissionsFalse(t *testing.T) {
	t.Setenv("BOSS_PLUGIN_dangerously_skip_permissions", "false")
	r := NewRunner(zerolog.Nop(), runnerOptsFromEnv()...)
	if r.dangerouslySkipPermissions {
		t.Errorf("dangerouslySkipPermissions = true, want false")
	}
}

func TestRunnerOptsFromEnv_Unset(t *testing.T) {
	// Make sure ambient env doesn't bleed in.
	t.Setenv("BOSS_PLUGIN_dangerously_skip_permissions", "")
	r := NewRunner(zerolog.Nop(), runnerOptsFromEnv()...)
	if r.dangerouslySkipPermissions {
		t.Errorf("dangerouslySkipPermissions = true, want false (env unset)")
	}
}

func TestEnsureSkillsInstalled_NoOpWhenNotInstalled(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	// No skills/ dir: BossSkillsInstalled returns false; ensureSkillsInstalled is a no-op.
	if err := ensureSkillsInstalled(); err != nil {
		t.Fatalf("ensureSkillsInstalled: %v", err)
	}
	skillsDir := filepath.Join(tmpHome, ".claude", "skills")
	if _, err := os.Stat(skillsDir); !os.IsNotExist(err) {
		t.Errorf("expected no skills dir, got err=%v", err)
	}
	_ = skilldata.SkillsFS // ref to avoid unused-import lint when test stubs out skill-related globals
}

func TestEnsureSkillsInstalled_UpdatesWhenInstalled(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	// Pre-create a marker file the existing IsInstalled checks for.
	skillsDir, err := libskillinstall.DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	nsDir := filepath.Join(skillsDir, libskillinstall.Namespace)
	if err := os.MkdirAll(filepath.Join(nsDir, "boss-finalize"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !libskillinstall.IsInstalled(skillsDir) {
		t.Skip("IsInstalled returned false after pre-seed; sentinel logic differs from assumption")
	}

	if err := ensureSkillsInstalled(); err != nil {
		t.Fatalf("ensureSkillsInstalled: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(skillsDir, "*", "SKILL.md"))
	if len(matches) == 0 {
		t.Errorf("expected SKILL.md files extracted, got none")
	}
}

// Regression: if the on-disk skills already match the embedded payload,
// ensureSkillsInstalled must NOT rewrite them. Rewriting unconditionally
// caused the CLI's startup prompt to fire on every `make dev` because each
// daemon restart silently re-extracted the plugin's embed over the install
// the CLI had just written.
func TestEnsureSkillsInstalled_NoOpWhenAlreadyUpToDate(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	skillsDir, err := libskillinstall.DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := libskillinstall.Extract(skillsDir, skilldata.SkillsFS); err != nil {
		t.Fatalf("seed Extract: %v", err)
	}
	probe := filepath.Join(skillsDir, libskillinstall.Namespace, "boss-finalize", "SKILL.md")
	infoBefore, err := os.Stat(probe)
	if err != nil {
		t.Fatalf("stat probe: %v", err)
	}
	// Backdate mtime so a no-op leaves it visibly distinct from "rewritten now".
	old := infoBefore.ModTime().Add(-time.Hour)
	if err := os.Chtimes(probe, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if err := ensureSkillsInstalled(); err != nil {
		t.Fatalf("ensureSkillsInstalled: %v", err)
	}

	infoAfter, err := os.Stat(probe)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if !infoAfter.ModTime().Equal(old) {
		t.Errorf("ensureSkillsInstalled rewrote probe (mtime %v → %v) when on-disk already matched embed",
			old, infoAfter.ModTime())
	}
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
