package git

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/gitremote"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// Transient and terminal stderr fragments, spelled the way git and OpenSSH
// actually emit them. transientStderr is the signature that fired eight times in
// the BOS-873 incident window; terminalStderr is deliberately the DISTINCTIVE
// line ALONE — git appends "fatal: Could not read from remote repository." to a
// real SSH-phase failure, and that line is itself a transient signature, so a
// composite would classify transient and prove nothing about the terminal path.
const (
	transientStderr = "exit status 128: git@github.com: Permission denied (publickey)."
	terminalStderr  = "exit status 128: ERROR: Repository not found."
)

// scriptedGitRemote installs a zero-sleep retry policy and a scripted
// replacement for the single git invocation runGitRemote retries, restoring both
// when the test ends.
//
// Zero-sleep is not an optimisation: the real ladder waits 1s then 3s, so a
// handful of these tests would add ~10s of pure sleeping to the package's wall
// clock. Injecting Policy.Sleep exercises the identical control flow with none
// of the waiting. Attempts stays at DefaultPolicy's 3 because the exhausted-error
// text under test names that number.
//
// results is consumed one entry per attempt; running past its end fails the test
// rather than silently repeating the last entry, so an unexpected extra attempt
// is loud. The returned func reports how many attempts were actually made.
//
// These tests swap PACKAGE-LEVEL vars and must therefore never be made parallel:
// t.Cleanup restoration is only sound while the package's tests run one at a
// time, which they do today (no t.Parallel anywhere in package git).
func scriptedGitRemote(t *testing.T, results ...error) (attempts func() int, argsFor func(int) []string) {
	t.Helper()

	previousPolicy := gitRemoteRetryPolicy
	previousFn := runGitRemoteFn
	t.Cleanup(func() {
		gitRemoteRetryPolicy = previousPolicy
		runGitRemoteFn = previousFn
	})

	policy := gitremote.DefaultPolicy()
	policy.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	gitRemoteRetryPolicy = policy

	var made int
	var recorded [][]string
	runGitRemoteFn = func(_ context.Context, _ string, args ...string) (string, error) {
		if made >= len(results) {
			t.Errorf("git %s: attempt %d exceeds the %d scripted results", strings.Join(args, " "), made+1, len(results))
			return "", errors.New("unscripted attempt")
		}
		err := results[made]
		made++
		recorded = append(recorded, append([]string(nil), args...))
		return "", err
	}

	return func() int { return made }, func(i int) []string {
		if i >= len(recorded) {
			t.Fatalf("no recorded args for attempt %d; only %d attempts were made", i+1, len(recorded))
		}
		return recorded[i]
	}
}

// A transient fetch failure followed by a success is the incident case: five
// session bootstraps died on exactly this, one attempt each.
func TestFetchBaseRetriesTransientFailureThenSucceeds(t *testing.T) {
	attempts, _ := scriptedGitRemote(t, errors.New(transientStderr), nil)

	if err := NewManager(zerolog.Nop()).FetchBase(context.Background(), t.TempDir(), "main"); err != nil {
		t.Fatalf("FetchBase after a transient first failure: %v, want nil", err)
	}
	if got := attempts(); got != 2 {
		t.Fatalf("attempts = %d, want exactly 2 (one failure, one success)", got)
	}
}

// The other half of the classifier's contract, and the one that keeps a real
// misconfiguration cheap: a terminal failure must not spend the ladder.
func TestFetchBaseDoesNotRetryTerminalFailure(t *testing.T) {
	attempts, _ := scriptedGitRemote(t, errors.New(terminalStderr))

	err := NewManager(zerolog.Nop()).FetchBase(context.Background(), t.TempDir(), "main")
	if err == nil {
		t.Fatal("FetchBase returned nil for a terminal failure, want an error")
	}
	if got := attempts(); got != 1 {
		t.Fatalf("attempts = %d, want exactly 1 — a terminal failure must not be retried", got)
	}
	if strings.Contains(err.Error(), "after") && strings.Contains(err.Error(), "attempts over") {
		t.Fatalf("FetchBase error = %q, want no attempts suffix on a single-attempt failure", err)
	}
}

// The exhausted-ladder text is the operator-facing half of this change: without
// the attempts suffix, a genuinely broken remote reads as a mystery hang. The
// caller's own "push:" wrapper must survive alongside it, unchanged.
func TestPushExhaustsRetriesAndReportsAttemptCount(t *testing.T) {
	repoDir := initTestRepo(t)
	attempts, _ := scriptedGitRemote(t,
		errors.New(transientStderr),
		errors.New(transientStderr),
		errors.New(transientStderr),
	)

	err := NewManager(zerolog.Nop()).Push(context.Background(), repoDir, "main")
	if err == nil {
		t.Fatal("Push returned nil after an exhausted ladder, want an error")
	}
	if got := attempts(); got != 3 {
		t.Fatalf("attempts = %d, want DefaultPolicy's 3", got)
	}
	if !strings.Contains(err.Error(), "push:") {
		t.Fatalf("Push error = %q, want the caller's own \"push:\" wrapper preserved", err)
	}
	if !strings.Contains(err.Error(), "after 3 attempts over") {
		t.Fatalf("Push error = %q, want the AttemptsError suffix \"after 3 attempts over\"", err)
	}
}

