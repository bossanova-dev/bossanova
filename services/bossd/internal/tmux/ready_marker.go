package tmux

// The composer-readiness wait: the gate every delivery passes through before
// its first keystroke. The poll that waits for a live composer, the three
// verdicts it can return, and the retry loop BOS-895 wrapped around it. It was
// split out of tmux.go, which had grown past 1700 lines, because this is the
// one region with its own invariants — which failures may be re-run and which
// must never be — and those read better with a file boundary around them than
// buried in the middle of the general tmux client.

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// waitForReadyMarker polls CapturePane until a live composer is observed or the
// deadline elapses. The first poll is immediate so already-ready sessions return
// without sleeping.
//
// Readiness is two conditions, checked against the same capture in this order,
// so the gate adds no tmux calls:
//
//  1. A row whose leading glyph is the ready marker exists — a drawn input box,
//     not the glyph occurring somewhere in the capture.
//  2. That row is not part of a modal. A modal is refused — not waited out —
//     with ErrBlockedByModal / OutcomeBlockedByModal, because the alternative is
//     an Enter into a menu whose side effect is unbounded. One such Enter
//     selected "Update now" on a codex update interstitial, and the reinstall
//     killed the pane and destroyed the chat (BOS-600).
//
// The order is load-bearing, not cosmetic. Condition 2 is answered by the
// agent's plugin over gRPC, so asking it first would cost one round-trip per
// poll tick — up to ~50 per send — and would need a memoizer to claw that back.
// Asking it only about a capture that already satisfies condition 1 makes the
// probe at-most-once per wait BY CONSTRUCTION: either no marker row is drawn
// (keep polling, no RPC at all) or one is, and the probe's answer ends the loop
// in both directions. This costs nothing in coverage — every modal that draws a
// marker-shaped row, which includes both captured fixtures, is still probed on
// the tick it appears — and the deadline branch below probes once more so a
// modal that draws no marker row at all is still NAMED rather than reported as
// a bare timeout.
//
// Refusing is deliberately conservative: it fails loud rather than guessing, and
// it never dismisses the modal (pressing Escape into an unknown TUI state is the
// same gamble). A composer whose text happens to match an agent's menu grammar
// would be refused too — a new, visible failure mode this trade accepts.
//
// On timeout, the error embeds the most recent successful pane capture
// (truncated). This matters for the cron path: the caller kills the
// tmux session as failure cleanup, so without this snapshot the operator
// has no way to see what Claude was actually showing — auth prompt,
// update banner, missing binary, or just slow startup.
//
// It also embeds how the capturing itself went — how many capture-pane calls
// succeeded, how many failed, over what elapsed wait, and tmux's own words for
// the last failure. Without that, a pane nothing ever read and a pane read and
// found blank are the same string, and the operator cannot tell a slow boot from
// a session that was never there to look at.
func (c *Client) waitForReadyMarker(ctx context.Context, sessionName string, opts sendPlanOpts) error {
	start := time.Now()
	deadline := start.Add(opts.deadline)
	var lastPane string
	// Capture accounting (BOS-892). Without it a pane that was never read and a
	// pane that was read and found blank both leave lastPane == "" and render
	// identically as `<empty>`, which is the difference between "the agent is
	// still booting" and "we never successfully looked at the pane".
	var capturesOK, capturesFailed int
	var lastCaptureErr error
	for {
		out, err := c.CapturePane(ctx, sessionName)
		if err == nil {
			capturesOK++
			lastPane = out
			if composerRowIndex(out, opts.readyMarker) != -1 {
				if paneShowsModal(ctx, opts.modalDetector, out) {
					return blockedByModalErr(sessionName, out)
				}
				return nil
			}
		} else {
			capturesFailed++
			lastCaptureErr = err
			// A failed capture has two causes and only one of them is worth
			// waiting out. Ask tmux directly, and act only on a DEFINITE "no
			// such session": an indeterminate answer (the probe itself errored)
			// keeps polling, because guessing the pane is gone would turn a
			// transient tmux hiccup into an abandoned session. This costs one
			// extra tmux call per FAILED capture only — a healthy pane never
			// reaches this branch — and it is what stops the retry added in
			// BOS-895 from spending a second full budget waiting for a session
			// that no longer exists.
			if alive, probeErr := c.HasSessionStatus(ctx, sessionName); probeErr == nil && !alive {
				return paneVanishedErr(sessionName, capturesOK, capturesFailed,
					time.Since(start).Round(time.Millisecond), lastCaptureErr, lastPane)
			}
		}
		if time.Now().After(deadline) {
			// The loop above never probed this pane: nothing composer-shaped was
			// ever drawn on it. Ask once here so a modal that renders no marker
			// row still refuses under its own name instead of masquerading as a
			// slow-starting agent.
			if lastPane != "" && paneShowsModal(ctx, opts.modalDetector, lastPane) {
				return blockedByModalErr(sessionName, lastPane)
			}
			// The accounting comes BEFORE the pane snapshot deliberately: the TUI
			// renders this string through firstLine() (services/boss/internal/
			// views/status.go), which cuts at the first newline, and a snapshot is
			// multi-line. Anything ordered after it is invisible to the operator
			// who most needs it.
			//
			// Branch on the successful-capture count, never on the emptiness of
			// the snapshot: a successful capture can legitimately return an empty
			// pane, which is exactly the case this diagnostic exists to keep
			// distinguishable. Using emptiness would rebuild the original
			// conflation one notch over.
			elapsed := time.Since(start).Round(time.Millisecond)
			budget := budgetText(opts)
			// Both returns build the SAME named type, so the retry loop can
			// recognise "this attempt merely ran out of budget" without parsing
			// the message. The type is constructed here and nowhere else: an
			// error minted above this branch would be a readiness verdict the
			// deadline never reached, and retrying on it would be wrong.
			if capturesOK == 0 {
				return readyMarkerTimeoutErr(sessionName, opts.readyMarker, opts.deadline, "", fmt.Sprintf(
					"ready marker %q not seen in pane %q within %s; capture-pane: 0 ok, %d failed in %s; no capture-pane call succeeded; last capture error: %s",
					opts.readyMarker, sessionName, budget, capturesFailed, elapsed, captureErrText(lastCaptureErr)))
			}
			return readyMarkerTimeoutErr(sessionName, opts.readyMarker, opts.deadline, lastPane, fmt.Sprintf(
				"ready marker %q not seen in pane %q within %s; capture-pane: %d ok, %d failed in %s%s; last pane (truncated): %s",
				opts.readyMarker, sessionName, budget, capturesOK, capturesFailed, elapsed,
				lastCaptureErrClause(capturesFailed, lastCaptureErr), truncatePaneForError(lastPane)))
		}
		select {
		case <-ctx.Done():
			return readyMarkerContextErr(sessionName, opts, ctx.Err(), capturesOK, capturesFailed,
				time.Since(start).Round(time.Millisecond), lastCaptureErr, lastPane)
		case <-time.After(opts.pollInterval):
		}
	}
}

