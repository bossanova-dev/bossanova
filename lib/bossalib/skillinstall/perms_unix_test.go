//go:build !windows

package skillinstall

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestExtractReportsCleanupPermissionErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses filesystem permissions")
	}
	for _, tc := range []struct {
		name    string
		blocked string
		mode    os.FileMode
		want    string
	}{
		{name: "read directory", blocked: ".", mode: 0o300, want: "read skill directory"},
		{name: "remove top-level skill", blocked: "boss-obsolete", mode: 0o500, want: "remove stale skill"},
		{name: "remove namespace", blocked: Namespace, mode: 0o500, want: "remove skill namespace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest := t.TempDir()
			blocked := filepath.Join(dest, tc.blocked)
			if err := os.MkdirAll(blocked, 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(blocked, "obsolete.md"), []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(blocked, tc.mode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.Chmod(blocked, 0o750); err != nil {
					t.Error(err)
				}
			})
			err := Extract(dest, testFS())
			if !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Extract = %v, want permission error from %s", err, tc.want)
			}
			if _, err := os.Stat(filepath.Join(dest, Namespace, "boss-test", "SKILL.md")); !os.IsNotExist(err) {
				t.Fatalf("new skill written after failed cleanup: %v", err)
			}
		})
	}
}

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
