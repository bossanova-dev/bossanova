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

// TestCreate_IgnoresBossDir verifies that after a worktree is created,
// the .boss/ directory used for Claude session state is git-ignored
// inside that worktree (so it doesn't pollute `git status`).
func TestCreate_IgnoresBossDir(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	result, err := mgr.Create(ctx, CreateOpts{
		RepoPath:        repoDir,
		BaseBranch:      "main",
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		Title:           "Ignore boss",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Drop a fake .boss/ entry so check-ignore has something to match.
	bossDir := filepath.Join(result.WorktreePath, ".boss")
	if err := os.MkdirAll(bossDir, 0o755); err != nil {
		t.Fatalf("mkdir .boss: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bossDir, "claude.log"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write claude.log: %v", err)
	}

	// `git check-ignore` exits 0 when the path is ignored, 1 when not.
	cmd := exec.Command("git", "check-ignore", "-v", ".boss/claude.log")
	cmd.Dir = result.WorktreePath
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf(".boss/claude.log is not ignored. git check-ignore output:\n%s\nerr: %v", out, err)
	}
	if !strings.Contains(string(out), ".boss") {
		t.Errorf("check-ignore output does not mention .boss: %s", out)
	}

	// `git status --porcelain` should be clean (no untracked .boss).
	status, err := runGit(ctx, result.WorktreePath, "status", "--porcelain")
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if status != "" {
		t.Errorf("expected clean status, got: %q", status)
	}
}

// TestCreate_IgnoreIsIdempotent verifies that creating multiple worktrees
// of the same repo does not append duplicate .boss/ entries to
// .git/info/exclude (which is shared via $GIT_COMMON_DIR).
func TestCreate_IgnoreIsIdempotent(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	for i, title := range []string{"first", "second", "third"} {
		if _, err := mgr.Create(ctx, CreateOpts{
			RepoPath:        repoDir,
			BaseBranch:      "main",
			WorktreeBaseDir: wtBase,
			RepoName:        "my-repo",
			Title:           title,
		}); err != nil {
			t.Fatalf("Create #%d (%q): %v", i, title, err)
		}
	}

	excludePath := filepath.Join(repoDir, ".git", "info", "exclude")
	body, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read exclude: %v", err)
	}
	count := strings.Count(string(body), ".boss/")
	if count != 1 {
		t.Errorf(".boss/ appears %d times in info/exclude, want 1. Body:\n%s", count, body)
	}
}