// readinessRetrier is the question the retry loop actually asks, expressed as a
// method so the answer travels with the error instead of alongside it.
//
// It exists because the first cut of BOS-895 answered that question in two
// places: the loop matched the shared *readyMarkerTimeoutError, and then
// consulted ctx.Err() separately to avoid re-running the context-cut flavour of
// it. Two facts held in step by a comment. Any later readiness error minted
// above that check would have inherited "retryable" silently, which is the one
// way this loop can turn a verdict into a second full budget spent on a pane
// that was never coming back.
//
// Whoever mints a readiness timeout now has to say whether it may be re-run,
// and the loop reads nothing else.
type readinessRetrier interface {
	error
	mayRetryReadiness() bool
}

var _ readinessRetrier = (*readyMarkerTimeoutError)(nil)

// budgetText renders the budget an attempt actually had, naming the clamp when
// the caller's context shortened it.
//
// Without the clause the two numbers are indistinguishable in the message: an
// operator reading "within 4.5s" against a configured 45s cannot tell a
// mis-set budget from a context ceiling that cut the attempt short, and those
// have opposite fixes — raise the setting, or find who imposed the deadline.
func budgetText(opts sendPlanOpts) string {
	if opts.clampedFrom > 0 && opts.clampedFrom != opts.deadline {
		return fmt.Sprintf("%s (shortened from %s to stay inside the caller's context)", opts.deadline, opts.clampedFrom)
	}
	return opts.deadline.String()
}

