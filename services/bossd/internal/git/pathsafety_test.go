package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
)

// makeSentinel creates a non-empty directory that a cleanup must never touch
// and returns its path.
func makeSentinel(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir sentinel: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	return dir
}

func assertSentinelSurvives(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, "keep.txt")); err != nil {
		t.Fatalf("sentinel %q was touched by cleanup: %v", dir, err)
	}
}

// TestCreateRejectsTraversalBranchWithoutDeleting proves an explicit branch
// that walks out of the worktree base is refused before clearStaleWorktree can
// recursively delete the directory it resolves to.
func TestCreateRejectsTraversalBranchWithoutDeleting(t *testing.T) {
	repoDir := initTestRepo(t)
	root := t.TempDir()
	wtBase := filepath.Join(root, "worktrees")
	sentinel := makeSentinel(t, filepath.Join(root, "sentinel"))
	mgr := NewManager(zerolog.Nop())

	// <wtBase>/my-repo/../../sentinel == <root>/sentinel
	_, err := mgr.Create(context.Background(), CreateOpts{
		RepoPath:        repoDir,
		BaseBranch:      "main",
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		BranchName:      "../../sentinel",
	})
	if err == nil {
		t.Fatal("Create accepted a traversal branch name")
	}
	assertSentinelSurvives(t, sentinel)
}

func TestCreateFromExistingBranchRejectsTraversalBranchWithoutDeleting(t *testing.T) {
	repoDir := initTestRepo(t)
	root := t.TempDir()
	wtBase := filepath.Join(root, "worktrees")
	sentinel := makeSentinel(t, filepath.Join(root, "sentinel"))
	mgr := NewManager(zerolog.Nop())

	_, err := mgr.CreateFromExistingBranch(context.Background(), CreateFromExistingBranchOpts{
		RepoPath:        repoDir,
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		BranchName:      "../../sentinel",
	})
	if err == nil {
		t.Fatal("CreateFromExistingBranch accepted a traversal branch name")
	}
	assertSentinelSurvives(t, sentinel)
}

// TestPurgeWorktreeRefusesEscapingBranch covers a stored session whose branch
// predates validation: the purge must refuse rather than delete outside the base.
func TestPurgeWorktreeRefusesEscapingBranch(t *testing.T) {
	repoDir := initTestRepo(t)
	root := t.TempDir()
	wtBase := filepath.Join(root, "worktrees")
	sentinel := makeSentinel(t, filepath.Join(root, "sentinel"))
	mgr := NewManager(zerolog.Nop())

	if err := mgr.PurgeWorktree(context.Background(), repoDir, "my-repo", wtBase, "../../sentinel"); err == nil {
		t.Fatal("PurgeWorktree accepted a traversal branch name")
	}
	assertSentinelSurvives(t, sentinel)
}

// TestEntryPointsRejectContainedInvalidBranchWithoutDeleting covers names git
// rejects that still resolve INSIDE the repo's worktree base, so ensureWithin
// cannot stand in for the branch-name check: a sentinel planted at the exact
// path each name derives must survive, and the refusal must be the
// branch-name one.
func TestEntryPointsRejectContainedInvalidBranchWithoutDeleting(t *testing.T) {
	entryPoints := map[string]func(mgr *Manager, repoDir, wtBase, branch string) error{
		"Create": func(mgr *Manager, repoDir, wtBase, branch string) error {
			_, err := mgr.Create(context.Background(), CreateOpts{
				RepoPath: repoDir, BaseBranch: "main", WorktreeBaseDir: wtBase,
				RepoName: "my-repo", BranchName: branch,
			})
			return err
		},
		"CreateFromExistingBranch": func(mgr *Manager, repoDir, wtBase, branch string) error {
			_, err := mgr.CreateFromExistingBranch(context.Background(), CreateFromExistingBranchOpts{
				RepoPath: repoDir, WorktreeBaseDir: wtBase, RepoName: "my-repo", BranchName: branch,
			})
			return err
		},
		"PurgeWorktree": func(mgr *Manager, repoDir, wtBase, branch string) error {
			return mgr.PurgeWorktree(context.Background(), repoDir, "my-repo", wtBase, branch)
		},
	}
	for name, call := range entryPoints {
		for _, branch := range []string{"a..b", "x.lock", "a b", "-x", "@{-1}"} {
			t.Run(name+" "+branch, func(t *testing.T) {
				repoDir := initTestRepo(t)
				wtBase := filepath.Join(t.TempDir(), "worktrees")
				target := filepath.Join(worktreeRepoBase(wtBase, "my-repo"), branch)
				if err := ensureWithin(worktreeRepoBase(wtBase, "my-repo"), target); err != nil {
					t.Fatalf("fixture %q must stay inside the base: %v", branch, err)
				}
				sentinel := makeSentinel(t, target)

				err := call(NewManager(zerolog.Nop()), repoDir, wtBase, branch)
				if !errors.Is(err, ErrInvalidBranchName) {
					t.Fatalf("%s(%q) = %v, want ErrInvalidBranchName", name, branch, err)
				}
				assertSentinelSurvives(t, sentinel)
			})
		}
	}
}

