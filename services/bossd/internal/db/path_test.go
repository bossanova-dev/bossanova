package db

import (
	"path/filepath"
	"testing"

	"github.com/recurser/bossalib/config"
)

func TestDefaultDBPathForSettingsUsesAppDataDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")

	got, err := DefaultDBPathForSettings(config.Settings{AppDataDir: dir})
	if err != nil {
		t.Fatalf("DefaultDBPathForSettings() returned error: %v", err)
	}

	want := filepath.Join(dir, "bossd.db")
	if got != want {
		t.Fatalf("DefaultDBPathForSettings() = %q, want %q", got, want)
	}
}

func TestDefaultDBPathForSettingsRejectsRelativeAppDataDir(t *testing.T) {
	_, err := DefaultDBPathForSettings(config.Settings{AppDataDir: "relative/data"})
	if err == nil {
		t.Fatal("DefaultDBPathForSettings() error = nil, want relative path error")
	}
}

func TestDefaultDBPathSitsUnderConfigAppDataDir(t *testing.T) {
	got, err := DefaultDBPath()
	if err != nil {
		t.Fatalf("DefaultDBPath() returned error: %v", err)
	}

	base, err := config.DefaultAppDataDir()
	if err != nil {
		t.Fatalf("config.DefaultAppDataDir() returned error: %v", err)
	}
	want := filepath.Join(base, "bossd.db")
	if got != want {
		t.Fatalf("DefaultDBPath() = %q, want %q", got, want)
	}
}

func TestDefaultDBPathForSettingsFallsBackToDefault(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("APPDATA", filepath.Join(root, "appdata"))
	t.Setenv("LOCALAPPDATA", filepath.Join(root, "localappdata"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg-config"))

	got, err := DefaultDBPathForSettings(config.Settings{})
	if err != nil {
		t.Fatalf("DefaultDBPathForSettings() returned error: %v", err)
	}

	want, err := DefaultDBPath()
	if err != nil {
		t.Fatalf("DefaultDBPath() returned error: %v", err)
	}
	if got != want {
		t.Fatalf("DefaultDBPathForSettings() = %q, want fallback %q", got, want)
	}
}
