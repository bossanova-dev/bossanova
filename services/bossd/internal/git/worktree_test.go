package git

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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

// remoteBranchSeams wires Manager's injectable remote-branch seams (BOS-539) to
// in-memory fakes and records how many queries each issued, so the batching
// contract can be asserted without a network remote.
type remoteBranchSeams struct {
	batch      map[string]struct{} // set returned by the batched lookup
	batchErr   error               // non-nil forces the fail-open fallback
	probeTaken map[string]bool     // per-candidate fallback answers

	batchCalls    int
	batchPrefixes []string
	probeCalls    int
	probedNames   []string
}

func (s *remoteBranchSeams) install(m *Manager) {
	m.remoteBranches = func(_ context.Context, _, prefix string) (map[string]struct{}, error) {
		s.batchCalls++
		s.batchPrefixes = append(s.batchPrefixes, prefix)
		if s.batchErr != nil {
			return nil, s.batchErr
		}
		return s.batch, nil
	}
	m.remoteBranchProbe = func(_ context.Context, _, branch string) bool {
		s.probeCalls++
		s.probedNames = append(s.probedNames, branch)
		return s.probeTaken[branch]
	}
}

func remoteBranchSet(names ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	return set
}

// A colliding walk must pay exactly one remote round trip no matter how many
// locally-absent candidates it steps over (BOS-539 acceptance criterion 1).
func TestAvailableNewBranchNameIssuesOneRemoteQueryForWholeWalk(t *testing.T) {
	repoDir := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())
	seams := &remoteBranchSeams{batch: remoteBranchSet("foo", "foo-2", "foo-3")}
	seams.install(mgr)

	got, err := mgr.availableNewBranchName(context.Background(), repoDir, "foo", true)
	if err != nil {
		t.Fatalf("availableNewBranchName: %v", err)
	}
	if got != "foo-4" {
		t.Fatalf("branch = %q, want foo-4", got)
	}
	if seams.batchCalls != 1 {
		t.Fatalf("batched remote queries = %d, want 1 (prefixes: %v)", seams.batchCalls, seams.batchPrefixes)
	}
	if len(seams.batchPrefixes) != 1 || seams.batchPrefixes[0] != "foo" {
		t.Fatalf("batch prefixes = %v, want [foo]", seams.batchPrefixes)
	}
	if seams.probeCalls != 0 {
		t.Fatalf("per-candidate probes = %d, want 0 on the batched path", seams.probeCalls)
	}
}

// The free local check must still short-circuit ahead of any network work, and
// the batch must be resolved lazily so a fully-local walk stays offline
// (BOS-539 acceptance criterion 2).
func TestAvailableNewBranchNameSkipsRemoteQueryForLocalCandidates(t *testing.T) {
	t.Run("no suffix allowed issues no remote query at all", func(t *testing.T) {
		repoDir := initTestRepo(t)
		gitOutput(t, repoDir, "branch", "foo")

		mgr := NewManager(zerolog.Nop())
		seams := &remoteBranchSeams{}
		seams.install(mgr)

		_, err := mgr.availableNewBranchName(context.Background(), repoDir, "foo", false)
		if !errors.Is(err, ErrBranchExists) {
			t.Fatalf("err = %v, want ErrBranchExists", err)
		}
		if seams.batchCalls != 0 || seams.probeCalls != 0 {
			t.Fatalf("remote queries = %d batched / %d per-candidate, want 0/0", seams.batchCalls, seams.probeCalls)
		}
	})

	t.Run("batch resolved lazily on the first local miss", func(t *testing.T) {
		repoDir := initTestRepo(t)
		gitOutput(t, repoDir, "branch", "foo")

		mgr := NewManager(zerolog.Nop())
		seams := &remoteBranchSeams{batch: remoteBranchSet()}
		seams.install(mgr)

		got, err := mgr.availableNewBranchName(context.Background(), repoDir, "foo", true)
		if err != nil {
			t.Fatalf("availableNewBranchName: %v", err)
		}
		if got != "foo-2" {
			t.Fatalf("branch = %q, want foo-2", got)
		}
		if seams.batchCalls != 1 {
			t.Fatalf("batched remote queries = %d, want 1", seams.batchCalls)
		}
		// The prefix must be the BASE, not the candidate that triggered the lazy
		// resolve. Only this subtest can tell them apart: here the first local
		// miss is "foo-2", so keying the query on the candidate would query
		// `refs/heads/foo-2*` — which does not cover foo-3, foo-4, … and would
		// hand back a name already taken on origin (the fail-CLOSED hazard).
		if len(seams.batchPrefixes) != 1 || seams.batchPrefixes[0] != "foo" {
			t.Fatalf("batch prefixes = %v, want [foo] (the base, not the candidate)", seams.batchPrefixes)
		}
	})
}

// A branch that lives only on origin must still be skipped — the batch must not
// silently narrow the check (BOS-539 acceptance criterion 3).
func TestAvailableNewBranchNameSkipsRemoteOnlyBranch(t *testing.T) {
	repoDir := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())
	seams := &remoteBranchSeams{batch: remoteBranchSet("foo")}
	seams.install(mgr)

	got, err := mgr.availableNewBranchName(context.Background(), repoDir, "foo", true)
	if err != nil {
		t.Fatalf("availableNewBranchName: %v", err)
	}
	if got != "foo-2" {
		t.Fatalf("branch = %q, want foo-2", got)
	}
}

// allowSuffix=false is the explicitly-requested-branch-name shape: there is no
// suffix walk to absorb a mistake, so "taken" is decided entirely by the batched
// set on the very first candidate. Pin that a remote-only collision is still
// caught here, so a "we cannot suffix anyway, skip the remote query" shortcut
// cannot slip in — it would hand back a name already taken on origin.
func TestAvailableNewBranchNameRejectsRemoteOnlyCollisionWithoutSuffix(t *testing.T) {
	repoDir := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())
	seams := &remoteBranchSeams{batch: remoteBranchSet("foo")}
	seams.install(mgr)

	_, err := mgr.availableNewBranchName(context.Background(), repoDir, "foo", false)
	if !errors.Is(err, ErrBranchExists) {
		t.Fatalf("err = %v, want ErrBranchExists for a branch taken only on origin", err)
	}
	if seams.batchCalls != 1 {
		t.Fatalf("batched remote queries = %d, want 1 (the remote must still be consulted)", seams.batchCalls)
	}
}

// LOAD-BEARING (BOS-539 acceptance criterion 4): this test is the guard against
// a fail-CLOSED regression. If a failed batched query were ever misread as "no
// remote branches exist", availableNewBranchName would hand back a name already
// taken on origin and the later push would fail confusingly. Assert the failure
// degrades to the per-candidate probe for the rest of the walk.
func TestAvailableNewBranchNameFallsBackWhenBatchedQueryFails(t *testing.T) {
	repoDir := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())
	seams := &remoteBranchSeams{
		batchErr:   errors.New("ls-remote: network unreachable"),
		probeTaken: map[string]bool{"foo": true, "foo-2": true},
	}
	seams.install(mgr)

	got, err := mgr.availableNewBranchName(context.Background(), repoDir, "foo", true)
	if err != nil {
		t.Fatalf("availableNewBranchName: %v", err)
	}
	if got != "foo-3" {
		t.Fatalf("branch = %q, want foo-3 (a failed batch must not be read as an empty remote)", got)
	}
	if seams.batchCalls != 1 {
		t.Fatalf("batched remote queries = %d, want 1 (retry the batch and the walk pays N round trips again)", seams.batchCalls)
	}
	want := []string{"foo", "foo-2", "foo-3"}
	if len(seams.probedNames) != len(want) {
		t.Fatalf("per-candidate probes = %v, want %v", seams.probedNames, want)
	}
	for i, name := range want {
		if seams.probedNames[i] != name {
			t.Fatalf("per-candidate probes = %v, want %v", seams.probedNames, want)
		}
	}
}

// The 99-candidate cap is unchanged by the batching (BOS-539 acceptance
// criterion 5).
func TestAvailableNewBranchNameExhaustionStillReturnsErrBranchExists(t *testing.T) {
	repoDir := initTestRepo(t)
	taken := remoteBranchSet("foo")
	for i := 2; i <= 100; i++ {
		taken[fmt.Sprintf("foo-%d", i)] = struct{}{}
	}

	mgr := NewManager(zerolog.Nop())
	seams := &remoteBranchSeams{batch: taken}
	seams.install(mgr)

	_, err := mgr.availableNewBranchName(context.Background(), repoDir, "foo", true)
	if !errors.Is(err, ErrBranchExists) {
		t.Fatalf("err = %v, want ErrBranchExists", err)
	}
	if seams.batchCalls != 1 {
		t.Fatalf("batched remote queries = %d, want 1 across the whole exhaustion walk", seams.batchCalls)
	}
}