// The draft-PR half of the incident: two sessions carried a permanent-looking
// blocked reason because a single flaky push was treated as terminal.
func TestPushRetriesTransientFailureThenSucceeds(t *testing.T) {
	repoDir := initTestRepo(t)
	attempts, _ := scriptedGitRemote(t, errors.New(transientStderr), nil)

	if err := NewManager(zerolog.Nop()).Push(context.Background(), repoDir, "main"); err != nil {
		t.Fatalf("Push after a transient first failure: %v, want nil", err)
	}
	if got := attempts(); got != 2 {
		t.Fatalf("attempts = %d, want exactly 2 (one failure, one success)", got)
	}
}

// LOAD-BEARING: retrying --force-with-lease is only safe while the lease is
// re-evaluated fresh on each attempt. Two things are pinned here that the
// real-git TestPushWithLeaseRejectsStaleExpectedRemoteHead cannot see, because
// it never reaches a second attempt:
//
//  1. a retry does not launder a stale lease into a success, and
//  2. the retry sends a BYTE-IDENTICAL --force-with-lease argument, so it cannot
//     silently widen the expectation it is asserting.
func TestPushWithLeaseRetriesTransientFailureAndStillRejectsStaleLease(t *testing.T) {
	repoDir := initTestRepo(t)
	staleRejection := "exit status 1: ! [rejected] HEAD -> refs/heads/main (stale info)\nerror: failed to push some refs to 'origin'"
	attempts, argsFor := scriptedGitRemote(t, errors.New(transientStderr), errors.New(staleRejection))

	head := gitOutput(t, repoDir, "rev-parse", "HEAD")
	_, err := NewManager(zerolog.Nop()).PushWithLease(context.Background(), repoDir, "main", head)
	if err == nil {
		t.Fatal("PushWithLease returned nil for a stale lease, want the rejection")
	}
	if got := attempts(); got != 2 {
		t.Fatalf("attempts = %d, want exactly 2 — the transient failure is retried, the stale lease is not", got)
	}
	if !strings.Contains(err.Error(), "push with lease:") || !strings.Contains(err.Error(), "stale info") {
		t.Fatalf("PushWithLease error = %q, want the stale-lease rejection preserved", err)
	}

	first, second := leaseArg(t, argsFor(0)), leaseArg(t, argsFor(1))
	if first != second {
		t.Fatalf("lease argument changed across the retry: %q then %q; a retry must re-send the SAME expectation", first, second)
	}
	if !strings.HasSuffix(first, ":"+head) {
		t.Fatalf("lease argument = %q, want it to pin the caller's expected remote SHA %s", first, head)
	}
}

// leaseArg extracts the --force-with-lease argument from a recorded push argv.
func leaseArg(t *testing.T, args []string) string {
	t.Helper()
	for _, arg := range args {
		if strings.HasPrefix(arg, "--force-with-lease=") {
			return arg
		}
	}
	t.Fatalf("no --force-with-lease argument in %v", args)
	return ""
}

// The amplifier from the incident: when the batched ls-remote failed, every
// session start degraded to a per-candidate probe — one EXTRA ssh handshake
// during exactly the window when handshakes were failing. Retrying the batch is
// what stops that firing on a blip, so this asserts the fallback is not reached
// at all.
func TestAvailableNewBranchNameDoesNotDegradeWhenBatchedProbeRetrySucceeds(t *testing.T) {
	repoDir := initTestRepo(t)
	attempts, _ := scriptedGitRemote(t, errors.New(transientStderr), nil)

	mgr := NewManager(zerolog.Nop())
	probes := 0
	mgr.remoteBranchProbe = func(_ context.Context, _, _ string) bool {
		probes++
		return false
	}

	branch, err := mgr.availableNewBranchName(context.Background(), repoDir, "foo", true)
	if err != nil {
		t.Fatalf("availableNewBranchName: %v", err)
	}
	if branch != "foo" {
		t.Fatalf("branch = %q, want foo — the retried batch reported no collision", branch)
	}
	if got := attempts(); got != 2 {
		t.Fatalf("batched probe attempts = %d, want exactly 2 (one failure, one success)", got)
	}
	if probes != 0 {
		t.Fatalf("per-candidate probes = %d, want 0 — a retried batch must not degrade to the fallback", probes)
	}
}

