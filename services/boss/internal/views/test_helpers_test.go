package views

import (
	"path/filepath"
	"runtime"
	"testing"
)

// withTempConfigHome redirects config.Path() at a per-test temp directory.
//
// It points BOSS_SETTINGS_PATH directly at a fresh temp settings.json, which is
// what config.Path() actually consults first — redirecting HOME alone is not
// enough, because BOSS_SETTINGS_PATH (set in the developer's shell via direnv)
// takes precedence and would otherwise route config.Save() straight at the real
// settings.json. HOME is redirected too so any HOME-derived paths (e.g.
// DefaultSettings' worktree_base_dir) also land in the temp tree.
//
// Mirrors the isolation that cmd/testmain_test.go gives the cmd package.
func withTempConfigHome(t *testing.T) {
	t.Helper()
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)
	if runtime.GOOS != "darwin" {
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempHome, ".config"))
	}
	t.Setenv("BOSS_SETTINGS_PATH", filepath.Join(tempHome, "settings.json"))
}
