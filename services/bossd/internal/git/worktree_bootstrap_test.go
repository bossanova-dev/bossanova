package git

import (
	"context"
	"errors"
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

// TestCreateReportsTheSettledBranchBeforeTheSetupScript pins the BOS-717
// OnWorktreeReady contract that AC 2 depends on for title-derived branches.
//
// The branch a title-only create ends up on is decided INSIDE Create (sanitized
// title, plus a uniquifying suffix), so until Create returns, a caller that
// already inserted a session row cannot name what to clean up. The hook must
// therefore fire with the settled branch while the setup script — the step that
// hangs — is still ahead of it.
func TestCreateReportsTheSettledBranchBeforeTheSetupScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell assumed")
	}
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	mgr := NewManager(zerolog.Nop())

	// The setup script must actually EMIT, or the output sink below is never
	// called, setupRan stays false, and the ordering assertion cannot fail.
	setup := "echo running-setup"
	var (
		mu               sync.Mutex
		readyBranch      string
		readyPath        string
		readyBeforeSetup bool
		setupRan         bool
	)
	markSetupRan := func() {
		mu.Lock()
		setupRan = true
		mu.Unlock()
	}

	result, err := mgr.Create(context.Background(), CreateOpts{
		RepoPath:        repoDir,
		BaseBranch:      "main",
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		Title:           "Test session",
		SetupScript:     &setup,
		SetupScriptOutput: writerFunc(func(p []byte) (int, error) {
			// Setup output only exists once the script is running.
			markSetupRan()
			return len(p), nil
		}),
		OnWorktreeReady: func(_ context.Context, branch, path string) {
			mu.Lock()
			readyBranch, readyPath = branch, path
			readyBeforeSetup = !setupRan
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if readyBranch == "" {
		t.Fatal("OnWorktreeReady never fired; a title-derived branch is unnameable until Create returns")
	}
	if readyBranch != result.BranchName {
		t.Fatalf("OnWorktreeReady branch = %q, want the settled branch %q", readyBranch, result.BranchName)
	}
	if readyPath != result.WorktreePath {
		t.Fatalf("OnWorktreeReady path = %q, want %q", readyPath, result.WorktreePath)
	}
	if !setupRan {
		t.Fatal("the setup script produced no output; the ordering assertion below would be vacuous")
	}
	if !readyBeforeSetup {
		t.Fatal("OnWorktreeReady fired after the setup script; a hung script would beat it")
	}
}

// TestCreateDoesNotReportABranchItDidNotCreate guards the placement of the hook:
// it fires only once `git worktree add` has landed. A create refused because the
// branch already exists never owned that branch, so reporting it would point the
// caller's failure cleanup at somebody else's worktree.
func TestCreateDoesNotReportABranchItDidNotCreate(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	mgr := NewManager(zerolog.Nop())

	if _, err := runGit(context.Background(), repoDir, "branch", "taken"); err != nil {
		t.Fatalf("create pre-existing branch: %v", err)
	}

	fired := false
	_, err := mgr.Create(context.Background(), CreateOpts{
		RepoPath:        repoDir,
		BaseBranch:      "main",
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		Title:           "irrelevant",
		BranchName:      "taken",
		OnWorktreeReady: func(context.Context, string, string) { fired = true },
	})
	if err == nil {
		t.Fatal("Create error = nil, want ErrBranchExists")
	}
	if fired {
		t.Fatal("OnWorktreeReady fired for a branch this create did not make")
	}
}

// TestConcurrentCreatesInOneRepoAreSerialized is the BOS-717 companion to
// regression test A. Scoping the session start lock per repo is what lets two
// creates in ONE repo bootstrap at the same time — but both then drive the same
// clone, and `git fetch` is a ref write.
//
// Two same-titled concurrent creates are the sharpest probe: without the clone
// gate both read availableNewBranchName before either writes, settle on the same
// name, and the loser dies with ErrBranchExists instead of being handed the `-2`
// suffix the uniquifier exists for. Concurrent fetches of the same refspec can
// also collide on refs/remotes/origin/main.lock. Both creates must succeed, on
// distinct branches.
func TestConcurrentCreatesInOneRepoAreSerialized(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	mgr := NewManager(zerolog.Nop())

	const concurrency = 4
	type outcome struct {
		branch string
		err    error
	}
	results := make(chan outcome, concurrency)

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := mgr.Create(context.Background(), CreateOpts{
				RepoPath:        repoDir,
				BaseBranch:      "main",
				WorktreeBaseDir: wtBase,
				RepoName:        "my-repo",
				Title:           "Same title",
			})
			if err != nil {
				results <- outcome{err: err}
				return
			}
			results <- outcome{branch: res.BranchName}
		}()
	}
	close(start)

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		t.Fatal("concurrent creates did not finish; the clone gate is not bounded")
	}
	close(results)

	seen := map[string]bool{}
	for got := range results {
		if got.err != nil {
			t.Fatalf("concurrent create failed: %v (concurrent git against one clone is not serialized)", got.err)
		}
		if seen[got.branch] {
			t.Fatalf("two creates settled on the same branch %q", got.branch)
		}
		seen[got.branch] = true
	}
	if len(seen) != concurrency {
		t.Fatalf("distinct branches = %d, want %d", len(seen), concurrency)
	}
	// The uniquifier's suffixes are what the serialization buys.
	if !seen["same-title"] {
		t.Fatalf("branches = %v, want the unsuffixed name among them", seen)
	}
}

