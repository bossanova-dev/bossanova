package views

import (
	"path/filepath"
	"runtime"
	"testing"
)

// withTempConfigHome redirects config.Path() at a per-test temp directory by
// pointing HOME (and on Linux XDG_CONFIG_HOME) at a fresh location. Mirrors
// the helper in cmd/config_init_test.go but kept local to avoid an internal
// test-package dep.
func withTempConfigHome(t *testing.T) {
	t.Helper()
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)
	if runtime.GOOS != "darwin" {
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempHome, ".config"))
	}
}