func TestValidateBranchName(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{
		"", "../x", "a/../../b", "-x", "--upload-pack=x", "a..b", "x.lock", "a b",
		"@{-1}", "main@{1}", "a*b", "a?b", "a[b", `a\b`, "/x", "x/", "HEAD",
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			err := ValidateBranchName(ctx, name)
			if !errors.Is(err, ErrInvalidBranchName) {
				t.Fatalf("ValidateBranchName(%q) = %v, want ErrInvalidBranchName", name, err)
			}
		})
	}
	for _, name := range []string{"feature/foo", "bos-1547-fix", "dependabot/npm_and_yarn/foo-1.2.3"} {
		t.Run("accepts "+name, func(t *testing.T) {
			if err := ValidateBranchName(ctx, name); err != nil {
				t.Fatalf("ValidateBranchName(%q) = %v, want nil", name, err)
			}
		})
	}
}

// TestValidateBranchNameSurvivesDeletedWorkingDirectory: git exits 128 both for
// an invalid name and for "Unable to read current working directory", so a
// probe run from the daemon's cwd reported a deleted cwd as an invalid branch.
// The probe runs from a stable directory instead.
func TestValidateBranchNameSurvivesDeletedWorkingDirectory(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "gone")
	if err := os.Mkdir(gone, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Chdir(gone)
	if err := os.Remove(gone); err != nil {
		t.Fatalf("remove cwd: %v", err)
	}
	if err := ValidateBranchName(context.Background(), "feature/foo"); err != nil {
		t.Fatalf("ValidateBranchName(feature/foo) with a deleted cwd = %v, want nil", err)
	}
}

// TestValidateBranchNameSurvivesMissingTempDir: the probe must not depend on
// $TMPDIR existing either — a missing one would fail the chdir and refuse every
// valid branch.
func TestValidateBranchNameSurvivesMissingTempDir(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "does-not-exist"))
	if err := ValidateBranchName(context.Background(), "feature/foo"); err != nil {
		t.Fatalf("ValidateBranchName(feature/foo) with a missing TMPDIR = %v, want nil", err)
	}
}

// TestValidateBranchNameCancelledProbeIsNotAVerdict: a probe stopped by its
// context says nothing about the name. runGit returns the context error rather
// than an *exec.ExitError, and the classification relies on that.
func TestValidateBranchNameCancelledProbeIsNotAVerdict(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := ValidateBranchName(ctx, "a..b")
	if errors.Is(err, ErrInvalidBranchName) || !errors.Is(err, context.Canceled) {
		t.Fatalf("ValidateBranchName on a cancelled context = %v, want context.Canceled, not ErrInvalidBranchName", err)
	}
}

// TestClassifyBranchProbeError: only a git process that exited with a status is
// a verdict on the name. A probe killed by a signal says nothing about it.
func TestClassifyBranchProbeError(t *testing.T) {
	exitErr := func(script string) error {
		t.Helper()
		err := exec.Command("sh", "-c", script).Run()
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("sh -c %q = %v, want *exec.ExitError", script, err)
		}
		// Wrapped the way runGit wraps a failed command.
		return fmt.Errorf("git check-ref-format: %w: stderr", err)
	}

	if err := classifyBranchProbeError("a..b", exitErr("exit 1")); !errors.Is(err, ErrInvalidBranchName) {
		t.Fatalf("exited probe = %v, want ErrInvalidBranchName", err)
	}
	killed := exitErr("kill -9 $$")
	if err := classifyBranchProbeError("feature/foo", killed); errors.Is(err, ErrInvalidBranchName) || !errors.Is(err, killed) {
		t.Fatalf("signal-killed probe = %v, want the probe error, not ErrInvalidBranchName", err)
	}
	notStarted := errors.New("chdir: no such file or directory")
	if err := classifyBranchProbeError("feature/foo", notStarted); errors.Is(err, ErrInvalidBranchName) || !errors.Is(err, notStarted) {
		t.Fatalf("probe that never ran = %v, want the probe error, not ErrInvalidBranchName", err)
	}
}

