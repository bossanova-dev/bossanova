package client

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/recurser/bossalib/config"
)

func TestDefaultSocketPathUsesSettingsSocketPath(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	socketPath := filepath.Join(dir, "profile.sock")
	t.Setenv("BOSS_SETTINGS_PATH", settingsPath)
	t.Setenv("BOSS_SOCKET", "")

	settings := config.DefaultSettings()
	settings.WorktreeBaseDir = filepath.Join(dir, "worktrees")
	settings.SocketPath = socketPath
	if err := config.SaveTo(settingsPath, settings); err != nil {
		t.Fatalf("SaveTo() returned error: %v", err)
	}

	got, err := DefaultSocketPath()
	if err != nil {
		t.Fatalf("DefaultSocketPath() returned error: %v", err)
	}
	if got != socketPath {
		t.Fatalf("DefaultSocketPath() = %q, want %q", got, socketPath)
	}
}

func TestDefaultSocketPathUsesSettingsAppDataDir(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	appDataDir := filepath.Join(dir, "data")
	t.Setenv("BOSS_SETTINGS_PATH", settingsPath)
	t.Setenv("BOSS_SOCKET", "")

	settings := config.DefaultSettings()
	settings.WorktreeBaseDir = filepath.Join(dir, "worktrees")
	settings.AppDataDir = appDataDir
	if err := config.SaveTo(settingsPath, settings); err != nil {
		t.Fatalf("SaveTo() returned error: %v", err)
	}

	got, err := DefaultSocketPath()
	if err != nil {
		t.Fatalf("DefaultSocketPath() returned error: %v", err)
	}
	want := filepath.Join(appDataDir, "bossd.sock")
	if got != want {
		t.Fatalf("DefaultSocketPath() = %q, want %q", got, want)
	}
}

func TestDefaultSocketPathFallsBackToConfigAppDataDir(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	t.Setenv("BOSS_SETTINGS_PATH", settingsPath)
	t.Setenv("BOSS_SOCKET", "")

	// No app_data_dir / socket_path in settings, so the client must fall back
	// to the shared platform default rather than a hardcoded macOS path.
	settings := config.DefaultSettings()
	settings.WorktreeBaseDir = filepath.Join(dir, "worktrees")
	if err := config.SaveTo(settingsPath, settings); err != nil {
		t.Fatalf("SaveTo() returned error: %v", err)
	}

	got, err := DefaultSocketPath()
	if err != nil {
		t.Fatalf("DefaultSocketPath() returned error: %v", err)
	}

	base, err := config.DefaultAppDataDir()
	if err != nil {
		t.Fatalf("config.DefaultAppDataDir() returned error: %v", err)
	}
	want := filepath.Join(base, "bossd.sock")
	if got != want {
		t.Fatalf("DefaultSocketPath() = %q, want %q", got, want)
	}
}

func TestDefaultSocketPathKeepsBossSocketOverride(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	envSocketPath := filepath.Join(dir, "env.sock")
	settingsSocketPath := filepath.Join(dir, "settings.sock")
	t.Setenv("BOSS_SETTINGS_PATH", settingsPath)
	t.Setenv("BOSS_SOCKET", envSocketPath)

	settings := config.DefaultSettings()
	settings.WorktreeBaseDir = filepath.Join(dir, "worktrees")
	settings.SocketPath = settingsSocketPath
	if err := config.SaveTo(settingsPath, settings); err != nil {
		t.Fatalf("SaveTo() returned error: %v", err)
	}

	got, err := DefaultSocketPath()
	if err != nil {
		t.Fatalf("DefaultSocketPath() returned error: %v", err)
	}
	if got != envSocketPath {
		t.Fatalf("DefaultSocketPath() = %q, want BOSS_SOCKET %q", got, envSocketPath)
	}
}

func TestDefaultSocketPathDoesNotCreateSettingsWorktreeBaseDir(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	socketPath := filepath.Join(dir, "profile.sock")
	worktreeBaseDir := filepath.Join(dir, "worktrees")
	t.Setenv("BOSS_SETTINGS_PATH", settingsPath)
	t.Setenv("BOSS_SOCKET", "")

	settings := config.DefaultSettings()
	settings.WorktreeBaseDir = worktreeBaseDir
	settings.SocketPath = socketPath
	if err := config.SaveTo(settingsPath, settings); err != nil {
		t.Fatalf("SaveTo() returned error: %v", err)
	}

	got, err := DefaultSocketPath()
	if err != nil {
		t.Fatalf("DefaultSocketPath() returned error: %v", err)
	}
	if got != socketPath {
		t.Fatalf("DefaultSocketPath() = %q, want %q", got, socketPath)
	}
	if _, err := os.Stat(worktreeBaseDir); !os.IsNotExist(err) {
		t.Fatalf("DefaultSocketPath() created WorktreeBaseDir; os.Stat(%q) error = %v", worktreeBaseDir, err)
	}
}