// TestCloneGateReleasesBeforeTheSetupScript proves the gate does NOT serialize
// the long step. A parallel epic runs many sessions in one repo, so holding the
// gate across a multi-minute setup script would trade one wedge for another.
func TestCloneGateReleasesBeforeTheSetupScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell assumed")
	}
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	mgr := NewManager(zerolog.Nop())

	// The first create's setup script parks until the test releases it, so the
	// first create is provably still inside its setup script while the second
	// runs. If the gate were held across that script, the second create could
	// not finish until the release below — and would time out here.
	releaseFile := filepath.Join(t.TempDir(), "release")
	script := "while [ ! -f " + releaseFile + " ]; do sleep 0.05; done"

	inSetup := make(chan struct{})
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		_, err := mgr.Create(context.Background(), CreateOpts{
			RepoPath:        repoDir,
			BaseBranch:      "main",
			WorktreeBaseDir: wtBase,
			RepoName:        "my-repo",
			Title:           "first",
			SetupScript:     &script,
			// Fires after `worktree add` and a beat before the setup script (a
			// git-ignore write and the gate release sit between), so this is
			// the signal that the parked script is about to run.
			OnWorktreeReady: func(context.Context, string, string) { close(inSetup) },
		})
		if err != nil {
			t.Errorf("first create: %v", err)
		}
	}()
	t.Cleanup(func() {
		if err := os.WriteFile(releaseFile, nil, 0o600); err != nil {
			t.Errorf("release the parked setup script: %v", err)
		}
		<-firstDone
	})

	select {
	case <-inSetup:
	case <-time.After(60 * time.Second):
		t.Fatal("first create never reached its setup script")
	}

	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		_, err := mgr.Create(context.Background(), CreateOpts{
			RepoPath:        repoDir,
			BaseBranch:      "main",
			WorktreeBaseDir: wtBase,
			RepoName:        "my-repo",
			Title:           "second",
		})
		if err != nil {
			t.Errorf("second create: %v", err)
		}
	}()

	select {
	case <-secondDone:
	case <-time.After(60 * time.Second):
		t.Fatal("second create never completed while the first sat in its setup script; the clone gate is held too long")
	}
}

