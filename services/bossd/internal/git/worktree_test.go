package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

func TestIsBranchAlreadyExistsGitOutput(t *testing.T) {
	err := fmt.Errorf("git worktree add -b dave/won-470 /tmp/wt origin/dev: exit status 255: Preparing worktree (new branch 'dave/won-470')\nfatal: a branch named 'dave/won-470' already exists")
	if !isBranchAlreadyExistsGitOutput(err) {
		t.Fatal("expected branch-already-exists git output to be detected")
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
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

// TestCreate_FailingSetupScriptIsNonFatal pins the degraded-but-created
// behaviour: a setup script that exits non-zero must not abort worktree
// creation. The worktree is valid, so Create returns a result with SetupErr
// populated and the directory present on disk.
func TestCreate_FailingSetupScriptIsNonFatal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell assumed")
	}
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	mgr := NewManager(zerolog.Nop())

	failing := "exit 3"
	result, err := mgr.Create(context.Background(), CreateOpts{
		RepoPath:        repoDir,
		BaseBranch:      "main",
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		Title:           "Test session",
		SetupScript:     &failing,
	})
	if err != nil {
		t.Fatalf("Create should not fail on a failing setup script: %v", err)
	}
	if result.SetupErr == nil {
		t.Fatal("expected result.SetupErr to be set when the setup script fails")
	}
	if !strings.Contains(result.SetupErr.Error(), "setup script") {
		t.Fatalf("SetupErr should identify the setup script: %v", result.SetupErr)
	}
	// The worktree itself must still exist and be on the created branch.
	if _, err := os.Stat(result.WorktreePath); err != nil {
		t.Fatalf("worktree dir should exist despite setup failure: %v", err)
	}
	current := gitOutput(t, result.WorktreePath, "branch", "--show-current")
	if current != result.BranchName {
		t.Fatalf("current branch = %q, want %q", current, result.BranchName)
	}
}

