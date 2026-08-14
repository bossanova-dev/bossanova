package gitremote

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// errTransient and errTerminal are the two error shapes every retry test needs:
// one the classifier recognises, one it must not. Neither carries git's trailing
// period, which staticcheck's ST1005 rejects on a package-level error var; the
// classifier's fidelity to git's real punctuation is proved by the tables in
// gitremote_test.go instead.
var (
	errTransient = errors.New("git fetch origin: exit status 128: git@github.com: Permission denied (publickey)")
	errTerminal  = errors.New("git push origin HEAD: exit status 1: error: failed to push some refs")
)

// sleepRecorder is the injected sleep seam. Every test in this file uses it (or
// a deliberately-tiny real duration) so the package's test binary never waits on
// the production backoff ladder.
type sleepRecorder struct {
	waits  []time.Duration
	calls  int
	onCall func(call int) error
}

func (s *sleepRecorder) sleep(ctx context.Context, d time.Duration) error {
	s.calls++
	s.waits = append(s.waits, d)
	if s.onCall != nil {
		if err := s.onCall(s.calls); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// countingOp returns an op that yields results in order, then repeats the last.
func countingOp(calls *int, results ...error) func(context.Context) error {
	return func(context.Context) error {
		*calls++
		if *calls <= len(results) {
			return results[*calls-1]
		}
		return results[len(results)-1]
	}
}

func TestDefaultPolicy(t *testing.T) {
	p := DefaultPolicy()
	if p.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", p.Attempts)
	}
	if p.Base != time.Second {
		t.Errorf("Base = %s, want 1s", p.Base)
	}
	if p.Factor != 3 {
		t.Errorf("Factor = %v, want 3", p.Factor)
	}
	if p.Jitter != 0.2 {
		t.Errorf("Jitter = %v, want 0.2", p.Jitter)
	}
	// The determinism seams stay nil so production gets the real timer and
	// math/rand/v2; only tests fill them in.
	if p.Sleep != nil {
		t.Error("Sleep seam = non-nil, want nil so production uses the real timer")
	}
	if p.Rand != nil {
		t.Error("Rand seam = non-nil, want nil so production uses math/rand/v2")
	}
}

func TestDoSucceedsOnFirstAttempt(t *testing.T) {
	sleeps := &sleepRecorder{}
	policy := DefaultPolicy()
	policy.Sleep = sleeps.sleep

	calls := 0
	if err := Do(context.Background(), policy, countingOp(&calls, nil)); err != nil {
		t.Fatalf("Do() = %v, want nil", err)
	}
	if calls != 1 {
		t.Errorf("attempts = %d, want 1", calls)
	}
	if len(sleeps.waits) != 0 {
		t.Errorf("waits = %v, want none", sleeps.waits)
	}
}

// TestDoTerminalErrorMakesOneAttemptAndReturnsUnwrapped is an acceptance
// criterion: a terminal failure must cost exactly one attempt and must surface
// as itself, not dressed up in AttemptsError.
func TestDoTerminalErrorMakesOneAttemptAndReturnsUnwrapped(t *testing.T) {
	sleeps := &sleepRecorder{}
	policy := DefaultPolicy()
	policy.Sleep = sleeps.sleep

	calls := 0
	err := Do(context.Background(), policy, countingOp(&calls, errTerminal))

	if !errors.Is(err, errTerminal) {
		t.Fatalf("Do() = %v, want %v", err, errTerminal)
	}
	// Identity, not errors.Is: the contract is that the error is returned
	// unwrapped, and errors.Is would also pass on a wrapped one.
	if err != errTerminal {
		t.Fatalf("Do() = %v (type %T), want the raw error unwrapped", err, err)
	}
	var attemptsErr *AttemptsError
	if errors.As(err, &attemptsErr) {
		t.Fatalf("Do() wrapped a single-attempt terminal error in %T", attemptsErr)
	}
	if calls != 1 {
		t.Errorf("attempts = %d, want exactly 1", calls)
	}
	if len(sleeps.waits) != 0 {
		t.Errorf("waits = %v, want none for a terminal error", sleeps.waits)
	}
}

func TestDoRetriesTransientThenSucceeds(t *testing.T) {
	sleeps := &sleepRecorder{}
	policy := DefaultPolicy()
	policy.Sleep = sleeps.sleep

	calls := 0
	if err := Do(context.Background(), policy, countingOp(&calls, errTransient, nil)); err != nil {
		t.Fatalf("Do() = %v, want nil", err)
	}
	if calls != 2 {
		t.Errorf("attempts = %d, want 2", calls)
	}
	if len(sleeps.waits) != 1 {
		t.Fatalf("waits = %v, want exactly one", sleeps.waits)
	}
}

// TestDoExhaustsAtDefaultPolicyAttempts is an acceptance criterion: three
// attempts, two waits, and an error whose text says "after 3 attempts over" so a
// genuine misconfiguration cannot be mistaken for one flaky call.
func TestDoExhaustsAtDefaultPolicyAttempts(t *testing.T) {
	sleeps := &sleepRecorder{}
	policy := DefaultPolicy()
	policy.Sleep = sleeps.sleep

	calls := 0
	err := Do(context.Background(), policy, countingOp(&calls, errTransient))

	if err == nil {
		t.Fatal("Do() = nil, want an exhaustion error")
	}
	if calls != DefaultPolicy().Attempts {
		t.Errorf("attempts = %d, want %d", calls, DefaultPolicy().Attempts)
	}
	if len(sleeps.waits) != DefaultPolicy().Attempts-1 {
		t.Errorf("waits = %v, want %d", sleeps.waits, DefaultPolicy().Attempts-1)
	}
	if !strings.Contains(err.Error(), "after 3 attempts over") {
		t.Errorf("Do() error = %q, want it to contain %q", err.Error(), "after 3 attempts over")
	}

	var attemptsErr *AttemptsError
	if !errors.As(err, &attemptsErr) {
		t.Fatalf("Do() = %v (type %T), want *AttemptsError", err, err)
	}
	if attemptsErr.Attempts != 3 {
		t.Errorf("AttemptsError.Attempts = %d, want 3", attemptsErr.Attempts)
	}
	if !errors.Is(err, errTransient) {
		t.Errorf("errors.Is(err, errTransient) = false; the underlying error must stay reachable")
	}
	if !errors.Is(attemptsErr.Unwrap(), errTransient) {
		t.Errorf("Unwrap() = %v, want %v", attemptsErr.Unwrap(), errTransient)
	}
}

// TestDoContextCancelledDuringBackoffAborts is an acceptance criterion: the
// caller's deadline outranks the retry ladder, so a cancellation mid-wait ends
// the run with the context error and never spends another attempt.
func TestDoContextCancelledDuringBackoffAborts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sleeps := &sleepRecorder{onCall: func(int) error {
		cancel()
		return nil // fall through to the seam's own ctx.Err() check
	}}
	policy := DefaultPolicy()
	policy.Sleep = sleeps.sleep

	calls := 0
	err := Do(ctx, policy, countingOp(&calls, errTransient))

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Do() = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Errorf("attempts = %d, want exactly 1 — no attempt may follow the cancellation", calls)
	}
	if len(sleeps.waits) != 1 {
		t.Errorf("waits = %v, want exactly one", sleeps.waits)
	}
	var attemptsErr *AttemptsError
	if errors.As(err, &attemptsErr) {
		t.Errorf("Do() wrapped the context error in %T; the context error must surface as itself", attemptsErr)
	}
}

