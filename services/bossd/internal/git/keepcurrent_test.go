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

// keepCurrentFixture builds a repo with a bare origin, a `main` base branch and
// a feature branch that has fallen `behind` commits behind origin/main. The
// feature branch is checked out and pushed, mirroring a live session worktree.
type keepCurrentFixture struct {
	dir    string
	branch string
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// newKeepCurrentFixture creates the session-like repo. When conflicting is true
// the base commit and the feature commit both touch the same file with
// different content, so a rebase must conflict.
func newKeepCurrentFixture(t *testing.T, conflicting bool) keepCurrentFixture {
	t.Helper()
	dir := initTestRepo(t)

	// Feature branch with one commit.
	mustGit(t, dir, "checkout", "-b", "feature")
	if conflicting {
		writeFile(t, dir, "shared.txt", "feature side\n")
		mustGit(t, dir, "add", "shared.txt")
	} else {
		writeFile(t, dir, "feature.txt", "feature\n")
		mustGit(t, dir, "add", "feature.txt")
	}
	mustGit(t, dir, "commit", "-m", "feat: feature work")
	mustGit(t, dir, "push", "-u", "origin", "feature")

	// Advance main by one commit and push, so feature is 1 behind origin/main.
	mustGit(t, dir, "checkout", "main")
	if conflicting {
		writeFile(t, dir, "shared.txt", "base side\n")
		mustGit(t, dir, "add", "shared.txt")
	} else {
		writeFile(t, dir, "base.txt", "base\n")
		mustGit(t, dir, "add", "base.txt")
	}
	mustGit(t, dir, "commit", "-m", "feat: base work")
	mustGit(t, dir, "push", "origin", "main")

	// Back to the feature branch, as a session worktree would be.
	mustGit(t, dir, "checkout", "feature")
	return keepCurrentFixture{dir: dir, branch: "feature"}
}

// TestCountBehindBase_ReportsBehindCount verifies the behind-count primitive
// reports how many commits origin/<base> has that the branch lacks, and returns
// 0 once the branch has been rebased onto that tip.
func TestCountBehindBase_ReportsBehindCount(t *testing.T) {
	fx := newKeepCurrentFixture(t, false)
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	behind, err := mgr.CountBehindBase(ctx, fx.dir, fx.branch, "main")
	if err != nil {
		t.Fatalf("CountBehindBase: %v", err)
	}
	if behind != 1 {
		t.Fatalf("behind = %d, want 1", behind)
	}

	mustGit(t, fx.dir, "rebase", "refs/remotes/origin/main")

	behind, err = mgr.CountBehindBase(ctx, fx.dir, fx.branch, "main")
	if err != nil {
		t.Fatalf("CountBehindBase after rebase: %v", err)
	}
	if behind != 0 {
		t.Fatalf("behind after rebase = %d, want 0", behind)
	}
}

// TestRebaseOntoBase_Success verifies the happy path: the branch is replayed on
// top of origin/<base>, the pre-rebase tip is reported for use as the
// force-with-lease anchor, and no merge commit is introduced.
func TestRebaseOntoBase_Success(t *testing.T) {
	fx := newKeepCurrentFixture(t, false)
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	priorTip := mustGit(t, fx.dir, "rev-parse", "HEAD")
	baseTip := mustGit(t, fx.dir, "rev-parse", "refs/remotes/origin/main")

	res, err := mgr.RebaseOntoBase(ctx, fx.dir, fx.branch, "main")
	if err != nil {
		t.Fatalf("RebaseOntoBase: %v", err)
	}
	if res.PriorHead != priorTip {
		t.Errorf("PriorHead = %s, want %s", res.PriorHead, priorTip)
	}
	if res.NewHead == priorTip {
		t.Error("NewHead unchanged after rebase, want a replayed commit")
	}
	if got := mustGit(t, fx.dir, "rev-parse", "HEAD"); got != res.NewHead {
		t.Errorf("worktree HEAD = %s, want reported NewHead %s", got, res.NewHead)
	}
	// The base tip must now be an ancestor of the branch.
	if err := exec.Command("git", "-C", fx.dir, "merge-base", "--is-ancestor", baseTip, "HEAD").Run(); err != nil {
		t.Errorf("origin/main tip %s is not an ancestor of the rebased branch", baseTip)
	}
	// Rebase, never merge: no merge commits may appear.
	if merges := mustGit(t, fx.dir, "rev-list", "--merges", "--count", "HEAD"); merges != "0" {
		t.Errorf("merge commit count = %s, want 0 (rebase must never merge)", merges)
	}
	if got := mustGit(t, fx.dir, "status", "--porcelain"); got != "" {
		t.Errorf("worktree dirty after rebase: %q", got)
	}
}

// TestRebaseOntoBase_ConflictAbortsAndRestores verifies the conflict path:
// ErrRebaseConflict is returned, the rebase is aborted, the branch tip and
// worktree are restored exactly, and no rebase state directory is left behind.
func TestRebaseOntoBase_ConflictAbortsAndRestores(t *testing.T) {
	fx := newKeepCurrentFixture(t, true)
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	priorTip := mustGit(t, fx.dir, "rev-parse", "HEAD")

	res, err := mgr.RebaseOntoBase(ctx, fx.dir, fx.branch, "main")
	if err == nil {
		t.Fatalf("RebaseOntoBase succeeded on a conflicting rebase, want ErrRebaseConflict (res=%+v)", res)
	}
	if !errors.Is(err, ErrRebaseConflict) {
		t.Fatalf("error = %v, want ErrRebaseConflict", err)
	}
	if got := mustGit(t, fx.dir, "rev-parse", "HEAD"); got != priorTip {
		t.Errorf("HEAD = %s after aborted rebase, want restored %s", got, priorTip)
	}
	if got := mustGit(t, fx.dir, "rev-parse", "--abbrev-ref", "HEAD"); got != fx.branch {
		t.Errorf("checked-out branch = %s, want %s", got, fx.branch)
	}
	if got := mustGit(t, fx.dir, "status", "--porcelain"); got != "" {
		t.Errorf("worktree dirty after aborted rebase: %q", got)
	}
	// No half-rebased residue: neither .git/rebase-merge nor .git/rebase-apply.
	for _, name := range []string{"rebase-merge", "rebase-apply"} {
		p := mustGit(t, fx.dir, "rev-parse", "--git-path", name)
		if !filepath.IsAbs(p) {
			p = filepath.Join(fx.dir, p)
		}
		if _, statErr := os.Stat(p); statErr == nil {
			t.Errorf("rebase residue left behind at %s", p)
		}
	}
}

// TestRebaseOntoBase_RejectsDirtyWorktree verifies the primitive refuses to
// rebase over uncommitted work rather than letting git stash or clobber it.
func TestRebaseOntoBase_RejectsDirtyWorktree(t *testing.T) {
	fx := newKeepCurrentFixture(t, false)
	writeFile(t, fx.dir, "scratch.txt", "uncommitted\n")
	mgr := NewManager(zerolog.Nop())

	if _, err := mgr.RebaseOntoBase(context.Background(), fx.dir, fx.branch, "main"); err == nil {
		t.Fatal("RebaseOntoBase succeeded on a dirty worktree, want an error")
	}
}

// TestRebaseOntoBaseAndPush_Success verifies the full git-level success path the
// keep-current sweep drives: rebase, then force-push with the pre-rebase tip as
// the lease anchor, leaving origin/<branch> at the rebased tip.
func TestRebaseOntoBaseAndPush_Success(t *testing.T) {
	fx := newKeepCurrentFixture(t, false)
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	res, err := mgr.RebaseOntoBaseAndPush(ctx, fx.dir, fx.branch, "main")
	if err != nil {
		t.Fatalf("RebaseOntoBaseAndPush: %v", err)
	}
	if got := mustGit(t, fx.dir, "rev-parse", "HEAD"); got != res.NewHead {
		t.Errorf("worktree HEAD = %s, want %s", got, res.NewHead)
	}
	mustGit(t, fx.dir, "fetch", "origin", fx.branch)
	if got := mustGit(t, fx.dir, "rev-parse", "refs/remotes/origin/"+fx.branch); got != res.NewHead {
		t.Errorf("origin/%s = %s, want %s", fx.branch, got, res.NewHead)
	}
	if merges := mustGit(t, fx.dir, "rev-list", "--merges", "--count", "HEAD"); merges != "0" {
		t.Errorf("merge commit count = %s, want 0 (rebase must never merge)", merges)
	}
}

// TestRebaseOntoBaseAndPush_RestoresWhenPushRejected verifies that a rebase
// which lands locally but cannot be pushed is unwound. Leaving the branch
// rebased-but-unpushed would wedge it permanently: the next sweep would anchor
// its lease on a tip origin has never seen, so every future push would be
// rejected too.
func TestRebaseOntoBaseAndPush_RestoresWhenPushRejected(t *testing.T) {
	fx := newKeepCurrentFixture(t, false)
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	// The local tip must already equal origin/<branch> (the fixture pushed it),
	// or the up-front verifyBranchMatchesOrigin precondition would refuse before
	// the rebase and this test would never reach the push it exists to cover.
	mustGit(t, fx.dir, "fetch", "origin", fx.branch)
	if local, remote := mustGit(t, fx.dir, "rev-parse", "refs/heads/"+fx.branch),
		mustGit(t, fx.dir, "rev-parse", "refs/remotes/origin/"+fx.branch); local != remote {
		t.Fatalf("fixture precondition: %s = %s but origin/%s = %s", fx.branch, local, fx.branch, remote)
	}

	// Make the push itself fail, after the rebase has landed locally.
	writeHook(t, fx.dir, "pre-push", "#!/bin/sh\nexit 1\n")

	priorTip := mustGit(t, fx.dir, "rev-parse", "HEAD")

	res, err := mgr.RebaseOntoBaseAndPush(ctx, fx.dir, fx.branch, "main")
	if err == nil {
		t.Fatalf("RebaseOntoBaseAndPush succeeded with a refusing pre-push hook (res=%+v)", res)
	}
	// A bare err != nil would pass vacuously if the up-front
	// verifyBranchMatchesOrigin precondition refused before the rebase ever
	// ran — the exact way an earlier version of this test covered nothing.
	// Reaching the push is the whole point, so pin that it did.
	if errors.Is(err, ErrBranchNotPushed) {
		t.Fatalf("error = %v (ErrBranchNotPushed): refused up front, never reached the push", err)
	}
	if got := mustGit(t, fx.dir, "rev-parse", "HEAD"); got != priorTip {
		t.Errorf("HEAD = %s after rejected push, want restored %s", got, priorTip)
	}
	if got := mustGit(t, fx.dir, "status", "--porcelain"); got != "" {
		t.Errorf("worktree dirty after rejected push: %q", got)
	}
	for _, p := range rebaseStateDirs(t, fx.dir) {
		if _, statErr := os.Stat(p); statErr == nil {
			t.Errorf("rebase residue left behind at %s after a rejected push", p)
		}
	}
}

// writeHook installs an executable git hook in a fixture worktree.
func writeHook(t *testing.T, dir, name, body string) {
	t.Helper()
	hookDir := mustGit(t, dir, "rev-parse", "--git-path", "hooks")
	if !filepath.IsAbs(hookDir) {
		hookDir = filepath.Join(dir, hookDir)
	}
	if err := os.MkdirAll(hookDir, 0o750); err != nil {
		t.Fatalf("mkdir hooks: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hookDir, name), []byte(body), 0o700); err != nil {
		t.Fatalf("write %s hook: %v", name, err)
	}
}

// rebaseStateDirs returns the resolved rebase-merge / rebase-apply paths for a
// worktree, so tests can assert git left no half-rebased residue behind.
func rebaseStateDirs(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	for _, name := range []string{"rebase-merge", "rebase-apply"} {
		p := mustGit(t, dir, "rev-parse", "--git-path", name)
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		out = append(out, p)
	}
	return out
}

// TestRestoreAfterFailedRebase_SurvivesCancelledContext pins the unwind's
// independence from the caller's context. The most likely reason a rebase fails
// is that the sweep's budget expired mid-rebase; if the recovery commands ran on
// that same cancelled context they would all be killed instantly, leaving the
// worktree half-rebased with rebase state on disk — the one outcome the unwind
// exists to prevent.
func TestRestoreAfterFailedRebase_SurvivesCancelledContext(t *testing.T) {
	fx := newKeepCurrentFixture(t, true)
	mgr := NewManager(zerolog.Nop())

	priorTip := mustGit(t, fx.dir, "rev-parse", "HEAD")

	// Drive git into a genuinely conflicted, half-rebased state.
	if err := exec.Command("git", "-C", fx.dir, "rebase", "refs/remotes/origin/main").Run(); err == nil {
		t.Fatal("fixture rebase succeeded, want a conflict to leave rebase state behind")
	}
	if mustGit(t, fx.dir, "rev-parse", "HEAD") == priorTip {
		t.Fatal("fixture did not move HEAD; nothing to restore")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the caller's budget is already gone when the unwind starts

	mgr.restoreAfterFailedRebase(ctx, fx.dir, fx.branch, priorTip)

	if got := mustGit(t, fx.dir, "rev-parse", "HEAD"); got != priorTip {
		t.Errorf("HEAD = %s after unwind on a cancelled ctx, want restored %s", got, priorTip)
	}
	if got := mustGit(t, fx.dir, "status", "--porcelain"); got != "" {
		t.Errorf("worktree dirty after unwind on a cancelled ctx: %q", got)
	}
	for _, p := range rebaseStateDirs(t, fx.dir) {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("rebase residue left behind at %s after unwind on a cancelled ctx", p)
		}
	}
}

// TestForceRestoreTip_RefusesToDestroyUncommittedWork pins the TOCTOU guard: the
// clean-worktree precondition was checked before the rebase, so uncommitted work
// found at unwind time was written since and a hard reset would destroy it
// irrecoverably. Refusing leaves a branch an operator can fix; resetting loses
// the work.
//
// The dirt here is a MODIFIED TRACKED file, which is exactly what
// `reset --hard` reverts — an untracked file would prove nothing, since a hard
// reset never touches those.
func TestForceRestoreTip_RefusesToDestroyUncommittedWork(t *testing.T) {
	fx := newKeepCurrentFixture(t, false)
	mgr := NewManager(zerolog.Nop())

	priorTip := mustGit(t, fx.dir, "rev-parse", "HEAD")

	// The tip has moved on since priorTip, as it would after a rebase...
	writeFile(t, fx.dir, "agent.txt", "committed content\n")
	mustGit(t, fx.dir, "add", "agent.txt")
	mustGit(t, fx.dir, "commit", "-m", "feat: later work")
	movedTip := mustGit(t, fx.dir, "rev-parse", "HEAD")

	// ...and an agent has modified a TRACKED file without committing it.
	writeFile(t, fx.dir, "agent.txt", "precious uncommitted work\n")

	mgr.forceRestoreTip(context.Background(), fx.dir, fx.branch, priorTip, "test")

	if got := mustGit(t, fx.dir, "rev-parse", "HEAD"); got != movedTip {
		t.Errorf("HEAD = %s, want %s: a dirty worktree must not be hard-reset", got, movedTip)
	}
	body, err := os.ReadFile(filepath.Join(fx.dir, "agent.txt"))
	if err != nil {
		t.Fatalf("read tracked file after unwind: %v", err)
	}
	if string(body) != "precious uncommitted work\n" {
		t.Errorf("tracked file = %q, want the uncommitted modification untouched", string(body))
	}
}

// TestForceRestoreTip_UntrackedFilesDoNotBlockRestore pins the other half of the
// guard: `reset --hard` never deletes untracked files, so treating them as
// "dirty" would make the unwind refuse over files it was never going to touch —
// leaving the branch rebased-but-unpushed for no gain.
func TestForceRestoreTip_UntrackedFilesDoNotBlockRestore(t *testing.T) {
	fx := newKeepCurrentFixture(t, false)
	mgr := NewManager(zerolog.Nop())

	priorTip := mustGit(t, fx.dir, "rev-parse", "HEAD")
	writeFile(t, fx.dir, "later.txt", "committed later\n")
	mustGit(t, fx.dir, "add", "later.txt")
	mustGit(t, fx.dir, "commit", "-m", "feat: later work")

	// Untracked scratch: not part of any commit, and immune to reset --hard.
	writeFile(t, fx.dir, "scratch.log", "untracked scratch\n")

	mgr.forceRestoreTip(context.Background(), fx.dir, fx.branch, priorTip, "test")

	if got := mustGit(t, fx.dir, "rev-parse", "HEAD"); got != priorTip {
		t.Errorf("HEAD = %s, want restored %s: untracked files must not block the unwind", got, priorTip)
	}
	body, err := os.ReadFile(filepath.Join(fx.dir, "scratch.log"))
	if err != nil {
		t.Fatalf("untracked file removed by the unwind: %v", err)
	}
	if string(body) != "untracked scratch\n" {
		t.Errorf("untracked file = %q, want it untouched", string(body))
	}
}

// TestForceRestoreTip_ClearsConflictStoppedRebase is the regression test for the
// path this function exists to unwind. A conflict-stopped rebase ALWAYS leaves
// unmerged index entries and conflict markers on disk — here they are reported
// as AA (both sides added, since the conflicted path postdates the merge base),
// but the code they exercise is the same — so a "refuse when dirty" guard would
// fire on every single conflicted rebase and leave the worktree detached
// mid-rebase, the permanent wedge.
func TestForceRestoreTip_ClearsConflictStoppedRebase(t *testing.T) {
	fx := newKeepCurrentFixture(t, true)
	mgr := NewManager(zerolog.Nop())

	priorTip := mustGit(t, fx.dir, "rev-parse", "HEAD")

	// Drive git into a genuinely conflict-stopped rebase.
	if err := exec.Command("git", "-C", fx.dir, "rebase", "refs/remotes/origin/main").Run(); err == nil {
		t.Fatal("fixture rebase succeeded, want a conflict to stop it")
	}
	if !rebaseInProgress(context.Background(), fx.dir) {
		t.Fatal("fixture left no rebase state behind; nothing to unwind")
	}
	// Unmerged index entries plus a dirty status: exactly the debris a
	// "refuse when dirty" guard would trip over. (git spells the porcelain
	// code AA or UU depending on whether the conflicted path predates the
	// merge base; `ls-files --unmerged` covers both.)
	if unmerged := mustGit(t, fx.dir, "ls-files", "--unmerged"); unmerged == "" {
		t.Fatal("fixture left no unmerged index entries; not a conflict-stopped rebase")
	}
	if dirty := mustGit(t, fx.dir, "status", "--porcelain"); dirty == "" {
		t.Fatal("fixture status is clean; the guard being regressed would not fire")
	}

	mgr.forceRestoreTip(context.Background(), fx.dir, fx.branch, priorTip, "test")

	if rebaseInProgress(context.Background(), fx.dir) {
		t.Error("rebase still in progress after the unwind")
	}
	if got := mustGit(t, fx.dir, "rev-parse", "HEAD"); got != priorTip {
		t.Errorf("HEAD = %s after the unwind, want restored %s", got, priorTip)
	}
	if got := mustGit(t, fx.dir, "rev-parse", "--abbrev-ref", "HEAD"); got != fx.branch {
		t.Errorf("checked-out branch = %s, want %s (must not be left detached)", got, fx.branch)
	}
	if got := mustGit(t, fx.dir, "status", "--porcelain"); got != "" {
		t.Errorf("worktree dirty after the unwind: %q", got)
	}
	for _, p := range rebaseStateDirs(t, fx.dir) {
		if _, statErr := os.Stat(p); statErr == nil {
			t.Errorf("rebase residue left behind at %s", p)
		}
	}
}

// TestForceRestoreTip_ReattachesBranchWhenAbortFails covers the fallback half of
// the rebase-in-progress unwind, which `rebase --abort` succeeding normally
// hides. `git rebase --quit` deliberately does NOT re-attach HEAD: it leaves it
// DETACHED at the partially replayed commit. A plain `reset --hard priorHead`
// there moves the detached HEAD and never touches refs/heads/<branch>, so
// `git status -sb` reports `## HEAD (no branch)`, the session agent's next
// commit never reaches the branch, and nothing in bossd re-attaches it — the
// exact wedge this function exists to prevent.
//
// The abort is made to fail by removing the rebase state's `orig-head`, which
// is the ref `--abort` reads to know where to go back. (Holding `.git/index.lock`
// also fails the abort, but it fails every later recovery command too — the
// quit, the reset, the checkout — so the fallback could never run and the test
// would assert nothing.)
func TestForceRestoreTip_ReattachesBranchWhenAbortFails(t *testing.T) {
	fx := newKeepCurrentFixture(t, true)
	mgr := NewManager(zerolog.Nop())

	priorTip := mustGit(t, fx.dir, "rev-parse", "HEAD")

	// Drive git into a genuinely conflict-stopped rebase: HEAD is detached at
	// the replay point and rebase state is on disk.
	if err := exec.Command("git", "-C", fx.dir, "rebase", "refs/remotes/origin/main").Run(); err == nil {
		t.Fatal("fixture rebase succeeded, want a conflict to stop it")
	}
	stateDir := mustGit(t, fx.dir, "rev-parse", "--git-path", "rebase-merge")
	if !filepath.IsAbs(stateDir) {
		stateDir = filepath.Join(fx.dir, stateDir)
	}
	if err := os.Remove(filepath.Join(stateDir, "orig-head")); err != nil {
		t.Fatalf("remove rebase orig-head: %v", err)
	}
	// Precondition: the abort really is broken, and the state is still there.
	if err := exec.Command("git", "-C", fx.dir, "rebase", "--abort").Run(); err == nil {
		t.Fatal("fixture rebase --abort succeeded; the fallback path would not run")
	}
	if !rebaseInProgress(context.Background(), fx.dir) {
		t.Fatal("fixture left no rebase state behind; nothing to unwind")
	}

	mgr.forceRestoreTip(context.Background(), fx.dir, fx.branch, priorTip, "test")

	head, err := exec.Command("git", "-C", fx.dir, "symbolic-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("HEAD is detached after the unwind (symbolic-ref: %v); the branch was never re-attached", err)
	}
	if got := strings.TrimSpace(string(head)); got != "refs/heads/"+fx.branch {
		t.Errorf("HEAD = %s, want refs/heads/%s", got, fx.branch)
	}
	if got := mustGit(t, fx.dir, "rev-parse", "refs/heads/"+fx.branch); got != priorTip {
		t.Errorf("refs/heads/%s = %s, want restored %s", fx.branch, got, priorTip)
	}
	if rebaseInProgress(context.Background(), fx.dir) {
		t.Error("rebase still in progress after the unwind")
	}
	if got := mustGit(t, fx.dir, "status", "--porcelain"); got != "" {
		t.Errorf("worktree dirty after the unwind: %q", got)
	}
}

// TestRebaseInProgressOrUnknown_TreatsUnreadableProbeAsInProgress pins the
// reading the unwind paths depend on. The rebase-state probe shells out to git,
// so it can fail to answer — most plausibly because the unwind's own budget has
// expired. Reading an unanswerable probe as "no rebase in progress" routes a
// genuinely mid-rebase worktree into the refuse branch, which leaves it
// mid-rebase on a detached HEAD; the conservative reading takes the branch that
// restores unconditionally.
func TestRebaseInProgressOrUnknown_TreatsUnreadableProbeAsInProgress(t *testing.T) {
	fx := newKeepCurrentFixture(t, true)

	if err := exec.Command("git", "-C", fx.dir, "rebase", "refs/remotes/origin/main").Run(); err == nil {
		t.Fatal("fixture rebase succeeded, want a conflict to stop it")
	}
	if !rebaseInProgress(context.Background(), fx.dir) {
		t.Fatal("fixture left no rebase state behind")
	}

	// A budget that is already gone: every probe command is killed on start.
	expired, cancel := context.WithCancel(context.Background())
	cancel()

	if rebaseInProgress(expired, fx.dir) {
		t.Error("rebaseInProgress reported a positive on an unreadable probe; it must report only what it can confirm")
	}
	if !rebaseInProgressOrUnknown(expired, fx.dir) {
		t.Error("rebaseInProgressOrUnknown = false on an unreadable probe over a mid-rebase worktree, want true")
	}
}

// TestForceRestoreTip_RefusesWhenUntrackedFileCollidesWithPriorTree pins the
// other half of the untracked rule. `git reset --hard <target>` does NOT refuse
// over an untracked file whose path exists in the target tree (unlike
// `git checkout`) — it silently overwrites it and exits 0. That is reachable
// whenever the base branch deleted a file the session's agent then recreated
// untracked, so such a file is destroyable work and must block the unwind.
func TestForceRestoreTip_RefusesWhenUntrackedFileCollidesWithPriorTree(t *testing.T) {
	fx := newKeepCurrentFixture(t, false)
	mgr := NewManager(zerolog.Nop())

	// priorTip's tree contains doomed.txt...
	writeFile(t, fx.dir, "doomed.txt", "original content\n")
	mustGit(t, fx.dir, "add", "doomed.txt")
	mustGit(t, fx.dir, "commit", "-m", "feat: add doomed.txt")
	priorTip := mustGit(t, fx.dir, "rev-parse", "HEAD")

	// ...a later commit deletes it (as a merged base branch would)...
	mustGit(t, fx.dir, "rm", "-q", "doomed.txt")
	mustGit(t, fx.dir, "commit", "-m", "feat: delete doomed.txt")
	movedTip := mustGit(t, fx.dir, "rev-parse", "HEAD")

	// ...and the session's agent recreates it, untracked.
	writeFile(t, fx.dir, "doomed.txt", "precious agent work\n")
	if got := mustGit(t, fx.dir, "status", "--porcelain"); !strings.HasPrefix(got, "??") {
		t.Fatalf("fixture status = %q, want an untracked (??) entry", got)
	}

	mgr.forceRestoreTip(context.Background(), fx.dir, fx.branch, priorTip, "test")

	if got := mustGit(t, fx.dir, "rev-parse", "HEAD"); got != movedTip {
		t.Errorf("HEAD = %s, want %s: a colliding untracked file must block the restore", got, movedTip)
	}
	body, err := os.ReadFile(filepath.Join(fx.dir, "doomed.txt"))
	if err != nil {
		t.Fatalf("read untracked file after unwind: %v", err)
	}
	if string(body) != "precious agent work\n" {
		t.Errorf("untracked file = %q, want the agent's content untouched", string(body))
	}
}

// TestRebaseOntoBaseAndPush_DeletedRemoteBranchIsBenign pins the classification
// of the commonest fetch failure. FetchBase asks for one branch by name, so a
// branch deleted on the remote (or never pushed) fails the fetch with
// `couldn't find remote ref` — the benign "nothing to keep current" skip, not
// infrastructure breakage. Reporting it as a hard rebase failure would make the
// sweep shout on every merged-and-deleted branch it walks past.
func TestRebaseOntoBaseAndPush_DeletedRemoteBranchIsBenign(t *testing.T) {
	fx := newKeepCurrentFixture(t, false)
	mgr := NewManager(zerolog.Nop())

	// The branch existed on origin (the fixture pushed it) and is now gone.
	mustGit(t, fx.dir, "push", "origin", "--delete", fx.branch)

	priorTip := mustGit(t, fx.dir, "rev-parse", "HEAD")

	_, err := mgr.RebaseOntoBaseAndPush(context.Background(), fx.dir, fx.branch, "main")
	if err == nil {
		t.Fatal("RebaseOntoBaseAndPush proceeded with a branch deleted on the remote")
	}
	if !errors.Is(err, ErrBranchNotPushed) {
		t.Fatalf("error = %v, want ErrBranchNotPushed", err)
	}
	// Refused before any write.
	if got := mustGit(t, fx.dir, "rev-parse", "HEAD"); got != priorTip {
		t.Errorf("HEAD = %s, want untouched %s", got, priorTip)
	}
}

// TestVerifyBranchMatchesOrigin_BrokenRemoteIsNotBenign is the control for the
// test above: when the fetch fails for a reason OTHER than a missing remote
// branch — here a remote that does not resolve at all — the failure is real
// breakage and must NOT be filed under the benign skip, or a dead remote would
// look identical to a merged branch.
func TestVerifyBranchMatchesOrigin_BrokenRemoteIsNotBenign(t *testing.T) {
	fx := newKeepCurrentFixture(t, false)
	mgr := NewManager(zerolog.Nop())

	// Point origin at a path that does not exist: both the fetch and the
	// ls-remote probe fail, which is the "cannot tell, assume breakage" case.
	mustGit(t, fx.dir, "remote", "set-url", "origin", filepath.Join(fx.dir, "no-such-remote.git"))

	err := mgr.verifyBranchMatchesOrigin(context.Background(), fx.dir, fx.branch)
	if err == nil {
		t.Fatal("verifyBranchMatchesOrigin succeeded against a broken remote")
	}
	if errors.Is(err, ErrBranchNotPushed) {
		t.Errorf("error = %v, want a loud fetch failure, not the benign ErrBranchNotPushed skip", err)
	}
}

// TestForceRestoreTip_ResetsWhenClean is the control for the guard above: with a
// clean worktree the unwind still does its job and puts the tip back.
func TestForceRestoreTip_ResetsWhenClean(t *testing.T) {
	fx := newKeepCurrentFixture(t, false)
	mgr := NewManager(zerolog.Nop())

	priorTip := mustGit(t, fx.dir, "rev-parse", "HEAD")
	writeFile(t, fx.dir, "later.txt", "committed later\n")
	mustGit(t, fx.dir, "add", "later.txt")
	mustGit(t, fx.dir, "commit", "-m", "feat: later work")

	mgr.forceRestoreTip(context.Background(), fx.dir, fx.branch, priorTip, "test")

	if got := mustGit(t, fx.dir, "rev-parse", "HEAD"); got != priorTip {
		t.Errorf("HEAD = %s, want restored %s", got, priorTip)
	}
}

// TestRebaseOntoBase_NonConflictFailureIsNotAConflict verifies a rebase that
// fails for a reason other than a conflict (here: a refusing pre-rebase hook) is
// reported as ErrRebaseFailed, not ErrRebaseConflict. The distinction is load
// bearing: the sweep logs a conflict quietly as an expected outcome, so
// misclassifying a hook failure, a held index.lock or a cancelled context as a
// conflict would hide real breakage.
func TestRebaseOntoBase_NonConflictFailureIsNotAConflict(t *testing.T) {
	fx := newKeepCurrentFixture(t, false)
	mgr := NewManager(zerolog.Nop())

	hookDir := mustGit(t, fx.dir, "rev-parse", "--git-path", "hooks")
	if !filepath.IsAbs(hookDir) {
		hookDir = filepath.Join(fx.dir, hookDir)
	}
	if err := os.MkdirAll(hookDir, 0o750); err != nil {
		t.Fatalf("mkdir hooks: %v", err)
	}
	hook := filepath.Join(hookDir, "pre-rebase")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatalf("write pre-rebase hook: %v", err)
	}

	priorTip := mustGit(t, fx.dir, "rev-parse", "HEAD")

	_, err := mgr.RebaseOntoBase(context.Background(), fx.dir, fx.branch, "main")
	if err == nil {
		t.Fatal("RebaseOntoBase succeeded with a refusing pre-rebase hook")
	}
	if errors.Is(err, ErrRebaseConflict) {
		t.Errorf("hook failure classified as ErrRebaseConflict: %v", err)
	}
	if !errors.Is(err, ErrRebaseFailed) {
		t.Errorf("error = %v, want ErrRebaseFailed", err)
	}
	if got := mustGit(t, fx.dir, "rev-parse", "HEAD"); got != priorTip {
		t.Errorf("HEAD = %s after a failed rebase, want restored %s", got, priorTip)
	}
}

// TestRebaseOntoBaseAndPush_SkipsWhenLocalCommitsUnpushed verifies the sweep
// refuses BEFORE doing any work when the branch has commits that never reached
// origin. The push half anchors its --force-with-lease on the pre-rebase LOCAL
// tip while git evaluates the lease against the real remote ref, so such a push
// is certain to be rejected ("stale info") — rebasing first would just be work
// thrown away on every merge, forever.
func TestRebaseOntoBaseAndPush_SkipsWhenLocalCommitsUnpushed(t *testing.T) {
	fx := newKeepCurrentFixture(t, false)
	mgr := NewManager(zerolog.Nop())

	// A commit the session made but has not pushed yet.
	writeFile(t, fx.dir, "wip.txt", "unpushed work\n")
	mustGit(t, fx.dir, "add", "wip.txt")
	mustGit(t, fx.dir, "commit", "-m", "feat: unpushed work")
	priorTip := mustGit(t, fx.dir, "rev-parse", "HEAD")

	_, err := mgr.RebaseOntoBaseAndPush(context.Background(), fx.dir, fx.branch, "main")
	if err == nil {
		t.Fatal("RebaseOntoBaseAndPush proceeded with unpushed local commits")
	}
	if !errors.Is(err, ErrBranchNotPushed) {
		t.Fatalf("error = %v, want ErrBranchNotPushed", err)
	}
	// Refused before any write: the branch was not rebased at all.
	if got := mustGit(t, fx.dir, "rev-parse", "HEAD"); got != priorTip {
		t.Errorf("HEAD = %s, want untouched %s (must refuse before rebasing)", got, priorTip)
	}
	behind, err := mgr.CountBehindBase(context.Background(), fx.dir, fx.branch, "main")
	if err != nil {
		t.Fatalf("CountBehindBase: %v", err)
	}
	if behind == 0 {
		t.Error("branch is no longer behind the base; the rebase must not have run")
	}
}
