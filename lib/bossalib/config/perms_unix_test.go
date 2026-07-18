//go:build !windows

package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// withZeroUmask pins the process umask to 0 for the duration of the test so
// directory/file mode assertions are deterministic regardless of the ambient
// umask on the dev machine or CI runner. Not parallel-safe (umask is
// process-global), so callers must not use t.Parallel.
func withZeroUmask(t *testing.T) {
	t.Helper()
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })
}

// TestSaveToCreatesDirWith0750 pins the G301 tightening: SaveTo creates its
// parent directory with owner-only+group-read perms (0o750), not 0o755.
func TestSaveToCreatesDirWith0750(t *testing.T) {
	withZeroUmask(t)
	dir := filepath.Join(t.TempDir(), "nested", "sub")
	path := filepath.Join(dir, "settings.json")

	if err := SaveTo(path, DefaultSettings()); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat %s: %v", dir, err)
	}
	if got := info.Mode().Perm(); got != 0o750 {
		t.Errorf("dir mode = %o, want 0750", got)
	}
}

// TestLoadCreatesWorktreeBaseDirWith0750 pins the G301 tightening at the Load
// path: the worktree base directory is materialized with 0o750.
func TestLoadCreatesWorktreeBaseDirWith0750(t *testing.T) {
	withZeroUmask(t)
	tmp := t.TempDir()
	settingsFile := filepath.Join(tmp, "settings.json")
	wtDir := filepath.Join(tmp, "worktrees") // does not exist yet
	t.Setenv(settingsPathEnv, settingsFile)

	s := DefaultSettings()
	s.WorktreeBaseDir = wtDir
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(settingsFile, data, 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	if _, err := Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	info, err := os.Stat(wtDir)
	if err != nil {
		t.Fatalf("stat %s: %v", wtDir, err)
	}
	if got := info.Mode().Perm(); got != 0o750 {
		t.Errorf("worktree base dir mode = %o, want 0750", got)
	}
}
