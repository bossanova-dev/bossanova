package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDefaultAppDataDir verifies the helper resolves to the platform config
// directory (XDG-aware via os.UserConfigDir) plus the bossanova subdirectory,
// so daemon data lives beside settings.json on every platform.
func TestDefaultAppDataDir(t *testing.T) {
	dir, err := DefaultAppDataDir()
	if err != nil {
		t.Fatalf("DefaultAppDataDir() returned error: %v", err)
	}

	cfgDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("os.UserConfigDir() returned error: %v", err)
	}
	want := filepath.Join(cfgDir, "bossanova")
	if dir != want {
		t.Fatalf("DefaultAppDataDir() = %q, want %q", dir, want)
	}
}

// TestPathSitsUnderDefaultAppDataDir locks the invariant that the default
// settings file lives directly inside DefaultAppDataDir(), so boss and bossd
// agree on one per-user directory.
func TestPathSitsUnderDefaultAppDataDir(t *testing.T) {
	t.Setenv("BOSS_SETTINGS_PATH", "")

	got, err := Path()
	if err != nil {
		t.Fatalf("Path() returned error: %v", err)
	}

	base, err := DefaultAppDataDir()
	if err != nil {
		t.Fatalf("DefaultAppDataDir() returned error: %v", err)
	}
	want := filepath.Join(base, "settings.json")
	if got != want {
		t.Fatalf("Path() = %q, want %q", got, want)
	}
}
