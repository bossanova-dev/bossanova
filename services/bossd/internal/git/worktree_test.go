package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// initTestRepo creates a git repo in a temp dir with an initial commit and a
// bare "origin" remote, so that `git fetch origin <branch>` works in tests.
func initTestRepo(t *testing.T) string {
	t.Helper()

	// Create a bare repo to act as "origin".
	bareDir := t.TempDir()
	bareCmd := exec.Command("git", "init", "--bare", "-b", "main")
	bareCmd.Dir = bareDir
	if out, err := bareCmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}

	// Create working repo, commit, and push to origin.
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"commit", "--allow-empty", "-m", "init"},
		{"remote", "add", "origin", bareDir},
		{"push", "-u", "origin", "main"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	return dir
}

func TestIsGitRepo(t *testing.T) {
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	t.Run("valid repo", func(t *testing.T) {
		dir := initTestRepo(t)
		if !mgr.IsGitRepo(ctx, dir) {
			t.Error("expected IsGitRepo to return true for git repo")
		}
	})

	t.Run("non-repo directory", func(t *testing.T) {
		dir := t.TempDir()
		if mgr.IsGitRepo(ctx, dir) {
			t.Error("expected IsGitRepo to return false for non-repo")
		}
	})

	t.Run("nonexistent path", func(t *testing.T) {
		if mgr.IsGitRepo(ctx, "/nonexistent/path/that/does/not/exist") {
			t.Error("expected IsGitRepo to return false for nonexistent path")
		}
	})
}

func TestDetectDefaultBranch_Fallback(t *testing.T) {
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	// A repo without origin/HEAD should fall back to "main".
	dir := initTestRepo(t)
	branch, err := mgr.DetectDefaultBranch(ctx, dir)
	if err != nil {
		t.Fatalf("DetectDefaultBranch: %v", err)
	}
	if branch != "main" {
		t.Errorf("branch = %q, want %q", branch, "main")
	}
}

func TestSanitizeBranchName(t *testing.T) {
	tests := []struct {
		title string
		want  string
	}{
		{"Fix the login bug!", "fix-the-login-bug"},
		{"Add README.md", "add-readme-md"},
		{"  spaces  ", "spaces"},
		{"UPPER CASE", "upper-case"},
		{"a/b/c", "a-b-c"},
		{strings.Repeat("x", 100), strings.Repeat("x", 60)},
	}

	for _, tt := range tests {
		t.Run(tt.title, func(t *testing.T) {
			got := sanitizeBranchName(tt.title)
			if got != tt.want {
				t.Errorf("sanitizeBranchName(%q) = %q, want %q", tt.title, got, tt.want)
			}
		})
	}
}

