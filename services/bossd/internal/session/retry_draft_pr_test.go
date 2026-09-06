package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sessionreason"
)

// recordingDraftPREnsurer stands in for EnsurePR so the sweep's selection,
// interlocks, cooldown and log-and-continue behaviour can be driven without a
// git remote or a GitHub provider. errs keys the sessions whose call should
// fail; every other call succeeds.
type recordingDraftPREnsurer struct {
	mu    sync.Mutex
	calls []string
	errs  map[string]error
}

func newRecordingDraftPREnsurer() *recordingDraftPREnsurer {
	return &recordingDraftPREnsurer{errs: make(map[string]error)}
}

func (r *recordingDraftPREnsurer) ensure(_ context.Context, sessionID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, sessionID)
	return r.errs[sessionID]
}

func (r *recordingDraftPREnsurer) callsCopy() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.calls))
	copy(out, r.calls)
	return out
}

func (r *recordingDraftPREnsurer) count() int {
	return len(r.callsCopy())
}

func (r *recordingDraftPREnsurer) called(sessionID string) bool {
	for _, id := range r.callsCopy() {
		if id == sessionID {
			return true
		}
	}
	return false
}

// newDraftPRRetryLifecycle builds a Lifecycle wired for the draft-PR retry
// sweep: a fake session store, a recording ensurer in place of EnsurePR, and a
// clock the test advances by hand so the cooldown is exercised without sleeping.
// The returned func sets the clock.
func newDraftPRRetryLifecycle(t *testing.T) (
	*Lifecycle, *reconcileMockSessionStore, *recordingDraftPREnsurer, func(time.Time),
) {
	t.Helper()
	lc, sessions, ensurer, _, setNow := newDraftPRRetryLifecycleWithWorktrees(t)
	return lc, sessions, ensurer, setNow
}

// newDraftPRRetryLifecycleWithWorktrees is newDraftPRRetryLifecycle plus the
// worktree mock, for the tests that drive the sweep's committed-work guard.
// The mock answers IsAncestor(HEAD, base) = false by default — i.e. the branch
// IS ahead of base and so carries committed work — because that is the state
// every other test in this file assumes. A test that wants the empty-branch
// path flips isAncestorFn.
func newDraftPRRetryLifecycleWithWorktrees(t *testing.T) (
	*Lifecycle, *reconcileMockSessionStore, *recordingDraftPREnsurer,
	*mockWorktreeManager, func(time.Time),
) {
	t.Helper()
	sessions := newReconcileMockSessionStore()
	worktrees := &mockWorktreeManager{
		isAncestorFn: func(_, _, _ string) (bool, error) { return false, nil },
	}
	lc := newTestLifecycle(sessions, nil, &mockAgentChatStore{}, nil, worktrees,
		newMockAgentRunner(), nil, nil, zerolog.Nop())

	ensurer := newRecordingDraftPREnsurer()
	lc.draftPREnsurer = ensurer.ensure

	var mu sync.Mutex
	current := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	lc.SetClockForTest(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return current
	})
	setNow := func(tm time.Time) {
		mu.Lock()
		defer mu.Unlock()
		current = tm
	}
	return lc, sessions, ensurer, worktrees, setNow
}

// failedDraftPRSession is a session in the state this sweep exists to repair:
// active, no PR number, carrying a draft-PR creation failure.
func failedDraftPRSession(id string) *models.Session {
	reason := sessionreason.DraftPRCreationFailure(errors.New("gh pr create: exit 1"))
	return &models.Session{
		ID:     id,
		RepoID: "repo-1",
		// BaseBranch and WorktreePath are what the sweep's committed-work guard
		// reads; without them it would refuse every session and no other test
		// here would exercise the retry it is actually asserting on.
		BranchName:    "feat/" + id,
		BaseBranch:    "main",
		WorktreePath:  "/tmp/worktrees/" + id,
		BlockedReason: &reason,
	}
}

// draftPRRetryStateFor reads a session's rate-limiter entry under the lock the
// sweep uses, so assertions never race the bookkeeping.
func (l *Lifecycle) draftPRRetryStateFor(sessionID string) (draftPRRetryState, bool) {
	l.draftPRRetryMu.Lock()
	defer l.draftPRRetryMu.Unlock()
	state, ok := l.draftPRRetries[sessionID]
	return state, ok
}