// readyMarkerTimeoutError is the readiness wait's OWN budget running out —
// nothing was typed, no Enter was sent, and the pane was simply not showing a
// composer yet. It is the one readiness failure that is safe to re-run, so it
// is given a name the retry loop can match on rather than a message the loop
// would have to parse.
//
// It is deliberately NOT passed through classifySubmit: a readiness timeout is
// not a submission verdict, and tagging it would make OutcomeOf report a
// submit-shaped outcome for a delivery that never reached the composer. Callers
// that switch on OutcomeOf must keep seeing OutcomeUnclassified here.
//
// msg is the fully rendered message rather than a format applied at Error()
// time, so the existing single-attempt wording stays byte-for-byte what it was
// before the retry existed.
type readyMarkerTimeoutError struct {
	// sessionName, marker and deadline are written by both constructors and
	// read only by tests today. That is deliberate: they are the assertion
	// surface that lets a test check WHICH wait failed and what budget it had
	// without parsing the rendered message — the coupling this type exists to
	// remove. Do not strip them as unused.
	sessionName string
	marker      string
	deadline    time.Duration
	// pane is the last SUCCESSFUL capture, empty when no capture-pane call
	// succeeded. The retry loop carries the most recent non-empty one across
	// attempts so a final attempt that could not read the pane at all still
	// reports what an earlier attempt saw.
	pane string
	msg  string

	// cause is the caller's context error when the wait ended because the
	// context did, and nil when the wait's own budget expired. It is what makes
	// errors.Is(err, context.Canceled) keep working on an error that also has
	// to satisfy errors.As for the readiness verdict.
	cause error

	// retryable is this type's answer to readinessRetrier, and the only input
	// the retry loop takes when deciding whether to spend another budget. It is
	// set by the two constructors below and nowhere else: a budget expiry is
	// safe to re-run, a wait the caller's context cut short is not, and no
	// third minting site may exist without choosing.
	retryable bool
}

// mayRetryReadiness reports whether re-running this wait could plausibly
// succeed. See readinessRetrier for why this is a method.
func (e *readyMarkerTimeoutError) mayRetryReadiness() bool { return e.retryable }

func (e *readyMarkerTimeoutError) Error() string { return e.msg }

// Unwrap exposes the context cause, and only that. A budget expiry wraps
// nothing: there is no underlying error, and inventing one would make
// errors.Is(err, context.DeadlineExceeded) true for a wait that finished
// entirely inside its own deadline.
func (e *readyMarkerTimeoutError) Unwrap() error { return e.cause }

func readyMarkerTimeoutErr(sessionName, marker string, deadline time.Duration, pane, msg string) error {
	return &readyMarkerTimeoutError{
		sessionName: sessionName, marker: marker, deadline: deadline, pane: pane, msg: msg,
		// The wait's own budget expired with the caller's context still live:
		// the one readiness failure another look can fix.
		retryable: true,
	}
}

// readyMarkerContextErr is the same verdict for a wait the CALLER's context cut
// short. It exists because clamping an attempt's deadline under the context
// only usually wins that race — a slow capture-pane can still leave the loop
// sleeping when the context fires — and "context deadline exceeded" on its own
// tells an operator nothing about what the agent was showing. Reporting the
// same accounting and pane snapshot here makes the diagnostic unconditional
// rather than a matter of scheduling luck.
//
// The enrichment is confined to the RETRIED path, and that boundary is an
// acceptance criterion rather than a taste call: the established-send wrapper
// must return the message it returned before BOS-895 existed, byte for byte.
// So a single-attempt wait renders exactly the historical
// `wait for ready marker on %q: <cause>` and stops there, while the retried
// session-start path — the only one that clamps, and so the only one where the
// clamp can lose its race with the context — appends the accounting and the
// snapshot after that same prefix.
//
// The cost of that split is real and accepted: a send whose context expires
// mid-poll still reports no pane. It is the narrower loss. The send path runs
// under bosso's relay deadline, where the caller already knows the deadline it
// imposed; the start path runs unattended under cron, where the pane snapshot
// is often the only evidence anyone will ever have.
//
// It is never retryable, whichever shape it takes: the budget that ran out
// belongs to the caller, and this loop cannot mint more of it.
func readyMarkerContextErr(sessionName string, opts sendPlanOpts, cause error, capturesOK, capturesFailed int, elapsed time.Duration, lastCaptureErr error, lastPane string) error {
	msg := fmt.Sprintf("wait for ready marker on %q: %s", sessionName, cause)
	if opts.retried {
		msg += fmt.Sprintf("; capture-pane: %d ok, %d failed in %s%s",
			capturesOK, capturesFailed, elapsed,
			lastCaptureErrClause(capturesFailed, lastCaptureErr))
		if capturesOK > 0 {
			msg += "; last pane (truncated): " + truncatePaneForError(lastPane)
		}
	}
	return &readyMarkerTimeoutError{
		sessionName: sessionName, marker: opts.readyMarker, deadline: opts.deadline,
		pane: lastPane, msg: msg, cause: cause, retryable: false,
	}
}

