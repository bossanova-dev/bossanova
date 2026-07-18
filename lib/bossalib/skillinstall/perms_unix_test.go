//go:build !windows

package skillinstall

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestExtractCreatesDirsWith0750 pins the G301 tightening: skill directories
// materialized during extraction are created with 0o750 rather than 0o755.
func TestExtractCreatesDirsWith0750(t *testing.T) {
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })

	dest := t.TempDir()
	if err := Extract(dest, testFS()); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	skillDir := filepath.Join(dest, Namespace, "boss-test")
	info, err := os.Stat(skillDir)
	if err != nil {
		t.Fatalf("stat %s: %v", skillDir, err)
	}
	if got := info.Mode().Perm(); got != 0o750 {
		t.Errorf("skill dir mode = %o, want 0750", got)
	}
}