func TestRetryFailedDraftPRsSelectsFailedSessionWithNoPR(t *testing.T) {
	lc, sessions, ensurer, _ := newDraftPRRetryLifecycle(t)
	sessions.addSession(failedDraftPRSession("sess-1"))

	n, err := lc.RetryFailedDraftPRsPeriodic(context.Background())
	if err != nil {
		t.Fatalf("sweep returned error: %v", err)
	}
	if n != 1 {
		t.Fatalf("retried = %d, want 1", n)
	}
	if got := ensurer.callsCopy(); len(got) != 1 || got[0] != "sess-1" {
		t.Fatalf("EnsurePR calls = %v, want [sess-1]", got)
	}
}

func TestRetryFailedDraftPRsSkipsSessionThatAlreadyHasAPR(t *testing.T) {
	lc, sessions, ensurer, _ := newDraftPRRetryLifecycle(t)

	// The failure reason is still parked on the row: another writer attached a
	// PR without clearing it. PRNumber is what decides, not the reason.
	sess := failedDraftPRSession("sess-1")
	sess.PRNumber = ptr(873)
	sessions.addSession(sess)

	// Pre-seed retry bookkeeping as though an earlier sweep had tried this
	// session, so the has-PR path's state clearing is observable.
	lc.recordDraftPRRetryAttempt("sess-1", lc.now())

	n, err := lc.RetryFailedDraftPRsPeriodic(context.Background())
	if err != nil {
		t.Fatalf("sweep returned error: %v", err)
	}
	if n != 0 {
		t.Fatalf("retried = %d, want 0", n)
	}
	if ensurer.count() != 0 {
		t.Fatalf("EnsurePR calls = %v, want none", ensurer.callsCopy())
	}
	if _, ok := lc.draftPRRetryStateFor("sess-1"); ok {
		t.Fatal("a sweep that observes the session now has a PR must clear its retry state")
	}
}

func TestRetryFailedDraftPRsSkipsInFlightMarkerReason(t *testing.T) {
	lc, sessions, ensurer, _ := newDraftPRRetryLifecycle(t)

	// The in-flight marker is a DISTINCT string from the failure reason, so a
	// create that is still running must not satisfy the predicate.
	reason := sessionreason.DraftPRCreationInFlight()
	sessions.addSession(&models.Session{
		ID:            "sess-1",
		RepoID:        "repo-1",
		BranchName:    "feat/sess-1",
		BlockedReason: &reason,
	})

	n, err := lc.RetryFailedDraftPRsPeriodic(context.Background())
	if err != nil {
		t.Fatalf("sweep returned error: %v", err)
	}
	if n != 0 {
		t.Fatalf("retried = %d, want 0", n)
	}
	if ensurer.count() != 0 {
		t.Fatalf("EnsurePR calls = %v, want none", ensurer.callsCopy())
	}
}

// An auth failure is terminal in a way the sweep cannot clear: gh has no usable
// credentials, so every re-attempt reissues the same unauthenticated request.
// It is a REGRESSION GUARD on the nesting, not a restatement of it — the auth
// marker sits inside the failure prefix, so IsDraftPRCreationFailure says yes to
// this row and only the narrower predicate rejects it. Delete the exclusion in
// shouldRetryDraftPR and this test goes red while every other test here stays
// green.
func TestRetryFailedDraftPRsSkipsTerminalAuthFailure(t *testing.T) {
	lc, sessions, ensurer, _ := newDraftPRRetryLifecycle(t)

	reason := sessionreason.DraftPRCreationAuthFailure(errors.New("gh pr create: HTTP 401: Requires authentication"))
	sess := failedDraftPRSession("sess-1")
	sess.BlockedReason = &reason
	sessions.addSession(sess)

	// The reason must still satisfy the broad predicate, or this test would pass
	// for the wrong reason (a row the sweep never selected in the first place).
	if !sessionreason.IsDraftPRCreationFailure(sess.BlockedReason) {
		t.Fatal("auth reason must still satisfy IsDraftPRCreationFailure — the nesting is load-bearing")
	}

	n, err := lc.RetryFailedDraftPRsPeriodic(context.Background())
	if err != nil {
		t.Fatalf("sweep returned error: %v", err)
	}
	if n != 0 {
		t.Fatalf("retried = %d, want 0 — an auth failure is unrecoverable by retry", n)
	}
	if ensurer.count() != 0 {
		t.Fatalf("EnsurePR calls = %v, want none", ensurer.callsCopy())
	}
	// No attempt may be charged either: a later RECOVERABLE failure on this same
	// session must arrive with a full budget.
	if _, ok := lc.draftPRRetryStateFor("sess-1"); ok {
		t.Fatal("a skipped auth failure must not consume an attempt from the ladder")
	}
}