// TestDoStopsWhenASleepReportsACleanWaitOnACancelledContext pins the half of that
// criterion a cooperative seam cannot express. sleepWithTimer selects over
// ctx.Done() and the timer, and when both are ready select picks uniformly — so a
// real cancellation landing on the timer's own tick returns nil, reporting a wait
// that completed on a context that did not. Do must not spend an attempt on that.
//
// The seam here models exactly that lost race: cancel, then report success. It is
// deliberately NOT sleepRecorder, whose sleep ends with its own ctx.Err() check
// and so can never reproduce the nil-on-cancelled return.
func TestDoStopsWhenASleepReportsACleanWaitOnACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	policy := DefaultPolicy()
	waits := 0
	policy.Sleep = func(context.Context, time.Duration) error {
		waits++
		cancel()
		return nil
	}

	calls := 0
	err := Do(ctx, policy, countingOp(&calls, errTransient))

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Do() = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Errorf("attempts = %d, want exactly 1 — a wait that reports nil on a cancelled context must not buy another attempt", calls)
	}
	if waits != 1 {
		t.Errorf("waits = %d, want exactly 1", waits)
	}
	var attemptsErr *AttemptsError
	if errors.As(err, &attemptsErr) {
		t.Errorf("Do() wrapped the context error in %T; the context error must surface as itself", attemptsErr)
	}
}

