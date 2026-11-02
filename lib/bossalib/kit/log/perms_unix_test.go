//go:build !windows

package log

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
)

// TestSetupCreatesLogDirWith0750 pins the G301 tightening: the rotated-log
// directory is created with 0o750 rather than 0o755.
func TestSetupCreatesLogDirWith0750(t *testing.T) {
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })

	t.Setenv("XDG_STATE_HOME", t.TempDir())
	closer := Setup("perm-service")
	t.Cleanup(func() { _ = closer.Close() })

	dir := filepath.Dir(Path("perm-service"))
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat log dir %s: %v", dir, err)
	}
	if got := info.Mode().Perm(); got != 0o750 {
		t.Errorf("log dir mode = %o, want 0750", got)
	}
}

// TestWithStderrLevelKeepsInfoOnStderrWhenTheFileCannotOpen covers a log dir
// that exists but refuses the rotated file: the lazily opened file sink fails
// on write, so stderr is the only record and must not withhold info.
func TestWithStderrLevelKeepsInfoOnStderrWhenTheFileCannotOpen(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := filepath.Dir(Path("unwritable"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	previous := zlog.Logger
	t.Cleanup(func() { zlog.Logger = previous })
	var stderr bytes.Buffer
	closer := Setup("unwritable", WithStderrLevel(zerolog.WarnLevel), WithStderr(&stderr))
	t.Cleanup(func() { _ = closer.Close() })

	zlog.Info().Msg("info with no file sink")

	if !strings.Contains(stderr.String(), "info with no file sink") {
		t.Errorf("info line reached neither sink: %q", stderr.String())
	}
}
