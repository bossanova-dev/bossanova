package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The upload directory must sit under the system temp dir, because that is the
// entire point of the move: a host temp reaper underneath the manager's
// janitor. Asserting the prefix rather than the whole string keeps the test
// honest about os.TempDir being environment-dependent (TMPDIR).
func TestChatUploadDirLivesUnderSystemTemp(t *testing.T) {
	dir := chatUploadDir()
	if parent := filepath.Dir(dir); parent != filepath.Clean(os.TempDir()) {
		t.Fatalf("upload dir %q sits under %q, want the system temp dir %q", dir, parent, os.TempDir())
	}
	// The uid suffix is what keeps two users on one host off the same name in a
	// shared /tmp. Without it the second daemon inherits the first's directory
	// or fails its ownership check, and which one happens is a race.
	if !strings.HasPrefix(filepath.Base(dir), chatUploadDirName+"-") {
		t.Fatalf("upload dir base %q must be %q plus a uid suffix", filepath.Base(dir), chatUploadDirName)
	}
	// A fresh random name per start would strand each run's leftovers in a
	// directory the next run's janitor never sweeps.
	if again := chatUploadDir(); again != dir {
		t.Fatalf("upload dir is not stable across calls: %q then %q", dir, again)
	}
}

// The legacy directory is what this change reclaims — files a previous daemon
// left under the app data dir, which nothing sweeps once the manager points at
// the system temp dir.
func TestRemoveLegacyChatUploadDirReclaimsUploads(t *testing.T) {
	appData := t.TempDir()
	legacy := filepath.Join(appData, legacyChatUploadDirName)
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatalf("create legacy dir: %v", err)
	}
	for _, name := range []string{"upload-123.jpg", "upload-456"} {
		if err := os.WriteFile(filepath.Join(legacy, name), []byte("x"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	if err := removeLegacyChatUploadDir(appData); err != nil {
		t.Fatalf("removeLegacyChatUploadDir: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy dir survived, stat err = %v", err)
	}
}

// Removal is scoped to the manager's own "upload-" basenames, so a
// mis-resolved app data dir cannot take an unrelated file with it. The
// directory is then left standing, because rmdir on a non-empty directory
// fails — and that failure is not an error worth reporting.
func TestRemoveLegacyChatUploadDirSparesForeignFiles(t *testing.T) {
	appData := t.TempDir()
	legacy := filepath.Join(appData, legacyChatUploadDirName)
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatalf("create legacy dir: %v", err)
	}
	keep := filepath.Join(legacy, "notes.txt")
	if err := os.WriteFile(keep, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed foreign file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "upload-1.bin"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seed upload: %v", err)
	}

	if err := removeLegacyChatUploadDir(appData); err != nil {
		t.Fatalf("removeLegacyChatUploadDir: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("foreign file was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacy, "upload-1.bin")); !os.IsNotExist(err) {
		t.Fatalf("upload file survived, stat err = %v", err)
	}
}

// The overwhelmingly common case: there is nothing to reclaim. It must not be
// reported as a startup warning, or every daemon start after the first logs one.
func TestRemoveLegacyChatUploadDirIsSilentWhenAbsent(t *testing.T) {
	if err := removeLegacyChatUploadDir(t.TempDir()); err != nil {
		t.Fatalf("absent legacy dir must be success, got %v", err)
	}
}