// Pins the glob semantics against a REAL `git ls-remote` rather than a fake: the
// plan flags "does refs/heads/<base>* actually cover the -N suffix shapes?" as
// the open question, so this exercises real git against a real origin.
func TestRemoteBranchesWithPrefixAgainstRealGit(t *testing.T) {
	repoDir := initTestRepo(t)
	ctx := context.Background()

	// Push the candidate shapes the walk can generate plus two decoys, then drop
	// the local branches so only origin has them.
	for _, branch := range []string{"foo", "foo-2", "foo-10", "foobar", "other"} {
		gitOutput(t, repoDir, "branch", branch)
		gitOutput(t, repoDir, "push", "origin", branch)
		gitOutput(t, repoDir, "branch", "-D", branch)
	}

	got, err := remoteBranchesWithPrefix(ctx, repoDir, "foo")
	if err != nil {
		t.Fatalf("remoteBranchesWithPrefix: %v", err)
	}
	// "foobar" is matched by the prefix glob and is harmless: membership is
	// tested by exact candidate name, never by prefix.
	want := remoteBranchSet("foo", "foo-2", "foo-10", "foobar")
	if len(got) != len(want) {
		t.Fatalf("remote branches = %v, want %v", got, want)
	}
	for name := range want {
		if _, ok := got[name]; !ok {
			t.Fatalf("remote branches = %v, want %v", got, want)
		}
	}

	// End to end through the real seams: foo and foo-2 are taken on origin only.
	branch, err := NewManager(zerolog.Nop()).availableNewBranchName(ctx, repoDir, "foo", true)
	if err != nil {
		t.Fatalf("availableNewBranchName: %v", err)
	}
	if branch != "foo-3" {
		t.Fatalf("branch = %q, want foo-3", branch)
	}
}

// A pattern that matches nothing exits 0 with empty output — "no remote
// branches", NOT a failed query. That distinction is the boundary between the
// batched path and the fail-open fallback, so pin it against real git.
func TestRemoteBranchesWithPrefixEmptyResultIsNotAFailure(t *testing.T) {
	repoDir := initTestRepo(t)
	ctx := context.Background()

	got, err := remoteBranchesWithPrefix(ctx, repoDir, "nothing-matches-this")
	if err != nil {
		t.Fatalf("remoteBranchesWithPrefix: %v, want nil error for an empty match", err)
	}
	if len(got) != 0 {
		t.Fatalf("remote branches = %v, want empty", got)
	}

	branch, err := NewManager(zerolog.Nop()).availableNewBranchName(ctx, repoDir, "nothing-matches-this", true)
	if err != nil {
		t.Fatalf("availableNewBranchName: %v", err)
	}
	if branch != "nothing-matches-this" {
		t.Fatalf("branch = %q, want nothing-matches-this", branch)
	}
}

// LOAD-BEARING, and the other half of the fail-open guarantee: an unreachable
// remote must surface as a non-nil ERROR, never as an empty set. The
// availableNewBranchName fallback test above proves the caller degrades on an
// error, but it fakes the seam, so it can never observe this side regressing.
// Without this test, swallowing the ls-remote failure here (returning an empty
// set and a nil error) is invisible — and that is exactly the fail-CLOSED
// mutation that would hand back a branch name already taken on origin.
func TestRemoteBranchesWithPrefixErrorsWhenRemoteUnreachable(t *testing.T) {
	repoDir := initTestRepo(t)
	// Point origin at a path that is not a repository; real git exits 128.
	gitOutput(t, repoDir, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "definitely-not-a-repo.git"))

	got, err := remoteBranchesWithPrefix(context.Background(), repoDir, "foo")
	if err == nil {
		t.Fatalf("remoteBranchesWithPrefix = %v, nil error; want an error so the caller can fail open", got)
	}
	if got != nil {
		t.Fatalf("remote branches = %v, want nil alongside the error", got)
	}
}