// TestCreate_IgnorePreservesExistingExcludes verifies that adding our
// pattern doesn't clobber pre-existing user content in info/exclude.
func TestCreate_IgnorePreservesExistingExcludes(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	// Seed info/exclude with a user pattern.
	excludePath := filepath.Join(repoDir, ".git", "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		t.Fatalf("mkdir info: %v", err)
	}
	const userPattern = "user-private.txt\n"
	if err := os.WriteFile(excludePath, []byte(userPattern), 0o644); err != nil {
		t.Fatalf("seed exclude: %v", err)
	}

	if _, err := mgr.Create(ctx, CreateOpts{
		RepoPath:        repoDir,
		BaseBranch:      "main",
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		Title:           "Preserve user",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	body, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read exclude: %v", err)
	}
	if !strings.Contains(string(body), "user-private.txt") {
		t.Errorf("user pattern lost. Body:\n%s", body)
	}
	if !strings.Contains(string(body), ".boss/") {
		t.Errorf(".boss/ pattern not added. Body:\n%s", body)
	}
}

// TestCreateFromExistingBranch_IgnoresBossDir verifies the same ignore
// behavior is applied when creating a worktree from an existing branch
// (e.g. for PR review sessions).
func TestCreateFromExistingBranch_IgnoresBossDir(t *testing.T) {
	repoDir := initTestRepo(t)

	// Create a branch on origin so CreateFromExistingBranch can fetch it.
	for _, args := range [][]string{
		{"checkout", "-b", "feature"},
		{"commit", "--allow-empty", "-m", "feature"},
		{"push", "origin", "feature"},
		{"checkout", "main"},
		{"branch", "-D", "feature"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	wtBase := filepath.Join(t.TempDir(), "worktrees")
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	result, err := mgr.CreateFromExistingBranch(ctx, CreateFromExistingBranchOpts{
		RepoPath:        repoDir,
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		BranchName:      "feature",
	})
	if err != nil {
		t.Fatalf("CreateFromExistingBranch: %v", err)
	}

	bossDir := filepath.Join(result.WorktreePath, ".boss")
	if err := os.MkdirAll(bossDir, 0o755); err != nil {
		t.Fatalf("mkdir .boss: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bossDir, "claude.log"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write claude.log: %v", err)
	}

	cmd := exec.Command("git", "check-ignore", "-v", ".boss/claude.log")
	cmd.Dir = result.WorktreePath
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf(".boss/claude.log not ignored. output:\n%s\nerr: %v", out, err)
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

// TestResurrect_IgnoresBossDir covers the case where a worktree predates
// the .boss/ ignore feature (or info/exclude was hand-cleaned): Resurrect
// must re-apply the bossd-managed exclude so .boss/ doesn't show up in
// `git status` after the worktree comes back.
func TestResurrect_IgnoresBossDir(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	result, err := mgr.Create(ctx, CreateOpts{
		RepoPath:        repoDir,
		BaseBranch:      "main",
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		Title:           "Resurrect ignore",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Archive(ctx, result.WorktreePath); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	// Simulate a stale info/exclude (worktree predates the feature, or
	// user wiped the file by hand) by truncating it.
	excludePath := filepath.Join(repoDir, ".git", "info", "exclude")
	if err := os.WriteFile(excludePath, nil, 0o644); err != nil {
		t.Fatalf("truncate exclude: %v", err)
	}

	if err := mgr.Resurrect(ctx, ResurrectOpts{
		RepoPath:     repoDir,
		WorktreePath: result.WorktreePath,
		BranchName:   result.BranchName,
	}); err != nil {
		t.Fatalf("Resurrect: %v", err)
	}

	bossDir := filepath.Join(result.WorktreePath, ".boss")
	if err := os.MkdirAll(bossDir, 0o755); err != nil {
		t.Fatalf("mkdir .boss: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bossDir, "claude.log"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write claude.log: %v", err)
	}

	cmd := exec.Command("git", "check-ignore", "-v", ".boss/claude.log")
	cmd.Dir = result.WorktreePath
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf(".boss/claude.log not ignored after Resurrect. output:\n%s\nerr: %v", out, err)
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
func commitOnOrigin(t *testing.T, workingRepo, branch string) string { //nolint:unparam // branch is always "main" today, but future tests will push to other branches (e.g. develop)
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

func TestIsAncestor(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	baseSHA, err := runGit(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}

	ok, err := mgr.IsAncestor(ctx, repo, baseSHA, "refs/heads/main")
	if err != nil {
		t.Fatalf("IsAncestor(self): %v", err)
	}
	if !ok {
		t.Error("IsAncestor(self) = false, want true")
	}

	// Commit on a sibling branch that isn't reachable from main.
	if _, err := runGit(ctx, repo, "checkout", "-b", "side", baseSHA); err != nil {
		t.Fatalf("checkout -b side: %v", err)
	}
	if _, err := runGit(ctx, repo, "commit", "--allow-empty", "-m", "diverged"); err != nil {
		t.Fatalf("commit on side: %v", err)
	}
	divergedSHA, err := runGit(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse side: %v", err)
	}
	if _, err := runGit(ctx, repo, "checkout", "main"); err != nil {
		t.Fatalf("checkout main: %v", err)
	}

	ok, err = mgr.IsAncestor(ctx, repo, divergedSHA, "refs/heads/main")
	if err != nil {
		t.Fatalf("IsAncestor(diverged): %v", err)
	}
	if ok {
		t.Error("IsAncestor(diverged commit) = true, want false")
	}
}

func TestFetchBase(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	// Add a commit to origin/main that the local clone hasn't seen.
	upstreamSHA := commitOnOrigin(t, repo, "main")

	if err := mgr.FetchBase(ctx, repo, "main"); err != nil {
		t.Fatalf("FetchBase: %v", err)
	}

	remoteSHA, err := runGit(ctx, repo, "rev-parse", "refs/remotes/origin/main")
	if err != nil {
		t.Fatalf("rev-parse origin/main: %v", err)
	}
	if remoteSHA != upstreamSHA {
		t.Errorf("origin/main = %s, want %s", remoteSHA, upstreamSHA)
	}

	if err := mgr.FetchBase(ctx, repo, ""); err == nil {
		t.Error("FetchBase with empty base should error")
	}
}

func TestMergeLocalBranch_MergeStrategy(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	// Create a feature branch with a commit.
	if _, err := runGit(ctx, repo, "checkout", "-b", "feat"); err != nil {
		t.Fatalf("checkout -b feat: %v", err)
	}
	featSHA, err := runGit(ctx, repo, "commit", "--allow-empty", "-m", "feat commit")
	_ = featSHA
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	featSHA, err = runGit(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse feat: %v", err)
	}
	if _, err := runGit(ctx, repo, "checkout", "main"); err != nil {
		t.Fatalf("checkout main: %v", err)
	}

	if err := mgr.MergeLocalBranch(ctx, repo, "main", "feat", "merge"); err != nil {
		t.Fatalf("MergeLocalBranch: %v", err)
	}

	// main should be a merge commit with feat's commit as a parent.
	ok, err := mgr.IsAncestor(ctx, repo, featSHA, "refs/heads/main")
	if err != nil {
		t.Fatalf("IsAncestor: %v", err)
	}
	if !ok {
		t.Error("feat commit is not reachable from main after merge")
	}

	// feat branch should have been deleted (`branch -d` on merged branch).
	if _, err := runGit(ctx, repo, "rev-parse", "--verify", "refs/heads/feat"); err == nil {
		t.Error("feat branch still exists; expected it to be deleted post-merge")
	}
}

func TestMergeLocalBranch_SquashStrategy(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	if _, err := runGit(ctx, repo, "checkout", "-b", "feat"); err != nil {
		t.Fatalf("checkout -b feat: %v", err)
	}
	// Use a real file change so the squash commit has content (empty squash
	// commits require --allow-empty, which isn't the common case).
	if err := os.WriteFile(filepath.Join(repo, "feat.txt"), []byte("feat content\n"), 0o644); err != nil {
		t.Fatalf("write feat.txt: %v", err)
	}
	if _, err := runGit(ctx, repo, "add", "feat.txt"); err != nil {
		t.Fatalf("add feat.txt: %v", err)
	}
	if _, err := runGit(ctx, repo, "commit", "-m", "feat1"); err != nil {
		t.Fatalf("commit 1: %v", err)
	}
	if _, err := runGit(ctx, repo, "checkout", "main"); err != nil {
		t.Fatalf("checkout main: %v", err)
	}

	mainBefore, err := runGit(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}

	if err := mgr.MergeLocalBranch(ctx, repo, "main", "feat", "squash"); err != nil {
		t.Fatalf("MergeLocalBranch squash: %v", err)
	}

	mainAfter, err := runGit(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse after: %v", err)
	}
	if mainAfter == mainBefore {
		t.Error("main didn't advance after squash merge")
	}
	// Squash produces a single linear commit — main should have exactly one
	// parent, not two (as a true merge would).
	parents, err := runGit(ctx, repo, "rev-list", "--parents", "-n", "1", "HEAD")
	if err != nil {
		t.Fatalf("rev-list parents: %v", err)
	}
	parts := strings.Fields(parents)
	if len(parts) != 2 { // [commit-sha, parent-sha]
		t.Errorf("squash merge produced %d parents, want 1: %s", len(parts)-1, parents)
	}
	// feat branch deleted after successful squash merge. Squash records no
	// merge relationship in the DAG, so this exercises the `-D` branch of
	// the deletion logic.
	if _, err := runGit(ctx, repo, "rev-parse", "--verify", "refs/heads/feat"); err == nil {
		t.Error("feat branch still exists after squash merge; expected deletion")
	}
}

func TestMergeLocalBranch_RebaseStrategy(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	// Create feat with real content so rebase has something to replay.
	if _, err := runGit(ctx, repo, "checkout", "-b", "feat"); err != nil {
		t.Fatalf("checkout feat: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("feat\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := runGit(ctx, repo, "add", "f.txt"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := runGit(ctx, repo, "commit", "-m", "feat"); err != nil {
		t.Fatalf("commit feat: %v", err)
	}
	if _, err := runGit(ctx, repo, "checkout", "main"); err != nil {
		t.Fatalf("checkout main: %v", err)
	}

	if err := mgr.MergeLocalBranch(ctx, repo, "main", "feat", "rebase"); err != nil {
		t.Fatalf("rebase merge: %v", err)
	}

	// Rebase must produce linear history — HEAD has exactly one parent.
	parents, err := runGit(ctx, repo, "rev-list", "--parents", "-n", "1", "HEAD")
	if err != nil {
		t.Fatalf("rev-list: %v", err)
	}
	if parts := strings.Fields(parents); len(parts) != 2 {
		t.Errorf("rebase produced %d parents, want 1 (linear): %s", len(parts)-1, parents)
	}
	// feat branch deleted after successful merge.
	if _, err := runGit(ctx, repo, "rev-parse", "--verify", "refs/heads/feat"); err == nil {
		t.Error("feat branch still exists after rebase-merge; expected deletion")
	}
}

func TestMergeLocalBranch_Rejections(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name       string
		base       string
		head       string
		strategy   string
		wantSubstr string
	}{
		{"empty base", "", "feat", "merge", "base branch is required"},
		{"empty head", "main", "", "merge", "head branch is required"},
		{"unknown strategy", "main", "feat", "cherry-pick", "unknown merge strategy"},
		{"missing base branch", "nope", "feat", "merge", "does not exist"},
		{"missing head branch", "main", "nope", "merge", "does not exist"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := initTestRepo(t)
			mgr := NewManager(zerolog.Nop())
			// Every case except "missing head" needs a feat branch to exist,
			// so create one unconditionally (it's cheap and simplifies setup).
			if _, err := runGit(ctx, repo, "checkout", "-b", "feat"); err != nil {
				t.Fatalf("checkout feat: %v", err)
			}
			if _, err := runGit(ctx, repo, "commit", "--allow-empty", "-m", "f"); err != nil {
				t.Fatalf("commit: %v", err)
			}
			if _, err := runGit(ctx, repo, "checkout", "main"); err != nil {
				t.Fatalf("checkout main: %v", err)
			}
			err := mgr.MergeLocalBranch(ctx, repo, tc.base, tc.head, tc.strategy)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSubstr)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("err = %v; want substring %q", err, tc.wantSubstr)
			}
		})
	}
}

func TestMergeLocalBranch_RejectsDivergedOrigin(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	// Push a commit directly to origin (via a throwaway clone) so origin
	// is ahead of local main.
	commitOnOrigin(t, repo, "main")

	// Now make a local-main commit without pulling. Local and origin diverge.
	if _, err := runGit(ctx, repo, "commit", "--allow-empty", "-m", "local-only"); err != nil {
		t.Fatalf("local commit: %v", err)
	}
	if _, err := runGit(ctx, repo, "checkout", "-b", "feat"); err != nil {
		t.Fatalf("checkout feat: %v", err)
	}
	if _, err := runGit(ctx, repo, "commit", "--allow-empty", "-m", "feat"); err != nil {
		t.Fatalf("commit feat: %v", err)
	}
	if _, err := runGit(ctx, repo, "checkout", "main"); err != nil {
		t.Fatalf("checkout main: %v", err)
	}

	err := mgr.MergeLocalBranch(ctx, repo, "main", "feat", "merge")
	if !errors.Is(err, ErrBaseBranchNotReady) {
		t.Fatalf("want ErrBaseBranchNotReady, got %v", err)
	}
}

func TestMergeLocalBranch_RejectsDirtyTree(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	if _, err := runGit(ctx, repo, "checkout", "-b", "feat"); err != nil {
		t.Fatalf("checkout -b feat: %v", err)
	}
	if _, err := runGit(ctx, repo, "commit", "--allow-empty", "-m", "feat"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := runGit(ctx, repo, "checkout", "main"); err != nil {
		t.Fatalf("checkout main: %v", err)
	}

	// Create an uncommitted change on main.
	if err := os.WriteFile(filepath.Join(repo, "dirty.txt"), []byte("wip"), 0o644); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}
	if _, err := runGit(ctx, repo, "add", "dirty.txt"); err != nil {
		t.Fatalf("git add: %v", err)
	}

	err := mgr.MergeLocalBranch(ctx, repo, "main", "feat", "merge")
	if !errors.Is(err, ErrBaseBranchNotReady) {
		t.Fatalf("want ErrBaseBranchNotReady, got %v", err)
	}
}

func TestMergeLocalBranch_Conflict(t *testing.T) {
	// Local-only repo (no origin) so the divergence check doesn't fire —
	// this test is specifically about conflict handling during the merge
	// step itself.
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	// Seed main with a base version of the file.
	conflictFile := filepath.Join(repo, "conflict.txt")
	if err := os.WriteFile(conflictFile, []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write base: %v", err)
	}
	if _, err := runGit(ctx, repo, "add", "conflict.txt"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := runGit(ctx, repo, "commit", "-m", "base content"); err != nil {
		t.Fatalf("commit base: %v", err)
	}

	// feat branch modifies the file one way.
	if _, err := runGit(ctx, repo, "checkout", "-b", "feat"); err != nil {
		t.Fatalf("checkout feat: %v", err)
	}
	if err := os.WriteFile(conflictFile, []byte("feat version\n"), 0o644); err != nil {
		t.Fatalf("write feat: %v", err)
	}
	if _, err := runGit(ctx, repo, "commit", "-am", "feat change"); err != nil {
		t.Fatalf("commit feat: %v", err)
	}

	// main modifies the same line a different way — this is the conflict.
	if _, err := runGit(ctx, repo, "checkout", "main"); err != nil {
		t.Fatalf("checkout main: %v", err)
	}
	if err := os.WriteFile(conflictFile, []byte("main version\n"), 0o644); err != nil {
		t.Fatalf("write main: %v", err)
	}
	if _, err := runGit(ctx, repo, "commit", "-am", "main change"); err != nil {
		t.Fatalf("commit main: %v", err)
	}

	// Capture main's SHA pre-merge so we can verify the abort left it untouched.
	preMergeSHA, err := runGit(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse pre-merge: %v", err)
	}

	err = mgr.MergeLocalBranch(ctx, repo, "main", "feat", "merge")
	if !errors.Is(err, ErrMergeConflict) {
		t.Fatalf("want ErrMergeConflict, got %v", err)
	}

	// Strong post-abort invariant: HEAD is exactly where it was, and the
	// working tree is clean. Substring-matching on `status --porcelain`
	// misses several valid conflict states (UD, DU, AU, DD, etc.), so assert
	// the real invariant instead.
	postMergeSHA, _ := runGit(ctx, repo, "rev-parse", "HEAD")
	if postMergeSHA != preMergeSHA {
		t.Errorf("HEAD moved after aborted merge: pre=%s post=%s", preMergeSHA, postMergeSHA)
	}
	status, _ := runGit(ctx, repo, "status", "--porcelain")
	if status != "" {
		t.Errorf("repo left with uncommitted state after abort: %s", status)
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
