package clitest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/recurser/bossalib/config"
)

func TestSettingsProfileFileCanBeSelectedByEnv(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	settingsPath := filepath.Join(dir, "profile", "settings.json")
	customWorktreeDir := filepath.Join(dir, "worktrees")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) returned error: %v", home, err)
	}

	h := New(t, WithEnv(
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"BOSS_SETTINGS_PATH="+settingsPath,
	))

	res := h.Run("settings", "--worktree-dir", customWorktreeDir)
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}

	if _, err := os.Stat(settingsPath); err != nil {
		t.Fatalf("settings file not written through BOSS_SETTINGS_PATH: %v", err)
	}

	loaded, err := config.LoadFrom(settingsPath)
	if err != nil {
		t.Fatalf("config.LoadFrom(%q) returned error: %v", settingsPath, err)
	}
	if loaded.WorktreeBaseDir != customWorktreeDir {
		t.Fatalf("WorktreeBaseDir = %q, want %q", loaded.WorktreeBaseDir, customWorktreeDir)
	}
}