// TestPurgeThenReapActuallyRemovesTheBranch is the real-git proof behind AC 2's
// "cleans up its worktree AND branch".
//
// `git branch -D` REFUSES a branch that is still checked out in a registered
// worktree, and ReapLocalBranches' `worktree prune` does not rescue it: prune
// only unregisters worktrees whose DIRECTORY is already gone, which is exactly
// not the case for a bootstrap that died after `worktree add`. Every caller
// therefore has to purge first and reap second.
//
// This has to be a real-git test. Every cleanup test at the server/session layer
// drives a mock WorktreeManager that records the call and returns nil, so it
// asserts that a reap was ATTEMPTED — which stays green while production is
// refused and leaks the branch.
func TestPurgeThenReapActuallyRemovesTheBranch(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	mgr := NewManager(zerolog.Nop())

	res, err := mgr.Create(context.Background(), CreateOpts{
		RepoPath:        repoDir,
		BaseBranch:      "main",
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		Title:           "doomed bootstrap",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The wrong order first, to prove the hazard is real rather than theoretical.
	if err := mgr.ReapLocalBranches(context.Background(), repoDir, []string{res.BranchName}); err == nil {
		t.Fatal("ReapLocalBranches succeeded with the worktree still registered; the ordering hazard this test guards is gone — simplify the callers")
	}
	if out := gitOutput(t, repoDir, "branch", "--list", res.BranchName); !strings.Contains(out, res.BranchName) {
		t.Fatalf("branch already gone after a refused reap: %q", out)
	}

	// The order every caller uses.
	if err := mgr.PurgeWorktree(context.Background(), repoDir, "my-repo", wtBase, res.BranchName); err != nil {
		t.Fatalf("PurgeWorktree: %v", err)
	}
	if err := mgr.ReapLocalBranches(context.Background(), repoDir, []string{res.BranchName}); err != nil {
		t.Fatalf("ReapLocalBranches after PurgeWorktree: %v", err)
	}

	if out := gitOutput(t, repoDir, "branch", "--list", res.BranchName); strings.TrimSpace(out) != "" {
		t.Fatalf("branch %q survived purge-then-reap: %q", res.BranchName, out)
	}
	if _, err := os.Stat(res.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree dir %q still present after purge (stat err %v)", res.WorktreePath, err)
	}
}

// TestRunGitAttributesACallerCancellationToTheCaller pins the error text: a
// command killed by the caller's context must not claim it exhausted its own
// per-invocation budget.
func TestRunGitAttributesACallerCancellationToTheCaller(t *testing.T) {
	repoDir := initTestRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := runGit(ctx, repoDir, "status")
	if err == nil {
		t.Fatal("runGit error = nil, want a cancellation")
	}
	if strings.Contains(err.Error(), "after "+GitCommandTimeout.String()) {
		t.Fatalf("error blames the per-invocation budget for a caller cancellation: %v", err)
	}
	if !strings.Contains(err.Error(), "caller's context ended") {
		t.Fatalf("error = %v, want it to name the caller's context", err)
	}
}

// TestPurgeAndReapSerializeWithoutDeadlockingAgainstACreate pins the BOS-717
// clone-gate contract for the destructive cleanup path.
//
// PurgeWorktree and ReapLocalBranches mutate the same shared clone a create is
// fetching and adding into, and the stranded-bootstrap reaper runs them from the
// daemon poller CONCURRENTLY with live creates by design — so they have to take
// the per-clone gate. That gate is capacity-1 and not reentrant, which is the
// hazard this test exists for: Create holds it across a window that calls
// clearStaleWorktree, so gating the wrong (unexported) body, or having a gated
// path reach a gated method, wedges the daemon permanently instead of
// serializing it.
//
// The create's OnWorktreeReady hook fires while it still holds the gate, so the
// cleanup below genuinely contends rather than trivially running after.
func TestPurgeAndReapSerializeWithoutDeadlockingAgainstACreate(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	victim, err := mgr.Create(ctx, CreateOpts{
		RepoPath:        repoDir,
		BaseBranch:      "main",
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		Title:           "victim session",
	})
	if err != nil {
		t.Fatalf("Create victim: %v", err)
	}

	var (
		inGate       = make(chan struct{})
		cleanupBegun = make(chan struct{})
		createDone   = make(chan error, 1)
		cleanupDone  = make(chan error, 1)
	)

	go func() {
		_, err := mgr.Create(ctx, CreateOpts{
			RepoPath:        repoDir,
			BaseBranch:      "main",
			WorktreeBaseDir: wtBase,
			RepoName:        "my-repo",
			Title:           "concurrent session",
			OnWorktreeReady: func(context.Context, string, string) {
				// Still inside the gate here. Hold it until the cleanup has
				// started contending, then let the create finish — waiting for
				// the cleanup to COMPLETE would be a deadlock this test wrote
				// itself, not one in the code under test.
				close(inGate)
				<-cleanupBegun
				time.Sleep(100 * time.Millisecond)
			},
		})
		createDone <- err
	}()

	<-inGate
	go func() {
		close(cleanupBegun)
		// The purge must RUN here, not merely be attempted: the reap that follows
		// is refused while the worktree is still registered, so a skipped purge
		// would turn this into a test of the wrong failure.
		if purgeErr := mgr.PurgeWorktree(ctx, repoDir, "my-repo", wtBase, victim.BranchName); purgeErr != nil {
			cleanupDone <- purgeErr
			return
		}
		cleanupDone <- mgr.ReapLocalBranches(ctx, repoDir, []string{victim.BranchName})
	}()

	for _, w := range []struct {
		name string
		ch   chan error
	}{{"cleanup", cleanupDone}, {"create", createDone}} {
		select {
		case err := <-w.ch:
			if err != nil {
				t.Fatalf("%s failed: %v", w.name, err)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("%s never returned; the clone gate deadlocked", w.name)
		}
	}

	// And the gated cleanup actually did its job.
	if out := gitOutput(t, repoDir, "branch", "--list", victim.BranchName); strings.TrimSpace(out) != "" {
		t.Fatalf("branch %q survived the gated reap: %q", victim.BranchName, out)
	}
	if _, err := os.Stat(victim.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree %q survived the gated purge (stat err %v)", victim.WorktreePath, err)
	}
}

// TestRunGitReturnsWhenAGrandchildHoldsTheOutputPipe pins the BOS-717 property
// that makes the per-invocation timeout real: cmd.Stdout is a *bytes.Buffer, so
// os/exec gives git a PIPE, and cmd.Run does not return until every writer of
// that pipe closes it. Killing the git PID does not close a pipe a grandchild
// still holds, so without cmd.WaitDelay this call blocks for as long as that
// process lives — while Manager.Create holds the per-clone gate across it.
//
// The shell alias spawns a background sleep that inherits git's stdout and
// outlives it, which is exactly the shape (a stuck credential helper, a
// lingering ssh) the bound exists for.
func TestRunGitReturnsWhenAGrandchildHoldsTheOutputPipe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell assumed")
	}
	repoDir := initTestRepo(t)

	prevWaitDelay := gitWaitDelay
	gitWaitDelay = 200 * time.Millisecond
	t.Cleanup(func() { gitWaitDelay = prevWaitDelay })

	// Far longer than the wait delay, so a pass cannot come from the sleep
	// simply ending on its own.
	const lingerFor = 30 * time.Second

	done := make(chan error, 1)
	go func() {
		// The per-invocation budget is deliberately generous here: this test is
		// about the pipe drain, not about the deadline. git itself exits at once.
		_, err := runGitWithTimeout(context.Background(), time.Minute, repoDir,
			"-c", "alias.linger=!sh -c 'sleep "+strconv.Itoa(int(lingerFor.Seconds()))+" & echo started'",
			"linger")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("runGitWithTimeout error = nil, want the wait-delay expiry reported")
		}
		if !errors.Is(err, exec.ErrWaitDelay) {
			t.Fatalf("error = %v, want exec.ErrWaitDelay", err)
		}
		if !strings.Contains(err.Error(), "held the output pipe open") {
			t.Fatalf("error = %v, want it to name the lingering child", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("runGitWithTimeout never returned while a grandchild held the output pipe")
	}
}

// TestIsAncestorReturnsWhenAGrandchildHoldsTheOutputPipe covers the one direct
// exec.CommandContext git site left in this package. IsAncestor cannot route
// through runGitWithTimeout (it reads the exit code to tell "not an ancestor"
// from a broken ref), so it needs its own cmd.WaitDelay for the same reason:
// its stderr is a *bytes.Buffer, and a grandchild holding that pipe keeps
// cmd.Run blocked long after the deadline killed git. Its caller is the
// stranded-bootstrap reaper, running on the deadline-less poller context.
//
// A `git` shim on PATH is what makes the shape reproducible: unlike runGit,
// IsAncestor takes no caller-supplied args to hang an alias off.
//
// The other half of the contract is that the expiry is an ERROR, never an
// answer: the shim exits 0, and Wait substitutes ErrWaitDelay only for a nil
// error, so a naive `err == nil` would report "ancestor" — the answer on which
// the reaper force-deletes a branch.
func TestIsAncestorReturnsWhenAGrandchildHoldsTheOutputPipe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell assumed")
	}
	repoDir := initTestRepo(t)

	prevWaitDelay := gitWaitDelay
	gitWaitDelay = 200 * time.Millisecond
	t.Cleanup(func() { gitWaitDelay = prevWaitDelay })

	// Far longer than the wait delay, so a pass cannot come from the sleep
	// simply ending on its own.
	const lingerFor = 30 * time.Second

	shimDir := t.TempDir()
	shim := "#!/bin/sh\nsleep " + strconv.Itoa(int(lingerFor.Seconds())) + " &\nexit 0\n"
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatalf("write git shim: %v", err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	mgr := NewManager(zerolog.Nop())
	type result struct {
		ok  bool
		err error
	}
	done := make(chan result, 1)
	go func() {
		ok, err := mgr.IsAncestor(context.Background(), repoDir, "refs/heads/main", "refs/heads/main")
		done <- result{ok, err}
	}()

	select {
	case got := <-done:
		if got.err == nil {
			t.Fatal("IsAncestor error = nil, want the wait-delay expiry reported")
		}
		if got.ok {
			t.Fatalf("IsAncestor = true with an abandoned output pipe; the reaper would delete a branch on this: %v", got.err)
		}
		if !errors.Is(got.err, exec.ErrWaitDelay) {
			t.Fatalf("error = %v, want exec.ErrWaitDelay", got.err)
		}
		if !strings.Contains(got.err.Error(), "held the output pipe open") {
			t.Fatalf("error = %v, want it to name the lingering child", got.err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("IsAncestor never returned while a grandchild held the output pipe")
	}
}

// TestContendedCleanupGivesUpOnTheCleanupBudget proves the WIRING the constant
// arithmetic in services/bossd/internal/session/start_lock_test.go assumes: the
// cleanup entry points acquire the clone gate on RepoCloneCleanupGateTimeout,
// not on the create-sized RepoCloneGateTimeout.
//
// It matters because every cleanup caller runs with no deadline of its own — the
// failure cleanups on context.WithoutCancel, the stranded-bootstrap reaper on
// the daemon poller — so whichever budget is wired here IS the wait. On the
// create budget the call below would block for twenty minutes.
//
// Skipped in short mode: the only honest observation is the give-up itself, and
// that costs the budget.
func TestContendedCleanupGivesUpOnTheCleanupBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out RepoCloneCleanupGateTimeout")
	}
	repoDir := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())

	// Hold the gate for longer than the cleanup budget, so the cleanup below
	// cannot pass by the holder simply letting go.
	release, _, err := repoCloneGates.Acquire(context.Background(), repoDir, time.Minute)
	if err != nil {
		t.Fatalf("hold the clone gate: %v", err)
	}
	defer release()

	// No deadline, deliberately: this is the shape the reaper and the failure
	// cleanups actually call with, so the gate budget is the only thing bounding
	// the wait.
	started := time.Now()
	err = mgr.ReapLocalBranches(context.Background(), repoDir, []string{"whatever"})
	waited := time.Since(started)

	if err == nil {
		t.Fatal("ReapLocalBranches succeeded while another holder had the clone gate")
	}
	// Generous ceiling: the point is that it is nowhere near the create budget,
	// not that the give-up is punctual to the millisecond.
	if ceiling := RepoCloneCleanupGateTimeout * 2; waited > ceiling {
		t.Fatalf("contended cleanup waited %s (> %s); it is on the create budget (%s), not the cleanup one (%s)",
			waited, ceiling, RepoCloneGateTimeout, RepoCloneCleanupGateTimeout)
	}
	// And it did wait: a cleanup that gives up instantly would defer every time
	// a create so much as overlaps it.
	if floor := RepoCloneCleanupGateTimeout / 2; waited < floor {
		t.Fatalf("contended cleanup gave up after %s (< %s); it is not waiting out the holder at all", waited, floor)
	}
}