// The companion guard: excluding auth must NOT be over-applied to transient
// failures. A flapping remote is precisely what the sweep exists for, so this
// pins that the narrower predicate — not the broad one — is what gates the skip.
func TestRetryFailedDraftPRsStillRetriesTransientFailure(t *testing.T) {
	lc, sessions, ensurer, _ := newDraftPRRetryLifecycle(t)

	reason := sessionreason.DraftPRCreationTransientFailure(errors.New("gh pr create: HTTP 502"))
	sess := failedDraftPRSession("sess-1")
	sess.BlockedReason = &reason
	sessions.addSession(sess)

	n, err := lc.RetryFailedDraftPRsPeriodic(context.Background())
	if err != nil {
		t.Fatalf("sweep returned error: %v", err)
	}
	if n != 1 {
		t.Fatalf("retried = %d, want 1 — a transient failure stays eligible", n)
	}
	if got := ensurer.callsCopy(); len(got) != 1 || got[0] != "sess-1" {
		t.Fatalf("EnsurePR calls = %v, want [sess-1]", got)
	}
}

func TestRetryFailedDraftPRsSkipsRegisteredInFlightCreate(t *testing.T) {
	lc, sessions, ensurer, _ := newDraftPRRetryLifecycle(t)
	sessions.addSession(failedDraftPRSession("sess-1"))

	// The row looks eligible, but a background create is registered right now.
	// Retrying would push the same branch from two places.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, release := lc.trackBackgroundDraftPR("sess-1", cancel)

	n, err := lc.RetryFailedDraftPRsPeriodic(ctx)
	if err != nil {
		t.Fatalf("sweep returned error: %v", err)
	}
	if n != 0 {
		t.Fatalf("retried = %d, want 0 while a create is in flight", n)
	}
	if ensurer.count() != 0 {
		t.Fatalf("EnsurePR calls = %v, want none", ensurer.callsCopy())
	}

	// Once the create deregisters, the same row is eligible again.
	release()
	if n, err = lc.RetryFailedDraftPRsPeriodic(ctx); err != nil {
		t.Fatalf("second sweep returned error: %v", err)
	}
	if n != 1 {
		t.Fatalf("retried after the create finished = %d, want 1", n)
	}
}

func TestRetryFailedDraftPRsHonoursCooldown(t *testing.T) {
	lc, sessions, ensurer, setNow := newDraftPRRetryLifecycle(t)
	sessions.addSession(failedDraftPRSession("sess-1"))
	// The attempt fails, so the session stays eligible on the facts and only the
	// cooldown decides whether the next tick touches it — a success would clear
	// the bookkeeping and prove nothing about the gap.
	ensurer.errs["sess-1"] = errors.New("gh pr create: exit 1")

	start := lc.now()
	if n, err := lc.RetryFailedDraftPRsPeriodic(context.Background()); err != nil || n != 1 {
		t.Fatalf("first sweep = (%d, %v), want (1, nil)", n, err)
	}

	// Inside the cooldown: skipped, however many ticks fire.
	setNow(start.Add(draftPRRetryCooldown - time.Second))
	if n, err := lc.RetryFailedDraftPRsPeriodic(context.Background()); err != nil || n != 0 {
		t.Fatalf("sweep inside cooldown = (%d, %v), want (0, nil)", n, err)
	}
	if ensurer.count() != 1 {
		t.Fatalf("EnsurePR calls = %v, want exactly the first", ensurer.callsCopy())
	}

	// Past it: eligible again.
	setNow(start.Add(draftPRRetryCooldown))
	if n, err := lc.RetryFailedDraftPRsPeriodic(context.Background()); err != nil || n != 1 {
		t.Fatalf("sweep past cooldown = (%d, %v), want (1, nil)", n, err)
	}
	if ensurer.count() != 2 {
		t.Fatalf("EnsurePR calls = %v, want two", ensurer.callsCopy())
	}
}

