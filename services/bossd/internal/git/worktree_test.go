package git

import (
	"context"
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
