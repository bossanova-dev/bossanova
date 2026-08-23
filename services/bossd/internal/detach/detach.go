// Package detach owns bossd's two "run this detached from the caller, under a
// budget of its own" primitives: a singleflight join whose flight body cannot
// omit its panic recovery (Flight, over Group), and a bounded best-effort
// cleanup that survives the caller's cancellation (Cleanup).
//
// It exists because both shapes are needed in two packages that cannot import
// each other: services/bossd/internal/server, which imports
// services/bossd/internal/session, and internal/session itself. A helper in
// either one is unreachable from the other, so — like lib/bossalib/keyedgate,
// which resolved the identical constraint for BOS-717's keyed gates — the
// primitive lives in a third package both can import.
//
// # What Flight prevents
//
// A singleflight.Group joined with DoChan re-raises a flight panic on a bare
// goroutine (`go panic(e); select{}`) that no recover() in the process can
// reach, so one nil-deref inside a flight body takes the whole daemon down.
// A Do-joined group does not carry that obligation, and nothing about the two
// declarations distinguishes them. The mechanism, and the Do → DoChan
// migration that walks into it, are written up in
// docs/solutions/design-patterns/a-dochan-flight-body-must-recover-its-own-panic.md;
// this package is the enforcement rather than a second copy of the
// explanation. Group deliberately exposes neither Do nor DoChan, so the only
// way to join one is Flight, which always recovers. The obligation is
// contagious — one DoChan joiner puts every flight of that group into the
// fatal branch, including flights a Do caller started — which is why no Do
// passthrough is offered either.
//
// # What Cleanup prevents
//
// A best-effort compensation (a pane kill, a start-failure stamp) that runs on
// the caller's context is disarmed by the very deadline that makes it run:
// the commonest way to reach the cleanup is the budget above it expiring, and
// on an expired context exec.CommandContext refuses to start and the store
// refuses the write — both swallowed by design. See
// docs/solutions/design-patterns/attaching-a-deadline-to-a-path-disarms-the-best-effort-cleanup-still-on-it.md.
// Duplicating the WithoutCancel-plus-timeout dance per call site is what
// produced three identical five-second constants across two packages, so the
// budget has one name here (CleanupBudget) and the derivation has one
// implementation.
package detach

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/sync/singleflight"
)

// ErrEmptyKey is returned by Flight when the caller supplies an empty key.
//
// It is refused rather than tolerated because the failure is silent and
// cross-cutting: on an empty key EVERY concurrent flight of a group coalesces
// into one regardless of what it was keyed on, so a caller can receive a
// completed result for work that was never run on its behalf.
var ErrEmptyKey = errors.New("detach: flight key must not be empty")

// ErrResultType is returned by Flight when the flight's result is not a T.
//
// singleflight carries results as `any`, so the generic parameter does not
// remove the runtime assertion — it is reachable whenever two callers join the
// same group and key with different T, where the joiner receives the leader's
// value. It is a real error path, not an impossible one.
var ErrResultType = errors.New("detach: unexpected flight result type")

// Group is a singleflight group whose flights can only be joined through
// Flight. It exposes neither Do nor DoChan: the panic obligation DoChan
// carries is contagious across a group, so the safe join is the only join.
//
// The zero value is usable. A Group must not be copied after first use.
type Group struct {
	g singleflight.Group
}

// FlightOption configures one Flight call. The parameter is variadic so that
// every existing call site keeps compiling and behaving exactly as it did: a
// Flight with no options is the unmodified primitive.
type FlightOption func(*flightConfig)

// flightConfig is the resolved option set for one Flight call. It is per-call
// and never shared, so an option cannot leak across flights or across callers
// joined to the same flight.
type flightConfig struct {
	onAttach func(key string)
}

// WithAttachHook registers fn to run on the CALLING goroutine at the instant
// that caller has attached to the flight for key — after DoChan returns and
// before the caller-owned select. It fires once per attached caller, not once
// per flight: a coalesced flight with a leader and a joiner invokes it twice.
//
// It exists so a test can observe "a caller has joined this flight" instead of
// sleeping and hoping (BOS-951). Production call sites pass no option at all.
//
// fn runs synchronously and inline, so it must not block: a hook that blocks —
// a send on a full channel, say — hangs the caller inside Flight with no
// diagnostic. A nil fn is accepted and never invoked.
//
// Nothing gates that obligation generally. internal/server ratchets its own
// seam — TestSwitchFlightAttachedIsAssignedOnlyByTests fails the build on any
// production assignment to Server.switchFlightAttached — but that gate is
// scoped to that package, and it is sufficient there only because Flight has
// exactly one production call site today. A second package adopting Flight
// carries the obligation itself, or brings its own ratchet.
func WithAttachHook(fn func(key string)) FlightOption {
	return func(c *flightConfig) { c.onAttach = fn }
}