// paneVanishedError is the opposite verdict: tmux answered, definitively, that
// the session is gone. Waiting longer cannot help, so this is the one readiness
// failure the retry loop must NOT re-run — hence a distinct name rather than a
// second flavour of timeout.
type paneVanishedError struct {
	sessionName string
	msg         string
}

func (e *paneVanishedError) Error() string { return e.msg }

// paneVanishedErr renders the verdict. It keeps the same accounting-before-pane
// ordering as the timeout message, for the same reason: the TUI cuts this
// string at its first newline, so anything after a multi-line pane snapshot is
// invisible to the operator who most needs it.
func paneVanishedErr(sessionName string, capturesOK, capturesFailed int, elapsed time.Duration, lastCaptureErr error, lastPane string) error {
	msg := fmt.Sprintf(
		"pane %q is gone: capture-pane failed and tmux has-session reports no such session; capture-pane: %d ok, %d failed in %s%s",
		sessionName, capturesOK, capturesFailed, elapsed,
		lastCaptureErrClause(capturesFailed, lastCaptureErr))
	if capturesOK > 0 {
		msg += "; last pane (truncated): " + truncatePaneForError(lastPane)
	}
	return &paneVanishedError{sessionName: sessionName, msg: msg}
}

// waitForReadyMarkerWithAttempts runs the readiness wait up to
// opts.readyAttempts times, returning as soon as one attempt sees a live
// composer.
//
// # Why a retry here is safe when retrying a delivery is not
//
// This project's standing rule is that a command is never re-sent after an
// ambiguous delivery, because a second send can double-type into a composer
// whose first payload did arrive. That rule is about the SEND. This helper is
// lexically confined to the phase before it: it is called by sendPlan and
// sendLine as their step 1, and every keystroke-producing call in both lives
// below that call site. No path through this function can reach a pane
// mutation, so "retry" here cannot mean "deliver twice" — the safety is a
// property of where the loop sits, not a judgement about whether delivery is
// idempotent (BOS-895).
//
// # What is retried and what is not
//
// Only a readyMarkerTimeoutError that reports itself retryable — the wait's own
// budget expiring, with the caller's context still live — is re-run. A modal
// refusal is a verdict, not a slow boot; a vanished pane cannot come back; a
// cancelled context has no budget left to spend. Each returns straight out.
//
// The loop asks the error rather than re-deriving that from the context, so a
// readiness error added later cannot default into being retried. See
// readinessRetrier.
//
// # The remaining-budget guard
//
// A second attempt is only started when the caller's context can actually fund
// it, INCLUDING the modal probe the attempt may make on its way out
// (modalProbeTimeout). Starting an attempt that the context will cut short
// converts a diagnostic timeout — which carries a pane snapshot — into a bare
// context error, which carries nothing. Attempt one always runs: a context too
// small even for that is the caller's problem to report, and the attempt still
// produces the better error. A context with no deadline funds the full count.
func (c *Client) waitForReadyMarkerWithAttempts(ctx context.Context, sessionName string, opts sendPlanOpts) error {
	attempts := resolveReadyAttemptsFloor(opts.readyAttempts)
	// # The frozen single-attempt path
	//
	// A one-attempt delivery is handed to the wait exactly as it arrived and its
	// error is returned exactly as it came back: no clamp, no enriched context
	// error, no composite wrapper counting attempts. The established-send path
	// runs here, and BOS-895 promised that path a byte-for-byte unchanged
	// failure message — a promise nothing above can keep for it, because both
	// the clamp and the enrichment alter the string.
	//
	// It is also the honest shape. Every mechanism below exists to make a
	// SECOND attempt possible or to explain why one was not started; applying
	// any of it to a delivery that will only ever run once buys the caller
	// nothing and costs it the wording it matches on.
	if attempts <= 1 {
		return c.waitForReadyMarker(ctx, sessionName, opts)
	}

	perAttemptDeadline := opts.deadline
	start := time.Now()

	var lastErr error
	// carried holds the most recent timeout whose pane snapshot is non-empty,
	// so a later attempt that could not read the pane at all still reports what
	// an earlier one saw instead of `<empty>`.
	var carried *readyMarkerTimeoutError
	attemptsRun := 0
	// stoppedForContext records that the loop ended because the CALLER's
	// context finished, not because the attempts ran out. The two are worth
	// telling apart: attempts running out means the readiness budget is too
	// small, while a context ending means the ceiling above it is — and only
	// one of them is a reason to raise sessionStartReadyAttempts.
	stoppedForContext := false

	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			// Compared against the UNCLAMPED per-attempt deadline on purpose:
			// a context with less than a full budget left is a context that
			// cannot fund a meaningful second look, and a clamped stub of one
			// would mostly buy latency. modalProbeTimeout is reserved whether
			// or not THIS delivery carries a detector — reserving it
			// unconditionally errs toward not starting an attempt that cannot
			// finish, which is the safe direction here.
			if d, ok := ctx.Deadline(); ok && time.Until(d) < perAttemptDeadline+modalProbeTimeout {
				break
			}
		}

		attemptOpts := opts
		attemptOpts.retried = true
		attemptOpts.deadline = clampAttemptDeadline(ctx, perAttemptDeadline, opts.pollInterval)
		if attemptOpts.deadline < perAttemptDeadline {
			// Remember what was asked for, so the timeout can name both numbers
			// rather than present the shortened one as the configured one.
			attemptOpts.clampedFrom = perAttemptDeadline
		}
		attemptsRun++

		err := c.waitForReadyMarker(ctx, sessionName, attemptOpts)
		if err == nil {
			return nil
		}
		lastErr = err

		var timeout *readyMarkerTimeoutError
		if !errors.As(err, &timeout) {
			// A modal refusal, a vanished pane, or anything a future change
			// adds: a verdict, not a slow boot, and so not this loop's to
			// re-run. Returning it unwrapped also keeps its own message intact,
			// which is what callers matching on ErrBlockedByModal expect.
			return err
		}
		if timeout.pane != "" {
			carried = timeout
		}
		// The error's own verdict first, the live context second. The first
		// catches a wait the context already cut short; the second catches a
		// context that died between that wait returning and this check, where
		// the attempt itself expired honestly but there is no budget left to
		// spend on another.
		if !timeout.mayRetryReadiness() || ctx.Err() != nil {
			stoppedForContext = true
			break
		}
	}

	base := lastErr
	if carried != nil {
		base = carried
	}
	// No single-attempt branch here: that path returned above, before any of
	// this ran. Everything from here down is reachable only with attempts >= 2.
	elapsed := time.Since(start).Round(time.Millisecond)
	if stoppedForContext && ctx.Err() != nil {
		// Two %w verbs, deliberately. A caller distinguishing an orderly
		// shutdown from a genuine failure asks errors.Is(err, context.Canceled)
		// and must still get true here; a caller diagnosing the pane asks
		// errors.As for the readiness timeout and must still get the snapshot.
		// Reporting only one of the two would make this error answer the wrong
		// half of the question no matter which half were kept.
		return fmt.Errorf(
			"ready marker %q not seen in pane %q after %d of %d attempts in %s (the caller's context ended first: %w): %w",
			opts.readyMarker, sessionName, attemptsRun, attempts, elapsed, ctx.Err(), base)
	}
	if attemptsRun < attempts {
		return fmt.Errorf(
			"ready marker %q not seen in pane %q after %d of %d attempts in %s (no context budget left for another attempt): %w",
			opts.readyMarker, sessionName, attemptsRun, attempts, elapsed, base)
	}
	return fmt.Errorf(
		"ready marker %q not seen in pane %q after %d attempts in %s: %w",
		opts.readyMarker, sessionName, attemptsRun, elapsed, base)
}

