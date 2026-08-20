package git

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The two halves of the signature, spelled the way git actually emits them when
// a concurrent fetch wins the race for refs/remotes/origin/<base>. Which half
// surfaces depends on the refspec form, so each is exercised alone as well as
// together.
const (
	refLockStderr    = "error: cannot lock ref 'refs/remotes/origin/main': is at 1a2b3c4 but expected 5d6e7f8"
	refUpdateStderr  = " ! 1a2b3c4..5d6e7f8  main       -> origin/main  (unable to update local ref)"
	refLockCombined  = refLockStderr + "\n" + refUpdateStderr
	nonFastForwardEr = "! [rejected] main -> main (non-fast-forward)"

	// Permanent ref-lock failures. Each carries the same "cannot lock ref"
	// prefix as the race above, so the needle alone cannot tell them apart —
	// refLockTerminalReasons is what keeps them out. Retrying any of these
	// wastes the ladder and then answers CodeUnavailable for something only an
	// operator can clear.
	dfConflictStderr   = "error: cannot lock ref 'refs/remotes/origin/feat': 'refs/remotes/origin/feat/api' exists; cannot create 'refs/remotes/origin/feat'"
	dfDirectoryStderr  = "error: cannot lock ref 'refs/remotes/origin/feat/api': there is a non-empty directory '/repo/.git/refs/remotes/origin/feat' blocking reference 'refs/remotes/origin/feat/api'"
	stillRefsStderr    = "error: cannot lock ref 'refs/remotes/origin/feat': there are still refs under 'refs/remotes/origin/feat'"
	refPermStderr      = "error: cannot lock ref 'refs/remotes/origin/main': Unable to create '/repo/.git/refs/remotes/origin/main.lock': Permission denied"
	brokenRefStderr    = "error: cannot lock ref 'refs/remotes/origin/main': unable to resolve reference 'refs/remotes/origin/main': reference broken"
	unresolvableStderr = "error: cannot lock ref 'refs/remotes/origin/main': unable to resolve reference 'refs/remotes/origin/main'"
	txnConflictStderr  = "error: cannot process 'refs/remotes/origin/feat' and 'refs/remotes/origin/feat/api' at the same time"

	// EEXIST on the ref's own lockfile. Reads like a wedged repository, is
	// usually a live concurrent fetch — see refLockTerminalReasons.
	liveRefLockStderr = "error: cannot lock ref 'refs/remotes/origin/main': Unable to create '/repo/.git/refs/remotes/origin/main.lock': File exists."
)

