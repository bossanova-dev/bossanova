package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain isolates the whole cmd test package from the developer's real
// settings.json. The config guard (config.Path) refuses the OS-default settings
// path under test, so any test that reaches config.Save/Load without isolating
// would otherwise error. Pointing BOSS_SETTINGS_PATH at a throwaway temp file
// gives every test a writable, disposable settings file by default. Individual
// tests can still t.Setenv("BOSS_SETTINGS_PATH", ...) to a specific path;
// t.Setenv restores this default afterward.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "boss-cmd-settings-*")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("BOSS_SETTINGS_PATH", filepath.Join(dir, "settings.json")); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