func TestEnsureWithin(t *testing.T) {
	base := filepath.Join(t.TempDir(), "worktrees", "my-repo")
	cases := []struct {
		target string
		ok     bool
	}{
		{filepath.Join(base, "feature", "foo"), true},
		{filepath.Join(base, "x"), true},
		{filepath.Join(base, "..x"), true}, // a name that merely starts with ".."
		{base, false},
		{filepath.Join(base, ".."), false},
		{filepath.Join(base, "..", "other-repo", "x"), false},
		{filepath.Join(base, "a", "..", "..", "b"), false},
		{"/", false},
	}
	for _, tc := range cases {
		err := ensureWithin(base, tc.target)
		if tc.ok && err != nil {
			t.Errorf("ensureWithin(base, %q) = %v, want nil", tc.target, err)
		}
		if !tc.ok && !errors.Is(err, ErrPathOutsideBase) {
			t.Errorf("ensureWithin(base, %q) = %v, want ErrPathOutsideBase", tc.target, err)
		}
	}
}

// TestEnsureWithinRejectsSymlinkedAncestor replaces a directory inside the base
// with a symlink to a directory outside it. The lexical path stays inside the
// base, so only the resolved comparison can catch it — and clearStaleWorktree
// must then delete nothing.
func TestEnsureWithinRejectsSymlinkedAncestor(t *testing.T) {
	repoDir := initTestRepo(t)
	root := t.TempDir()
	repoBase := filepath.Join(root, "worktrees", "my-repo")
	outside := makeSentinel(t, filepath.Join(root, "outside"))
	victim := makeSentinel(t, filepath.Join(outside, "victim"))
	if err := os.MkdirAll(repoBase, 0o755); err != nil {
		t.Fatalf("mkdir repo base: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(repoBase, "feature")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	target := filepath.Join(repoBase, "feature", "victim")

	if err := ensureWithin(repoBase, target); !errors.Is(err, ErrPathOutsideBase) {
		t.Fatalf("ensureWithin through a symlinked ancestor = %v, want ErrPathOutsideBase", err)
	}

	mgr := NewManager(zerolog.Nop())
	if err := mgr.clearStaleWorktree(context.Background(), repoDir, repoBase, target); err == nil {
		t.Fatal("clearStaleWorktree followed a symlinked ancestor out of the base")
	}
	assertSentinelSurvives(t, victim)
	assertSentinelSurvives(t, outside)
}

// TestEnsureWithinAcceptsSymlinkedBase covers macOS's /var -> /private/var: a
// base reached through a symlink is fine as long as the target resolves under
// the same real directory.
func TestEnsureWithinAcceptsSymlinkedBase(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(filepath.Join(real, "my-repo", "feature"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := ensureWithin(filepath.Join(link, "my-repo"), filepath.Join(link, "my-repo", "feature")); err != nil {
		t.Fatalf("ensureWithin under a symlinked base = %v, want nil", err)
	}
	// A target that does not exist yet (the usual Create case) under a base
	// that does not exist yet either.
	if err := ensureWithin(filepath.Join(link, "new-repo"), filepath.Join(link, "new-repo", "a", "b")); err != nil {
		t.Fatalf("ensureWithin for not-yet-created paths = %v, want nil", err)
	}
}

// TestRemoveWorktreeDirRefusesPathsOutsideRepoBase feeds a deleting function,
// so every input stays inside this test's own temp dir: if the guard ever
// regresses, the worst it can remove is test scratch. Never add "/", "" or any
// real host path here — with the guard disabled, removeWorktreeDir("/") is
// os.RemoveAll("/"). Root and empty paths are covered by TestEnsureWithin,
// which only checks.
func TestRemoveWorktreeDirRefusesPathsOutsideRepoBase(t *testing.T) {
	root := t.TempDir()
	repoBase := filepath.Join(root, "worktrees", "my-repo")
	outside := makeSentinel(t, filepath.Join(root, "outside"))
	for _, p := range []string{repoBase, outside, filepath.Join(repoBase, "..", "..", "outside")} {
		if err := removeWorktreeDir(repoBase, p); !errors.Is(err, ErrPathOutsideBase) {
			t.Errorf("removeWorktreeDir(repoBase, %q) = %v, want ErrPathOutsideBase", p, err)
		}
	}
	assertSentinelSurvives(t, outside)
}

// TestArchiveRefusesWorktreeOutsideRepoBase hands Archive a stored path that
// is a real, registered git worktree living outside <worktree base>/<repo>.
// Before containment was threaded through Archive, `git worktree remove
// --force` deleted it. Both removal routes must now refuse.
func TestArchiveRefusesWorktreeOutsideRepoBase(t *testing.T) {
	ctx := context.Background()
	repoDir := initTestRepo(t)
	root := t.TempDir()
	wtBase := filepath.Join(root, "worktrees")
	if err := os.MkdirAll(filepath.Join(wtBase, "my-repo"), 0o755); err != nil {
		t.Fatalf("mkdir repo base: %v", err)
	}
	mgr := NewManager(zerolog.Nop())

	// git worktree remove route: a registered worktree outside the base.
	registered := filepath.Join(root, "outside", "registered")
	if _, err := runGit(ctx, repoDir, "worktree", "add", "-b", "outside-branch", registered, "main"); err != nil {
		t.Fatalf("worktree add: %v", err)
	}
	makeSentinel(t, filepath.Join(registered, "data"))
	if err := mgr.Archive(ctx, "my-repo", wtBase, registered); !errors.Is(err, ErrPathOutsideBase) {
		t.Fatalf("Archive(registered worktree outside base) = %v, want ErrPathOutsideBase", err)
	}
	assertSentinelSurvives(t, filepath.Join(registered, "data"))

	// Direct os.RemoveAll fallback route: a plain directory (not a git repo)
	// outside the base, including one reached through `..` from inside it.
	plain := makeSentinel(t, filepath.Join(root, "outside", "plain"))
	for _, p := range []string{plain, filepath.Join(wtBase, "my-repo", "..", "..", "outside", "plain")} {
		if err := mgr.Archive(ctx, "my-repo", wtBase, p); !errors.Is(err, ErrPathOutsideBase) {
			t.Fatalf("Archive(%q) = %v, want ErrPathOutsideBase", p, err)
		}
	}
	assertSentinelSurvives(t, plain)

	// Another repo's worktree under the same base is outside this repo's base.
	other := makeSentinel(t, filepath.Join(wtBase, "other-repo", "feature"))
	if err := mgr.Archive(ctx, "my-repo", wtBase, other); !errors.Is(err, ErrPathOutsideBase) {
		t.Fatalf("Archive(other repo's worktree) = %v, want ErrPathOutsideBase", err)
	}
	assertSentinelSurvives(t, other)

	if err := mgr.Archive(ctx, "my-repo", "", plain); !errors.Is(err, ErrPathOutsideBase) {
		t.Fatalf("Archive with no worktree base = %v, want ErrPathOutsideBase", err)
	}
	assertSentinelSurvives(t, plain)
}

// TestArchiveRefusesSymlinkedAncestor stores a worktree path that is lexically
// inside <worktree base>/<repo> but whose ancestor is a symlink to a directory
// outside it. Archive must resolve the ancestor and delete nothing.
func TestArchiveRefusesSymlinkedAncestor(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	wtBase := filepath.Join(root, "worktrees")
	repoBase := filepath.Join(wtBase, "my-repo")
	outside := makeSentinel(t, filepath.Join(root, "outside"))
	victim := makeSentinel(t, filepath.Join(outside, "victim"))
	if err := os.MkdirAll(repoBase, 0o755); err != nil {
		t.Fatalf("mkdir repo base: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(repoBase, "feature")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	mgr := NewManager(zerolog.Nop())
	if err := mgr.Archive(ctx, "my-repo", wtBase, filepath.Join(repoBase, "feature", "victim")); !errors.Is(err, ErrPathOutsideBase) {
		t.Fatalf("Archive through a symlinked ancestor = %v, want ErrPathOutsideBase", err)
	}
	// A leaf symlink inside the base that points outside is refused too.
	if err := mgr.Archive(ctx, "my-repo", wtBase, filepath.Join(repoBase, "feature")); !errors.Is(err, ErrPathOutsideBase) {
		t.Fatalf("Archive of a leaf symlink pointing outside = %v, want ErrPathOutsideBase", err)
	}
	assertSentinelSurvives(t, victim)
	assertSentinelSurvives(t, outside)
}

// TestArchiveRemovesWorktreeInsideRepoBase is the positive control: a worktree
// Create made under the base is still archived through a symlinked base path
// (macOS /var -> /private/var).
func TestArchiveRemovesWorktreeInsideRepoBase(t *testing.T) {
	ctx := context.Background()
	repoDir := initTestRepo(t)
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	mgr := NewManager(zerolog.Nop())
	result, err := mgr.Create(ctx, CreateOpts{
		RepoPath:        repoDir,
		BaseBranch:      "main",
		WorktreeBaseDir: link,
		RepoName:        "my-repo",
		Title:           "Archive inside base",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Archive(ctx, "my-repo", link, result.WorktreePath); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if _, err := os.Stat(result.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree still present after Archive: err=%v", err)
	}
}