// Flight runs fn once per in-flight key on g, detached from ctx's cancellation
// and bounded by budget, and returns the result to every caller joined to that
// flight.
//
// Go forbids type-parameterized methods, so this is a package-level function
// over *Group rather than a method on it.
//
// The order of operations is load-bearing:
//
//  1. An empty key is refused with ErrEmptyKey before anything is seeded.
//  2. An already-dead caller is refused with its own ctx.Err() before a flight
//     is seeded, so a caller that has already given up never STARTS detached
//     work it will immediately be told it did not get. The guard is narrow by
//     design: it fences only the caller that would create the flight. A joiner
//     arriving dead is harmless (it attaches to work someone else wanted), and
//     a caller leaving mid-flight still leaves the leader running.
//  3. The flight body recovers its own panic (see the package doc) and turns it
//     into the flight's error, so every waiter is unblocked normally.
//  4. fn runs on a context derived with WithoutCancel from the LEADER's ctx and
//     bounded by budget. Detaching is the point: the leader is just whoever won
//     the race, and letting its departure cancel work a healthy joiner is still
//     waiting on is the defect this shape exists to remove. It also makes
//     budget the single authority over the flight's lifetime.
//  5. Any WithAttachHook option then fires, once per caller, on the caller's
//     own goroutine. The placement is load-bearing rather than incidental:
//     DoChan appends the caller's channel to the call's chans slice UNDER the
//     group's mutex and only then returns, so attachment is complete the
//     instant DoChan returns. A signal fired BEFORE the call cannot distinguish
//     "this caller reached the call" from "this caller attached to the live
//     flight", and only the second is an attachment. It follows that the hook
//     does NOT fire on the two paths above that return before DoChan — the
//     ErrEmptyKey guard and the already-dead-caller fence — because neither of
//     them attaches to anything.
//  6. Each caller then waits on its OWN context versus the result, so no caller
//     inherits another's clock.
//
// Errors are returned VERBATIM and never wrapped — neither fn's error nor the
// caller's context error. Callers classify them with errors.Is against their
// own sentinels, and a wrap here would silently reclassify every one of them.
func Flight[T any](
	ctx context.Context,
	g *Group,
	logger zerolog.Logger,
	key string,
	budget time.Duration,
	fn func(context.Context) (T, error),
	opts ...FlightOption,
) (T, error) {
	var cfg flightConfig
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	var zero T
	if key == "" {
		return zero, ErrEmptyKey
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}

	// The body names its error result so the deferred recover below can replace
	// the return value, and so the unwrap after the select can tell "the work
	// failed" from "the work returned something unexpected".
	ch := g.g.DoChan(key, func() (_ any, err error) {
		defer func() {
			r := recover()
			if r == nil {
				return
			}
			logger.Error().
				Interface("panic", r).
				Str("flightKey", key).
				Str("stack", string(debug.Stack())).
				Msg("detached flight panicked; recovered in the singleflight body")
			err = fmt.Errorf("detached flight panicked: %v", r)
		}()
		budgetCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
		defer cancel()
		return fn(budgetCtx)
	})

	// This caller is now attached — DoChan appended its channel under the
	// group's mutex before returning — so the signal here is a true attach
	// event rather than a "reached the call" one. See step 5 of the order of
	// operations above for why nothing earlier would do.
	if cfg.onAttach != nil {
		cfg.onAttach(key)
	}

	select {
	case <-ctx.Done():
		return zero, ctx.Err()
	case r := <-ch:
		// Ahead of the assertion deliberately: a body that failed returns the
		// zero T alongside its error, and asserting on that would report a type
		// problem where there is only an ordinary failure.
		if r.Err != nil {
			return zero, r.Err
		}
		typed, ok := r.Val.(T)
		if !ok {
			return zero, fmt.Errorf("%w: %T", ErrResultType, r.Val)
		}
		return typed, nil
	}
}

// CleanupBudget is the deadline every detached best-effort cleanup in bossd
// runs under. One name for one meaning: "long enough to kill a pane or write a
// row, short enough that a wedged cleanup cannot outlive the failure it is
// recording."
const CleanupBudget = 5 * time.Second

// Cleanup runs fn on a context detached from ctx's cancellation, carrying its
// values, and bounded by budget. The derived context is cancelled once fn
// returns, including when fn panics.
//
// Cleanup runs the work rather than handing back a (context, cancel) pair on
// purpose. govet's lostcancel analyzer flags a dropped cancel from an inline
// context.WithTimeout but does not follow one through a wrapper, so a
// pair-returning helper would silently retire that check at every call site
// while looking like a pure refactor. There is no cancel to drop here.
func Cleanup(ctx context.Context, budget time.Duration, fn func(context.Context)) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
	defer cancel()
	fn(cleanupCtx)
}