// clampAttemptDeadline shortens one attempt's budget so the attempt's OWN
// deadline fires before the caller's context does. That ordering is the whole
// point: the attempt's timeout carries a pane snapshot and the capture
// accounting, while a context expiry mid-poll carries neither.
//
// It subtracts one poll interval, because the wait checks its deadline right
// after each capture and then sleeps; landing inside that sleep is what loses
// the diagnostic. When less than one interval remains, half of what is left is
// used instead — still strictly ahead of the context, and still enough for the
// first, immediate capture. A context already past its deadline is left alone:
// there is nothing to clamp to, and attempt one still runs.
func clampAttemptDeadline(ctx context.Context, deadline, pollInterval time.Duration) time.Duration {
	d, ok := ctx.Deadline()
	if !ok {
		return deadline
	}
	remaining := time.Until(d)
	if remaining <= 0 {
		return deadline
	}
	clamped := remaining - pollInterval
	if clamped <= 0 {
		clamped = remaining / 2
	}
	if clamped < deadline {
		return clamped
	}
	return deadline
}

// captureErrMaxBytes bounds the capture-error text folded into the readiness
// timeout. tmux's own messages are short, but this clause sits ahead of the pane
// snapshot in the first line, so a pathological stderr must not be able to push
// the rest of the diagnostic out of the operator's view.
const captureErrMaxBytes = 200