func TestDoReturnsContextErrorBeforeFirstAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	sleeps := &sleepRecorder{}
	policy := DefaultPolicy()
	policy.Sleep = sleeps.sleep

	calls := 0
	err := Do(ctx, policy, countingOp(&calls, nil))

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Do() = %v, want context.Canceled", err)
	}
	if calls != 0 {
		t.Errorf("attempts = %d, want 0 for an already-done context", calls)
	}
}

// TestDoWrapsTerminalErrorAfterARetry pins the other half of the unwrapped rule:
// once the ladder has been entered, even a terminal final error reports the
// attempt count, because the run really did cost more than one attempt.
func TestDoWrapsTerminalErrorAfterARetry(t *testing.T) {
	sleeps := &sleepRecorder{}
	policy := DefaultPolicy()
	policy.Sleep = sleeps.sleep

	calls := 0
	err := Do(context.Background(), policy, countingOp(&calls, errTransient, errTerminal))

	if calls != 2 {
		t.Errorf("attempts = %d, want 2", calls)
	}
	var attemptsErr *AttemptsError
	if !errors.As(err, &attemptsErr) {
		t.Fatalf("Do() = %v (type %T), want *AttemptsError", err, err)
	}
	if attemptsErr.Attempts != 2 {
		t.Errorf("AttemptsError.Attempts = %d, want 2", attemptsErr.Attempts)
	}
	if !errors.Is(err, errTerminal) {
		t.Error("the terminal error must stay reachable through the wrapper")
	}
	if !strings.Contains(err.Error(), "after 2 attempts over") {
		t.Errorf("Do() error = %q, want it to contain %q", err.Error(), "after 2 attempts over")
	}
}

func TestDoWaitLadderWithoutJitter(t *testing.T) {
	sleeps := &sleepRecorder{}
	policy := DefaultPolicy()
	policy.Jitter = 0
	policy.Sleep = sleeps.sleep

	calls := 0
	_ = Do(context.Background(), policy, countingOp(&calls, errTransient))

	want := []time.Duration{time.Second, 3 * time.Second}
	if len(sleeps.waits) != len(want) {
		t.Fatalf("waits = %v, want %v", sleeps.waits, want)
	}
	for i, w := range want {
		if sleeps.waits[i] != w {
			t.Errorf("wait[%d] = %s, want %s", i, sleeps.waits[i], w)
		}
	}
}

// TestDoJitterBounds is an acceptance criterion: an injected Rand of 0 and 1
// must land exactly on the [0.8x, 1.2x] edges of the nominal ladder.
func TestDoJitterBounds(t *testing.T) {
	nominal := []time.Duration{time.Second, 3 * time.Second}

	for _, tc := range []struct {
		name  string
		rand  float64
		scale float64
	}{
		{name: "rand 0 hits the lower bound", rand: 0, scale: 0.8},
		{name: "rand 1 hits the upper bound", rand: 1, scale: 1.2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sleeps := &sleepRecorder{}
			policy := DefaultPolicy()
			policy.Sleep = sleeps.sleep
			policy.Rand = func() float64 { return tc.rand }

			calls := 0
			_ = Do(context.Background(), policy, countingOp(&calls, errTransient))

			if len(sleeps.waits) != len(nominal) {
				t.Fatalf("waits = %v, want %d entries", sleeps.waits, len(nominal))
			}
			for i, n := range nominal {
				want := time.Duration(float64(n) * tc.scale)
				if sleeps.waits[i] != want {
					t.Errorf("wait[%d] = %s, want %s (%.1fx of %s)", i, sleeps.waits[i], want, tc.scale, n)
				}
			}
		})
	}
}