// TestPurgeWorktreeReportsThatItDidNotRun is the source side of the suppression
// the session-layer cleanups rely on: a purge that skipped itself must SAY so.
//
// It returns nil for a purge that ran and non-nil only for one that did not, so
// the callers can skip the branch reap — `git branch -D` refuses a branch still
// checked out in a registered worktree, so reaping after a skipped purge leaks
// the branch behind a generic delete failure. While PurgeWorktree returned
// nothing, that was unobservable at every call site.
//
// A cancelled context is the cheap way to reach the refusal: keyedgate declines
// before contending on one, so no 30-second wait is needed to produce it.
func TestPurgeWorktreeReportsThatItDidNotRun(t *testing.T) {
	repoDir := initTestRepo(t)
	wtBase := filepath.Join(t.TempDir(), "worktrees")
	mgr := NewManager(zerolog.Nop())

	res, err := mgr.Create(context.Background(), CreateOpts{
		RepoPath:        repoDir,
		BaseBranch:      "main",
		WorktreeBaseDir: wtBase,
		RepoName:        "my-repo",
		Title:           "doomed bootstrap",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := mgr.PurgeWorktree(cancelled, repoDir, "my-repo", wtBase, res.BranchName); err == nil {
		t.Fatal("PurgeWorktree returned nil for a purge it could not run; every caller would go on to a reap git refuses")
	}
	// And it really did skip, rather than reporting an error for work it did.
	if _, err := os.Stat(res.WorktreePath); err != nil {
		t.Fatalf("worktree %q was removed by a purge that reported it did not run (stat err %v)", res.WorktreePath, err)
	}

	// The happy path still reports success, so the error is not a constant.
	if err := mgr.PurgeWorktree(context.Background(), repoDir, "my-repo", wtBase, res.BranchName); err != nil {
		t.Fatalf("PurgeWorktree on an uncontended clone: %v", err)
	}
}

// writerFunc adapts a func to io.Writer for the setup-output sink.
type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