func TestIsRefLockContentionGitOutput(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"cannot lock ref alone", errors.New(refLockStderr), true},
		{"unable to update local ref alone", errors.New(refUpdateStderr), true},
		{"both halves together", errors.New(refLockCombined), true},
		{"wrapped several layers down", errors.New("fetch origin/main: git fetch origin main: exit status 1: " + refLockCombined), true},
		// The likeliest collision of all: the other fetch is still holding the
		// ref's lockfile. Retryable, and git says so in its own advice text.
		{"live lock holder on the ref", errors.New(liveRefLockStderr), true},
		{"live lock holder with the update summary", errors.New(liveRefLockStderr + "\n" + refUpdateStderr), true},
		// Negative cases. Each is a real git failure that must surface on the
		// first attempt: retrying it wastes the caller's budget and then labels
		// it retryable at the RPC boundary, telling the caller to re-run
		// something that can never succeed.
		{"non-fast-forward rejection", errors.New(nonFastForwardEr), false},
		{"bare exit status with empty stderr", errors.New("git fetch origin main: exit status 1: "), false},
		{"index.lock is a wedged repo, not a lost race", errors.New("fatal: Unable to create '/repo/.git/index.lock': File exists."), false},
		{"tag clobber rejection", errors.New("! [rejected] v1 -> v1 (would clobber existing tag)"), false},
		{"missing branch", errors.New("fatal: couldn't find remote ref refs/heads/nope"), false},
		{"nil", nil, false},
		// Permanent failures that DO carry the needle. These are the reason the
		// classifier subtracts refLockTerminalReasons: without it each would be
		// retried twice and then reported to the caller as "try again".
		{"transaction ref-name conflict", errors.New(txnConflictStderr + "\n" + refUpdateStderr), false},
		{"directory/file ref conflict", errors.New(dfConflictStderr), false},
		{"non-empty directory blocking the ref", errors.New(dfDirectoryStderr), false},
		{"still refs under the name", errors.New(stillRefsStderr), false},
		{"permission denied on the refs tree", errors.New(refPermStderr), false},
		{"broken reference", errors.New(brokenRefStderr), false},
		{"unresolvable reference", errors.New(unresolvableStderr), false},
		// The realistic fetch shape: the terminal reason on stderr AND the
		// ref-update summary line that a lost race also prints. The summary
		// needle must not resurrect a failure the reason already excluded.
		{"terminal reason alongside the update summary", errors.New(dfConflictStderr + "\n" + refUpdateStderr), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRefLockContentionGitOutput(tc.err); got != tc.want {
				t.Errorf("isRefLockContentionGitOutput(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// zeroRefLockBackoff removes the ladder's waits without changing its length —
// the retry count under test is len(refLockRetryBackoff), so the replacement
// must keep the same number of entries.
//
// This swaps a PACKAGE-LEVEL var, so these tests must never be made parallel
// (no t.Parallel anywhere in package git today).
func zeroRefLockBackoff(t *testing.T) {
	t.Helper()
	previous := refLockRetryBackoff
	t.Cleanup(func() { refLockRetryBackoff = previous })
	zeroed := make([]time.Duration, len(previous))
	refLockRetryBackoff = zeroed
}

// scriptedFetch returns a gitRunFn that consumes one result per attempt and
// reports how many attempts were made. Running past the script fails the test
// rather than repeating the last entry, so an unexpected extra attempt is loud.
func scriptedFetch(t *testing.T, results ...error) (gitRunFn, func() int) {
	t.Helper()
	made := 0
	run := func(_ context.Context, _ string, args ...string) (string, error) {
		if made >= len(results) {
			t.Errorf("git %s: attempt %d exceeds the %d scripted results", strings.Join(args, " "), made+1, len(results))
			return "", errors.New("unscripted attempt")
		}
		err := results[made]
		made++
		return "fetched", err
	}
	return run, func() int { return made }
}

func TestFetchWithRefLockRetry_RetriesThenSucceeds(t *testing.T) {
	zeroRefLockBackoff(t)

	run, attempts := scriptedFetch(t, errors.New(refLockCombined), nil)
	out, err := fetchWithRefLockRetry(context.Background(), "/repo", run, "fetch", "--prune", "origin")
	if err != nil {
		t.Fatalf("fetchWithRefLockRetry: %v", err)
	}
	if out != "fetched" {
		t.Errorf("output = %q, want the successful attempt's output", out)
	}
	if got := attempts(); got != 2 {
		t.Errorf("attempts = %d, want exactly 2 (one loss, one retry that won)", got)
	}
}

func TestFetchWithRefLockRetry_SucceedsFirstTryWithoutRetrying(t *testing.T) {
	zeroRefLockBackoff(t)

	run, attempts := scriptedFetch(t, nil)
	if _, err := fetchWithRefLockRetry(context.Background(), "/repo", run, "fetch", "origin", "main"); err != nil {
		t.Fatalf("fetchWithRefLockRetry: %v", err)
	}
	if got := attempts(); got != 1 {
		t.Errorf("attempts = %d, want exactly 1", got)
	}
}

// A failure that is NOT ref-lock contention must be returned from the first
// attempt untouched: retrying it can only spend the caller's budget hiding a
// real repository problem.
func TestFetchWithRefLockRetry_UnclassifiedFailureIsNotRetried(t *testing.T) {
	zeroRefLockBackoff(t)

	want := errors.New(nonFastForwardEr)
	run, attempts := scriptedFetch(t, want)
	_, err := fetchWithRefLockRetry(context.Background(), "/repo", run, "fetch", "origin", "main:main")
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want the original error unwrapped", err)
	}
	if errors.Is(err, ErrRefLockContended) {
		t.Error("an unrelated failure was classified as ref-lock contention")
	}
	if got := attempts(); got != 1 {
		t.Errorf("attempts = %d, want exactly 1", got)
	}
}

func TestFetchWithRefLockRetry_ExhaustedWrapsSentinelAndKeepsGitText(t *testing.T) {
	zeroRefLockBackoff(t)

	want := 1 + len(refLockRetryBackoff)
	results := make([]error, want)
	for i := range results {
		results[i] = errors.New(refLockCombined)
	}
	run, attempts := scriptedFetch(t, results...)

	_, err := fetchWithRefLockRetry(context.Background(), "/repo", run, "fetch", "--prune", "origin")
	if err == nil {
		t.Fatal("fetchWithRefLockRetry succeeded; want the exhausted error")
	}
	if !errors.Is(err, ErrRefLockContended) {
		t.Errorf("errors.Is(err, ErrRefLockContended) = false for %v; the RPC boundary keys on this", err)
	}
	// The sentinel is added ALONGSIDE the git text, never instead of it: a human
	// reading the log still needs to see which ref lost and to what.
	if !strings.Contains(err.Error(), "cannot lock ref") {
		t.Errorf("err = %v, want the raw git text preserved", err)
	}
	if got := attempts(); got != want {
		t.Errorf("attempts = %d, want exactly %d (at most two retries)", got, want)
	}
}

// The retry budget is an absolute number the plan capped at "at most twice", not
// whatever len(refLockRetryBackoff) happens to be — every other test in this
// file derives its expected attempt count from that var, so growing the ladder
// would keep them all green while silently multiplying the budget. Pin both the
// count and the wall clock it can cost.
func TestRefLockRetryBackoffStaysWithinItsBudget(t *testing.T) {
	if got := len(refLockRetryBackoff); got != 2 {
		t.Errorf("len(refLockRetryBackoff) = %d, want 2 (at most two retries, three attempts in all)", got)
	}
	var total time.Duration
	for _, d := range refLockRetryBackoff {
		if d <= 0 {
			t.Errorf("refLockRetryBackoff contains %v; a zero wait retries into the same unsettled ref", d)
		}
		total += d
	}
	// Far inside, not merely inside: the ladder must never be the reason a
	// caller's budget runs out.
	if total > GitCommandTimeout/10 {
		t.Errorf("refLockRetryBackoff totals %v, want well under a tenth of GitCommandTimeout (%v)", total, GitCommandTimeout)
	}
}

// A fetch killed by the caller's context is not a lost race, even when its
// stderr says "cannot lock ref". runGitWithTimeout appends git's raw stderr to
// the context error, and `fetch --prune origin` reports refs one at a time as it
// walks them, so a kill mid-walk can carry a genuine contention line for a ref
// it really was racing on. Classifying that would burn the ladder re-running git
// against a context that cancels every attempt instantly, then answer
// CodeUnavailable — "try again" — to a caller who already stopped asking.
func TestFetchWithRefLockRetry_ContextEndedFailureIsNotClassified(t *testing.T) {
	zeroRefLockBackoff(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Shaped like what runGitWithTimeout actually returns on the context path:
	// the context error wrapped with %w, then git's partial stderr appended.
	gitErr := fmt.Errorf("git fetch --prune origin: %w (caller's context ended): %s", context.Canceled, refLockCombined)

	run, attempts := scriptedFetch(t, gitErr)
	_, err := fetchWithRefLockRetry(ctx, "/repo", run, "fetch", "--prune", "origin")

	if errors.Is(err, ErrRefLockContended) {
		t.Error("a context-killed fetch was labelled ref-lock contention; the RPC boundary would answer CodeUnavailable for it")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the context error still reachable through errors.Is", err)
	}
	if got := attempts(); got != 1 {
		t.Errorf("attempts = %d, want exactly 1 — a dead context cannot be retried into a win", got)
	}
}

// Cancelled mid-backoff, after a genuine contention match. The race was real,
// but it is no longer the actionable fact: leading with ErrRefLockContended
// answers CodeUnavailable to a caller who is already gone, and buries the
// cancellation that whoever is deciding how loudly to log needs to see.
func TestFetchWithRefLockRetry_ContextEndedMidBackoffReportsTheContext(t *testing.T) {
	previous := refLockRetryBackoff
	t.Cleanup(func() { refLockRetryBackoff = previous })
	// Long enough that the cancellation below lands inside the wait rather than
	// racing the next attempt.
	refLockRetryBackoff = []time.Duration{30 * time.Second}

	ctx, cancel := context.WithCancel(context.Background())
	run, attempts := scriptedFetch(t, errors.New(refLockCombined))

	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	started := time.Now()
	_, err := fetchWithRefLockRetry(ctx, "/repo", run, "fetch", "origin", "main")
	if elapsed := time.Since(started); elapsed >= 30*time.Second {
		t.Fatalf("waited %s — the cancellation did not cut the backoff short", elapsed)
	}

	if errors.Is(err, ErrRefLockContended) {
		t.Error("a cancelled wait was labelled ref-lock contention; the RPC boundary would answer CodeUnavailable for it")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled reachable through errors.Is", err)
	}
	if !strings.Contains(err.Error(), "cannot lock ref") {
		t.Errorf("err = %v, want git's own message kept behind the context error", err)
	}
	if got := attempts(); got != 1 {
		t.Errorf("attempts = %d, want exactly 1 — the retry never ran", got)
	}
}