func TestRetryFailedDraftPRsStopsAtAttemptCap(t *testing.T) {
	lc, sessions, ensurer, setNow := newDraftPRRetryLifecycle(t)
	sessions.addSession(failedDraftPRSession("sess-1"))
	// Every attempt fails, so nothing ever clears the bookkeeping — the budget
	// is what stops it.
	ensurer.errs["sess-1"] = errors.New("push branch: permission denied")

	start := lc.now()
	for attempt := 1; attempt <= draftPRRetryMaxAttempts; attempt++ {
		setNow(start.Add(time.Duration(attempt-1) * draftPRRetryCooldown))
		n, err := lc.RetryFailedDraftPRsPeriodic(context.Background())
		if err != nil {
			t.Fatalf("sweep %d returned error: %v", attempt, err)
		}
		if n != 1 {
			t.Fatalf("sweep %d retried = %d, want 1", attempt, n)
		}
	}

	// Budget exhausted: no later sweep touches it again, however far the clock
	// advances.
	for _, gap := range []time.Duration{draftPRRetryCooldown, 24 * time.Hour} {
		setNow(lc.now().Add(gap))
		n, err := lc.RetryFailedDraftPRsPeriodic(context.Background())
		if err != nil {
			t.Fatalf("post-cap sweep returned error: %v", err)
		}
		if n != 0 {
			t.Fatalf("post-cap sweep retried = %d, want 0", n)
		}
	}
	if ensurer.count() != draftPRRetryMaxAttempts {
		t.Fatalf("EnsurePR called %d times, want %d", ensurer.count(), draftPRRetryMaxAttempts)
	}
}

func TestRetryFailedDraftPRsCapsWorkPerSweep(t *testing.T) {
	lc, sessions, ensurer, _ := newDraftPRRetryLifecycle(t)

	eligible := draftPRRetryMaxPerSweep + 2
	// Every attempt fails, so no session drops out of the eligible set by
	// succeeding — the per-sweep cap is the only thing bounding the first tick.
	for i := 0; i < eligible; i++ {
		id := fmt.Sprintf("sess-%d", i)
		sessions.addSession(failedDraftPRSession(id))
		ensurer.errs[id] = errors.New("gh pr create: exit 1")
	}

	n, err := lc.RetryFailedDraftPRsPeriodic(context.Background())
	if err != nil {
		t.Fatalf("sweep returned error: %v", err)
	}
	if n != draftPRRetryMaxPerSweep {
		t.Fatalf("retried = %d, want the per-sweep cap %d", n, draftPRRetryMaxPerSweep)
	}
	if ensurer.count() != draftPRRetryMaxPerSweep {
		t.Fatalf("EnsurePR called %d times, want %d", ensurer.count(), draftPRRetryMaxPerSweep)
	}

	// The next tick picks up the rest: the ones already tried are inside their
	// cooldown, and the remainder have no entry at all.
	n, err = lc.RetryFailedDraftPRsPeriodic(context.Background())
	if err != nil {
		t.Fatalf("second sweep returned error: %v", err)
	}
	if want := eligible - draftPRRetryMaxPerSweep; n != want {
		t.Fatalf("second sweep retried = %d, want the remaining %d", n, want)
	}
	for i := 0; i < eligible; i++ {
		if id := fmt.Sprintf("sess-%d", i); !ensurer.called(id) {
			t.Fatalf("%s was never retried across two sweeps", id)
		}
	}
}