func TestCreateLeavesWorktreeOnCreatedBranch(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	mgr := NewManager(zerolog.Nop())

	result, err := mgr.Create(context.Background(), CreateOpts{
		RepoPath:        repoDir,
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		Title:           "Fix Camera Crash",
		BaseBranch:      "main",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	current := gitOutput(t, result.WorktreePath, "branch", "--show-current")
	if current != result.BranchName {
		t.Fatalf("current branch = %q, want %q", current, result.BranchName)
	}
}

func TestManager_CurrentBranch_ReturnsCheckedOutBranch(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	result, err := mgr.Create(ctx, CreateOpts{
		RepoPath:        repoDir,
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		Title:           "Fix Camera Crash",
		BaseBranch:      "main",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	gitOutput(t, result.WorktreePath, "checkout", "-b", "feature/x")

	got, err := mgr.CurrentBranch(ctx, result.WorktreePath)
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	if got != "feature/x" {
		t.Fatalf("CurrentBranch = %q, want %q", got, "feature/x")
	}
}

func TestCreateUsesOriginBaseWhenRootCheckoutIsDifferent(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	mgr := NewManager(zerolog.Nop())

	mainSHA := gitOutput(t, repoDir, "rev-parse", "origin/main")
	gitOutput(t, repoDir, "checkout", "-b", "production")
	gitOutput(t, repoDir, "commit", "--allow-empty", "-m", "production-only")

	result, err := mgr.Create(context.Background(), CreateOpts{
		RepoPath:        repoDir,
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		Title:           "Fix Camera Crash",
		BaseBranch:      "main",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	gotSHA := gitOutput(t, result.WorktreePath, "rev-parse", "HEAD")
	if gotSHA != mainSHA {
		t.Fatalf("worktree HEAD = %s, want origin/main %s", gotSHA, mainSHA)
	}
}

func TestCreateSuffixesGeneratedBranchWhenRemoteBranchExists(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	mgr := NewManager(zerolog.Nop())

	gitOutput(t, repoDir, "branch", "test-session")
	gitOutput(t, repoDir, "push", "origin", "test-session")
	gitOutput(t, repoDir, "branch", "-D", "test-session")

	result, err := mgr.Create(context.Background(), CreateOpts{
		RepoPath:        repoDir,
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		Title:           "Test session",
		BaseBranch:      "main",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if result.BranchName != "test-session-2" {
		t.Fatalf("branch = %q, want test-session-2", result.BranchName)
	}
}

func TestCreateDoesNotSuffixGeneratedBranchWhenOnlyNamespacedRemoteBranchExists(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	mgr := NewManager(zerolog.Nop())

	gitOutput(t, repoDir, "checkout", "-b", "team/test-session")
	gitOutput(t, repoDir, "commit", "--allow-empty", "-m", "remote namespaced branch")
	gitOutput(t, repoDir, "push", "origin", "team/test-session")
	gitOutput(t, repoDir, "checkout", "main")
	gitOutput(t, repoDir, "branch", "-D", "team/test-session")

	result, err := mgr.Create(context.Background(), CreateOpts{
		RepoPath:        repoDir,
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		Title:           "Test session",
		BaseBranch:      "main",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if result.BranchName != "test-session" {
		t.Fatalf("branch = %q, want test-session", result.BranchName)
	}
}

func TestCreateFailsWhenRemoteBaseMissing(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	mgr := NewManager(zerolog.Nop())

	_, err := mgr.Create(context.Background(), CreateOpts{
		RepoPath:        repoDir,
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		Title:           "Test session",
		BaseBranch:      "missing",
	})
	if err == nil {
		t.Fatal("Create: want error for missing base branch, got nil")
	}
	if !strings.Contains(err.Error(), "fetch origin/missing") {
		t.Fatalf("error = %v, want fetch origin/missing", err)
	}
}

func TestVerifyCurrentBranchDetectsMismatch(t *testing.T) {
	repoDir := initTestRepo(t)
	ctx := context.Background()
	mgr := NewManager(zerolog.Nop())

	for _, args := range [][]string{
		{"checkout", "-b", "production"},
		{"commit", "--allow-empty", "-m", "production commit"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	err := mgr.verifyCurrentBranch(ctx, repoDir, "fix-camera-crash")
	if err == nil {
		t.Fatal("verifyCurrentBranch returned nil, want mismatch error")
	}
	if !strings.Contains(err.Error(), `worktree is on branch "production", expected "fix-camera-crash"`) {
		t.Fatalf("error = %q, want branch mismatch details", err)
	}
}

func TestBranchDebugSnapshotReportsBranchState(t *testing.T) {
	repoDir := initTestRepo(t)
	ctx := context.Background()
	mgr := NewManager(zerolog.Nop())

	gitOutput(t, repoDir, "checkout", "-b", "fix-camera-crash")
	gitOutput(t, repoDir, "commit", "--allow-empty", "-m", "remote feature")
	gitOutput(t, repoDir, "push", "-u", "origin", "fix-camera-crash")
	remoteHead := gitOutput(t, repoDir, "rev-parse", "HEAD")
	gitOutput(t, repoDir, "commit", "--allow-empty", "-m", "local feature")
	head := gitOutput(t, repoDir, "rev-parse", "HEAD")

	snapshot, err := mgr.BranchDebugSnapshot(ctx, repoDir, "fix-camera-crash", "main")
	if err != nil {
		t.Fatalf("BranchDebugSnapshot: %v", err)
	}

	if snapshot.CurrentBranch != "fix-camera-crash" {
		t.Fatalf("CurrentBranch = %q, want fix-camera-crash", snapshot.CurrentBranch)
	}
	if snapshot.HeadSHA != head {
		t.Fatalf("HeadSHA = %q, want %q", snapshot.HeadSHA, head)
	}
	if snapshot.RemoteHeadSHA != remoteHead {
		t.Fatalf("RemoteHeadSHA = %q, want %q", snapshot.RemoteHeadSHA, remoteHead)
	}
	if snapshot.AheadBehind != "0\t2" {
		t.Fatalf("AheadBehind = %q, want %q", snapshot.AheadBehind, "0\t2")
	}
}

// TestBranchDebugSnapshotToleratesDetachedHeadAndMissingRemote verifies the
// snapshot stays useful in the failure states it exists to diagnose: a detached
// HEAD is reported as "(detached)" rather than aborting the snapshot, and a
// missing remote ref / base yields the explicit "<none>" sentinel instead of an
// ambiguous empty string.
func TestBranchDebugSnapshotToleratesDetachedHeadAndMissingRemote(t *testing.T) {
	repoDir := initTestRepo(t)
	ctx := context.Background()
	mgr := NewManager(zerolog.Nop())

	head := gitOutput(t, repoDir, "rev-parse", "HEAD")
	// Detach HEAD and ask about a branch that was never pushed.
	gitOutput(t, repoDir, "checkout", "--detach", "HEAD")

	snapshot, err := mgr.BranchDebugSnapshot(ctx, repoDir, "never-pushed", "missing-base")
	if err != nil {
		t.Fatalf("BranchDebugSnapshot: %v", err)
	}

	if snapshot.CurrentBranch != "(detached)" {
		t.Fatalf("CurrentBranch = %q, want %q", snapshot.CurrentBranch, "(detached)")
	}
	if snapshot.HeadSHA != head {
		t.Fatalf("HeadSHA = %q, want %q", snapshot.HeadSHA, head)
	}
	if snapshot.RemoteHeadSHA != "<none>" {
		t.Fatalf("RemoteHeadSHA = %q, want %q", snapshot.RemoteHeadSHA, "<none>")
	}
	if snapshot.AheadBehind != "<none>" {
		t.Fatalf("AheadBehind = %q, want %q", snapshot.AheadBehind, "<none>")
	}
}

// TestCreate_RemovesStaleWorktreeDir verifies that Create self-heals when a
// leftover directory occupies the target path (e.g. from an orphaned prior
// session). Without the cleanup, `git worktree add` fails with "already exists"
// and wedges the branch forever (the dependabot repair loop).
func TestCreate_RemovesStaleWorktreeDir(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	// Pre-create a stale, non-empty directory at the exact path Create will use.
	stalePath := filepath.Join(wtBase, "my-repo", "test-session")
	if err := os.MkdirAll(stalePath, 0o755); err != nil {
		t.Fatalf("mkdir stale: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stalePath, "leftover.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write leftover: %v", err)
	}

	result, err := mgr.Create(ctx, CreateOpts{
		RepoPath:        repoDir,
		BaseBranch:      "main",
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		Title:           "Test session",
	})
	if err != nil {
		t.Fatalf("Create over stale dir: %v", err)
	}
	if result.WorktreePath != stalePath {
		t.Fatalf("worktree path = %q, want %q", result.WorktreePath, stalePath)
	}
	// The stale file must be gone (the dir was recreated as a fresh worktree).
	if _, err := os.Stat(filepath.Join(stalePath, "leftover.txt")); !os.IsNotExist(err) {
		t.Fatalf("stale file still present (err=%v)", err)
	}
}

func TestCreateFromExistingBranchLeavesWorktreeOnRequestedBranch(t *testing.T) {
	repoDir := initTestRepo(t)
	for _, args := range [][]string{
		{"checkout", "-b", "fix-camera-crash"},
		{"commit", "--allow-empty", "-m", "feature"},
		{"push", "origin", "fix-camera-crash"},
		{"checkout", "main"},
		{"branch", "-D", "fix-camera-crash"},
	} {
		gitOutput(t, repoDir, args...)
	}

	wtBase := filepath.Join(t.TempDir(), "worktrees")
	mgr := NewManager(zerolog.Nop())
	result, err := mgr.CreateFromExistingBranch(context.Background(), CreateFromExistingBranchOpts{
		RepoPath:        repoDir,
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		BranchName:      "fix-camera-crash",
	})
	if err != nil {
		t.Fatalf("CreateFromExistingBranch: %v", err)
	}

	current := gitOutput(t, result.WorktreePath, "branch", "--show-current")
	if current != "fix-camera-crash" {
		t.Fatalf("current branch = %q, want fix-camera-crash", current)
	}
}

func TestCreateFromExistingBranchForceRefreshesRewrittenBranch(t *testing.T) {
	repoDir := initTestRepo(t)
	branch := "force-refresh-existing"

	for _, args := range [][]string{
		{"checkout", "-b", branch},
		{"commit", "--allow-empty", "-m", "old feature"},
		{"push", "origin", branch},
		{"fetch", "origin", branch + ":refs/remotes/origin/" + branch},
		{"checkout", "main"},
		{"checkout", "-b", "rewritten-feature"},
		{"commit", "--allow-empty", "-m", "rewritten feature"},
		{"push", "--force", "origin", "rewritten-feature:" + branch},
	} {
		gitOutput(t, repoDir, args...)
	}
	newCommit := gitOutput(t, repoDir, "rev-parse", "HEAD")
	for _, args := range [][]string{
		{"checkout", "main"},
		{"branch", "-D", "rewritten-feature"},
	} {
		gitOutput(t, repoDir, args...)
	}

	wtBase := filepath.Join(t.TempDir(), "worktrees")
	mgr := NewManager(zerolog.Nop())
	result, err := mgr.CreateFromExistingBranch(context.Background(), CreateFromExistingBranchOpts{
		RepoPath:        repoDir,
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		BranchName:      branch,
	})
	if err != nil {
		t.Fatalf("CreateFromExistingBranch after force-pushed origin branch: %v", err)
	}

	current := gitOutput(t, result.WorktreePath, "branch", "--show-current")
	if current != branch {
		t.Fatalf("current branch = %q, want %s", current, branch)
	}
	gotCommit := gitOutput(t, result.WorktreePath, "rev-parse", "HEAD")
	if gotCommit != newCommit {
		t.Fatalf("worktree HEAD = %s, want force-pushed commit %s", gotCommit, newCommit)
	}
	originCommit := gitOutput(t, repoDir, "rev-parse", "refs/remotes/origin/"+branch)
	if originCommit != newCommit {
		t.Fatalf("origin/%s = %s, want %s", branch, originCommit, newCommit)
	}
}

// TestCreateFromExistingBranch_RemovesStaleWorktreeDir verifies the same
// self-heal for the existing-branch (PR / dependabot) path.
func TestCreateFromExistingBranch_RemovesStaleWorktreeDir(t *testing.T) {
	repoDir := initTestRepo(t)
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

	stalePath := filepath.Join(wtBase, "my-repo", "feature")
	if err := os.MkdirAll(stalePath, 0o755); err != nil {
		t.Fatalf("mkdir stale: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stalePath, "leftover.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write leftover: %v", err)
	}

	result, err := mgr.CreateFromExistingBranch(ctx, CreateFromExistingBranchOpts{
		RepoPath:        repoDir,
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		BranchName:      "feature",
	})
	if err != nil {
		t.Fatalf("CreateFromExistingBranch over stale dir: %v", err)
	}
	if result.WorktreePath != stalePath {
		t.Fatalf("worktree path = %q, want %q", result.WorktreePath, stalePath)
	}
	if _, err := os.Stat(filepath.Join(stalePath, "leftover.txt")); !os.IsNotExist(err) {
		t.Fatalf("stale file still present (err=%v)", err)
	}
}

func TestCreateFromExistingBranch_RemovesRegisteredStaleWorktreeBeforeFetch(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	stalePath := filepath.Join(wtBase, "my-repo", "fix-camera-crash")

	for _, args := range [][]string{
		{"checkout", "-b", "fix-camera-crash"},
		{"commit", "--allow-empty", "-m", "feature"},
		{"push", "-u", "origin", "fix-camera-crash"},
		{"checkout", "main"},
		{"worktree", "add", stalePath, "fix-camera-crash"},
	} {
		gitOutput(t, repoDir, args...)
	}

	originURL := gitOutput(t, repoDir, "config", "--get", "remote.origin.url")
	cloneDir := filepath.Join(t.TempDir(), "clone")
	gitOutput(t, repoDir, "clone", originURL, cloneDir)
	for _, args := range [][]string{
		{"checkout", "fix-camera-crash"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test User"},
		{"commit", "--allow-empty", "-m", "remote feature"},
		{"push", "origin", "fix-camera-crash"},
	} {
		gitOutput(t, cloneDir, args...)
	}

	mgr := NewManager(zerolog.Nop())
	result, err := mgr.CreateFromExistingBranch(context.Background(), CreateFromExistingBranchOpts{
		RepoPath:        repoDir,
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		BranchName:      "fix-camera-crash",
	})
	if err != nil {
		t.Fatalf("CreateFromExistingBranch: %v", err)
	}
	if result.WorktreePath != stalePath {
		t.Fatalf("worktree path = %q, want %q", result.WorktreePath, stalePath)
	}

	current := gitOutput(t, result.WorktreePath, "branch", "--show-current")
	if current != "fix-camera-crash" {
		t.Fatalf("current branch = %q, want fix-camera-crash", current)
	}
	subject := gitOutput(t, result.WorktreePath, "log", "-1", "--format=%s")
	if subject != "remote feature" {
		t.Fatalf("latest commit subject = %q, want remote feature", subject)
	}
	upstream := gitOutput(t, result.WorktreePath, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
	if upstream != "origin/fix-camera-crash" {
		t.Fatalf("upstream = %q, want origin/fix-camera-crash", upstream)
	}
}

func TestCreateFromExistingBranchMissingRemoteKeepsStaleWorktreeForFallback(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	stalePath := filepath.Join(wtBase, "my-repo", "local-only")

	if err := os.MkdirAll(stalePath, 0o755); err != nil {
		t.Fatalf("mkdir stale worktree: %v", err)
	}
	leftover := filepath.Join(stalePath, "leftover.txt")
	if err := os.WriteFile(leftover, []byte("x"), 0o644); err != nil {
		t.Fatalf("write leftover: %v", err)
	}
	for _, args := range [][]string{
		{"checkout", "-b", "local-only"},
		{"commit", "--allow-empty", "-m", "local-only"},
		{"checkout", "main"},
	} {
		gitOutput(t, repoDir, args...)
	}

	mgr := NewManager(zerolog.Nop())
	_, err := mgr.CreateFromExistingBranch(context.Background(), CreateFromExistingBranchOpts{
		RepoPath:        repoDir,
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		BranchName:      "local-only",
	})
	if err == nil {
		t.Fatal("CreateFromExistingBranch succeeded for missing remote branch")
	}
	if _, statErr := os.Stat(leftover); statErr != nil {
		t.Fatalf("stale worktree was removed before fallback could run: %v", statErr)
	}
}

func TestCreateFromExistingBranchQualifiesFetchRefspecs(t *testing.T) {
	repoDir := initTestRepo(t)
	branch := "v1.2.3"

	for _, args := range [][]string{
		{"tag", branch},
		{"checkout", "-b", branch},
		{"commit", "--allow-empty", "-m", "remote branch"},
		{"push", "origin", "refs/heads/" + branch},
		{"push", "origin", "refs/tags/" + branch},
		{"checkout", "main"},
	} {
		gitOutput(t, repoDir, args...)
	}
	tagCommit := gitOutput(t, repoDir, "rev-parse", "refs/tags/"+branch)
	branchCommit := gitOutput(t, repoDir, "rev-parse", "refs/heads/"+branch)

	mgr := NewManager(zerolog.Nop())
	result, err := mgr.CreateFromExistingBranch(context.Background(), CreateFromExistingBranchOpts{
		RepoPath:        repoDir,
		WorktreeBaseDir: filepath.Join(t.TempDir(), "worktrees"),
		RepoName:        "my-repo",
		BranchName:      branch,
	})
	if err != nil {
		t.Fatalf("CreateFromExistingBranch with branch/tag collision: %v", err)
	}
	current := gitOutput(t, result.WorktreePath, "branch", "--show-current")
	if current != branch {
		t.Fatalf("current branch = %q, want %s", current, branch)
	}
	if got := gitOutput(t, repoDir, "rev-parse", "refs/tags/"+branch); got != tagCommit {
		t.Fatalf("tag ref moved to %s, want %s", got, tagCommit)
	}
	if got := gitOutput(t, repoDir, "rev-parse", "refs/remotes/origin/"+branch); got != branchCommit {
		t.Fatalf("origin branch ref = %s, want %s", got, branchCommit)
	}
}

func TestCreateFromExistingBranch_PrunesMissingRegisteredStaleWorktreeBeforeFetch(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	stalePath := filepath.Join(wtBase, "my-repo", "fix-camera-crash")

	for _, args := range [][]string{
		{"checkout", "-b", "fix-camera-crash"},
		{"commit", "--allow-empty", "-m", "feature"},
		{"push", "-u", "origin", "fix-camera-crash"},
		{"checkout", "main"},
		{"worktree", "add", stalePath, "fix-camera-crash"},
	} {
		gitOutput(t, repoDir, args...)
	}
	if err := os.RemoveAll(stalePath); err != nil {
		t.Fatalf("remove stale worktree dir: %v", err)
	}

	originURL := gitOutput(t, repoDir, "config", "--get", "remote.origin.url")
	cloneDir := filepath.Join(t.TempDir(), "clone")
	gitOutput(t, repoDir, "clone", originURL, cloneDir)
	for _, args := range [][]string{
		{"checkout", "fix-camera-crash"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test User"},
		{"commit", "--allow-empty", "-m", "remote feature"},
		{"push", "origin", "fix-camera-crash"},
	} {
		gitOutput(t, cloneDir, args...)
	}

	mgr := NewManager(zerolog.Nop())
	result, err := mgr.CreateFromExistingBranch(context.Background(), CreateFromExistingBranchOpts{
		RepoPath:        repoDir,
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		BranchName:      "fix-camera-crash",
	})
	if err != nil {
		t.Fatalf("CreateFromExistingBranch: %v", err)
	}
	if result.WorktreePath != stalePath {
		t.Fatalf("worktree path = %q, want %q", result.WorktreePath, stalePath)
	}

	current := gitOutput(t, result.WorktreePath, "branch", "--show-current")
	if current != "fix-camera-crash" {
		t.Fatalf("current branch = %q, want fix-camera-crash", current)
	}
	subject := gitOutput(t, result.WorktreePath, "log", "-1", "--format=%s")
	if subject != "remote feature" {
		t.Fatalf("latest commit subject = %q, want remote feature", subject)
	}
	upstream := gitOutput(t, result.WorktreePath, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
	if upstream != "origin/fix-camera-crash" {
		t.Fatalf("upstream = %q, want origin/fix-camera-crash", upstream)
	}
}

func TestCreateWithSetupScript(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	logger := zerolog.Nop()
	mgr := NewManager(logger)

	// The script writes both a marker file and the env vars so we
	// can verify they are set correctly.
	script := `echo hello > setup-done.txt && echo "$REPO_DIR" > repo-dir.txt && echo "$WORKTREE_DIR" > worktree-dir.txt`
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

	// Verify REPO_DIR was set to the main repository path.
	gotRepo, err := os.ReadFile(filepath.Join(result.WorktreePath, "repo-dir.txt"))
	if err != nil {
		t.Fatalf("read repo-dir.txt: %v", err)
	}
	if got := strings.TrimSpace(string(gotRepo)); got != repoDir {
		t.Errorf("REPO_DIR = %q, want %q", got, repoDir)
	}

	// Verify WORKTREE_DIR was set to the worktree path.
	gotWT, err := os.ReadFile(filepath.Join(result.WorktreePath, "worktree-dir.txt"))
	if err != nil {
		t.Fatalf("read worktree-dir.txt: %v", err)
	}
	if got := strings.TrimSpace(string(gotWT)); got != result.WorktreePath {
		t.Errorf("WORKTREE_DIR = %q, want %q", got, result.WorktreePath)
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

// TestCreate_IgnoresClaudeSettingsLocal verifies that the bossd Stop-hook
// config at .claude/settings.local.json is git-ignored in every newly
// created worktree. bossd writes that file with a bearer token; without
// the ignore, `git status` shows it as untracked which (a) misclassifies
// "no changes" cron runs as having Claude changes and (b) risks `git add
// .` ever staging the token. Pairs with the in-process porcelain filter
// in services/bossd/internal/session/finalize.go.
func TestCreate_IgnoresClaudeSettingsLocal(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	result, err := mgr.Create(ctx, CreateOpts{
		RepoPath:        repoDir,
		BaseBranch:      "main",
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		Title:           "Ignore hook config",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Drop a fake hook-config file so check-ignore has something concrete
	// to match against (the file bossd actually writes carries a bearer
	// token; we don't need real content here).
	claudeDir := filepath.Join(result.WorktreePath, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatalf("mkdir .claude: %v", err)
	}
	hookFile := filepath.Join(claudeDir, "settings.local.json")
	if err := os.WriteFile(hookFile, []byte(`{"hooks":{}}`), 0o644); err != nil {
		t.Fatalf("write settings.local.json: %v", err)
	}

	cmd := exec.Command("git", "check-ignore", "-v", ".claude/settings.local.json")
	cmd.Dir = result.WorktreePath
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf(".claude/settings.local.json is not ignored. git check-ignore output:\n%s\nerr: %v", out, err)
	}
	if !strings.Contains(string(out), ".claude/settings.local.json") {
		t.Errorf("check-ignore output does not mention the hook config: %s", out)
	}

	// `git status --porcelain` must be clean — the hook config must NOT
	// surface as untracked, otherwise the finalize pipeline misclassifies it.
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

	current := gitOutput(t, result.WorktreePath, "branch", "--show-current")
	if current != result.BranchName {
		t.Fatalf("current branch = %q, want %q", current, result.BranchName)
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

func TestEmptyCommitBypassesCommitHooks(t *testing.T) {
	repoDir := initTestRepo(t)
	hookPath := filepath.Join(repoDir, ".git", "hooks", "commit-msg")
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write commit-msg hook: %v", err)
	}

	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	if err := mgr.EmptyCommit(ctx, repoDir, "chore: [skip ci] create pull request"); err != nil {
		t.Fatalf("EmptyCommit: %v", err)
	}

	subject, err := runGit(ctx, repoDir, "log", "-1", "--format=%s")
	if err != nil {
		t.Fatalf("read commit subject: %v", err)
	}
	if subject != "chore: [skip ci] create pull request" {
		t.Errorf("commit subject = %q, want %q", subject, "chore: [skip ci] create pull request")
	}
}

func TestPushSendsCurrentHeadToRequestedRemoteBranch(t *testing.T) {
	repoDir := initTestRepo(t)
	ctx := context.Background()
	mgr := NewManager(zerolog.Nop())

	for _, args := range [][]string{
		{"checkout", "-b", "fix-camera-crash"},
		{"push", "origin", "fix-camera-crash"},
		{"checkout", "main"},
		{"checkout", "-b", "production"},
		{"commit", "--allow-empty", "-m", "placeholder on wrong local branch"},
	} {
		gitOutput(t, repoDir, args...)
	}

	if err := mgr.Push(ctx, repoDir, "fix-camera-crash"); err == nil {
		t.Fatal("Push returned nil, want branch mismatch error")
	} else if !strings.Contains(err.Error(), `worktree is on branch "production", expected "fix-camera-crash"`) {
		t.Fatalf("Push error = %q, want branch mismatch details", err)
	}

	gitOutput(t, repoDir, "checkout", "fix-camera-crash")
	gitOutput(t, repoDir, "commit", "--allow-empty", "-m", "placeholder on target branch")
	if err := mgr.Push(ctx, repoDir, "fix-camera-crash"); err != nil {
		t.Fatalf("Push after checkout target branch: %v", err)
	}

	localHead := gitOutput(t, repoDir, "rev-parse", "HEAD")
	remoteHead := gitOutput(t, repoDir, "ls-remote", "origin", "refs/heads/fix-camera-crash")
	if !strings.HasPrefix(remoteHead, localHead) {
		t.Fatalf("remote head = %q, want prefix %q", remoteHead, localHead)
	}
}

func TestPushQualifiesRemoteBranchDestination(t *testing.T) {
	repoDir := initTestRepo(t)
	ctx := context.Background()
	mgr := NewManager(zerolog.Nop())
	branch := "v1.2.3"

	for _, args := range [][]string{
		{"tag", branch},
		{"push", "origin", "refs/tags/" + branch},
		{"checkout", "-b", branch},
		{"commit", "--allow-empty", "-m", "initial branch commit"},
		{"push", "origin", "refs/heads/" + branch},
		{"commit", "--allow-empty", "-m", "updated branch commit"},
	} {
		gitOutput(t, repoDir, args...)
	}
	tagCommit := gitOutput(t, repoDir, "rev-parse", "refs/tags/"+branch)
	localHead := gitOutput(t, repoDir, "rev-parse", "HEAD")

	if err := mgr.Push(ctx, repoDir, branch); err != nil {
		t.Fatalf("Push with branch/tag name collision: %v", err)
	}

	remoteHead := gitOutput(t, repoDir, "ls-remote", "origin", "refs/heads/"+branch)
	if !strings.HasPrefix(remoteHead, localHead) {
		t.Fatalf("remote head = %q, want prefix %q", remoteHead, localHead)
	}
	remoteTag := gitOutput(t, repoDir, "ls-remote", "origin", "refs/tags/"+branch)
	if !strings.HasPrefix(remoteTag, tagCommit) {
		t.Fatalf("remote tag = %q, want prefix %q", remoteTag, tagCommit)
	}
}

func TestPushWithLeaseUsesExpectedRemoteHead(t *testing.T) {
	repoDir := initTestRepo(t)
	ctx := context.Background()
	mgr := NewManager(zerolog.Nop())
	branch := leaseTestBranch

	for _, args := range [][]string{
		{"checkout", "-b", branch},
		{"commit", "--allow-empty", "-m", "initial branch commit"},
		{"push", "origin", "refs/heads/" + branch},
		{"commit", "--allow-empty", "-m", "updated branch commit"},
	} {
		gitOutput(t, repoDir, args...)
	}
	expectedRemote := currentRemoteHead(t, repoDir)

	pushedSHA, err := mgr.PushWithLease(ctx, repoDir, branch, expectedRemote)
	if err != nil {
		t.Fatalf("PushWithLease: %v", err)
	}

	got := currentRemoteHead(t, repoDir)
	if got != pushedSHA {
		t.Fatalf("remote head = %q, want pushed SHA %q", got, pushedSHA)
	}
	if got == expectedRemote {
		t.Fatalf("remote head did not change after lease push")
	}
}

func TestPushWithLeaseRejectsStaleExpectedRemoteHead(t *testing.T) {
	repoDir := initTestRepo(t)
	ctx := context.Background()
	mgr := NewManager(zerolog.Nop())
	branch := leaseTestBranch

	for _, args := range [][]string{
		{"checkout", "-b", branch},
		{"commit", "--allow-empty", "-m", "initial branch commit"},
		{"push", "origin", "refs/heads/" + branch},
	} {
		gitOutput(t, repoDir, args...)
	}
	expectedRemote := currentRemoteHead(t, repoDir)

	originURL := gitOutput(t, repoDir, "config", "--get", "remote.origin.url")
	competingDir := t.TempDir()
	gitOutput(t, t.TempDir(), "clone", originURL, competingDir)
	for _, args := range [][]string{
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"checkout", "-B", branch, "origin/" + branch},
		{"commit", "--allow-empty", "-m", "competing branch commit"},
		{"push", "origin", "HEAD:refs/heads/" + branch},
	} {
		gitOutput(t, competingDir, args...)
	}
	competingRemote := currentRemoteHead(t, repoDir)
	if competingRemote == expectedRemote {
		t.Fatalf("competing push did not advance remote branch")
	}

	gitOutput(t, repoDir, "commit", "--allow-empty", "-m", "local repair commit")
	if _, err := mgr.PushWithLease(ctx, repoDir, branch, expectedRemote); err == nil {
		t.Fatalf("PushWithLease succeeded with stale expected remote head")
	}

	got := currentRemoteHead(t, repoDir)
	if got != competingRemote {
		t.Fatalf("remote head = %q, want competing SHA %q", got, competingRemote)
	}
}

func TestPushWithLeaseRejectsRemoteHeadNotIncludedLocally(t *testing.T) {
	repoDir := initTestRepo(t)
	ctx := context.Background()
	mgr := NewManager(zerolog.Nop())
	branch := leaseTestBranch

	for _, args := range [][]string{
		{"checkout", "-b", branch},
		{"commit", "--allow-empty", "-m", "initial branch commit"},
		{"push", "origin", "refs/heads/" + branch},
	} {
		gitOutput(t, repoDir, args...)
	}

	originURL := gitOutput(t, repoDir, "config", "--get", "remote.origin.url")
	competingDir := t.TempDir()
	gitOutput(t, t.TempDir(), "clone", originURL, competingDir)
	for _, args := range [][]string{
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"checkout", "-B", branch, "origin/" + branch},
		{"commit", "--allow-empty", "-m", "remote-only branch commit"},
		{"push", "origin", "HEAD:refs/heads/" + branch},
	} {
		gitOutput(t, competingDir, args...)
	}
	remoteOnlyHead := currentRemoteHead(t, repoDir)

	gitOutput(t, repoDir, "fetch", "origin", branch)
	gitOutput(t, repoDir, "checkout", "--detach", remoteOnlyHead)
	gitOutput(t, repoDir, "checkout", branch)
	gitOutput(t, repoDir, "commit", "--allow-empty", "-m", "local repair commit")
	_, err := mgr.PushWithLease(ctx, repoDir, branch, remoteOnlyHead)
	if err == nil {
		t.Fatalf("PushWithLease succeeded when local HEAD did not include expected remote head")
	}
	if !strings.Contains(err.Error(), "is not integrated or rebased in local branch") {
		t.Fatalf("PushWithLease error = %v, want local lease inclusion error", err)
	}

	got := currentRemoteHead(t, repoDir)
	if got != remoteOnlyHead {
		t.Fatalf("remote head = %q, want remote-only SHA %q", got, remoteOnlyHead)
	}
}

func TestPushWithLeaseAllowsRebasedHeadFromExpectedRemoteHead(t *testing.T) {
	repoDir := initTestRepo(t)
	ctx := context.Background()
	mgr := NewManager(zerolog.Nop())
	branch := leaseTestBranch

	for _, args := range [][]string{
		{"checkout", "-b", branch},
	} {
		gitOutput(t, repoDir, args...)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "branch.txt"), []byte("branch\n"), 0o644); err != nil {
		t.Fatalf("write branch file: %v", err)
	}
	for _, args := range [][]string{
		{"add", "branch.txt"},
		{"commit", "-m", "initial branch commit"},
		{"push", "origin", "refs/heads/" + branch},
	} {
		gitOutput(t, repoDir, args...)
	}
	expectedRemote := currentRemoteHead(t, repoDir)

	gitOutput(t, repoDir, "checkout", "main")
	if err := os.WriteFile(filepath.Join(repoDir, "main.txt"), []byte("main\n"), 0o644); err != nil {
		t.Fatalf("write main file: %v", err)
	}
	for _, args := range [][]string{
		{"add", "main.txt"},
		{"commit", "-m", "main branch commit"},
		{"checkout", branch},
		{"rebase", "main"},
	} {
		gitOutput(t, repoDir, args...)
	}

	pushedSHA, err := mgr.PushWithLease(ctx, repoDir, branch, expectedRemote)
	if err != nil {
		t.Fatalf("PushWithLease after rebase: %v", err)
	}
	got := currentRemoteHead(t, repoDir)
	if got != pushedSHA {
		t.Fatalf("remote head = %q, want pushed SHA %q", got, pushedSHA)
	}
}

// leaseTestBranch is the fixture branch used by the PushWithLease tests and
// their currentRemoteHead helper.
const leaseTestBranch = "fix-camera-focus-issues"

func currentRemoteHead(t *testing.T, repoDir string) string {
	t.Helper()
	out := gitOutput(t, repoDir, "ls-remote", "origin", "refs/heads/"+leaseTestBranch)
	fields := strings.Fields(out)
	if len(fields) == 0 {
		t.Fatalf("remote branch %q has no head", leaseTestBranch)
	}
	return fields[0]
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
	err = runSetupScript(context.Background(), worktree, worktree, spec, "", nil)
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

// makeLocalCommit authors a local commit on the currently-checked-out branch
// of repo and returns the new HEAD SHA. Used to diverge a local base from
// origin so the never-force-move guards can be exercised.
func makeLocalCommit(t *testing.T, repo, message string) string {
	t.Helper()
	if _, err := runGit(context.Background(), repo, "commit", "--allow-empty", "-m", message); err != nil {
		t.Fatalf("local commit %q: %v", message, err)
	}
	sha, err := runGit(context.Background(), repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return sha
}

// TestSyncBaseBranch_BaseCheckedOutDirty_Defers is case A (dirty variant): the
// base is checked out with an uncommitted change while origin/<base> is ahead.
// The GitHub merge already happened, so the local fast-forward must defer
// (ErrLocalSyncDeferred) rather than fail — leaving the local ref untouched
// while still freshening refs/remotes/origin/<base> and recording the pending
// entry for a later retry.
func TestSyncBaseBranch_BaseCheckedOutDirty_Defers(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	oldLocal, err := runGit(ctx, repo, "rev-parse", "refs/heads/main")
	if err != nil {
		t.Fatalf("rev-parse main: %v", err)
	}
	wantSHA := commitOnOrigin(t, repo, "main")

	// Uncommitted change on the checked-out base.
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write untracked: %v", err)
	}

	err = mgr.SyncBaseBranch(ctx, repo, "main")
	if !errors.Is(err, ErrLocalSyncDeferred) {
		t.Fatalf("expected ErrLocalSyncDeferred, got %v", err)
	}

	// Local base ref unchanged (still at the old tip).
	gotLocal, err := runGit(ctx, repo, "rev-parse", "refs/heads/main")
	if err != nil {
		t.Fatalf("rev-parse main after sync: %v", err)
	}
	if gotLocal != oldLocal {
		t.Errorf("local main moved to %s, want unchanged %s", gotLocal, oldLocal)
	}

	// refs/remotes/origin/main freshened regardless.
	gotOrigin, err := runGit(ctx, repo, "rev-parse", "refs/remotes/origin/main")
	if err != nil {
		t.Fatalf("rev-parse origin/main: %v", err)
	}
	if gotOrigin != wantSHA {
		t.Errorf("origin/main = %s, want freshened %s", gotOrigin, wantSHA)
	}

	// Pending entry recorded for a later retry.
	mgr.mu.Lock()
	pending := mgr.pendingBaseSync[repo]
	mgr.mu.Unlock()
	if pending != "main" {
		t.Errorf("pending base sync = %q, want %q", pending, "main")
	}
}

// TestSyncBaseBranch_DetachedHead_AdvancesRef is case A (detached variant): with
// a detached HEAD and the local base behind origin, the base ref advances via a
// direct fetch (never touching the working tree) and returns nil.
func TestSyncBaseBranch_DetachedHead_AdvancesRef(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	if _, err := runGit(ctx, repo, "checkout", "--detach", "HEAD"); err != nil {
		t.Fatalf("detach HEAD: %v", err)
	}
	wantSHA := commitOnOrigin(t, repo, "main")

	if err := mgr.SyncBaseBranch(ctx, repo, "main"); err != nil {
		t.Fatalf("SyncBaseBranch: %v", err)
	}
	gotSHA, err := runGit(ctx, repo, "rev-parse", "refs/heads/main")
	if err != nil {
		t.Fatalf("rev-parse main: %v", err)
	}
	if gotSHA != wantSHA {
		t.Errorf("local main at %s, want %s", gotSHA, wantSHA)
	}
}

// TestSyncBaseBranch_UpToDateDirty_NoDefer is case A (no-FF-needed variant):
// the base is checked out and dirty, but already up to date with origin, so
// there is nothing to fast-forward — returns nil and records nothing pending.
func TestSyncBaseBranch_UpToDateDirty_NoDefer(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	// Dirty tree, but origin has no new commits.
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write untracked: %v", err)
	}

	if err := mgr.SyncBaseBranch(ctx, repo, "main"); err != nil {
		t.Fatalf("expected nil (nothing to fast-forward), got %v", err)
	}
	mgr.mu.Lock()
	_, pending := mgr.pendingBaseSync[repo]
	mgr.mu.Unlock()
	if pending {
		t.Error("expected nothing pending when base is already up to date")
	}
}

// TestSyncBaseBranch_DivergedCheckedOutClean_NeverForces is case B (checked-out
// clean variant): local <base> holds a commit origin does not, and origin holds
// a different commit. Sync must NOT force-move the operator's commit — it warns,
// returns nil, leaves the local ref untouched, and records nothing pending.
func TestSyncBaseBranch_DivergedCheckedOutClean_NeverForces(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	commitOnOrigin(t, repo, "main")                               // origin advances one way…
	localSHA := makeLocalCommit(t, repo, "operator local commit") // …local another.

	if err := mgr.SyncBaseBranch(ctx, repo, "main"); err != nil {
		t.Fatalf("expected nil on diverged base, got %v", err)
	}
	gotLocal, err := runGit(ctx, repo, "rev-parse", "refs/heads/main")
	if err != nil {
		t.Fatalf("rev-parse main: %v", err)
	}
	if gotLocal != localSHA {
		t.Errorf("local main moved to %s, want operator commit %s (never force-moved)", gotLocal, localSHA)
	}
	mgr.mu.Lock()
	_, pending := mgr.pendingBaseSync[repo]
	mgr.mu.Unlock()
	if pending {
		t.Error("diverged base must not be left pending")
	}
}

// TestSyncBaseBranch_DivergedNotCheckedOut_NeverForces is case B (not-checked-out
// variant): same divergence but the operator is on another branch. The direct
// `fetch base:base` refuses the non-fast-forward; sync warns, returns nil, and
// leaves the local ref untouched.
func TestSyncBaseBranch_DivergedNotCheckedOut_NeverForces(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	commitOnOrigin(t, repo, "main")
	localSHA := makeLocalCommit(t, repo, "operator local commit")
	if _, err := runGit(ctx, repo, "checkout", "-b", "feature"); err != nil {
		t.Fatalf("checkout feature: %v", err)
	}

	if err := mgr.SyncBaseBranch(ctx, repo, "main"); err != nil {
		t.Fatalf("expected nil on diverged base, got %v", err)
	}
	gotLocal, err := runGit(ctx, repo, "rev-parse", "refs/heads/main")
	if err != nil {
		t.Fatalf("rev-parse main: %v", err)
	}
	if gotLocal != localSHA {
		t.Errorf("local main moved to %s, want operator commit %s (never force-moved)", gotLocal, localSHA)
	}
	mgr.mu.Lock()
	_, pending := mgr.pendingBaseSync[repo]
	mgr.mu.Unlock()
	if pending {
		t.Error("diverged base must not be left pending")
	}
}

// TestRetryDeferredBaseSyncs_AppliesAfterTreeCleaned is case C (AC3 proof): a
// deferred sync is retried once the working tree is clean, fast-forwarding the
// local base to the origin tip. A second retry is a no-op. Proves safety is
// re-validated at apply time, not captured at defer time.
func TestRetryDeferredBaseSyncs_AppliesAfterTreeCleaned(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	wantSHA := commitOnOrigin(t, repo, "main")
	dirtyPath := filepath.Join(repo, "untracked.txt")
	if err := os.WriteFile(dirtyPath, []byte("x"), 0o600); err != nil {
		t.Fatalf("write untracked: %v", err)
	}

	if err := mgr.SyncBaseBranch(ctx, repo, "main"); !errors.Is(err, ErrLocalSyncDeferred) {
		t.Fatalf("expected ErrLocalSyncDeferred, got %v", err)
	}

	// Clean the tree, then retry — the local base should now fast-forward.
	if err := os.Remove(dirtyPath); err != nil {
		t.Fatalf("remove dirty file: %v", err)
	}
	mgr.RetryDeferredBaseSyncs(ctx)

	gotSHA, err := runGit(ctx, repo, "rev-parse", "refs/heads/main")
	if err != nil {
		t.Fatalf("rev-parse main: %v", err)
	}
	if gotSHA != wantSHA {
		t.Errorf("local main at %s after retry, want fast-forwarded %s", gotSHA, wantSHA)
	}
	mgr.mu.Lock()
	_, pending := mgr.pendingBaseSync[repo]
	mgr.mu.Unlock()
	if pending {
		t.Error("pending entry should be cleared after a successful retry")
	}

	// A second retry is a no-op (nothing pending) and must not error.
	mgr.RetryDeferredBaseSyncs(ctx)
	gotSHA2, err := runGit(ctx, repo, "rev-parse", "refs/heads/main")
	if err != nil {
		t.Fatalf("rev-parse main: %v", err)
	}
	if gotSHA2 != wantSHA {
		t.Errorf("local main at %s after second retry, want %s", gotSHA2, wantSHA)
	}
}

// TestSyncBaseBranch_DeferredFreshensOriginRefForWorktree is case D: after a
// deferred local sync, refs/remotes/origin/<base> points at the merged tip, so a
// worktree created from the base branches from the merged tip rather than the
// stale local <base>.
func TestSyncBaseBranch_DeferredFreshensOriginRefForWorktree(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	wantSHA := commitOnOrigin(t, repo, "main")
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write untracked: %v", err)
	}
	if err := mgr.SyncBaseBranch(ctx, repo, "main"); !errors.Is(err, ErrLocalSyncDeferred) {
		t.Fatalf("expected ErrLocalSyncDeferred, got %v", err)
	}

	gotOrigin, err := runGit(ctx, repo, "rev-parse", "refs/remotes/origin/main")
	if err != nil {
		t.Fatalf("rev-parse origin/main: %v", err)
	}
	if gotOrigin != wantSHA {
		t.Errorf("origin/main = %s, want merged tip %s", gotOrigin, wantSHA)
	}

	// A new worktree branches from the merged tip (origin/main), not stale local.
	result, err := mgr.Create(ctx, CreateOpts{
		RepoPath:        repo,
		BaseBranch:      "main",
		WorktreeBaseDir: filepath.Join(t.TempDir(), "worktrees"),
		RepoName:        "my-repo",
		Title:           "branch from merged tip",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	wtHead := gitOutput(t, result.WorktreePath, "rev-parse", "HEAD")
	if wtHead != wantSHA {
		t.Errorf("worktree HEAD = %s, want merged tip %s", wtHead, wantSHA)
	}
}

func TestIsNonFastForwardGitOutput(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"fetch rejected non-fast-forward", errors.New("! [rejected] main -> main (non-fast-forward)"), true},
		{"merge not possible", errors.New("fatal: Not possible to fast-forward, aborting."), true},
		{"push rejected non-fast-forward", errors.New("! [rejected] main -> main (non-fast-forward)\nerror: failed to push some refs"), true},
		// A bare "rejected" with NO fast-forward phrase must NOT be classified
		// as a non-fast-forward: it appears in unrelated ref-update failures
		// (tag clobbers, shallow refusals) that must surface, not be swallowed
		// as a benign diverged-base warning.
		{"rejected without ff phrase not matched", errors.New("! [rejected] v1 -> v1 (would clobber existing tag)"), false},
		{"unrelated failure", errors.New("fatal: not a git repository"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isNonFastForwardGitOutput(tc.err); got != tc.want {
				t.Errorf("isNonFastForwardGitOutput(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
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

	if err := runSetupScript(context.Background(), worktree, worktree, spec, "", nil); err != nil {
		t.Fatalf("runSetupScript: %v", err)
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatalf("sentinel was created — argv was interpreted as shell")
	} else if !os.IsNotExist(err) {
		t.Fatalf("unexpected stat error: %v", err)
	}
}

// injectTestBranch sets up a branch with the given tagless commit subjects,
// pushed to origin, and returns the worktree dir. Each subject becomes one
// empty commit on top of main.
func injectTestBranch(t *testing.T, branch string, subjects ...string) string {
	t.Helper()
	repoDir := initTestRepo(t)
	gitOutput(t, repoDir, "checkout", "-b", branch)
	for _, s := range subjects {
		gitOutput(t, repoDir, "commit", "--allow-empty", "-m", s)
	}
	gitOutput(t, repoDir, "push", "-u", "origin", "HEAD:refs/heads/"+branch)
	return repoDir
}

func TestInjectPRNumbers_RewritesTaglessConventionalCommits(t *testing.T) {
	ctx := context.Background()
	branch := "cron-inject-test"
	repoDir := injectTestBranch(t, branch,
		"feat(web): add widget",
		"fix: repair thing",
		"chore(ci) tweak pipeline", // not conventional (no colon) — appended fallback
	)
	mgr := NewManager(zerolog.Nop())

	if err := mgr.InjectPRNumbers(ctx, repoDir, branch, 42, "refs/remotes/origin/main"); err != nil {
		t.Fatalf("InjectPRNumbers: %v", err)
	}

	subjects := gitOutput(t, repoDir, "log", "--format=%s", "origin/main..HEAD")
	for _, want := range []string{
		"feat(web): [#42] add widget",
		"fix: [#42] repair thing",
		"chore(ci) tweak pipeline [#42]",
	} {
		if !strings.Contains(subjects, want) {
			t.Fatalf("subjects =\n%s\nwant to contain %q", subjects, want)
		}
	}

	// The rewrite must be force-pushed: origin/<branch> matches local HEAD.
	localHead := gitOutput(t, repoDir, "rev-parse", "HEAD")
	remoteHead := gitOutput(t, repoDir, "rev-parse", "origin/"+branch)
	if localHead != remoteHead {
		t.Fatalf("remote head %q != local head %q after injection", remoteHead, localHead)
	}
}

func TestInjectPRNumbers_RewritesSubjectWhenBodyContainsTag(t *testing.T) {
	ctx := context.Background()
	branch := "cron-inject-body-tag"
	repoDir := initTestRepo(t)
	gitOutput(t, repoDir, "checkout", "-b", branch)
	gitOutput(t, repoDir, "commit", "--allow-empty",
		"-m", "feat(web): add widget",
		"-m", "Body mentions [#42] but the subject is still tagless.",
	)
	gitOutput(t, repoDir, "push", "-u", "origin", "HEAD:refs/heads/"+branch)
	mgr := NewManager(zerolog.Nop())

	if err := mgr.InjectPRNumbers(ctx, repoDir, branch, 42, "refs/remotes/origin/main"); err != nil {
		t.Fatalf("InjectPRNumbers: %v", err)
	}

	if got := gitOutput(t, repoDir, "log", "-1", "--format=%s"); got != "feat(web): [#42] add widget" {
		t.Fatalf("subject = %q, want PR tag inserted into subject", got)
	}
	body := gitOutput(t, repoDir, "log", "-1", "--format=%b")
	if !strings.Contains(body, "Body mentions [#42] but the subject is still tagless.") {
		t.Fatalf("body = %q, want original body preserved", body)
	}
}

func TestInjectPRNumbers_IdempotentWhenAlreadyTagged(t *testing.T) {
	ctx := context.Background()
	branch := "cron-inject-idem"
	repoDir := injectTestBranch(t, branch, "feat(web): [#42] add widget")
	mgr := NewManager(zerolog.Nop())

	headBefore := gitOutput(t, repoDir, "rev-parse", "HEAD")
	if err := mgr.InjectPRNumbers(ctx, repoDir, branch, 42, "refs/remotes/origin/main"); err != nil {
		t.Fatalf("InjectPRNumbers: %v", err)
	}

	// Nothing to do: HEAD unchanged (no rebase, no double tag).
	if got := gitOutput(t, repoDir, "rev-parse", "HEAD"); got != headBefore {
		t.Fatalf("HEAD changed on idempotent run: before=%s after=%s", headBefore, got)
	}
	if got := gitOutput(t, repoDir, "log", "-1", "--format=%s"); got != "feat(web): [#42] add widget" {
		t.Fatalf("subject = %q, want unchanged single tag", got)
	}
}

func TestInjectPRNumbers_PushesAlreadyTaggedLocalCommits(t *testing.T) {
	ctx := context.Background()
	branch := "cron-inject-tagged-ahead"
	repoDir := injectTestBranch(t, branch, "feat(web): [#42] add widget")
	gitOutput(t, repoDir, "commit", "--allow-empty", "-m", "fix(web): [#42] repair widget")
	mgr := NewManager(zerolog.Nop())

	localHead := gitOutput(t, repoDir, "rev-parse", "HEAD")
	remoteBefore := gitOutput(t, repoDir, "rev-parse", "origin/"+branch)
	if remoteBefore == localHead {
		t.Fatalf("test setup failed: remote branch already at local HEAD")
	}

	if err := mgr.InjectPRNumbers(ctx, repoDir, branch, 42, "refs/remotes/origin/main"); err != nil {
		t.Fatalf("InjectPRNumbers: %v", err)
	}

	remoteHead := gitOutput(t, repoDir, "rev-parse", "origin/"+branch)
	if remoteHead != localHead {
		t.Fatalf("remote head %q != local head %q after already-tagged injection", remoteHead, localHead)
	}
}

func TestInjectPRNumbers_RejectsMismatchedCurrentBranch(t *testing.T) {
	ctx := context.Background()
	branch := "cron-inject-branch-guard"
	repoDir := injectTestBranch(t, branch, "feat(web): add widget")
	gitOutput(t, repoDir, "checkout", "-b", "other-branch", "origin/main")
	gitOutput(t, repoDir, "commit", "--allow-empty", "-m", "fix(web): change other branch")
	mgr := NewManager(zerolog.Nop())

	err := mgr.InjectPRNumbers(ctx, repoDir, branch, 42, "refs/remotes/origin/main")
	if err == nil {
		t.Fatalf("InjectPRNumbers succeeded from mismatched current branch")
	}
	if !strings.Contains(err.Error(), `expected "cron-inject-branch-guard"`) {
		t.Fatalf("InjectPRNumbers error = %v, want current-branch guard", err)
	}
}

func TestInjectPRNumbers_NoCommitsIsNoOp(t *testing.T) {
	ctx := context.Background()
	branch := "cron-inject-empty"
	// Branch with no commits beyond main.
	repoDir := injectTestBranch(t, branch)
	mgr := NewManager(zerolog.Nop())

	if err := mgr.InjectPRNumbers(ctx, repoDir, branch, 42, "refs/remotes/origin/main"); err != nil {
		t.Fatalf("InjectPRNumbers on empty branch: %v", err)
	}
}
