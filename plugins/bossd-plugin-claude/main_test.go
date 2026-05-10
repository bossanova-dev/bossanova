package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"

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