func TestCreate(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	logger := zerolog.Nop()
	mgr := NewManager(logger)

	result, err := mgr.Create(context.Background(), CreateOpts{
		RepoPath:        repoDir,
		BaseBranch:      "main",
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		Title:           "Test session",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if result.BranchName != "test-session" {
		t.Errorf("branch = %q, want %q", result.BranchName, "test-session")
	}

	// Verify worktree directory exists under <base>/<repo>/<branch>.
	if _, err := os.Stat(result.WorktreePath); err != nil {
		t.Errorf("worktree dir not found: %v", err)
	}
	wantPath := filepath.Join(wtBase, "my-repo", "test-session")
	if result.WorktreePath != wantPath {
		t.Errorf("worktree path = %q, want %q", result.WorktreePath, wantPath)
	}

	// Verify branch exists.
	out, err := runGit(context.Background(), repoDir, "branch", "--list", result.BranchName)
	if err != nil {
		t.Fatalf("list branches: %v", err)
	}
	if !strings.Contains(out, "test-session") {
		t.Errorf("branch not found in: %q", out)
	}
}

func TestCreateWithSetupScript(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	logger := zerolog.Nop()
	mgr := NewManager(logger)

	// The script writes both a marker file and the BOSS_ env vars so we
	// can verify they are set correctly.
	script := `echo hello > setup-done.txt && echo "$BOSS_REPO_DIR" > boss-repo-dir.txt && echo "$BOSS_WORKTREE_DIR" > boss-worktree-dir.txt`
	result, err := mgr.Create(context.Background(), CreateOpts{
		RepoPath:        repoDir,
		BaseBranch:      "main",
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		Title:           "Setup test",
		SetupScript:     &script,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Verify setup script ran.
	markerPath := filepath.Join(result.WorktreePath, "setup-done.txt")
	if _, err := os.Stat(markerPath); err != nil {
		t.Errorf("setup script marker not found: %v", err)
	}

	// Verify BOSS_REPO_DIR was set to the main repository path.
	gotRepo, err := os.ReadFile(filepath.Join(result.WorktreePath, "boss-repo-dir.txt"))
	if err != nil {
		t.Fatalf("read boss-repo-dir.txt: %v", err)
	}
	if got := strings.TrimSpace(string(gotRepo)); got != repoDir {
		t.Errorf("BOSS_REPO_DIR = %q, want %q", got, repoDir)
	}

	// Verify BOSS_WORKTREE_DIR was set to the worktree path.
	gotWT, err := os.ReadFile(filepath.Join(result.WorktreePath, "boss-worktree-dir.txt"))
	if err != nil {
		t.Fatalf("read boss-worktree-dir.txt: %v", err)
	}
	if got := strings.TrimSpace(string(gotWT)); got != result.WorktreePath {
		t.Errorf("BOSS_WORKTREE_DIR = %q, want %q", got, result.WorktreePath)
	}
}

func TestArchive(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	logger := zerolog.Nop()
	mgr := NewManager(logger)

	result, err := mgr.Create(context.Background(), CreateOpts{
		RepoPath:        repoDir,
		BaseBranch:      "main",
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		Title:           "Archive test",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Archive it.
	if err := mgr.Archive(context.Background(), result.WorktreePath); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	// Worktree directory should be gone.
	if _, err := os.Stat(result.WorktreePath); !os.IsNotExist(err) {
		t.Errorf("worktree dir still exists after archive")
	}

	// Branch should still exist.
	out, err := runGit(context.Background(), repoDir, "branch", "--list", result.BranchName)
	if err != nil {
		t.Fatalf("list branches: %v", err)
	}
	if !strings.Contains(out, "archive-test") {
		t.Errorf("branch should still exist after archive, got: %q", out)
	}
}

func TestArchive_CorruptedWorktree(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	logger := zerolog.Nop()
	mgr := NewManager(logger)

	result, err := mgr.Create(context.Background(), CreateOpts{
		RepoPath:        repoDir,
		BaseBranch:      "main",
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		Title:           "Corrupted test",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Corrupt the worktree by removing its .git file.
	if err := os.Remove(filepath.Join(result.WorktreePath, ".git")); err != nil {
		t.Fatalf("remove .git: %v", err)
	}

	// Archive should succeed via the fallback path.
	if err := mgr.Archive(context.Background(), result.WorktreePath); err != nil {
		t.Fatalf("Archive of corrupted worktree should succeed, got: %v", err)
	}

	// Worktree directory should be gone.
	if _, err := os.Stat(result.WorktreePath); !os.IsNotExist(err) {
		t.Errorf("worktree dir still exists after archive of corrupted worktree")
	}
}

func TestArchive_MissingWorktree(t *testing.T) {
	logger := zerolog.Nop()
	mgr := NewManager(logger)

	// Archive a path that doesn't exist — should succeed (os.RemoveAll is a no-op).
	nonexistent := filepath.Join(t.TempDir(), "does-not-exist")
	if err := mgr.Archive(context.Background(), nonexistent); err != nil {
		t.Fatalf("Archive of non-existent path should succeed, got: %v", err)
	}
}

func TestResurrect(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	logger := zerolog.Nop()
	mgr := NewManager(logger)

	// Create and archive.
	result, err := mgr.Create(context.Background(), CreateOpts{
		RepoPath:        repoDir,
		BaseBranch:      "main",
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		Title:           "Resurrect test",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := mgr.Archive(context.Background(), result.WorktreePath); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	// Resurrect.
	if err := mgr.Resurrect(context.Background(), ResurrectOpts{
		RepoPath:     repoDir,
		WorktreePath: result.WorktreePath,
		BranchName:   result.BranchName,
	}); err != nil {
		t.Fatalf("Resurrect: %v", err)
	}

	// Worktree directory should be back.
	if _, err := os.Stat(result.WorktreePath); err != nil {
		t.Errorf("worktree dir not found after resurrect: %v", err)
	}
}

func TestEmptyTrash(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	logger := zerolog.Nop()
	mgr := NewManager(logger)

	// Create a worktree and archive it (so the branch exists without a worktree).
	result, err := mgr.Create(context.Background(), CreateOpts{
		RepoPath:        repoDir,
		BaseBranch:      "main",
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		Title:           "Trash test",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := mgr.Archive(context.Background(), result.WorktreePath); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	// EmptyTrash should delete the local branch (no remote in test).
	if err := mgr.EmptyTrash(context.Background(), repoDir, []string{result.BranchName}); err != nil {
		t.Fatalf("EmptyTrash: %v", err)
	}

	// Branch should be gone.
	out, err := runGit(context.Background(), repoDir, "branch", "--list", result.BranchName)
	if err != nil {
		t.Fatalf("list branches: %v", err)
	}
	if strings.Contains(out, "trash-test") {
		t.Errorf("branch should be deleted after empty trash, got: %q", out)
	}
}

func TestDetectOriginURL(t *testing.T) {
	logger := zerolog.Nop()
	mgr := NewManager(logger)

	t.Run("no origin", func(t *testing.T) {
		// Create a repo without an origin remote.
		dir := t.TempDir()
		for _, args := range [][]string{
			{"init", "-b", "main"},
			{"config", "user.email", "test@test.com"},
			{"config", "user.name", "Test"},
			{"commit", "--allow-empty", "-m", "init"},
		} {
			cmd := exec.Command("git", args...)
			cmd.Dir = dir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
			}
		}

		url, err := mgr.DetectOriginURL(context.Background(), dir)
		if err != nil {
			t.Fatalf("DetectOriginURL: %v", err)
		}
		if url != "" {
			t.Errorf("expected empty URL, got %q", url)
		}
	})

	t.Run("with origin", func(t *testing.T) {
		repoDir := initTestRepo(t)

		url, err := mgr.DetectOriginURL(context.Background(), repoDir)
		if err != nil {
			t.Fatalf("DetectOriginURL: %v", err)
		}
		if url == "" {
			t.Error("expected non-empty URL")
		}
	})
}

// TestRunSetupScript_RejectsPathTraversal is the plan's integration-level
// gate: a repo whose stored setup_script attempts to escape the worktree
// via .. must error out *before* exec so no command is invoked.
func TestRunSetupScript_RejectsPathTraversal(t *testing.T) {
	worktree := t.TempDir()

	// Plant a bait script outside the worktree — if the traversal were
	// honored, this is what the attacker would target.
	bait := filepath.Join(t.TempDir(), "evil.sh")
	if err := os.WriteFile(bait, []byte("#!/bin/sh\ntouch /tmp/pwned\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	relToBait, err := filepath.Rel(worktree, bait)
	if err != nil {
		t.Fatal(err)
	}

	spec := `{"type":"script","path":"` + relToBait + `"}`
	err = runSetupScript(context.Background(), worktree, worktree, spec, nil)
	if err == nil {
		t.Fatal("expected an error, got nil — traversal was not rejected")
	}
	if !strings.Contains(err.Error(), "outside worktree") &&
		!strings.Contains(err.Error(), "escape the worktree") {
		t.Fatalf("error doesn't look like a traversal rejection: %v", err)
	}
}

// commitOnOrigin appends a commit on `branch` in the bare origin repo for
// `workingRepo`, so a subsequent fetch in `workingRepo` can observe a new
// upstream tip. Returns the new SHA on origin/<branch>.
func commitOnOrigin(t *testing.T, workingRepo, branch string) string {
	t.Helper()

	// Discover the origin URL (the bare repo path) from the working clone.
	originDir, err := runGit(context.Background(), workingRepo, "remote", "get-url", "origin")
	if err != nil {
		t.Fatalf("get origin URL: %v", err)
	}

	// Use a throwaway clone to author the commit, then push to origin.
	tmp := t.TempDir()
	for _, args := range [][]string{
		{"clone", originDir, tmp},
		{"-C", tmp, "config", "user.email", "upstream@test.com"},
		{"-C", tmp, "config", "user.name", "Upstream"},
		{"-C", tmp, "checkout", branch},
		{"-C", tmp, "commit", "--allow-empty", "-m", "upstream commit"},
		{"-C", tmp, "push", "origin", branch},
	} {
		cmd := exec.Command("git", args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	sha, err := runGit(context.Background(), tmp, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return sha
}

func TestEnsureBaseBranchReadyForSync_CleanAndFFSafe(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())

	if err := mgr.EnsureBaseBranchReadyForSync(context.Background(), repo, "main"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestEnsureBaseBranchReadyForSync_DirtyTreeOnBase(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())

	// Create an untracked file so `git status --porcelain` reports dirty.
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write untracked: %v", err)
	}

	err := mgr.EnsureBaseBranchReadyForSync(context.Background(), repo, "main")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrBaseBranchNotReady) {
		t.Errorf("expected ErrBaseBranchNotReady, got %v", err)
	}
	if !strings.Contains(err.Error(), "uncommitted changes") {
		t.Errorf("expected 'uncommitted changes' in message, got %q", err.Error())
	}
}

func TestEnsureBaseBranchReadyForSync_DirtyTreeNotOnBase(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())

	// Check out a different branch so the dirty-tree rule does not apply.
	if _, err := runGit(context.Background(), repo, "checkout", "-b", "feature"); err != nil {
		t.Fatalf("checkout feature: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write untracked: %v", err)
	}

	if err := mgr.EnsureBaseBranchReadyForSync(context.Background(), repo, "main"); err != nil {
		t.Errorf("expected nil when base is not checked out, got %v", err)
	}
}

func TestEnsureBaseBranchReadyForSync_Diverged(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())

	// Advance origin/main by one commit…
	commitOnOrigin(t, repo, "main")

	// …then advance local main by a different commit. Now local main is
	// neither an ancestor of nor equal to origin/main.
	for _, args := range [][]string{
		{"commit", "--allow-empty", "-m", "local commit"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	err := mgr.EnsureBaseBranchReadyForSync(context.Background(), repo, "main")
	if err == nil {
		t.Fatal("expected divergence error, got nil")
	}
	if !errors.Is(err, ErrBaseBranchNotReady) {
		t.Errorf("expected ErrBaseBranchNotReady, got %v", err)
	}
	if !strings.Contains(err.Error(), "diverged") {
		t.Errorf("expected 'diverged' in message, got %q", err.Error())
	}
}

func TestSyncBaseBranch_BaseNotCheckedOut(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())

	// Switch away from main before syncing.
	if _, err := runGit(context.Background(), repo, "checkout", "-b", "feature"); err != nil {
		t.Fatalf("checkout feature: %v", err)
	}

	wantSHA := commitOnOrigin(t, repo, "main")

	if err := mgr.SyncBaseBranch(context.Background(), repo, "main"); err != nil {
		t.Fatalf("SyncBaseBranch: %v", err)
	}

	gotSHA, err := runGit(context.Background(), repo, "rev-parse", "refs/heads/main")
	if err != nil {
		t.Fatalf("rev-parse main: %v", err)
	}
	if gotSHA != wantSHA {
		t.Errorf("local main at %s, want %s", gotSHA, wantSHA)
	}

	// Working tree stayed on feature.
	head, err := runGit(context.Background(), repo, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		t.Fatalf("symbolic-ref: %v", err)
	}
	if head != "feature" {
		t.Errorf("HEAD = %q, want %q", head, "feature")
	}
}

func TestSyncBaseBranch_BaseCheckedOut(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())

	wantSHA := commitOnOrigin(t, repo, "main")

	if err := mgr.SyncBaseBranch(context.Background(), repo, "main"); err != nil {
		t.Fatalf("SyncBaseBranch: %v", err)
	}

	gotSHA, err := runGit(context.Background(), repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	if gotSHA != wantSHA {
		t.Errorf("HEAD at %s, want %s", gotSHA, wantSHA)
	}
}

// TestRunSetupScript_CommandArgvStaysLiteral confirms that shell metachars
// in a type=command argv never hit a shell interpreter — this is the core
// reason `sh -c` was removed.
func TestRunSetupScript_CommandArgvStaysLiteral(t *testing.T) {
	worktree := t.TempDir()

	// If argv were ever concatenated into a shell command, the ';' would
	// split and the second half would run. With direct exec, the second
	// arg is a literal string passed to echo.
	sentinel := filepath.Join(t.TempDir(), "sentinel")
	spec := `{"type":"command","argv":["echo","; touch ` + sentinel + `"]}`

	if err := runSetupScript(context.Background(), worktree, worktree, spec, nil); err != nil {
		t.Fatalf("runSetupScript: %v", err)
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatalf("sentinel was created — argv was interpreted as shell")
	} else if !os.IsNotExist(err) {
		t.Fatalf("unexpected stat error: %v", err)
	}
}