func TestRetryFailedDraftPRsClearsStateOnSuccess(t *testing.T) {
	lc, sessions, _, _ := newDraftPRRetryLifecycle(t)
	sessions.addSession(failedDraftPRSession("sess-1"))

	if n, err := lc.RetryFailedDraftPRsPeriodic(context.Background()); err != nil || n != 1 {
		t.Fatalf("sweep = (%d, %v), want (1, nil)", n, err)
	}
	if _, ok := lc.draftPRRetryStateFor("sess-1"); ok {
		t.Fatal("a successful EnsurePR must clear the session's retry bookkeeping")
	}
}

func TestRetryFailedDraftPRsKeepsStateOnFailure(t *testing.T) {
	lc, sessions, ensurer, _ := newDraftPRRetryLifecycle(t)
	sessions.addSession(failedDraftPRSession("sess-1"))
	ensurer.errs["sess-1"] = errors.New("push branch: permission denied")

	if n, err := lc.RetryFailedDraftPRsPeriodic(context.Background()); err != nil || n != 1 {
		t.Fatalf("sweep = (%d, %v), want (1, nil)", n, err)
	}
	state, ok := lc.draftPRRetryStateFor("sess-1")
	if !ok {
		t.Fatal("a failed retry must keep the session's bookkeeping so the cooldown and cap apply")
	}
	if state.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", state.Attempts)
	}
}

func TestRetryFailedDraftPRsContinuesAfterPerSessionError(t *testing.T) {
	lc, sessions, ensurer, _ := newDraftPRRetryLifecycle(t)
	sessions.addSession(failedDraftPRSession("sess-bad"))
	sessions.addSession(failedDraftPRSession("sess-good"))
	ensurer.errs["sess-bad"] = errors.New("resolve origin URL: no such remote")

	n, err := lc.RetryFailedDraftPRsPeriodic(context.Background())
	if err != nil {
		t.Fatalf("a per-session EnsurePR error must not fail the sweep: %v", err)
	}
	// Both were attempted; the count is attempts, not successes, because a
	// failed attempt still consumed the tick's budget.
	if n != 2 {
		t.Fatalf("retried = %d, want 2", n)
	}
	if !ensurer.called("sess-good") {
		t.Fatalf("the sweep stopped at the failing session; calls = %v", ensurer.callsCopy())
	}
}

func TestRetryFailedDraftPRsSkipsBranchWithNoCommittedWork(t *testing.T) {
	// createDraftPR reaches its EmptyCommit only after the origin lookup and
	// the branch verification, so a failure raised before that point parks a
	// draft-PR-failure reason on a branch that carries no commit. EnsurePR
	// cannot open a PR for such a branch, and its failure path would overwrite
	// the reason naming the real cause. Skip, and spend no attempt.
	lc, sessions, ensurer, worktrees, _ := newDraftPRRetryLifecycleWithWorktrees(t)
	worktrees.isAncestorFn = func(_, _, _ string) (bool, error) { return true, nil }
	sessions.addSession(failedDraftPRSession("sess-1"))

	n, err := lc.RetryFailedDraftPRsPeriodic(context.Background())
	if err != nil {
		t.Fatalf("sweep returned error: %v", err)
	}
	if n != 0 {
		t.Fatalf("retried = %d, want 0", n)
	}
	if ensurer.count() != 0 {
		t.Fatalf("EnsurePR calls = %v, want none for a zero-commit branch", ensurer.callsCopy())
	}
	if _, ok := lc.draftPRRetryStateFor("sess-1"); ok {
		t.Fatal("a skipped session must not spend an attempt from its retry budget")
	}
}

func TestRetryFailedDraftPRsRetriesOnceTheBranchHasCommittedWork(t *testing.T) {
	// The other half of the guard: skipping is a deferral, not a refusal. Once
	// the agent commits, the very next sweep retries — no cooldown to wait out,
	// because the skip spent nothing.
	lc, sessions, ensurer, worktrees, _ := newDraftPRRetryLifecycleWithWorktrees(t)
	worktrees.isAncestorFn = func(_, _, _ string) (bool, error) { return true, nil }
	sessions.addSession(failedDraftPRSession("sess-1"))

	if n, err := lc.RetryFailedDraftPRsPeriodic(context.Background()); err != nil || n != 0 {
		t.Fatalf("first sweep = (%d, %v), want (0, nil)", n, err)
	}

	worktrees.isAncestorFn = func(_, _, _ string) (bool, error) { return false, nil }
	if n, err := lc.RetryFailedDraftPRsPeriodic(context.Background()); err != nil || n != 1 {
		t.Fatalf("second sweep = (%d, %v), want (1, nil)", n, err)
	}
	if !ensurer.called("sess-1") {
		t.Fatalf("EnsurePR calls = %v, want [sess-1]", ensurer.callsCopy())
	}
}