// lastCaptureErrClause renders the optional cause clause. It keys on the FAILED
// count, not on "no capture succeeded": the likeliest production shape of this
// timeout is an agent that boots, draws a pane, then dies with its tmux session,
// where early captures succeed and every later one fails. Gating the cause on
// the success count would report the stale pre-crash snapshot and discard the
// message that names why — in the very run where it names the cause.
func lastCaptureErrClause(capturesFailed int, err error) string {
	if capturesFailed == 0 || err == nil {
		return ""
	}
	return "; last capture error: " + captureErrText(err)
}

// captureErrText renders a CapturePane failure using tmux's OWN words.
//
// CapturePane calls cmd.Output() without setting Stderr, which is precisely the
// condition under which os/exec parks the child's stderr on
// (*exec.ExitError).Stderr. Formatting the error alone prints only "exit status
// 1"; errors.As reaches the real text ("can't find session: …"). Recovering it
// here rather than inside CapturePane keeps that method's error string — shared
// by the status poller and the submit verifier — byte-for-byte unchanged.
//
// The result is deliberately single-line: newlines here would push the pane
// snapshot past the TUI's firstLine() cut.
func captureErrText(err error) string {
	if err == nil {
		// Unreachable from the call sites above, which both require a recorded
		// failure. Named as an observation rather than left blank so a future
		// caller cannot render a dangling "last capture error: ".
		return "<none recorded>"
	}
	text := err.Error()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if stderr := strings.TrimSpace(string(exitErr.Stderr)); stderr != "" {
			text += ": " + stderr
		}
	}
	// strings.Fields splits on every kind of whitespace, so this collapses
	// embedded newlines as well as runs of spaces.
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > captureErrMaxBytes {
		text = text[:captureErrMaxBytes] + " (truncated)"
	}
	return text
}

// truncatePaneForError trims the pane snapshot embedded in a SendPlan
// timeout error. The raw capture can be ~80 cols × 1000 rows; we want
// enough for diagnosis (the input-box area and any error banner) without
// flooding logs or wrapping past usefulness.
func truncatePaneForError(pane string) string {
	const maxBytes = 800
	pane = strings.TrimSpace(pane)
	if pane == "" {
		return "<empty>"
	}
	if len(pane) <= maxBytes {
		return pane
	}
	return pane[len(pane)-maxBytes:] + " (head truncated)"
}
