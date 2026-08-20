package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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

// A symlink at the legacy path must be refused, not followed. os.ReadDir
// resolves it, so without a gate the loop's os.Remove would unlink every
// "upload-" file in the TARGET — unrelated files belonging to whoever owns
// that directory — silently, once per daemon start.
func TestRemoveLegacyChatUploadDirRefusesASymlinkedPath(t *testing.T) {
	appData := t.TempDir()
	target := t.TempDir()
	upload := filepath.Join(target, "upload-1.bin")
	if err := os.WriteFile(upload, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed target upload: %v", err)
	}
	foreign := filepath.Join(target, "notes.txt")
	if err := os.WriteFile(foreign, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed target foreign file: %v", err)
	}
	legacy := filepath.Join(appData, legacyChatUploadDirName)
	if err := os.Symlink(target, legacy); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}

	err := removeLegacyChatUploadDir(appData)
	if err == nil {
		t.Fatalf("removeLegacyChatUploadDir accepted a symlinked legacy path %q", legacy)
	}
	// The refusal is a recurring startup warning, so its text has to name the
	// reason — and, per the function, the remediation that ends it.
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error %v does not name the symlink as the reason", err)
	}
	// Asserted separately from the reason: without this the remediation could
	// be reworded away and the suite would stay green, silently regressing the
	// one thing that makes a per-start warning terminable for the operator.
	if !strings.Contains(err.Error(), "remove the stale symlink") {
		t.Fatalf("error %v does not name the remediation that ends the recurring warning", err)
	}
	// Nothing may be enumerated or removed: both the target's files and the
	// link itself survive untouched.
	if _, statErr := os.Stat(upload); statErr != nil {
		t.Fatalf("upload file in the symlink target was removed: %v", statErr)
	}
	if _, statErr := os.Stat(foreign); statErr != nil {
		t.Fatalf("foreign file in the symlink target was removed: %v", statErr)
	}
	info, statErr := os.Lstat(legacy)
	if statErr != nil {
		t.Fatalf("the symlink itself was removed: %v", statErr)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("legacy path %q is no longer a symlink (mode %v)", legacy, info.Mode())
	}
}

// A non-directory at the legacy path is refused before it is opened at all.
//
// The regular file is the cheap half of that hazard. The pre-gate function did
// fail here too — os.ReadDir returns ENOTDIR on a regular file — but it failed
// only AFTER opening the path, and with different wording, so the message
// assertion below is what ties this test to the !IsDir() branch rather than to
// that incidental failure. The branch exists for the expensive half, a
// non-regular file such as a FIFO or a device node where the open is itself
// the hazard; TestRemoveLegacyChatUploadDirRefusesAFifoWithoutOpeningIt covers
// that one.
func TestRemoveLegacyChatUploadDirRefusesANonDirectory(t *testing.T) {
	appData := t.TempDir()
	legacy := filepath.Join(appData, legacyChatUploadDirName)
	if err := os.WriteFile(legacy, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed legacy file: %v", err)
	}

	err := removeLegacyChatUploadDir(appData)
	if err == nil {
		t.Fatalf("removeLegacyChatUploadDir accepted a regular file at %q", legacy)
	}
	// Pin the refusal to the !IsDir() branch. Non-nil alone is satisfied just
	// as well by the pre-gate ENOTDIR from os.ReadDir, so without this the
	// test cannot tell "refused before opening" from "failed after opening" —
	// and refusing before the open is the whole point for a FIFO or a device
	// node, where the open is itself the hazard.
	if !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("error %v did not come from the not-a-directory refusal", err)
	}
	if _, statErr := os.Stat(legacy); statErr != nil {
		t.Fatalf("the file at the legacy path was removed: %v", statErr)
	}
}

// A FIFO at the legacy path must be refused WITHOUT being opened — the
// non-regular-file hazard the !IsDir() branch exists for, which a regular file
// cannot stand in for.
//
// What makes it worse than the regular file is that its failure mode is a
// hang, not an error: opening a writerless FIFO read-only blocks until a
// writer arrives, indefinitely, and removeLegacyChatUploadDir runs inline from
// run() in main.go — so a block here is not a slow reclaim, it is a daemon
// that never finishes starting. Measured on go1.26.2/darwin, os.ReadDir
// happens to open with O_DIRECTORY and so fails fast with ENOTDIR instead;
// go.work and this module target Go 1.25.x, where that was not re-measured.
// A plain os.Open on the same path blocks. That is a stdlib
// implementation detail this function must not depend on, and the Lstat gate
// is what makes it independent of it: the refusal lands before any open at
// all. Hence both assertions below — the refusal comes from the gate, AND it
// arrives rather than hanging, so an open reintroduced here is caught either
// way.
func TestRemoveLegacyChatUploadDirRefusesAFifoWithoutOpeningIt(t *testing.T) {
	appData := t.TempDir()
	legacy := filepath.Join(appData, legacyChatUploadDirName)
	if err := syscall.Mkfifo(legacy, 0o600); err != nil {
		t.Skipf("FIFOs unavailable on this platform: %v", err)
	}

	// Behind a timeout on purpose: the regression this guards against does not
	// fail, it hangs, and an unguarded call would take the whole package down
	// with it rather than naming the cause.
	done := make(chan error, 1)
	go func() {
		done <- removeLegacyChatUploadDir(appData)
	}()
	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("removeLegacyChatUploadDir hung: the FIFO at %q was opened for read instead of refused", legacy)
	}

	if err == nil {
		t.Fatalf("removeLegacyChatUploadDir accepted a FIFO at %q", legacy)
	}
	if !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("error %v did not come from the not-a-directory refusal", err)
	}
	// Nothing enumerated, nothing removed: the FIFO is still there, still a FIFO.
	info, statErr := os.Lstat(legacy)
	if statErr != nil {
		t.Fatalf("the FIFO at the legacy path was removed: %v", statErr)
	}
	if info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("legacy path %q is no longer a FIFO (mode %v)", legacy, info.Mode())
	}
}

// An Lstat failure that is neither "absent" nor a symlink/non-directory
// verdict must fail closed — a wrapped error, nothing enumerated or removed.
// A legacy path whose PARENT is a regular file produces exactly that: Lstat
// returns ENOTDIR, which is not fs.ErrNotExist, so the absent-is-success
// branch must not swallow it.
func TestRemoveLegacyChatUploadDirFailsClosedOnUnexpectedLstatError(t *testing.T) {
	base := t.TempDir()
	appData := filepath.Join(base, "not-a-dir")
	if err := os.WriteFile(appData, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed non-directory app data path: %v", err)
	}

	err := removeLegacyChatUploadDir(appData)
	if err == nil {
		t.Fatalf("removeLegacyChatUploadDir reported success for an unstattable legacy path")
	}
	// Pin it to the Lstat branch rather than to any later failure, so the test
	// keeps proving that nothing was enumerated before the refusal.
	if !strings.Contains(err.Error(), "stat legacy upload dir") {
		t.Fatalf("error %v did not come from the fail-closed Lstat branch", err)
	}
	if _, statErr := os.Stat(appData); statErr != nil {
		t.Fatalf("the app data path was removed: %v", statErr)
	}
}
