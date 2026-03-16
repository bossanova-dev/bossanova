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

// initTestRepo creates a bare-minimum git repo in a temp dir with an initial commit.
func initTestRepo(t *testing.T) string {
	t.Helper()
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
	return dir
}

func TestSanitizeBranchName(t *testing.T) {
	tests := []struct {
		title string
		want  string
	}{
		{"Fix the login bug!", "boss/fix-the-login-bug"},
		{"Add README.md", "boss/add-readme-md"},
		{"  spaces  ", "boss/spaces"},
		{"UPPER CASE", "boss/upper-case"},
		{"a/b/c", "boss/a-b-c"},
		{strings.Repeat("x", 100), "boss/" + strings.Repeat("x", 60)},
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
		Title:           "Test session",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if result.BranchName != "boss/test-session" {
		t.Errorf("branch = %q, want %q", result.BranchName, "boss/test-session")
	}

	// Verify worktree directory exists.
	if _, err := os.Stat(result.WorktreePath); err != nil {
		t.Errorf("worktree dir not found: %v", err)
	}

	// Verify branch exists.
	out, err := runGit(context.Background(), repoDir, "branch", "--list", result.BranchName)
	if err != nil {
		t.Fatalf("list branches: %v", err)
	}
	if !strings.Contains(out, "boss/test-session") {
		t.Errorf("branch not found in: %q", out)
	}
}

func TestCreateWithSetupScript(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	logger := zerolog.Nop()
	mgr := NewManager(logger)

	script := "echo hello > setup-done.txt"
	result, err := mgr.Create(context.Background(), CreateOpts{
		RepoPath:        repoDir,
		BaseBranch:      "main",
		WorktreeBaseDir: wtBase,
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
	if !strings.Contains(out, "boss/archive-test") {
		t.Errorf("branch should still exist after archive, got: %q", out)
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
	if strings.Contains(out, "boss/trash-test") {
		t.Errorf("branch should be deleted after empty trash, got: %q", out)
	}
}

func TestDetectOriginURL(t *testing.T) {
	repoDir := initTestRepo(t)
	logger := zerolog.Nop()
	mgr := NewManager(logger)

	// No origin configured yet — should return empty.
	url, err := mgr.DetectOriginURL(context.Background(), repoDir)
	if err != nil {
		t.Fatalf("DetectOriginURL: %v", err)
	}
	if url != "" {
		t.Errorf("expected empty URL, got %q", url)
	}

	// Add an origin remote.
	if _, err := runGit(context.Background(), repoDir, "remote", "add", "origin", "https://github.com/test/repo.git"); err != nil {
		t.Fatalf("add remote: %v", err)
	}

	url, err = mgr.DetectOriginURL(context.Background(), repoDir)
	if err != nil {
		t.Fatalf("DetectOriginURL: %v", err)
	}
	if url != "https://github.com/test/repo.git" {
		t.Errorf("URL = %q, want %q", url, "https://github.com/test/repo.git")
	}
}