// TestDoDefaultRandStaysWithinJitterBounds exercises the production math/rand/v2
// path (Rand seam unset) and asserts only the bounds, which is all that is
// deterministic about it.
func TestDoDefaultRandStaysWithinJitterBounds(t *testing.T) {
	nominal := []time.Duration{time.Second, 3 * time.Second}

	for range 25 {
		sleeps := &sleepRecorder{}
		policy := DefaultPolicy()
		policy.Sleep = sleeps.sleep // sleep is faked; Rand deliberately is not

		calls := 0
		_ = Do(context.Background(), policy, countingOp(&calls, errTransient))

		if len(sleeps.waits) != len(nominal) {
			t.Fatalf("waits = %v, want %d entries", sleeps.waits, len(nominal))
		}
		for i, n := range nominal {
			low := time.Duration(float64(n) * 0.8)
			high := time.Duration(float64(n) * 1.2)
			if sleeps.waits[i] < low || sleeps.waits[i] > high {
				t.Fatalf("wait[%d] = %s, want within [%s, %s]", i, sleeps.waits[i], low, high)
			}
		}
	}
}

func TestDoSingleAttemptPolicyReturnsRawTransientError(t *testing.T) {
	sleeps := &sleepRecorder{}
	policy := DefaultPolicy()
	policy.Attempts = 1
	policy.Sleep = sleeps.sleep

	calls := 0
	err := Do(context.Background(), policy, countingOp(&calls, errTransient))

	// Identity, not errors.Is: a one-attempt run must report no ladder at all.
	if err != errTransient {
		t.Fatalf("Do() = %v (type %T), want the raw error", err, err)
	}
	if calls != 1 {
		t.Errorf("attempts = %d, want 1", calls)
	}
	if len(sleeps.waits) != 0 {
		t.Errorf("waits = %v, want none", sleeps.waits)
	}
}

func TestDoZeroAttemptsPolicyStillMakesOneAttempt(t *testing.T) {
	sleeps := &sleepRecorder{}
	policy := Policy{Sleep: sleeps.sleep}

	calls := 0
	err := Do(context.Background(), policy, countingOp(&calls, errTransient))

	if calls != 1 {
		t.Errorf("attempts = %d, want 1 — a zero-value policy must still run the op once", calls)
	}
	if err == nil {
		t.Error("Do() = nil, want the op's error")
	}
	if len(sleeps.waits) != 0 {
		t.Errorf("waits = %v, want none", sleeps.waits)
	}
}

// TestDefaultSleepHonorsContextCancellation exercises the production timer path
// (Sleep seam unset). The nominal first wait is 1s; a cancelled context must cut
// it short immediately, which is what keeps the caller's deadline authoritative.
func TestDefaultSleepHonorsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	policy := DefaultPolicy() // Sleep unset on purpose: real timer

	calls := 0
	start := time.Now()
	err := Do(ctx, policy, func(context.Context) error {
		calls++
		cancel() // the caller's budget ends while the op is failing
		return errTransient
	})
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Do() = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Errorf("attempts = %d, want 1", calls)
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("Do() took %s, want a near-immediate return: the real timer ignored the cancellation", elapsed)
	}
}

// TestDefaultSleepWaitsWithARealTimer covers the timer's success branch with a
// deliberately tiny base so the package's test binary stays fast.
func TestDefaultSleepWaitsWithARealTimer(t *testing.T) {
	policy := Policy{Attempts: 2, Base: time.Millisecond, Factor: 1} // Sleep unset: real timer

	calls := 0
	start := time.Now()
	err := Do(context.Background(), policy, countingOp(&calls, errTransient, nil))
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Do() = %v, want nil", err)
	}
	if calls != 2 {
		t.Errorf("attempts = %d, want 2", calls)
	}
	if elapsed < time.Millisecond {
		t.Errorf("Do() took %s, want at least the 1ms base wait", elapsed)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("Do() took %s, far longer than the 1ms base wait", elapsed)
	}
}

func TestAttemptsError(t *testing.T) {
	cause := errors.New("boom")
	err := &AttemptsError{Attempts: 3, Elapsed: 4 * time.Second, Err: cause}

	if !strings.Contains(err.Error(), "after 3 attempts over") {
		t.Errorf("Error() = %q, want it to contain %q", err.Error(), "after 3 attempts over")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("Error() = %q, want it to name the underlying failure", err.Error())
	}
	if !errors.Is(err, cause) {
		t.Error("errors.Is(err, cause) = false, want true")
	}
	if !errors.Is(err.Unwrap(), cause) {
		t.Errorf("Unwrap() = %v, want %v", err.Unwrap(), cause)
	}
}