func TestRetryFailedDraftPRsSkipsWhenCommittedWorkLookupFails(t *testing.T) {
	// Fail-safe in the same direction as every other ambiguity in this sweep:
	// an unreadable branch is left alone rather than pushed at speculatively.
	// Unlike a branch that is merely not ready yet, though, the attempt IS
	// charged — an unreadable row fails identically on every tick, so a free
	// skip would warn once a minute forever instead of going quiet.
	lc, sessions, ensurer, worktrees, _ := newDraftPRRetryLifecycleWithWorktrees(t)
	worktrees.isAncestorFn = func(_, _, _ string) (bool, error) {
		return false, errors.New("not a git repository")
	}
	sessions.addSession(failedDraftPRSession("sess-1"))
	sessions.addSession(failedDraftPRSession("sess-2"))

	n, err := lc.RetryFailedDraftPRsPeriodic(context.Background())
	if err != nil {
		t.Fatalf("a committed-work lookup error must not fail the sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("retried = %d, want 0", n)
	}
	if ensurer.count() != 0 {
		t.Fatalf("EnsurePR calls = %v, want none", ensurer.callsCopy())
	}
	// Both sessions, not just the first: the error is per-session, so the sweep
	// must reach the second one and charge it too.
	for _, id := range []string{"sess-1", "sess-2"} {
		state, ok := lc.draftPRRetryStateFor(id)
		if !ok {
			t.Fatalf("%s: a lookup error must spend an attempt so the session eventually goes quiet", id)
		}
		if state.Attempts != 1 {
			t.Fatalf("%s: attempts = %d, want 1", id, state.Attempts)
		}
	}
}

func TestRetryFailedDraftPRsGoesQuietAfterRepeatedLookupFailures(t *testing.T) {
	// The point of charging the lookup error: a permanently unreadable session
	// exhausts its budget and stops being touched, rather than logging a
	// warning on every 60s tick for the life of the daemon.
	lc, sessions, _, worktrees, setNow := newDraftPRRetryLifecycleWithWorktrees(t)
	worktrees.isAncestorFn = func(_, _, _ string) (bool, error) {
		return false, errors.New("not a git repository")
	}
	sessions.addSession(failedDraftPRSession("sess-1"))

	now := lc.now()
	for i := 0; i < draftPRRetryMaxAttempts+2; i++ {
		if _, err := lc.RetryFailedDraftPRsPeriodic(context.Background()); err != nil {
			t.Fatalf("sweep %d returned error: %v", i, err)
		}
		now = now.Add(draftPRRetryCooldown)
		setNow(now)
	}

	state, ok := lc.draftPRRetryStateFor("sess-1")
	if !ok {
		t.Fatal("expected retry bookkeeping for the failing session")
	}
	if state.Attempts != draftPRRetryMaxAttempts {
		t.Fatalf("attempts = %d, want the cap %d — the session must stop being retried",
			state.Attempts, draftPRRetryMaxAttempts)
	}
}

func TestRetryFailedDraftPRsSkipsSessionMissingBranchOrWorktree(t *testing.T) {
	// Both fields are checked before the git call rather than left to
	// cleanBranchHasCommittedWork. An empty path would reach cmd.Dir and run
	// `git merge-base` in bossd's own working directory; an empty branch name
	// comes back as (false, nil), which is indistinguishable from a branch
	// that simply has no commit yet and would take the free-skip path.
	for _, tc := range []struct {
		name    string
		mutate  func(*models.Session)
		wantMsg string
	}{
		{"no worktree path", func(s *models.Session) { s.WorktreePath = "" },
			"a session with no worktree path must spend an attempt so it eventually goes quiet"},
		{"no branch name", func(s *models.Session) { s.BranchName = "" },
			"a session with no branch name must spend an attempt rather than be skipped for free forever"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lc, sessions, ensurer, worktrees, _ := newDraftPRRetryLifecycleWithWorktrees(t)
			worktrees.isAncestorFn = func(_, _, _ string) (bool, error) {
				t.Error("the sweep must not reach the git call for a session missing these fields")
				return false, nil
			}
			sess := failedDraftPRSession("sess-1")
			tc.mutate(sess)
			sessions.addSession(sess)

			n, err := lc.RetryFailedDraftPRsPeriodic(context.Background())
			if err != nil {
				t.Fatalf("sweep returned error: %v", err)
			}
			if n != 0 {
				t.Fatalf("retried = %d, want 0", n)
			}
			if ensurer.count() != 0 {
				t.Fatalf("EnsurePR calls = %v, want none", ensurer.callsCopy())
			}
			if _, ok := lc.draftPRRetryStateFor("sess-1"); !ok {
				t.Fatal(tc.wantMsg)
			}
		})
	}
}