// captureGlobalLog redirects the zerolog global — the logger runGitRemote uses,
// because it is a package function with no Manager to take m.logger from — into
// a buffer for the duration of one test. The global level is pinned too, so the
// assertion cannot pass or fail on whatever level another test left behind.
func captureGlobalLog(t *testing.T) func() string {
	t.Helper()

	previousLogger := log.Logger
	previousLevel := zerolog.GlobalLevel()
	t.Cleanup(func() {
		log.Logger = previousLogger
		zerolog.SetGlobalLevel(previousLevel)
	})

	var buf bytes.Buffer
	log.Logger = zerolog.New(&buf)
	zerolog.SetGlobalLevel(zerolog.TraceLevel)

	return buf.String
}

// The one path where the returned error changes shape: gitremote.Do answers a
// context that ended mid-backoff with a BARE ctx.Err(), so git's own error is
// unreachable through it. That is precisely the incident scenario — a bootstrap
// deadline expiring inside the ladder — and left alone the operator would see
// "context canceled" where they used to see the publickey failure. This pins
// both halves of the repair: the warning is logged, and the returned error
// carries git's message back out while still answering errors.Is for the
// context cause.
func TestRunGitRemoteLogsTheGitErrorWhenTheLadderIsAbandonedMidBackoff(t *testing.T) {
	attempts, _ := scriptedGitRemote(t, errors.New(transientStderr))
	logged := captureGlobalLog(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The sleep seam is the only point between attempt 1 and attempt 2, so
	// cancelling here reproduces "the caller's deadline expired during a backoff
	// wait" exactly. Assigning after scriptedGitRemote is safe: its Cleanup
	// restores the whole policy value.
	gitRemoteRetryPolicy.Sleep = func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}

	err := NewManager(zerolog.Nop()).FetchBase(ctx, t.TempDir(), "main")
	if err == nil {
		t.Fatal("FetchBase returned nil on a cancelled ladder, want an error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("FetchBase error = %q, want it to wrap context.Canceled", err)
	}
	if got := attempts(); got != 1 {
		t.Fatalf("attempts = %d, want exactly 1 — a cancelled wait must not spend another attempt", got)
	}
	// The half that matters to every caller: git's message survives the bare
	// ctx.Err() gitremote.Do hands back, so a wrapped error's text — and the
	// blocked reason persisted from it — still carries a signature the shared
	// classifier can read.
	if !strings.Contains(err.Error(), "publickey") {
		t.Fatalf("FetchBase error = %q, want it to re-attach git's own error", err)
	}
	out := logged()
	if !strings.Contains(out, "ladder abandoned") {
		t.Fatalf("global log = %q, want the abandoned-ladder warning", out)
	}
	if !strings.Contains(out, "publickey") {
		t.Fatalf("global log = %q, want it to name the git error that ended the ladder", out)
	}
}

// remoteBranchExists collapses "no such branch" and "could not ask" into the
// same false, and availableNewBranchName's per-candidate fallback relies on that
// fail-open. The error is DISCARDED, so nothing else would notice a regression
// here — which is exactly why it needs a test.
func TestRemoteBranchExistsFailsOpenAfterAnExhaustedLadder(t *testing.T) {
	attempts, _ := scriptedGitRemote(t,
		errors.New(transientStderr),
		errors.New(transientStderr),
		errors.New(transientStderr),
	)

	if remoteBranchExists(context.Background(), t.TempDir(), "feature") {
		t.Fatal("remoteBranchExists = true after an exhausted ladder, want false")
	}
	if got := attempts(); got != 3 {
		t.Fatalf("attempts = %d, want DefaultPolicy's 3 before failing open", got)
	}
}

// The other error-swallowing branch: CountMergeCommits treats an unreachable
// head as "not pushed yet" and falls back to the local branch. Retrying must not
// change that outcome, only how many attempts it takes to reach it.
func TestCountMergeCommitsFallsBackToLocalHeadAfterAnExhaustedFetch(t *testing.T) {
	repoDir := initTestRepo(t)
	mustGit(t, repoDir, "branch", "feature")

	// One result for FetchBase's base fetch, then the head fetch's full ladder.
	attempts, _ := scriptedGitRemote(t,
		nil,
		errors.New(transientStderr),
		errors.New(transientStderr),
		errors.New(transientStderr),
	)

	count, err := NewManager(zerolog.Nop()).CountMergeCommits(context.Background(), repoDir, "main", "feature")
	if err != nil {
		t.Fatalf("CountMergeCommits: %v, want the local-branch fallback to succeed", err)
	}
	if count != 0 {
		t.Fatalf("merge commits = %d, want 0", count)
	}
	if got := attempts(); got != 4 {
		t.Fatalf("attempts = %d, want 4 (one base fetch, then three for the head fetch)", got)
	}
}
