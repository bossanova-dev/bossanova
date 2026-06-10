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

// TestRealDefaultPathMatchesAppDataDir locks the package-init invariant that
// realDefaultPath (config.go:33, computed once before any t.Setenv can redirect
// HOME) resolves to DefaultAppDataDir()/settings.json whenever the platform can
// resolve an app-data dir. realDefaultPath is the exact path the write guard
// refuses; if init silently dropped the resolved dir it would be "" and the
// guard would no-op, letting a forgetful test clobber the developer's real
// settings.json. TestSaveRefusesRealDefaultUnderTest only *skips* when this is
// empty, so it cannot catch that regression — this test asserts it directly.
func TestRealDefaultPathMatchesAppDataDir(t *testing.T) {
	dir, err := DefaultAppDataDir()
	if err != nil {
		t.Skip("no app data dir on this platform")
	}
	want := filepath.Join(dir, "settings.json")
	if realDefaultPath != want {
		t.Fatalf("realDefaultPath = %q, want %q", realDefaultPath, want)
	}
}

// TestPathSitsUnderDefaultAppDataDir locks the invariant that the default
// settings file lives directly inside DefaultAppDataDir(), so boss and bossd
// agree on one per-user directory. Path() resolves the location (reads are never
// blocked); only writes to the real default are guarded, in SaveTo.
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