func TestRetryFailedDraftPRsStopsOnCancelledContext(t *testing.T) {
	// Shutdown: every git call and every EnsurePR below the loop head fails
	// instantly once pollerCtx is cancelled, and a guard-skip does not
	// increment retried — so without the check the final sweep would warn once
	// per eligible session.
	lc, sessions, ensurer, worktrees, _ := newDraftPRRetryLifecycleWithWorktrees(t)
	worktrees.isAncestorFn = func(_, _, _ string) (bool, error) {
		t.Error("a cancelled sweep must not spawn git subprocesses")
		return false, nil
	}
	sessions.addSession(failedDraftPRSession("sess-1"))
	sessions.addSession(failedDraftPRSession("sess-2"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	n, err := lc.RetryFailedDraftPRsPeriodic(ctx)
	if err != nil {
		t.Fatalf("a cancelled sweep must return cleanly, not error: %v", err)
	}
	if n != 0 {
		t.Fatalf("retried = %d, want 0", n)
	}
	if ensurer.count() != 0 {
		t.Fatalf("EnsurePR calls = %v, want none", ensurer.callsCopy())
	}
	if _, ok := lc.draftPRRetryStateFor("sess-1"); ok {
		t.Fatal("a cancelled sweep must not spend attempts")
	}
}

func TestRetryFailedDraftPRsSurfacesListError(t *testing.T) {
	// The one error that DOES abort the sweep: it has nothing to iterate.
	lc := newTestLifecycle(&listErrSessionStore{}, nil, &mockAgentChatStore{}, nil, nil,
		newMockAgentRunner(), nil, nil, zerolog.Nop())

	n, err := lc.RetryFailedDraftPRsPeriodic(context.Background())
	if err == nil {
		t.Fatal("expected the list error to abort the sweep")
	}
	if n != 0 {
		t.Fatalf("retried = %d, want 0", n)
	}
}

// listErrSessionStore fails ListActive so the sweep's only aborting path is
// covered.
type listErrSessionStore struct {
	reconcileMockSessionStore
	err error
}

func (s *listErrSessionStore) ListActive(context.Context, string) ([]*models.Session, error) {
	if s.err != nil {
		return nil, s.err
	}
	return nil, errors.New("database is locked")
}

func TestRetryFailedDraftPRsStaysQuietWhenListIsCancelled(t *testing.T) {
	// A cancellation that lands mid-query is shutdown, not a fault: reporting
	// it would make main.go log the sweep as FAILED at exactly the moment
	// there is nothing wrong with it.
	lc := newTestLifecycle(&listErrSessionStore{err: fmt.Errorf("query sessions: %w", context.Canceled)},
		nil, &mockAgentChatStore{}, nil, nil, newMockAgentRunner(), nil, nil, zerolog.Nop())

	n, err := lc.RetryFailedDraftPRsPeriodic(context.Background())
	if err != nil {
		t.Fatalf("a cancelled list must not be reported as a sweep failure: %v", err)
	}
	if n != 0 {
		t.Fatalf("retried = %d, want 0", n)
	}
}
