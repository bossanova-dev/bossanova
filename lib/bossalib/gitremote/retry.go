package gitremote

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"time"
)

// Policy bounds a retry ladder. The zero value is usable — Do treats it as a
// single attempt with the default ladder shape — but callers should start from
// DefaultPolicy and adjust.
//
// Sleep and Rand are determinism seams: tests inject them so the suite never
// waits on a real backoff and jitter is reproducible. Left nil, production gets a
// context-aware timer and math/rand/v2.
type Policy struct {
	// Attempts is the total number of tries, not the number of retries. Values
	// below 1 are treated as 1, so a zero-value Policy still runs the op once.
	//
	// N attempts means N-1 waits: DefaultPolicy's 3 attempts wait 1s and 3s and
	// then return the third failure. Do not raise this to compensate for a
	// classifier that is matching too much — see the package doc.
	Attempts int

	// Base is the first wait. Zero means DefaultPolicy's base.
	Base time.Duration

	// Factor multiplies the wait per attempt: wait(n) = Base * Factor^(n-1).
	// Values at or below zero mean DefaultPolicy's factor.
	Factor float64

	// Jitter is the fractional spread applied to each wait, so the actual wait
	// falls in [1-Jitter, 1+Jitter] x nominal. Zero disables jitter; values are
	// clamped to [0, 1] so a wait can never come out negative.
	Jitter float64

	// Sleep waits for d or returns early with the context's error. nil means a
	// real context-aware timer.
	Sleep func(ctx context.Context, d time.Duration) error

	// Rand returns a value in [0, 1) used to place the jitter within its band.
	// nil means math/rand/v2.
	Rand func() float64
}

// DefaultPolicy is the ladder every caller should start from: three attempts
// with 1s and 3s waits, spread by +/-20% jitter.
//
// It is deliberately short. The classifier includes genuinely ambiguous
// signatures (see the package doc), so the attempt count is what keeps a real
// misconfiguration cheap — roughly four seconds wasted before AttemptsError says
// so plainly.
func DefaultPolicy() Policy {
	return Policy{
		Attempts: 3,
		Base:     time.Second,
		Factor:   3,
		Jitter:   0.2,
	}
}

// AttemptsError reports a failure that survived a retry ladder. Its message
// appends "(after N attempts over D)" to the underlying error so a genuine
// misconfiguration reads as a repeated failure rather than one flaky call.
//
// Do returns it only when the run actually made more than one attempt; a
// single-attempt failure is returned unwrapped.
type AttemptsError struct {
	// Attempts is how many tries were actually made.
	Attempts int

	// Elapsed is wall-clock time across the whole run, including the operation
	// itself — the number an operator wants when asking what a session start
	// spent.
	Elapsed time.Duration

	// Err is the final attempt's error.
	Err error
}

func (e *AttemptsError) Error() string {
	return fmt.Sprintf("%v (after %d attempts over %s)", e.Err, e.Attempts, e.Elapsed.Round(time.Millisecond))
}

// Unwrap keeps the underlying failure reachable through errors.Is/errors.As, so
// wrapping never costs a caller its ability to inspect the real error.
func (e *AttemptsError) Unwrap() error { return e.Err }

// Do runs op, retrying only while its error is transient and attempts remain.
//
// The context is authoritative: Do returns ctx.Err() before the first attempt if
// the context is already done, and a cancellation during a backoff wait ends the
// run without spending another attempt. Retries can therefore only shorten the
// caller's budget, never extend it.
//
// The returned error is:
//   - nil, when an attempt succeeds;
//   - op's error unwrapped, when the run made exactly one attempt (a terminal
//     failure on the first try, or a single-attempt policy);
//   - ctx.Err(), when the context ended;
//   - *AttemptsError wrapping the last error, when the run made more than one
//     attempt. That holds even if the final error was terminal — the run really
//     did cost several attempts, and the count is the useful fact.
//
// The ctx.Err() case is bare on purpose: it is ctx.Err() alone, so op's own last
// error is NOT recoverable from Do's return once the context ends mid-backoff.
// A caller that wants to know what git actually said on a cancelled run has to
// observe it inside op — log it there, or record it in a closure — rather than
// expecting Do to carry it out.
func Do(ctx context.Context, policy Policy, op func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	limit := policy.attemptLimit()
	start := time.Now()

	var lastErr error
	made := 0
	for made < limit {
		made++

		lastErr = op(ctx)
		if lastErr == nil {
			return nil
		}
		if !IsTransient(lastErr) {
			break
		}
		if made == limit {
			break
		}
		if err := policy.sleepFor(ctx, policy.waitFor(made)); err != nil {
			return err
		}
		// A completed wait is not proof the context survived it. When the timer
		// and ctx.Done() become ready together, select picks uniformly, so
		// sleepWithTimer can report a clean wait on an already-cancelled context
		// — and without this check the next iteration would spend an attempt on
		// it, contradicting the contract above. Re-check before looping.
		if err := ctx.Err(); err != nil {
			return err
		}
	}

	if made <= 1 {
		return lastErr
	}
	return &AttemptsError{Attempts: made, Elapsed: time.Since(start), Err: lastErr}
}

// attemptLimit resolves Attempts, floored at a single attempt so a zero-value
// Policy still runs the operation.
func (p Policy) attemptLimit() int {
	if p.Attempts < 1 {
		return 1
	}
	return p.Attempts
}

// waitFor computes the wait before attempt+1: Base * Factor^(attempt-1), scaled
// into the jitter band.
func (p Policy) waitFor(attempt int) time.Duration {
	defaults := DefaultPolicy()

	base := p.Base
	if base <= 0 {
		base = defaults.Base
	}
	factor := p.Factor
	if factor <= 0 {
		factor = defaults.Factor
	}

	wait := float64(base) * math.Pow(factor, float64(attempt-1))

	if jitter := clampJitter(p.Jitter); jitter > 0 {
		// Place the wait within [1-jitter, 1+jitter] x nominal.
		wait *= 1 + jitter*(2*p.random()-1)
	}
	if wait <= 0 {
		return 0
	}
	return time.Duration(wait)
}

// clampJitter keeps the spread inside [0, 1] so a jittered wait is never
// negative, whatever a caller passes.
func clampJitter(jitter float64) float64 {
	if jitter <= 0 {
		return 0
	}
	if jitter > 1 {
		return 1
	}
	return jitter
}

// random resolves the Rand seam.
func (p Policy) random() float64 {
	if p.Rand != nil {
		return p.Rand()
	}
	// #nosec G404 -- backoff jitter; spreading retry timing needs no crypto-grade randomness and gates no security decision
	return rand.Float64()
}

// sleepFor resolves the Sleep seam.
func (p Policy) sleepFor(ctx context.Context, d time.Duration) error {
	if p.Sleep != nil {
		return p.Sleep(ctx, d)
	}
	return sleepWithTimer(ctx, d)
}

// sleepWithTimer is the production wait: it honours context cancellation, which
// is what keeps a caller's deadline ahead of the retry ladder.
func sleepWithTimer(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	// Both cases can be ready at once, and select then picks uniformly at random
	// — so a nil return here does NOT mean the context outlived the wait. Do
	// re-checks ctx.Err() after every wait for exactly that reason; do not lean
	// on this function to report a cancellation it may legally miss.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
