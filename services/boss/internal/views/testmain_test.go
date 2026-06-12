package views

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain isolates the whole views test package from the developer's real
// settings.json. The config write guard refuses the OS-default settings path —
// and the start-of-process BOSS_SETTINGS_PATH — under test, so any test that
// reaches config.Save/Load without isolating would otherwise error. Pointing
// BOSS_SETTINGS_PATH at a throwaway temp file gives every test a writable,
// disposable settings file by default; withTempConfigHome still overrides it
// per-test where finer isolation is needed (t.Setenv restores this default
// afterward). Mirrors services/boss/cmd/testmain_test.go.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "boss-views-settings-*")
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
