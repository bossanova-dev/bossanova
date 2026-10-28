package accountwiring

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReapOrphanedUsageProbeHomes_Accounts_AC12(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name    string
		path    string
		kind    string
		age     time.Duration
		removed bool
	}{
		{"old probe", "boss-usage-probe-old", "dir", 2 * time.Hour, true},
		{"young probe", "boss-usage-probe-young", "dir", 30 * time.Minute, false},
		{"at cutoff", "boss-usage-probe-cutoff", "dir", time.Hour, false},
		{"future probe", "boss-usage-probe-future", "dir", -time.Hour, false},
		{"unrelated directory", "other-home", "dir", 2 * time.Hour, false},
		{"matching file", "boss-usage-probe-file", "file", 2 * time.Hour, false},
		{"matching symlink", "boss-usage-probe-link", "symlink", 2 * time.Hour, false},
		{"nested probe", "other-home/boss-usage-probe-nested", "dir", 2 * time.Hour, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, tt.path)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			switch tt.kind {
			case "dir":
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "credentials"), []byte("probe"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "file":
				if err := os.WriteFile(path, []byte("probe"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := t.TempDir()
				if err := os.WriteFile(filepath.Join(target, "credentials"), []byte("preserve"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(target, now.Add(-tt.age), now.Add(-tt.age)); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if _, err := os.Stat(filepath.Join(target, "credentials")); err != nil {
						t.Errorf("symlink target changed: %v", err)
					}
				})
			}
			if tt.kind != "symlink" {
				if err := os.Chtimes(path, now.Add(-tt.age), now.Add(-tt.age)); err != nil {
					t.Fatal(err)
				}
			}
			count, err := ReapOrphanedUsageProbeHomes(root, time.Hour, now)
			if err != nil {
				t.Fatal(err)
			}
			wantCount := 0
			if tt.removed {
				wantCount = 1
			}
			if count != wantCount {
				t.Errorf("removed = %d, want %d", count, wantCount)
			}
			_, err = os.Lstat(path)
			if tt.removed {
				if !os.IsNotExist(err) {
					t.Errorf("removed path stat = %v", err)
				}
			} else if err != nil {
				t.Errorf("preserved path stat = %v", err)
			}
		})
	}
}

func TestReapOrphanedUsageProbeHomesReadError(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	if count, err := ReapOrphanedUsageProbeHomes(root, time.Hour, time.Now()); err == nil || count != 0 {
		t.Fatalf("got (%d,%v), want (0,error)", count, err)
	}
}
