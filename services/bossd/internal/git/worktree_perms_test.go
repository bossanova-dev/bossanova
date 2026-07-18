package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestEnsureGitInfoExclude_Perms locks in the gosec-driven permission tightening
// (BOS-423): the git info directory is created 0o750 (G301) and the git-exclude
// file is written 0o600 (G306). A regression to world-readable/traversable modes
// must fail this test.
func TestEnsureGitInfoExclude_Perms(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	infoDir := filepath.Join(dir, ".git", "info")
	excludePath := filepath.Join(infoDir, "exclude")

	// `git init` pre-creates .git/info with git's own (world-readable) mode, so
	// the MkdirAll under test would be a no-op. Remove it first so the 0o750
	// creation path actually runs and the dir-mode assertion is meaningful.
	if err := os.RemoveAll(infoDir); err != nil {
		t.Fatalf("remove pre-existing info dir: %v", err)
	}

	if err := ensureGitInfoExclude(ctx, dir, []string{"worktree-artifact-*"}); err != nil {
		t.Fatalf("ensureGitInfoExclude: %v", err)
	}

	dirInfo, err := os.Stat(infoDir)
	if err != nil {
		t.Fatalf("stat info dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o750 {
		t.Errorf("info dir mode = %#o, want 0o750 (G301)", perm)
	}

	fileInfo, err := os.Stat(excludePath)
	if err != nil {
		t.Fatalf("stat exclude file: %v", err)
	}
	if perm := fileInfo.Mode().Perm(); perm != 0o600 {
		t.Errorf("exclude file mode = %#o, want 0o600 (G306)", perm)
	}
}

// TestEnsureGitInfoExclude_Perms_ExistingFile covers the common production path
// the new-file test masks: git init pre-creates info/exclude at 0o644, and
// os.WriteFile does not re-mode an existing file. The explicit chmod must still
// bring an already-present, world-readable exclude down to 0o600 (G306).
func TestEnsureGitInfoExclude_Perms_ExistingFile(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	infoDir := filepath.Join(dir, ".git", "info")
	excludePath := filepath.Join(infoDir, "exclude")

	// Simulate git's default: a pre-existing, world-readable exclude with content.
	if err := os.MkdirAll(infoDir, 0o750); err != nil {
		t.Fatalf("mkdir info dir: %v", err)
	}
	if err := os.WriteFile(excludePath, []byte("# pre-existing\n*.log\n"), 0o644); err != nil {
		t.Fatalf("seed exclude file: %v", err)
	}
	if err := os.Chmod(excludePath, 0o644); err != nil { // ensure 0o644 despite umask
		t.Fatalf("chmod seed exclude: %v", err)
	}

	if err := ensureGitInfoExclude(ctx, dir, []string{"worktree-artifact-*"}); err != nil {
		t.Fatalf("ensureGitInfoExclude: %v", err)
	}

	fileInfo, err := os.Stat(excludePath)
	if err != nil {
		t.Fatalf("stat exclude file: %v", err)
	}
	if perm := fileInfo.Mode().Perm(); perm != 0o600 {
		t.Errorf("pre-existing exclude file mode = %#o, want 0o600 (G306) after tightening", perm)
	}
}
