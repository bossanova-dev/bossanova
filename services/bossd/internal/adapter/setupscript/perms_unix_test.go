//go:build !windows

package setupscript

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestWriteLegacyScriptPerms pins the G301 (directory) and G306 (file)
// tightenings for the materialized legacy setup script: the .boss directory is
// 0o750 and the setup.sh file is 0o600 (no exec bit — it is invoked via `sh`).
func TestWriteLegacyScriptPerms(t *testing.T) {
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })

	wt := t.TempDir()
	scriptPath, err := writeLegacyScript(wt, "echo legacy")
	if err != nil {
		t.Fatalf("writeLegacyScript: %v", err)
	}

	dirInfo, err := os.Stat(filepath.Join(wt, ".boss"))
	if err != nil {
		t.Fatalf("stat .boss dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o750 {
		t.Errorf(".boss dir mode = %o, want 0750", got)
	}

	fileInfo, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatalf("stat setup.sh: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("setup.sh mode = %o, want 0600", got)
	}
}