// remoteBranchExists used to be called straight from the collision walk, so the
// Create tests covered it incidentally. Batching demoted it to the fail-open
// fallback seam, which the walk tests fake — leaving the probe that every
// DEGRADED create now depends on with no coverage at all. Pin it directly
// against real git so it cannot rot undetected.
func TestRemoteBranchExistsAgainstRealGit(t *testing.T) {
	repoDir := initTestRepo(t)
	ctx := context.Background()

	// Push "foo" and drop it locally, so only origin has it.
	gitOutput(t, repoDir, "branch", "foo")
	gitOutput(t, repoDir, "push", "origin", "foo")
	gitOutput(t, repoDir, "branch", "-D", "foo")

	if !remoteBranchExists(ctx, repoDir, "foo") {
		t.Fatal("remoteBranchExists(foo) = false, want true for a branch present only on origin")
	}
	if remoteBranchExists(ctx, repoDir, "bar") {
		t.Fatal("remoteBranchExists(bar) = true, want false for a branch on neither side")
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

// TestCreate_IgnoresBossdManagedPatterns verifies every bossd-written artifact
// path lands in the worktree's shared info/exclude. Each entry is a file bossd
// (or an agent plugin acting for it) drops into the worktree; left untracked
// they surface in `git status` and make the finalize pipeline misclassify a
// "did nothing" run as having agent changes → pr_failed → Blocked.
//
// The expectations are literals, not a loop over bossdManagedExcludePatterns,
// so dropping a pattern from that slice fails here instead of quietly agreeing
// with itself.
func TestCreate_IgnoresBossdManagedPatterns(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	mgr := NewManager(zerolog.Nop())

	if _, err := mgr.Create(context.Background(), CreateOpts{
		RepoPath:        repoDir,
		BaseBranch:      "main",
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		Title:           "Managed patterns",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(repoDir, ".git", "info", "exclude"))
	if err != nil {
		t.Fatalf("read exclude: %v", err)
	}
	want := []string{
		".boss/",
		".claude/settings.local.json",
		".superpowers/",
		// BOS-486: the opencode question-signal hook the plugin injects at
		// StartRun and removes at run end. A crashed run can leave it behind.
		".opencode/plugins/bossd-question.js",
	}
	for _, pattern := range want {
		if !strings.Contains(string(body), pattern) {
			t.Errorf("info/exclude is missing %q. Body:\n%s", pattern, body)
		}
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

// TestResurrect_BranchDeletedRecreatesFromBase covers BOS-421: after BOS-180
// safe-deletes a session's local branch on archive, resurrection must recreate
// the branch from the base branch instead of failing with
// `fatal: invalid reference: <branch>`.
func TestResurrect_BranchDeletedRecreatesFromBase(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	result, err := mgr.Create(ctx, CreateOpts{
		RepoPath:        repoDir,
		BaseBranch:      "main",
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		Title:           "Resurrect recreate",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Commit a change on the branch, then merge it into main so the branch is
	// an ancestor of main (mimicking a merged session that BOS-180 reaps).
	if err := os.WriteFile(filepath.Join(result.WorktreePath, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	gitOutput(t, result.WorktreePath, "add", "f.txt")
	gitOutput(t, result.WorktreePath, "commit", "-m", "work")
	gitOutput(t, repoDir, "merge", "--no-ff", "-m", "merge", result.BranchName)
	// Publish the merge to origin so origin/main (the canonical base, preferred
	// as the recreate start point) matches the local base tip.
	gitOutput(t, repoDir, "push", "origin", "main")

	// Archive, then simulate BOS-180's safe-delete: prune worktree refs and
	// delete the local branch. initTestRepo never pushes the feature branch, so
	// there is no origin/<branch> to DWIM to.
	if err := mgr.Archive(ctx, result.WorktreePath); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	gitOutput(t, repoDir, "worktree", "prune")
	gitOutput(t, repoDir, "branch", "-D", result.BranchName)

	if err := mgr.Resurrect(ctx, ResurrectOpts{
		RepoPath:     repoDir,
		WorktreePath: result.WorktreePath,
		BranchName:   result.BranchName,
		BaseBranch:   "main",
	}); err != nil {
		t.Fatalf("Resurrect after branch delete: %v", err)
	}

	if _, err := os.Stat(result.WorktreePath); err != nil {
		t.Errorf("worktree dir not found after resurrect: %v", err)
	}
	current := gitOutput(t, result.WorktreePath, "branch", "--show-current")
	if current != result.BranchName {
		t.Fatalf("current branch = %q, want %q", current, result.BranchName)
	}
	// Recreated branch tip should equal main's tip.
	branchTip := gitOutput(t, repoDir, "rev-parse", "refs/heads/"+result.BranchName)
	mainTip := gitOutput(t, repoDir, "rev-parse", "refs/heads/main")
	if branchTip != mainTip {
		t.Errorf("recreated branch tip = %q, want main tip %q", branchTip, mainTip)
	}
}

// TestResurrect_ZeroCommitBranchRecreatesFromBase covers the second BOS-180
// safe-delete trigger: a NO_CHANGE branch that never received a commit (its tip
// equals base) and was never pushed, so there is no origin/<branch> to DWIM to.
// Resurrection must recreate it purely from base.
func TestResurrect_ZeroCommitBranchRecreatesFromBase(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	result, err := mgr.Create(ctx, CreateOpts{
		RepoPath:        repoDir,
		BaseBranch:      "main",
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		Title:           "Resurrect no change",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// No commits on the branch: its tip is main's tip (a zero-commit NO_CHANGE
	// branch). Archive, then simulate BOS-180's safe-delete.
	if err := mgr.Archive(ctx, result.WorktreePath); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	gitOutput(t, repoDir, "worktree", "prune")
	gitOutput(t, repoDir, "branch", "-D", result.BranchName)

	if err := mgr.Resurrect(ctx, ResurrectOpts{
		RepoPath:     repoDir,
		WorktreePath: result.WorktreePath,
		BranchName:   result.BranchName,
		BaseBranch:   "main",
	}); err != nil {
		t.Fatalf("Resurrect after zero-commit branch delete: %v", err)
	}

	current := gitOutput(t, result.WorktreePath, "branch", "--show-current")
	if current != result.BranchName {
		t.Fatalf("current branch = %q, want %q", current, result.BranchName)
	}
	branchTip := gitOutput(t, repoDir, "rev-parse", "refs/heads/"+result.BranchName)
	mainTip := gitOutput(t, repoDir, "rev-parse", "refs/heads/main")
	if branchTip != mainTip {
		t.Errorf("recreated branch tip = %q, want main tip %q", branchTip, mainTip)
	}
}

// TestResurrect_PrefersOriginBranchWhenPresent verifies that when origin still
// has the branch (ahead of base), the recreated branch restores the exact
// remote tip rather than falling back to base.
func TestResurrect_PrefersOriginBranchWhenPresent(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	result, err := mgr.Create(ctx, CreateOpts{
		RepoPath:        repoDir,
		BaseBranch:      "main",
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		Title:           "Resurrect origin pref",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Commit a change and push the feature branch to origin so origin/<branch>
	// exists and is ahead of main.
	if err := os.WriteFile(filepath.Join(result.WorktreePath, "f.txt"), []byte("ahead"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	gitOutput(t, result.WorktreePath, "add", "f.txt")
	gitOutput(t, result.WorktreePath, "commit", "-m", "ahead of main")
	gitOutput(t, result.WorktreePath, "push", "-u", "origin", result.BranchName)
	originTip := gitOutput(t, result.WorktreePath, "rev-parse", "HEAD")

	if err := mgr.Archive(ctx, result.WorktreePath); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	gitOutput(t, repoDir, "worktree", "prune")
	gitOutput(t, repoDir, "branch", "-D", result.BranchName)

	if err := mgr.Resurrect(ctx, ResurrectOpts{
		RepoPath:     repoDir,
		WorktreePath: result.WorktreePath,
		BranchName:   result.BranchName,
		BaseBranch:   "main",
	}); err != nil {
		t.Fatalf("Resurrect after branch delete: %v", err)
	}

	branchTip := gitOutput(t, repoDir, "rev-parse", "refs/heads/"+result.BranchName)
	if branchTip != originTip {
		mainTip := gitOutput(t, repoDir, "rev-parse", "refs/heads/main")
		t.Errorf("recreated branch tip = %q, want origin tip %q (main tip %q)", branchTip, originTip, mainTip)
	}
}

// TestResurrect_PrefersLocalBaseOverStaleOriginBase covers the case where the
// branch was merged into the LOCAL base but that merge has not been pushed, so
// refs/heads/main is ahead of refs/remotes/origin/main. Because BOS-180's
// safe-delete is judged against the local base, the local base is the only ref
// guaranteed to carry the reaped branch's work; resurrection must recreate from
// it rather than from the stale origin base.
func TestResurrect_PrefersLocalBaseOverStaleOriginBase(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	result, err := mgr.Create(ctx, CreateOpts{
		RepoPath:        repoDir,
		BaseBranch:      "main",
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		Title:           "Resurrect local base",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Commit on the branch, then merge into local main WITHOUT pushing, so
	// refs/heads/main advances past the (now stale) refs/remotes/origin/main.
	// The feature branch is never pushed, so there is no origin/<branch> either.
	if err := os.WriteFile(filepath.Join(result.WorktreePath, "f.txt"), []byte("local"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	gitOutput(t, result.WorktreePath, "add", "f.txt")
	gitOutput(t, result.WorktreePath, "commit", "-m", "work")
	gitOutput(t, repoDir, "merge", "--no-ff", "-m", "merge", result.BranchName)

	localMainTip := gitOutput(t, repoDir, "rev-parse", "refs/heads/main")
	originMainTip := gitOutput(t, repoDir, "rev-parse", "refs/remotes/origin/main")
	if localMainTip == originMainTip {
		t.Fatalf("precondition failed: local main should be ahead of origin/main")
	}

	if err := mgr.Archive(ctx, result.WorktreePath); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	gitOutput(t, repoDir, "worktree", "prune")
	gitOutput(t, repoDir, "branch", "-D", result.BranchName)

	if err := mgr.Resurrect(ctx, ResurrectOpts{
		RepoPath:     repoDir,
		WorktreePath: result.WorktreePath,
		BranchName:   result.BranchName,
		BaseBranch:   "main",
	}); err != nil {
		t.Fatalf("Resurrect after branch delete: %v", err)
	}

	branchTip := gitOutput(t, repoDir, "rev-parse", "refs/heads/"+result.BranchName)
	if branchTip != localMainTip {
		t.Errorf("recreated branch tip = %q, want local main tip %q (stale origin/main %q)", branchTip, localMainTip, originMainTip)
	}
}

// TestResurrect_BaseMissingReturnsError verifies that when the branch is
// missing and no start point resolves (bogus base, no origin ref), Resurrect
// returns a clear error naming branch + base and creates no worktree.
func TestResurrect_BaseMissingReturnsError(t *testing.T) {
	repoDir := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	wtPath := filepath.Join(t.TempDir(), "worktrees", "gone")
	err := mgr.Resurrect(ctx, ResurrectOpts{
		RepoPath:     repoDir,
		WorktreePath: wtPath,
		BranchName:   "gone-branch",
		BaseBranch:   "does-not-exist",
	})
	if err == nil {
		t.Fatalf("expected error when branch and base are both missing, got nil")
	}
	if !strings.Contains(err.Error(), "gone-branch") || !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error should name branch + base, got: %v", err)
	}
	if _, statErr := os.Stat(wtPath); statErr == nil {
		t.Errorf("no worktree should have been created at %s", wtPath)
	}
}

func TestReapLocalBranches_NeverDeletesRemoteBranch(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	logger := zerolog.Nop()
	mgr := NewManager(logger)
	ctx := context.Background()

	// Push this branch because initTestRepo configures origin but does not push
	// worktree branches. The precondition makes this regression non-vacuous.
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

	if _, err := runGit(ctx, repoDir, "push", "-u", "origin", result.BranchName); err != nil {
		t.Fatalf("push branch to origin: %v", err)
	}
	remoteHeadBefore, err := runGit(ctx, repoDir, "ls-remote", "origin", "refs/heads/"+result.BranchName)
	if err != nil {
		t.Fatalf("read remote branch before reaping: %v", err)
	}
	if remoteHeadBefore == "" {
		t.Fatal("remote branch must exist before reaping")
	}

	// Leave the worktree registered but remove it from disk. Reaping must prune
	// this stale registration before branch deletion can succeed.
	if err := os.RemoveAll(result.WorktreePath); err != nil {
		t.Fatalf("remove worktree directory: %v", err)
	}

	if err := mgr.ReapLocalBranches(ctx, repoDir, []string{result.BranchName}); err != nil {
		t.Fatalf("ReapLocalBranches: %v", err)
	}

	remoteHeadAfter, err := runGit(ctx, repoDir, "ls-remote", "origin", "refs/heads/"+result.BranchName)
	if err != nil {
		t.Fatalf("read remote branch after reaping: %v", err)
	}
	if remoteHeadAfter != remoteHeadBefore {
		t.Errorf("remote branch changed after local reaping:\nbefore: %q\nafter:  %q", remoteHeadBefore, remoteHeadAfter)
	}
	if out := gitOutput(t, repoDir, "branch", "--list", result.BranchName); out != "" {
		t.Errorf("local branch should be deleted after reaping, got: %q", out)
	}
	if worktrees := gitOutput(t, repoDir, "worktree", "list", "--porcelain"); strings.Contains(worktrees, result.WorktreePath) {
		t.Errorf("stale worktree registration remains after reaping: %q", worktrees)
	}
}

func TestReapLocalBranches_ContinuesAfterFailure(t *testing.T) {
	ctx := context.Background()
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	mgr := NewManager(zerolog.Nop())

	// These branches remain checked out in live worktrees, so git branch -D
	// fails for both even after a prune. The third branch must still be tried.
	stuckOne, err := mgr.Create(ctx, CreateOpts{
		RepoPath: repoDir, BaseBranch: "main", WorktreeBaseDir: wtBase, RepoName: "my-repo", Title: "Stuck one",
	})
	if err != nil {
		t.Fatalf("Create first stuck worktree: %v", err)
	}
	stuckTwo, err := mgr.Create(ctx, CreateOpts{
		RepoPath: repoDir, BaseBranch: "main", WorktreeBaseDir: wtBase, RepoName: "my-repo", Title: "Stuck two",
	})
	if err != nil {
		t.Fatalf("Create second stuck worktree: %v", err)
	}
	gitOutput(t, repoDir, "branch", "reaped", "main")

	err = mgr.ReapLocalBranches(ctx, repoDir, []string{stuckOne.BranchName, stuckTwo.BranchName, "reaped"})
	if err == nil {
		t.Fatal("ReapLocalBranches error = nil, want joined errors for checked-out branches")
	}
	if !strings.Contains(err.Error(), stuckOne.BranchName) || !strings.Contains(err.Error(), stuckTwo.BranchName) {
		t.Errorf("joined error = %v, want both failed branch names", err)
	}
	if out := gitOutput(t, repoDir, "branch", "--list", "reaped"); out != "" {
		t.Errorf("later branch was not deleted after earlier failures: %q", out)
	}
}

func TestReapLocalBranches_DedupesAndIgnoresAbsentBranches(t *testing.T) {
	ctx := context.Background()
	repoDir := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())
	gitOutput(t, repoDir, "branch", "doomed", "main")

	if err := mgr.ReapLocalBranches(ctx, repoDir, []string{"absent", "doomed", "doomed"}); err != nil {
		t.Fatalf("ReapLocalBranches should ignore absent and duplicate branches: %v", err)
	}
	if out := gitOutput(t, repoDir, "branch", "--list", "doomed"); out != "" {
		t.Errorf("doomed branch should be deleted, got: %q", out)
	}
}

func TestBranchSafeToDelete(t *testing.T) {
	ctx := context.Background()
	mgr := NewManager(zerolog.Nop())

	t.Run("merged branch is an ancestor of base", func(t *testing.T) {
		repoDir := initTestRepo(t)
		// Branch off main, add a commit, then fast-forward main onto it so the
		// branch tip is an ancestor of main (the merged case).
		gitOutput(t, repoDir, "checkout", "-b", "feature")
		gitOutput(t, repoDir, "commit", "--allow-empty", "-m", "feature work")
		gitOutput(t, repoDir, "checkout", "main")
		gitOutput(t, repoDir, "merge", "--ff-only", "feature")

		safe, err := mgr.BranchSafeToDelete(ctx, repoDir, "feature", "main")
		if err != nil {
			t.Fatalf("BranchSafeToDelete: %v", err)
		}
		if !safe {
			t.Error("merged branch should be safe to delete")
		}
	})

	t.Run("no-change branch at same commit as base is safe", func(t *testing.T) {
		repoDir := initTestRepo(t)
		// Branch with no commits of its own — tip equals base, trivially an
		// ancestor.
		gitOutput(t, repoDir, "checkout", "-b", "nochange")

		safe, err := mgr.BranchSafeToDelete(ctx, repoDir, "nochange", "main")
		if err != nil {
			t.Fatalf("BranchSafeToDelete: %v", err)
		}
		if !safe {
			t.Error("no-change branch should be safe to delete")
		}
	})

	t.Run("branch ahead of base is not safe", func(t *testing.T) {
		repoDir := initTestRepo(t)
		gitOutput(t, repoDir, "checkout", "-b", "unmerged")
		gitOutput(t, repoDir, "commit", "--allow-empty", "-m", "unmerged work")

		safe, err := mgr.BranchSafeToDelete(ctx, repoDir, "unmerged", "main")
		if err != nil {
			t.Fatalf("BranchSafeToDelete: %v", err)
		}
		if safe {
			t.Error("branch with an unmerged commit should NOT be safe to delete")
		}
	})
}

func TestDeleteLocalBranch(t *testing.T) {
	ctx := context.Background()
	mgr := NewManager(zerolog.Nop())
	repoDir := initTestRepo(t)

	// Record the remote's refs so we can assert they are untouched.
	remoteBefore := gitOutput(t, repoDir, "ls-remote", "origin")

	// Create two local branches; only "doomed" is deleted.
	gitOutput(t, repoDir, "branch", "doomed", "main")
	gitOutput(t, repoDir, "branch", "keeper", "main")

	if err := mgr.DeleteLocalBranch(ctx, repoDir, "doomed"); err != nil {
		t.Fatalf("DeleteLocalBranch: %v", err)
	}

	// The deleted branch is gone.
	if out := gitOutput(t, repoDir, "branch", "--list", "doomed"); out != "" {
		t.Errorf("branch 'doomed' should be deleted, got: %q", out)
	}
	// Other branches remain.
	if out := gitOutput(t, repoDir, "branch", "--list", "keeper"); !strings.Contains(out, "keeper") {
		t.Errorf("branch 'keeper' should remain, got: %q", out)
	}

	// No remote interaction: the remote refs are unchanged (nothing pushed or
	// deleted on origin).
	remoteAfter := gitOutput(t, repoDir, "ls-remote", "origin")
	if remoteAfter != remoteBefore {
		t.Errorf("remote refs changed after DeleteLocalBranch:\nbefore: %q\nafter:  %q", remoteBefore, remoteAfter)
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
	_, err = runSetupScript(context.Background(), worktree, worktree, spec, "", nil, nil)
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

// TestHasDiffAgainstBase pins BOS-591's mark-ready backstop primitive: a
// branch whose only commits are empty (`--allow-empty`, the shape of bossd's
// draft-PR bootstrap commit) has no diff against the base and must report
// false, while a branch that actually changed a file must report true. It also
// pins the three-dot (merge-base) semantics: a base that has since moved ahead
// on an unrelated commit must NOT make an otherwise-empty branch look changed.
func TestHasDiffAgainstBase(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	if _, err := runGit(ctx, repo, "checkout", "-b", "empty-branch"); err != nil {
		t.Fatalf("checkout -b empty-branch: %v", err)
	}
	if _, err := runGit(ctx, repo, "commit", "--allow-empty", "-m", "chore: [skip ci] create pull request"); err != nil {
		t.Fatalf("empty commit: %v", err)
	}

	hasDiff, err := mgr.HasDiffAgainstBase(ctx, repo, "refs/heads/main")
	if err != nil {
		t.Fatalf("HasDiffAgainstBase(empty): %v", err)
	}
	if hasDiff {
		t.Error("HasDiffAgainstBase(empty-commit-only branch) = true, want false")
	}

	// Move the base forward independently. Three-dot semantics compare against
	// the merge base, so the branch must still read as empty.
	if _, err := runGit(ctx, repo, "checkout", "main"); err != nil {
		t.Fatalf("checkout main: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "upstream.txt"), []byte("upstream\n"), 0o600); err != nil {
		t.Fatalf("write upstream.txt: %v", err)
	}
	for _, args := range [][]string{{"add", "upstream.txt"}, {"commit", "-m", "feat: upstream work"}} {
		if _, err := runGit(ctx, repo, args...); err != nil {
			t.Fatalf("git %s: %v", strings.Join(args, " "), err)
		}
	}
	if _, err := runGit(ctx, repo, "checkout", "empty-branch"); err != nil {
		t.Fatalf("checkout empty-branch: %v", err)
	}
	hasDiff, err = mgr.HasDiffAgainstBase(ctx, repo, "refs/heads/main")
	if err != nil {
		t.Fatalf("HasDiffAgainstBase(empty, advanced base): %v", err)
	}
	if hasDiff {
		t.Error("HasDiffAgainstBase(empty branch, advanced base) = true, want false (three-dot semantics)")
	}

	// Now make a real change on the branch.
	if err := os.WriteFile(filepath.Join(repo, "real.txt"), []byte("real\n"), 0o600); err != nil {
		t.Fatalf("write real.txt: %v", err)
	}
	for _, args := range [][]string{{"add", "real.txt"}, {"commit", "-m", "feat: real work"}} {
		if _, err := runGit(ctx, repo, args...); err != nil {
			t.Fatalf("git %s: %v", strings.Join(args, " "), err)
		}
	}
	hasDiff, err = mgr.HasDiffAgainstBase(ctx, repo, "refs/heads/main")
	if err != nil {
		t.Fatalf("HasDiffAgainstBase(real): %v", err)
	}
	if !hasDiff {
		t.Error("HasDiffAgainstBase(branch with a real change) = false, want true")
	}

	if _, err := mgr.HasDiffAgainstBase(ctx, repo, "  "); err == nil {
		t.Error("HasDiffAgainstBase with a blank base ref should error")
	}
}

// TestHasDiffAgainstBase_WhitespaceOnlyPathIsStillADiff pins the `-z` in
// HasDiffAgainstBase. runGit TrimSpace's every command's stdout, so with
// newline-separated `--name-only` output a diff whose sole changed path is
// made entirely of spaces trims away to "" and reads as "no diff" — the
// backstop would then refuse to mark a legitimate PR ready. Dropping `-z`
// fails this test and nothing else.
func TestHasDiffAgainstBase_WhitespaceOnlyPathIsStillADiff(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	if _, err := runGit(ctx, repo, "checkout", "-b", "spacey-branch"); err != nil {
		t.Fatalf("checkout -b spacey-branch: %v", err)
	}
	// A filename consisting only of spaces. git does not quote it (quoting
	// covers control/non-ASCII chars, not plain spaces), so `--name-only`
	// without -z emits a line of pure whitespace.
	spacey := filepath.Join(repo, "   ")
	if err := os.WriteFile(spacey, []byte("content\n"), 0o600); err != nil {
		t.Fatalf("write whitespace-named file: %v", err)
	}
	for _, args := range [][]string{{"add", "--", "   "}, {"commit", "-m", "feat: add a spacey path"}} {
		if _, err := runGit(ctx, repo, args...); err != nil {
			t.Fatalf("git %s: %v", strings.Join(args, " "), err)
		}
	}

	hasDiff, err := mgr.HasDiffAgainstBase(ctx, repo, "refs/heads/main")
	if err != nil {
		t.Fatalf("HasDiffAgainstBase(whitespace-only path): %v", err)
	}
	if !hasDiff {
		t.Error("HasDiffAgainstBase(branch changing only a whitespace-named path) = false, want true")
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

	// An empty local path must be rejected, not run in the daemon's process
	// cwd. A fetch is a ref WRITE and runGit sets cmd.Dir unconditionally, so
	// without this guard an empty path silently writes refs into whatever repo
	// the daemon happens to be sitting in (BOS-591).
	if err := mgr.FetchBase(ctx, "", "main"); err == nil {
		t.Error("FetchBase with empty local path should error")
	}
}

func TestCountMergeCommits_NoMergeCommits(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	if _, err := runGit(ctx, repo, "checkout", "-b", "feat"); err != nil {
		t.Fatalf("checkout -b feat: %v", err)
	}
	if _, err := runGit(ctx, repo, "commit", "--allow-empty", "-m", "feat commit"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := runGit(ctx, repo, "push", "-u", "origin", "feat"); err != nil {
		t.Fatalf("push feat: %v", err)
	}

	count, err := mgr.CountMergeCommits(ctx, repo, "main", "feat")
	if err != nil {
		t.Fatalf("CountMergeCommits: %v", err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0", count)
	}
}

func TestCountMergeCommits_WithMergeCommit(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	if _, err := runGit(ctx, repo, "checkout", "-b", "feat"); err != nil {
		t.Fatalf("checkout -b feat: %v", err)
	}
	if _, err := runGit(ctx, repo, "commit", "--allow-empty", "-m", "feat commit"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := runGit(ctx, repo, "checkout", "-b", "side"); err != nil {
		t.Fatalf("checkout -b side: %v", err)
	}
	if _, err := runGit(ctx, repo, "commit", "--allow-empty", "-m", "side commit"); err != nil {
		t.Fatalf("commit on side: %v", err)
	}
	if _, err := runGit(ctx, repo, "checkout", "feat"); err != nil {
		t.Fatalf("checkout feat: %v", err)
	}
	if _, err := runGit(ctx, repo, "merge", "--no-ff", "--no-edit", "-m", "merge side into feat", "side"); err != nil {
		t.Fatalf("merge --no-ff side into feat: %v", err)
	}
	if _, err := runGit(ctx, repo, "push", "-u", "origin", "feat"); err != nil {
		t.Fatalf("push feat: %v", err)
	}

	count, err := mgr.CountMergeCommits(ctx, repo, "main", "feat")
	if err != nil {
		t.Fatalf("CountMergeCommits: %v", err)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}
}

func TestCountMergeCommits_RequiresBaseAndHead(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	if _, err := mgr.CountMergeCommits(ctx, repo, "", "feat"); err == nil || err.Error() != "base branch is required" {
		t.Errorf("CountMergeCommits with empty base = %v, want error %q", err, "base branch is required")
	}
	if _, err := mgr.CountMergeCommits(ctx, repo, "main", ""); err == nil || err.Error() != "head branch is required" {
		t.Errorf("CountMergeCommits with empty head = %v, want error %q", err, "head branch is required")
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

	if _, err := runSetupScript(context.Background(), worktree, worktree, spec, "", nil, nil); err != nil {
		t.Fatalf("runSetupScript: %v", err)
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatalf("sentinel was created — argv was interpreted as shell")
	} else if !os.IsNotExist(err) {
		t.Fatalf("unexpected stat error: %v", err)
	}
}

// TestRunSetupScript_TeesToLogSinkAndReportsDuration pins the daemon-side half
// of the tee: even when a caller claims the live output stream, the log sink
// still receives the script's output, and the run's own duration comes back so
// it can be attributed in the daemon log.
func TestRunSetupScript_TeesToLogSinkAndReportsDuration(t *testing.T) {
	worktree := t.TempDir()

	var stream, logSink bytes.Buffer
	spec := `{"type":"command","argv":["sh","-c","echo tee-marker; sleep 0.05"]}`

	got, err := runSetupScript(context.Background(), worktree, worktree, spec, "", &stream, &logSink)
	if err != nil {
		t.Fatalf("runSetupScript: %v", err)
	}
	if !strings.Contains(stream.String(), "tee-marker") {
		t.Fatalf("client stream missed the output: %q", stream.String())
	}
	if !strings.Contains(logSink.String(), "tee-marker") {
		t.Fatalf("log sink missed the output: %q", logSink.String())
	}
	if got < 40*time.Millisecond {
		t.Fatalf("duration = %v, want at least 40ms for a 50ms sleep", got)
	}
}

// lockedBuffer is a concurrency-safe io.Writer for capturing zerolog output in
// tests. Defensive rather than currently required: nothing in this package
// logs off-goroutine today, but a zerolog.Logger writing into a bare
// bytes.Buffer is one background-goroutine refactor away from a data race.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestCreate_LogsSetupScriptDurationAndOutput is the structural check behind
// "a TUI-created session in a setup-script repo leaves a duration record in
// the daemon log": Create is driven with a client stream attached (exactly
// what the create-session RPC does, and the configuration that previously left
// the daemon log empty), and the manager's logger must still carry both the
// measured duration and the script's output.
func TestCreate_LogsSetupScriptDurationAndOutput(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")

	logs := &lockedBuffer{}
	mgr := NewManager(zerolog.New(logs))

	var stream bytes.Buffer
	script := `echo create-log-marker`
	result, err := mgr.Create(context.Background(), CreateOpts{
		RepoPath:          repoDir,
		BaseBranch:        "main",
		WorktreeBaseDir:   wtBase,
		RepoName:          "my-repo",
		Title:             "Setup log test",
		SetupScript:       &script,
		SetupScriptOutput: &stream,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if result.SetupErr != nil {
		t.Fatalf("SetupErr = %v, want nil", result.SetupErr)
	}
	if !strings.Contains(stream.String(), "create-log-marker") {
		t.Fatalf("client stream missed the output: %q", stream.String())
	}

	got := logs.String()
	if !strings.Contains(got, "create-log-marker") {
		t.Fatalf("daemon log missing the setup-script output tail:\n%s", got)
	}

	// Assert the duration is a real measurement, not a zero value that happens
	// to serialize.
	event, ok := findSetupScriptRunEvent(t, got, "setup script finished")
	if !ok {
		t.Fatalf("daemon log missing the setup-script duration record:\n%s", got)
	}
	if event.Op != "create" {
		t.Errorf(`op = %q, want "create"`, event.Op)
	}
	if event.Duration <= 0 {
		t.Errorf("setup_script_run_ms = %v, want a positive measurement", event.Duration)
	}
}

// TestResurrect_LogsSetupScriptDurationAndOutput pins the resurrect wiring end
// to end. It matters more than the create case: Resurrect returns no duration
// to any caller, so this log event is the only record that path produces —
// deleting the wiring there would otherwise leave the suite green.
func TestResurrect_LogsSetupScriptDurationAndOutput(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")

	logs := &lockedBuffer{}
	mgr := NewManager(zerolog.New(logs))
	ctx := context.Background()

	result, err := mgr.Create(ctx, CreateOpts{
		RepoPath:        repoDir,
		BaseBranch:      "main",
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		Title:           "Resurrect setup log test",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Archive(ctx, result.WorktreePath); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	script := `echo resurrect-log-marker`
	if err := mgr.Resurrect(ctx, ResurrectOpts{
		RepoPath:     repoDir,
		WorktreePath: result.WorktreePath,
		BranchName:   result.BranchName,
		SetupScript:  &script,
	}); err != nil {
		t.Fatalf("Resurrect: %v", err)
	}

	event, ok := findSetupScriptRunEvent(t, logs.String(), "setup script finished")
	if !ok {
		t.Fatalf("daemon log missing the resurrect duration record:\n%s", logs.String())
	}
	if event.Op != "resurrect" {
		t.Errorf("op = %q, want resurrect", event.Op)
	}
	if event.Duration <= 0 {
		t.Errorf("setup_script_run_ms = %v, want a positive measurement", event.Duration)
	}
	if !strings.Contains(logs.String(), "resurrect-log-marker") {
		t.Errorf("daemon log missing the resurrect setup output tail:\n%s", logs.String())
	}
}

// TestCreate_BlankSetupScriptIsSkippedEverywhere pins the harmonized guard.
// Before runAndLogSetup the three call sites disagreed: create and resurrect
// tested the raw string, create-from-existing-branch trimmed first, so a
// whitespace-only setup_script surfaced as an ErrInvalidSpec SetupErr on two
// paths and was skipped on the third. It is now treated as "no setup step"
// everywhere — no run, no SetupErr, no duration, and no log event to mislead
// someone reading the create.
func TestCreate_BlankSetupScriptIsSkippedEverywhere(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")

	logs := &lockedBuffer{}
	mgr := NewManager(zerolog.New(logs))

	script := "   \n\t"
	result, err := mgr.Create(context.Background(), CreateOpts{
		RepoPath:        repoDir,
		BaseBranch:      "main",
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		Title:           "Blank setup script",
		SetupScript:     &script,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if result.SetupErr != nil {
		t.Errorf("SetupErr = %v, want nil for a whitespace-only setup_script", result.SetupErr)
	}
	if result.SetupScriptDuration != 0 {
		t.Errorf("SetupScriptDuration = %v, want 0 — nothing ran", result.SetupScriptDuration)
	}
	if _, ok := findSetupScriptRunEvent(t, logs.String(), "setup script finished"); ok {
		t.Errorf("logged a setup-script run that never happened:\n%s", logs.String())
	}
	if _, ok := findSetupScriptRunEvent(t, logs.String(), "setup script failed; continuing"); ok {
		t.Errorf("logged a setup-script failure that never happened:\n%s", logs.String())
	}
}

// setupScriptRunEvent is the subset of the structured setup-script log event
// the tests assert on.
type setupScriptRunEvent struct {
	Level    string  `json:"level"`
	Message  string  `json:"message"`
	Op       string  `json:"op"`
	Branch   string  `json:"branch"`
	Error    string  `json:"error"`
	Duration float64 `json:"setup_script_run_ms"`
}

// findSetupScriptRunEvent scans captured zerolog output for the last event with
// the given message. Non-JSON and unrelated lines are skipped.
func findSetupScriptRunEvent(t *testing.T, logs, message string) (setupScriptRunEvent, bool) {
	t.Helper()
	var found setupScriptRunEvent
	var ok bool
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		var event setupScriptRunEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		if event.Message != message {
			continue
		}
		found, ok = event, true
	}
	return found, ok
}

// TestLogSetupScriptRun_FailureBranchAndOpTag covers what the Create-path test
// cannot reach: the warning branch, and an op tag other than "create". The op
// tag exists so a resurrect of a worktree is distinguishable from its create in
// the log, which is only true if the failure branch carries it too.
func TestLogSetupScriptRun_FailureBranchAndOpTag(t *testing.T) {
	logs := &lockedBuffer{}
	mgr := NewManager(zerolog.New(logs))

	mgr.logSetupScriptRun("resurrect", "/tmp/wt", "feature-branch", 1500*time.Millisecond,
		errors.New("exit status 2"))

	event, ok := findSetupScriptRunEvent(t, logs.String(), "setup script failed; continuing")
	if !ok {
		t.Fatalf("no failure event logged:\n%s", logs.String())
	}
	if event.Level != "warn" {
		t.Errorf("level = %q, want warn (a failed setup must not log as success)", event.Level)
	}
	if event.Op != "resurrect" {
		t.Errorf("op = %q, want resurrect", event.Op)
	}
	if event.Branch != "feature-branch" {
		t.Errorf("branch = %q, want feature-branch", event.Branch)
	}
	if !strings.Contains(event.Error, "exit status 2") {
		t.Errorf("error = %q, want the wrapped failure", event.Error)
	}
	if event.Duration != 1500 {
		t.Errorf("duration = %v ms, want 1500 — a failed run still cost its time", event.Duration)
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

// withInjectedTag inserts "[#n] " immediately after the placeholder's
// conventional-commit "type: " prefix, mirroring what a prior injector run
// (or a rebase against a different PR number) can leave behind. Built from
// the constant rather than a retyped literal so the test can't drift from
// production's prefix.
func withInjectedTag(t *testing.T, n int) string {
	t.Helper()
	placeholder := DraftPRPlaceholderCommitSubject
	idx := strings.Index(placeholder, ": ")
	if idx < 0 {
		t.Fatalf("placeholder %q has no conventional-commit prefix", placeholder)
	}
	prefixEnd := idx + 2
	return placeholder[:prefixEnd] + "[#" + strconv.Itoa(n) + "] " + placeholder[prefixEnd:]
}

// TestInjectPRNumbers_LeavesDraftPRPlaceholderSubjectUnchanged pins BOS-591:
// the injector must never rewrite the draft-PR bootstrap commit's subject,
// even while it's still tagging real commits on the same branch normally.
func TestInjectPRNumbers_LeavesDraftPRPlaceholderSubjectUnchanged(t *testing.T) {
	ctx := context.Background()
	branch := "cron-inject-placeholder-and-real"
	repoDir := injectTestBranch(t, branch,
		DraftPRPlaceholderCommitSubject,
		"feat(web): add widget",
	)
	mgr := NewManager(zerolog.Nop())

	if err := mgr.InjectPRNumbers(ctx, repoDir, branch, 42, "refs/remotes/origin/main"); err != nil {
		t.Fatalf("InjectPRNumbers: %v", err)
	}

	subjects := strings.Split(gitOutput(t, repoDir, "log", "--format=%s", "origin/main..HEAD"), "\n")
	var gotPlaceholder, gotReal bool
	for _, s := range subjects {
		switch s {
		case DraftPRPlaceholderCommitSubject:
			gotPlaceholder = true
		case "feat(web): [#42] add widget":
			gotReal = true
		}
	}
	if !gotPlaceholder {
		t.Fatalf("subjects = %v, want the placeholder subject unchanged (%q)", subjects, DraftPRPlaceholderCommitSubject)
	}
	if !gotReal {
		t.Fatalf("subjects = %v, want the real commit tagged with [#42]", subjects)
	}
}

// TestInjectPRNumbers_LeavesAlreadyTaggedPlaceholderUnchangedAlongsideRealWork
// is the only test that actually executes injectPRTagExec's placeholder
// tag-stripping sed. The sibling cases can't: a placeholder-only branch (tagged
// or not) short-circuits in the needsTag pre-scan so the rebase --exec never
// runs at all, and the mixed branch above carries an UNTAGGED placeholder, for
// which the sed is an identity no-op that the bare equality check would pass on
// its own.
//
// This shape — a placeholder already tagged for an earlier PR, plus a real
// untagged commit forcing needsTag true — is the one the plan calls out as
// existing in the wild. Delete stripPlaceholderTagSed and the placeholder comes
// back double-tagged ("chore: [#42] [#7] [skip ci] create pull request"), which
// IsDraftPRPlaceholderSubject (single-tag tolerance by design) then classifies
// as REAL WORK — silently re-creating the BOS-591 defeat with every other test
// still green.
func TestInjectPRNumbers_LeavesAlreadyTaggedPlaceholderUnchangedAlongsideRealWork(t *testing.T) {
	ctx := context.Background()
	branch := "cron-inject-tagged-placeholder-and-real"
	taggedPlaceholder := withInjectedTag(t, 7)
	repoDir := injectTestBranch(t, branch,
		taggedPlaceholder,
		"feat(web): add widget",
	)
	mgr := NewManager(zerolog.Nop())

	if err := mgr.InjectPRNumbers(ctx, repoDir, branch, 42, "refs/remotes/origin/main"); err != nil {
		t.Fatalf("InjectPRNumbers: %v", err)
	}

	subjects := strings.Split(gitOutput(t, repoDir, "log", "--format=%s", "origin/main..HEAD"), "\n")
	var gotPlaceholder, gotReal bool
	for _, s := range subjects {
		switch s {
		case taggedPlaceholder:
			gotPlaceholder = true
		case "feat(web): [#42] add widget":
			gotReal = true
		}
	}
	if !gotPlaceholder {
		t.Fatalf("subjects = %v, want the already-tagged placeholder byte-identical (%q); a double tag means the strip is gone", subjects, taggedPlaceholder)
	}
	if !gotReal {
		t.Fatalf("subjects = %v, want the real commit tagged with [#42]", subjects)
	}
	// Belt and braces: the failure mode is specifically a second tag, so name it.
	for _, s := range subjects {
		if strings.Contains(s, "[#42]") && strings.Contains(s, "[#7]") {
			t.Fatalf("subject %q carries both PR tags; the placeholder was retagged", s)
		}
	}
	// The classifier must still agree the placeholder is a placeholder — that is
	// what the finalize guard ultimately calls.
	if !IsDraftPRPlaceholderSubject(taggedPlaceholder) {
		t.Fatalf("IsDraftPRPlaceholderSubject(%q) = false, want true", taggedPlaceholder)
	}
}

// TestInjectPRNumbers_OnlyPlaceholderCommitIsNoOp pins the needsTag pre-scan:
// a branch whose only commit is the untagged placeholder must not be
// rebased at all — same SHA before and after, not just an unchanged subject.
func TestInjectPRNumbers_OnlyPlaceholderCommitIsNoOp(t *testing.T) {
	ctx := context.Background()
	branch := "cron-inject-placeholder-only"
	repoDir := injectTestBranch(t, branch, DraftPRPlaceholderCommitSubject)
	mgr := NewManager(zerolog.Nop())

	headBefore := gitOutput(t, repoDir, "rev-parse", "HEAD")
	if err := mgr.InjectPRNumbers(ctx, repoDir, branch, 42, "refs/remotes/origin/main"); err != nil {
		t.Fatalf("InjectPRNumbers: %v", err)
	}
	if got := gitOutput(t, repoDir, "rev-parse", "HEAD"); got != headBefore {
		t.Fatalf("HEAD changed on placeholder-only branch: before=%s after=%s (a no-op rebase still mints a new SHA)", headBefore, got)
	}
	if got := gitOutput(t, repoDir, "log", "-1", "--format=%s"); got != DraftPRPlaceholderCommitSubject {
		t.Fatalf("subject = %q, want unchanged placeholder", got)
	}
}

// TestInjectPRNumbers_OnlyPlaceholderAlreadyTaggedWithDifferentPRIsNoOp
// covers a placeholder tagged by an earlier PR number (which exists on
// branches in the wild): a later run for a different PR number must not
// retag it.
func TestInjectPRNumbers_OnlyPlaceholderAlreadyTaggedWithDifferentPRIsNoOp(t *testing.T) {
	ctx := context.Background()
	branch := "cron-inject-placeholder-tagged"
	taggedPlaceholder := withInjectedTag(t, 7)
	repoDir := injectTestBranch(t, branch, taggedPlaceholder)
	mgr := NewManager(zerolog.Nop())

	headBefore := gitOutput(t, repoDir, "rev-parse", "HEAD")
	if err := mgr.InjectPRNumbers(ctx, repoDir, branch, 42, "refs/remotes/origin/main"); err != nil {
		t.Fatalf("InjectPRNumbers: %v", err)
	}
	if got := gitOutput(t, repoDir, "rev-parse", "HEAD"); got != headBefore {
		t.Fatalf("HEAD changed on already-tagged placeholder branch: before=%s after=%s", headBefore, got)
	}
	if got := gitOutput(t, repoDir, "log", "-1", "--format=%s"); got != taggedPlaceholder {
		t.Fatalf("subject = %q, want unchanged (not retagged to [#42]): %q", got, taggedPlaceholder)
	}
}

// TestInjectPRNumbers_LeavesStackedTagPlaceholderUnchangedAlongsideRealWork
// drives the sed's repeat group end to end, through real sh and real git.
//
// Tags stack because the boss-finalize skill's add-pr-numbers.sh (which this
// branch does not touch) tags the placeholder unconditionally and skips only
// when the CURRENT run's number is present. With a single-tag strip the sed
// fails to recognise a two-tag placeholder and prepends a THIRD tag, and the
// Go classifier then counts it as real work — reopening the BOS-591 guard.
// The real commit on the branch is what makes needsTag true so the --exec
// actually runs.
func TestInjectPRNumbers_LeavesStackedTagPlaceholderUnchangedAlongsideRealWork(t *testing.T) {
	ctx := context.Background()
	branch := "cron-inject-stacked-placeholder-and-real"
	stackedPlaceholder := "chore: [#7] [#42] [skip ci] create pull request"
	if !IsDraftPRPlaceholderSubject(stackedPlaceholder) {
		t.Fatalf("precondition: IsDraftPRPlaceholderSubject(%q) = false", stackedPlaceholder)
	}
	repoDir := injectTestBranch(t, branch,
		stackedPlaceholder,
		"feat(web): add widget",
	)
	mgr := NewManager(zerolog.Nop())

	if err := mgr.InjectPRNumbers(ctx, repoDir, branch, 1689, "refs/remotes/origin/main"); err != nil {
		t.Fatalf("InjectPRNumbers: %v", err)
	}

	subjects := strings.Split(gitOutput(t, repoDir, "log", "--format=%s", "origin/main..HEAD"), "\n")
	var gotPlaceholder, gotReal bool
	for _, s := range subjects {
		switch s {
		case stackedPlaceholder:
			gotPlaceholder = true
		case "feat(web): [#1689] add widget":
			gotReal = true
		}
	}
	if !gotPlaceholder {
		t.Fatalf("subjects = %v, want the stacked-tag placeholder byte-identical (%q); a third tag means the sed's repeat group is gone", subjects, stackedPlaceholder)
	}
	if !gotReal {
		t.Fatalf("subjects = %v, want the real commit tagged with [#1689]", subjects)
	}
	for _, s := range subjects {
		if strings.Contains(s, "[skip ci]") && strings.Contains(s, "[#1689]") {
			t.Fatalf("placeholder %q was retagged with this run's number", s)
		}
	}
}

// TestInjectPRNumbers_OnlyPlaceholderCommitSkipsRebase pins the needsTag
// pre-scan itself, which the SHA-equality tests above do not: git preserves
// the SHA of a commit whose --exec leaves the message alone, so removing
// `|| IsDraftPRPlaceholderSubject(trimmed)` from the pre-scan leaves those
// tests green while the injector silently starts running a rebase (and a
// leased force-push) against every placeholder-only branch.
//
// HEAD's reflog is the discriminator: a rebase always appends entries to it
// ("rebase (start)"/"rebase (finish)") even when every replayed SHA is
// unchanged, whereas the pre-scan's skip path touches no ref at all.
func TestInjectPRNumbers_OnlyPlaceholderCommitSkipsRebase(t *testing.T) {
	ctx := context.Background()
	branch := "cron-inject-placeholder-no-rebase"
	repoDir := injectTestBranch(t, branch, DraftPRPlaceholderCommitSubject)
	mgr := NewManager(zerolog.Nop())

	reflogBefore := gitOutput(t, repoDir, "reflog", "show", "--format=%gs", "HEAD")
	if err := mgr.InjectPRNumbers(ctx, repoDir, branch, 42, "refs/remotes/origin/main"); err != nil {
		t.Fatalf("InjectPRNumbers: %v", err)
	}
	reflogAfter := gitOutput(t, repoDir, "reflog", "show", "--format=%gs", "HEAD")

	if reflogAfter != reflogBefore {
		t.Fatalf("InjectPRNumbers ran a rebase on a placeholder-only branch; HEAD reflog grew:\nbefore:\n%s\nafter:\n%s",
			reflogBefore, reflogAfter)
	}
}

// TestIsDraftPRPlaceholderSubject unit-tests the placeholder-detection helper
// InjectPRNumbers' needsTag pre-scan relies on.
func TestIsDraftPRPlaceholderSubject(t *testing.T) {
	tests := []struct {
		name    string
		subject string
		want    bool
	}{
		{"untagged placeholder", DraftPRPlaceholderCommitSubject, true},
		{"tagged placeholder", withInjectedTag(t, 42), true},
		{"placeholder tagged with a different PR number", withInjectedTag(t, 7), true},
		{"placeholder with surrounding whitespace", "  " + DraftPRPlaceholderCommitSubject + "  \n", true},
		{"genuine tagged commit", "feat(boss): [#1689] add X", false},
		{"genuine untagged commit", "feat(boss): add X", false},
		// Shares the placeholder's own "chore: " prefix AND carries a valid tag,
		// so only the final whole-subject equality check can reject it. Without
		// this case, weakening that check to "matching prefix + a tag" would
		// misclassify genuine chore work as the placeholder and suppress a real
		// run — the exact failure BOS-591 exists to prevent.
		{"tagged chore commit whose suffix is not the placeholder", "chore: [#1689] something else entirely", false},
		{"empty subject", "", false},
		// Tags STACK: the boss-finalize skill's add-pr-numbers.sh tags the
		// placeholder unconditionally and skips only when the CURRENT run's
		// number is present, so two runs for different PRs leave two tags. A
		// single-tag strip would call these real work and reopen the guard.
		{"placeholder with two stacked tags", "chore: [#7] [#42] [skip ci] create pull request", true},
		{"placeholder with three stacked tags", "chore: [#7] [#42] [#1689] [skip ci] create pull request", true},
		// The safety half: repeating the strip must not swallow genuine work.
		{"genuine commit with two stacked tags", "feat(boss): [#7] [#1689] add X", false},
		{"stacked-tag chore commit that is not the placeholder", "chore: [#7] [#42] something else entirely", false},
		// `[skip ci]` is not a tag (the shape needs `#` + digits), so the
		// repeat group can never consume it and leave a bare "chore: create
		// pull request" that would fail the equality check.
		{"bracketed non-tag is not stripped", "chore: [skip ci] [#42] create pull request", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsDraftPRPlaceholderSubject(tt.subject); got != tt.want {
				t.Errorf("IsDraftPRPlaceholderSubject(%q) = %v, want %v", tt.subject, got, tt.want)
			}
		})
	}
}

// TestCreate_PopulatesPhaseDurations pins BOS-536: a fresh Create (no setup
// script) must populate the per-phase timing fields on CreateResult so
// worktree_duration becomes attributable. SetupScriptDuration stays zero when
// no setup script is configured, and the sum of the timed phases must never
// exceed the wall-clock time actually spent in Create — there is un-timed
// glue (MkdirAll, branchExists, clearStaleWorktree, ensureGitInfoExclude,
// verifyCurrentBranch) between the phases.
func TestCreate_PopulatesPhaseDurations(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	mgr := NewManager(zerolog.Nop())

	start := time.Now()
	result, err := mgr.Create(context.Background(), CreateOpts{
		RepoPath:        repoDir,
		BaseBranch:      "main",
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		Title:           "Test session",
	})
	wallClock := time.Since(start)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if result.FetchDuration <= 0 {
		t.Errorf("FetchDuration = %v, want > 0", result.FetchDuration)
	}
	if result.BranchProbeDuration <= 0 {
		t.Errorf("BranchProbeDuration = %v, want > 0", result.BranchProbeDuration)
	}
	if result.WorktreeAddDuration <= 0 {
		t.Errorf("WorktreeAddDuration = %v, want > 0", result.WorktreeAddDuration)
	}
	if result.SetupScriptDuration != 0 {
		t.Errorf("SetupScriptDuration = %v, want 0 (no setup script configured)", result.SetupScriptDuration)
	}

	sum := result.FetchDuration + result.BranchProbeDuration + result.WorktreeAddDuration + result.SetupScriptDuration
	if sum > wallClock {
		t.Errorf("phase duration sum %v exceeds wall-clock aggregate %v", sum, wallClock)
	}
}

// TestCreate_SetupScriptDurationPositiveWhenScriptRuns pins that
// SetupScriptDuration reflects real elapsed time when a setup script is
// configured, using a deterministic sleep so the measured duration is
// reliably nonzero without flaking on fast machines.
func TestCreate_SetupScriptDurationPositiveWhenScriptRuns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell assumed")
	}
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	mgr := NewManager(zerolog.Nop())

	// Derive the script from the bound below so the two cannot drift apart.
	const sleepFor = 50 * time.Millisecond
	script := fmt.Sprintf("sleep %v", sleepFor.Seconds())
	start := time.Now()
	result, err := mgr.Create(context.Background(), CreateOpts{
		RepoPath:        repoDir,
		BaseBranch:      "main",
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		Title:           "Test session",
		SetupScript:     &script,
	})
	wallClock := time.Since(start)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A script that failed to parse or exited non-zero would still record a
	// positive duration, so assert the run actually succeeded before reading
	// the timing — otherwise this test greens while measuring a broken script.
	if result.SetupErr != nil {
		t.Fatalf("SetupErr = %v, want nil (the sleep script must actually run)", result.SetupErr)
	}
	// Bound against the sleep's own floor rather than zero: shell startup
	// alone would satisfy "> 0" even if the sleep never happened.
	if result.SetupScriptDuration < sleepFor {
		t.Errorf("SetupScriptDuration = %v, want >= %v", result.SetupScriptDuration, sleepFor)
	}

	sum := result.FetchDuration + result.BranchProbeDuration + result.WorktreeAddDuration + result.SetupScriptDuration
	if sum > wallClock {
		t.Errorf("phase duration sum %v exceeds wall-clock aggregate %v", sum, wallClock)
	}
}

// TestCreate_ForceSkipsBranchProbeDuration pins the documented zero semantics
// of BranchProbeDuration on the Create path: CreateOpts.Force skips
// availableNewBranchName entirely, so the probe duration must stay zero while
// the other phases are still attributed.
func TestCreate_ForceSkipsBranchProbeDuration(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	mgr := NewManager(zerolog.Nop())

	result, err := mgr.Create(context.Background(), CreateOpts{
		RepoPath:        repoDir,
		BaseBranch:      "main",
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		Title:           "Test session",
		Force:           true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if result.BranchProbeDuration != 0 {
		t.Errorf("BranchProbeDuration = %v, want 0 (probe skipped under Force)", result.BranchProbeDuration)
	}
	if result.FetchDuration <= 0 {
		t.Errorf("FetchDuration = %v, want > 0", result.FetchDuration)
	}
	if result.WorktreeAddDuration <= 0 {
		t.Errorf("WorktreeAddDuration = %v, want > 0", result.WorktreeAddDuration)
	}
}

// TestCreateFromExistingBranch_PopulatesPhaseDurations pins BOS-536 for the
// existing-branch path: FetchDuration and WorktreeAddDuration must be
// populated, BranchProbeDuration must stay zero (there is no branch-name
// collision probe on this path — see the CreateFromExistingBranch doc
// comment), and the phase sum must never exceed the wall-clock aggregate.
func TestCreateFromExistingBranch_PopulatesPhaseDurations(t *testing.T) {
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

	start := time.Now()
	result, err := mgr.CreateFromExistingBranch(context.Background(), CreateFromExistingBranchOpts{
		RepoPath:        repoDir,
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		BranchName:      "fix-camera-crash",
	})
	wallClock := time.Since(start)
	if err != nil {
		t.Fatalf("CreateFromExistingBranch: %v", err)
	}

	if result.FetchDuration <= 0 {
		t.Errorf("FetchDuration = %v, want > 0", result.FetchDuration)
	}
	if result.WorktreeAddDuration <= 0 {
		t.Errorf("WorktreeAddDuration = %v, want > 0", result.WorktreeAddDuration)
	}
	if result.BranchProbeDuration != 0 {
		t.Errorf("BranchProbeDuration = %v, want 0 (no collision probe on this path)", result.BranchProbeDuration)
	}

	sum := result.FetchDuration + result.BranchProbeDuration + result.WorktreeAddDuration + result.SetupScriptDuration
	if sum > wallClock {
		t.Errorf("phase duration sum %v exceeds wall-clock aggregate %v", sum, wallClock)
	}
}

// verifyFixture is the state createDraftPR reaches just before it calls
// VerifyPushedBranchAheadOfBase.
type verifyFixture struct {
	worktreePath string
	branch       string
	headSHA      string
}

// newVerifyFixture reproduces createDraftPR's prelude in production shape: a
// linked worktree created off the base (which fetches it), the placeholder
// commit, and Push. Running the verification from the linked worktree — not
// the main clone — is what makes the shared-common-dir claim load-bearing.
func newVerifyFixture(t *testing.T, mgr *Manager, repo string) verifyFixture {
	t.Helper()
	ctx := context.Background()

	result, err := mgr.Create(ctx, CreateOpts{
		RepoPath:        repo,
		BaseBranch:      "main",
		WorktreeBaseDir: filepath.Join(t.TempDir(), "worktrees"),
		RepoName:        "my-repo",
		Title:           "Verify fixture",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.EmptyCommit(ctx, result.WorktreePath, "chore: [skip ci] create pull request"); err != nil {
		t.Fatalf("EmptyCommit: %v", err)
	}
	if err := mgr.Push(ctx, result.WorktreePath, result.BranchName); err != nil {
		t.Fatalf("Push: %v", err)
	}

	headSHA, err := runGit(ctx, result.WorktreePath, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return verifyFixture{worktreePath: result.WorktreePath, branch: result.BranchName, headSHA: headSHA}
}

// TestVerifyPushedBranchAheadOfBase_SkipFetchUsesExistingRefs proves the
// skip-fetch option really skips the fetch: origin/main has advanced upstream,
// so a BaseSHA still pointing at the stale remote-tracking ref can only mean no
// fetch ran. All three SHAs must still resolve from the refs already present.
func TestVerifyPushedBranchAheadOfBase_SkipFetchUsesExistingRefs(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	fixture := newVerifyFixture(t, mgr, repo)
	staleBaseSHA, err := runGit(ctx, fixture.worktreePath, "rev-parse", "origin/main")
	if err != nil {
		t.Fatalf("rev-parse origin/main: %v", err)
	}
	commitOnOrigin(t, repo, "main")

	verification, err := mgr.VerifyPushedBranchAheadOfBase(ctx, fixture.worktreePath, fixture.branch, "main", VerifyPushedBranchAheadOfBaseOpts{SkipFetch: true})
	if err != nil {
		t.Fatalf("VerifyPushedBranchAheadOfBase: %v", err)
	}
	if verification.BaseSHA != staleBaseSHA {
		t.Errorf("BaseSHA = %s, want the un-refreshed ref %s — a fetch ran despite SkipFetch", verification.BaseSHA, staleBaseSHA)
	}
	if verification.HeadSHA != fixture.headSHA {
		t.Errorf("HeadSHA = %s, want %s", verification.HeadSHA, fixture.headSHA)
	}
	if verification.RemoteHeadSHA != fixture.headSHA {
		t.Errorf("RemoteHeadSHA = %s, want %s", verification.RemoteHeadSHA, fixture.headSHA)
	}
	if verification.AheadCount != 1 {
		t.Errorf("AheadCount = %d, want 1", verification.AheadCount)
	}
}

// TestVerifyPushedBranchAheadOfBase_DefaultFetchesBase pins the zero value to
// today's behavior, so callers that have not just fetched are unaffected.
func TestVerifyPushedBranchAheadOfBase_DefaultFetchesBase(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	fixture := newVerifyFixture(t, mgr, repo)
	freshBaseSHA := commitOnOrigin(t, repo, "main")

	verification, err := mgr.VerifyPushedBranchAheadOfBase(ctx, fixture.worktreePath, fixture.branch, "main", VerifyPushedBranchAheadOfBaseOpts{})
	if err != nil {
		t.Fatalf("VerifyPushedBranchAheadOfBase: %v", err)
	}
	if verification.BaseSHA != freshBaseSHA {
		t.Errorf("BaseSHA = %s, want the freshly fetched upstream tip %s", verification.BaseSHA, freshBaseSHA)
	}
}

// TestVerifyPushedBranchAheadOfBase_SkipFetchStillDetectsRemoteHeadMismatch
// proves skipping the base fetch does not weaken the head-vs-remote check: the
// branch ref is fetch-independent (push maintains it), so a local commit made
// after the push must still be caught.
func TestVerifyPushedBranchAheadOfBase_SkipFetchStillDetectsRemoteHeadMismatch(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	fixture := newVerifyFixture(t, mgr, repo)
	pushedSHA := fixture.headSHA
	localSHA := makeLocalCommit(t, fixture.worktreePath, "feat: unpushed work")

	verification, err := mgr.VerifyPushedBranchAheadOfBase(ctx, fixture.worktreePath, fixture.branch, "main", VerifyPushedBranchAheadOfBaseOpts{SkipFetch: true})
	if err == nil {
		t.Fatal("expected a remote head mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "remote head mismatch") {
		t.Fatalf("error = %v, want a remote head mismatch", err)
	}
	if verification == nil {
		t.Fatal("expected the verification to be returned alongside the mismatch error")
	}
	if verification.HeadSHA != localSHA {
		t.Errorf("HeadSHA = %s, want %s", verification.HeadSHA, localSHA)
	}
	if verification.RemoteHeadSHA != pushedSHA {
		t.Errorf("RemoteHeadSHA = %s, want %s", verification.RemoteHeadSHA, pushedSHA)
	}
}

// TestVerifyPushedBranchAheadOfBase_SkipFetchWithoutBaseRefErrors pins the
// failure mode createDraftPR's skip-site comment promises for a caller that
// skips the fetch without one having happened: origin/<base> does not resolve,
// so the verification errors instead of silently passing. Unlike the
// remote-head-mismatch and ahead-count errors, which return a populated
// *BranchVerification alongside theirs, this one fails before the struct is
// built and hands back nil.
func TestVerifyPushedBranchAheadOfBase_SkipFetchWithoutBaseRefErrors(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	fixture := newVerifyFixture(t, mgr, repo)
	if _, err := runGit(ctx, fixture.worktreePath, "update-ref", "-d", "refs/remotes/origin/main"); err != nil {
		t.Fatalf("update-ref -d refs/remotes/origin/main: %v", err)
	}

	verification, err := mgr.VerifyPushedBranchAheadOfBase(ctx, fixture.worktreePath, fixture.branch, "main", VerifyPushedBranchAheadOfBaseOpts{SkipFetch: true})
	if err == nil {
		t.Fatal("expected an unresolvable base error, got nil")
	}
	if !strings.Contains(err.Error(), "resolve origin/main") {
		t.Fatalf("error = %v, want a resolve origin/main failure", err)
	}
	if verification != nil {
		t.Errorf("verification = %+v, want nil when the base ref cannot be resolved", verification)
	}
}
