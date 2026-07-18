//go:build !windows

package log

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestSetupCreatesLogDirWith0750 pins the G301 tightening: the rotated-log
// directory is created with 0o750 rather than 0o755.
func TestSetupCreatesLogDirWith0750(t *testing.T) {
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })

	t.Setenv("XDG_STATE_HOME", t.TempDir())
	closer := Setup("perm-service")
	t.Cleanup(func() { _ = closer.Close() })

	dir := filepath.Dir(LogPath("perm-service"))
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat log dir %s: %v", dir, err)
	}
	if got := info.Mode().Perm(); got != 0o750 {
		t.Errorf("log dir mode = %o, want 0750", got)
	}
}
