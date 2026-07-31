package tmux

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// This file holds the submission-verification and pane-scraping heuristics used
// by the Send* delivery paths in tmux.go: after an Enter is sent, poll
// capture-pane and decide — shape-aware, failing toward "still pending" — whether
// the payload actually left the composer, retrying the Enter once before erroring
// loudly. The cluster is churn-prone (its glyph grammars track the Claude/Codex
// TUIs) so it lives beside, but separate from, the stable tmux CLI wrappers.

// errSubmissionPending is the sentinel returned by waitForSubmission when the
// wait budget elapses with the payload still sitting at the prompt (a confirmed
// "not submitted" result). verifyWithEnterRetry retries the Enter only for this
// sentinel — an infrastructure failure (capture-pane error, context
// cancellation) is NOT a confirmed still-pending, so it must not fire a stray
// extra Enter into a pane that may already have submitted.
var errSubmissionPending = errors.New("payload still present at the tmux prompt")

// errSubmissionQueued is the sentinel for a payload the agent has accepted into
// its queue while a turn runs: the payload LEFT the composer (the same positive
// signal that produces OutcomeSubmitted) and the pane shows it echoed into the
// agent's queued-message list with the composer cleared to the queue hint.
//
// It is therefore a variant of submitted, never of pending. An earlier revision
// derived it instead from "budget elapsed + the agent says it is working", which
// real captures showed to be exactly backwards — see the deadline branch in
// waitForSubmission.
//
// It is deliberately NOT errSubmissionPending, and that is load-bearing rather
// than cosmetic: verifyWithEnterRetry gates its whole recovery dance — C-u
// clear, re-type, second Enter — on errors.Is(err, errSubmissionPending). A
// queued payload must skip every one of those steps, because the message is
// already in the agent's queue and re-delivering would queue a SECOND copy. A
// distinct sentinel makes that exclusion structural: the retry cannot reach a
// queued payload even if a later edit forgets why (BOS-599).
var errSubmissionQueued = errors.New("payload queued behind the agent's running turn")

// SubmitOutcome classifies what the submit verifier actually observed on the
// pane. The three verifier outcomes demand opposite recoveries from a caller —
// a confirmed-pending payload must be re-sent, an unconfirmed one must not be
// (re-sending may double-type into a composer that did submit) — so the
// distinction is carried out of the package instead of collapsing into one
// opaque error string (BOS-598).
type SubmitOutcome int

const (
	// OutcomeUnclassified is the zero value and means "this error did not come
	// from the submit verifier": a ready-marker timeout, a delivery command that
	// failed to run, an empty session name. Such a failure keeps the pre-existing
	// error handling — only the three verifier outcomes below are reclassified.
	OutcomeUnclassified SubmitOutcome = iota
	// OutcomeSubmitted: a positive signal confirmed the payload left the composer.
	OutcomeSubmitted
	// OutcomeNotSubmitted: the wait budget elapsed with the payload still held by
	// the live composer (errSubmissionPending). A genuine failure — but a safely
	// retryable one, because the pane state is known.
	OutcomeNotSubmitted
	// OutcomeUnconfirmed: verification itself failed (capture-pane error or a
	// cancelled context), so the pane state is unknown and the payload may or may
	// not have been submitted.
	OutcomeUnconfirmed
	// OutcomeQueued: the payload left the composer into the agent's visible
	// message queue, so it was accepted mid-turn and will be consumed when the
	// running turn ends. A SUCCESS — a more precise OutcomeSubmitted, not a
	// softened OutcomeNotSubmitted — and the one outcome a caller must never
	// retry: the message is not lost, and a resend would queue a second copy
	// (BOS-599).
	OutcomeQueued
	// OutcomeBlockedByModal: the readiness gate saw a modal (an approval menu, a
	// question picker, an update interstitial) instead of a live composer, so
	// delivery was refused before anything was typed and before any Enter. It is
	// deliberately distinct from OutcomeNotSubmitted: nothing was delivered, so a
	// caller must surface the modal for a human rather than re-send into it
	// (BOS-600).
	OutcomeBlockedByModal
)

// String renders the outcome for an error message or a log line.
func (o SubmitOutcome) String() string {
	switch o {
	case OutcomeSubmitted:
		return "submitted"
	case OutcomeNotSubmitted:
		return "not submitted"
	case OutcomeUnconfirmed:
		return "unconfirmed"
	case OutcomeQueued:
		return "queued"
	case OutcomeBlockedByModal:
		return "blocked by modal"
	default:
		return "unclassified"
	}
}

// submitError carries a SubmitOutcome across the error-only Send* boundary.
// Threading a second return value through sendPlan / sendLine / SendMessage and
// the server's tmuxSpawner interface would touch every fake in the tree for a
// value only the submit path produces; wrapping it in the error keeps those
// signatures untouched and makes the outcome retrievable with OutcomeOf.
type submitError struct {
	outcome SubmitOutcome
	err     error
}

func (e *submitError) Error() string { return e.err.Error() }

func (e *submitError) Unwrap() error { return e.err }

// OutcomeOf reports the submit outcome an error carries. A nil error is
// OutcomeSubmitted; an error from anywhere other than the verifier is
// OutcomeUnclassified, so a caller that maps outcomes to behaviour keeps its
// existing handling for every non-verifier failure.
func OutcomeOf(err error) SubmitOutcome {
	if err == nil {
		return OutcomeSubmitted
	}
	var se *submitError
	if errors.As(err, &se) {
		return se.outcome
	}
	return OutcomeUnclassified
}

// classifySubmit tags err with outcome, preserving errors.Is/As on the original.
func classifySubmit(outcome SubmitOutcome, err error) error {
	if err == nil {
		return nil
	}
	return &submitError{outcome: outcome, err: err}
}

// verifyWithEnterRetry confirms the payload was submitted, retrying once if the
// TUI swallowed the first Enter. It verifies within the wait budget; on a
// CONFIRMED still-pending result it clears the composer, re-delivers the payload
// via redeliver, sends a fresh Enter, and re-verifies within a fresh budget. If
// that still fails it returns a loud error naming the session and the delivery
// state. Used by both the sendPlan and sendLine submit paths; payload is the raw
// (untrimmed) text so waitForSubmission can detect its shape.
//
// The retry clears and re-delivers rather than sending a bare second Enter: when
// the first Enter was in fact accepted and the verifier merely failed to read the
// signal, a bare Enter lands in a fresh composer and (for a TUI that restores
// the last input on Up/Enter) can re-run the payload. Clearing first makes the
// retry idempotent — the composer is emptied, the payload is typed exactly once
// more, and a single Enter submits that one copy (BOS-598).
//
// Clearing is safe here precisely because it is gated on errSubmissionPending,
// which waitForSubmission now returns ONLY when the final poll saw a prompt
// marker drawn (BOS-597's live-composer resolution) — so C-u lands in a
// composer, never in a full-screen working pane. That guarantee is not free:
// multiLineSubmitted fails toward "still pending" when NO marker is present at
// all, so a multi-line send into a booting agent or a full-screen overlay would
// otherwise reach the deadline as "pending" with no composer on screen and take
// this branch. waitForSubmission classifies that case as OutcomeUnconfirmed
// instead, which never reaches the clear.
func (c *Client) verifyWithEnterRetry(ctx context.Context, sessionName, payload string, opts sendPlanOpts, redeliver func(context.Context) error) error {
	outcome, err := c.waitForSubmission(ctx, sessionName, payload, opts.submitVerifyWait, opts.submitVerifyTick)
	if err == nil {
		return nil
	}
	// Only retry on a CONFIRMED still-pending result. A capture-pane failure or
	// a cancelled context is not evidence the payload is unsubmitted, so firing
	// another Enter could inject a stray keystroke into a pane that already
	// submitted — surface the classified infrastructure error instead.
	if !errors.Is(err, errSubmissionPending) {
		return classifySubmit(outcome, err)
	}
	// Below this point the payload is confirmed still in the composer, so every
	// failure keeps that state: nothing here can have submitted it.
	//
	// C-u is kill-to-start-of-LINE, not a guaranteed whole-composer clear, so one
	// press against a multi-line plan can leave the preceding lines held. Press it
	// until the composer is proven empty (bounded), because BOTH of the other
	// outcomes are bad: re-delivering onto a composer that still holds the payload
	// submits two concatenated copies, and pressing Enter on a partially-killed
	// payload submits mutilated user text. The postcondition is CHECKED after
	// every press rather than assumed.
	// The baseline poll below shares the ONE submitVerifyWait budget with the
	// presses: its window is carved OUT of the budget before the per-press slices
	// are cut, never added on top, so licensing the Enter positively cannot
	// lengthen the verifier's worst-case wall clock.
	baselineWindow := 2 * opts.submitVerifyTick
	if maxBaseline := opts.submitVerifyWait / (2 * maxComposerClearPresses); baselineWindow <= 0 || baselineWindow > maxBaseline {
		baselineWindow = maxBaseline
	}
	clearSlice := (opts.submitVerifyWait - baselineWindow) / maxComposerClearPresses
	// A press can spend its slice on TWO bounded windows — the post-clear check
	// and, when that check is inconclusive, the re-read below — so each window
	// gets half the slice and the retry loop still fits inside submitVerifyWait.
	// The re-read exists to outlast a lagging repaint, which takes a capture or
	// two, not a whole slice; buying it a second full slice would double the
	// verifier's worst-case wall clock for no extra evidence.
	clearWindow := clearSlice / 2
	if clearWindow < opts.submitVerifyTick {
		clearWindow = opts.submitVerifyTick
	}
	// The re-read has a narrower job than the window above: it is not waiting for
	// a slow composer to empty (that is what the presses are for), only for a
	// repaint that is already owed, which lands within a capture or two. A couple
	// of ticks buys it that — the tick is this code's own unit of "how often a
	// pane is worth re-reading" — while keeping the presses cheap against a
	// composer that never changes at all, where the re-read can prove nothing and
	// would otherwise burn a full window on every press.
	reReadWindow := 2 * opts.submitVerifyTick
	if reReadWindow <= 0 || reReadWindow > clearWindow {
		reReadWindow = clearWindow
	}
	// baseline is what the composer renders BEFORE any key is pressed: the ONE
	// stable reading every mutilation decision below is taken against. The bare
	// Enter is licensed POSITIVELY — the authoritative post-clear sample must
	// render exactly this — rather than by the absence of evidence of a cut.
	//
	// That direction is the whole point. A per-press pre-clear snapshot can land
	// mid-repaint and draw no input box at all, and an ABSENT footprint can
	// neither prove nor disprove a cut; reading it as "no evidence" let a press
	// that truncated the payload go unlatched and fall through to a bare Enter on
	// the fragment, which then left the composer and was reported as submitted —
	// delivered=true for silently corrupted user text. Against a fixed baseline a
	// marker-less capture on either side decides nothing: it is neither equal to
	// the baseline (so it never licenses the Enter) nor evidence of a cut (so the
	// benign mid-repaint snapshot no longer manufactures a hard failure either).
	baseline, baselineErr := c.awaitComposerFootprint(ctx, sessionName, payload, baselineWindow, opts.submitVerifyTick)
	if baselineErr != nil {
		return classifySubmit(OutcomeNotSubmitted, fmt.Errorf("snapshot composer before clear on tmux session %q: %w", sessionName, baselineErr))
	}
	if baseline == "" {
		// No reading of the payload in the box, so nothing below could ever be
		// compared against it and no Enter may be pressed. Not a single key has
		// been sent since the confirmed-pending verdict, so this is precisely
		// "not submitted": loud, safe, and retryable by the caller.
		return classifySubmit(OutcomeNotSubmitted, fmt.Errorf("command was not submitted to tmux session %q: the composer could not be read before the clear, so no key was sent: %w", sessionName, errSubmissionPending))
	}
	// mutilated latches once the payload on screen is PROVABLY not the intact
	// one. It guards the one way a post-clear sample can match the baseline and
	// still be worthless: agreeing with a baseline that was never a reading of
	// the whole payload. Once that is so, a reading that agrees with the baseline
	// is either a stale screen replaying the pre-clear pane or a faithful reading
	// of a fragment — never evidence the box holds the payload intact.
	//
	// It starts latched when the baseline itself does not account for the whole
	// payload. Comparing against the baseline can only ever establish that
	// NOTHING CHANGED, so a composer that was already holding a truncated payload
	// when this retry first looked at it produces a perfectly stable reading and
	// would license a bare Enter onto the fragment — the same silent submit of
	// corrupted user text that the per-press comparison below exists to prevent,
	// arrived at without any press being at fault. The recovery path is
	// untouched: a clear that PROVES the box empty still re-delivers and submits,
	// which is exactly the right answer for a composer that started out wrong.
	mutilated := !footprintRendersWholePayload(baseline, payload)
	// held is the last authoritative reading, and answers the different question
	// "did THIS press change anything" — the tell that separates a clear which is
	// still making progress from one that has stalled, and the trigger for the
	// re-read below.
	held := baseline
	for press := 1; ; press++ {
		if clearErr := c.clearComposer(ctx, sessionName); clearErr != nil {
			return classifySubmit(OutcomeNotSubmitted, clearErr)
		}
		state, afterPane, clearedErr := c.awaitComposerCleared(ctx, sessionName, payload, clearWindow, opts.submitVerifyTick)
		if clearedErr != nil {
			// A capture failure or cancelled context here is NOT unconfirmed: the
			// only key this path has pressed since the confirmed-pending verdict is
			// C-u, which cannot submit. Reporting "may already have been submitted"
			// for a payload proven unsubmitted would invert the operator's advice.
			return classifySubmit(OutcomeNotSubmitted, clearedErr)
		}
		// A "still holding" verdict whose footprint is UNCHANGED across the clear is
		// not yet evidence of anything. It is what a no-op C-u looks like — and
		// equally what a composer that DID empty looks like while it has yet to
		// repaint, since every capture the window took landed within one clear
		// window of the press. Deciding on it blind pressed a bare Enter into an
		// empty box, and the verifier then read that cleared composer as
		// "submitted": delivered=true for a payload that was silently discarded.
		//
		// So re-read on a FRESH bounded window and let ITS last capture be the
		// authoritative post-clear sample. It is a poll, not one capture taken at
		// one arbitrary instant, which would merely move the race: a lagging
		// repaint is observed rather than sampled around. The decision below then
		// runs exactly ONCE, over that sample — including the footprint comparison,
		// so a repaint that reveals a MUTILATED remainder latches mutilated and
		// loops instead of Entering the fragment.
		after := composerFootprint(afterPane, payload)
		if state == composerClearMissed && (after == baseline || after == held) {
			state, afterPane, clearedErr = c.awaitComposerCleared(ctx, sessionName, payload, reReadWindow, opts.submitVerifyTick)
			if clearedErr != nil {
				// Still nothing but C-u has been pressed since the confirmed-pending
				// verdict, so this cannot have submitted — same classification as the
				// other pre-Enter failures above.
				return classifySubmit(OutcomeNotSubmitted, fmt.Errorf("re-read composer after an unchanged clear on tmux session %q: %w", sessionName, clearedErr))
			}
			after = composerFootprint(afterPane, payload)
		}
		switch state {
		case composerClearUnknown:
			// The input box vanished, so the agent may have accepted the payload
			// after all. Typing or pressing Enter into that is the double-submit
			// this change exists to prevent.
			return composerVanishedErr(sessionName)
		case composerClearTook:
			// An input box is drawn and proven empty, so the payload can be typed
			// exactly once more and submitted.
			if redeliver != nil {
				if reErr := redeliver(ctx); reErr != nil {
					return classifySubmit(OutcomeNotSubmitted, reErr)
				}
			}
		case composerClearMissed:
			if after != baseline {
				// The box still reads as holding the payload but renders something
				// OTHER than what it held before a key was pressed: the clear killed
				// part of it. What is on screen is now mutilated user text, so never
				// submit it — press again to finish emptying the box so the
				// re-delivery starts clean. Give up loudly rather than unboundedly.
				//
				// A marker-less or otherwise unreadable sample lands here too, and
				// that is deliberate: it cannot show the payload intact, so it must
				// not license an Enter. It costs another press, never a stray key.
				mutilated = true
				if press == maxComposerClearPresses {
					return classifySubmit(OutcomeNotSubmitted, fmt.Errorf("command was not submitted to tmux session %q: %d composer clears left a partial payload behind, so no Enter was sent: %w", sessionName, press, errSubmissionPending))
				}
				if after == held {
					// A stuck partial clear: the box holds a fragment and this press
					// changed nothing, so it will not empty. Fail exactly as loudly as
					// the press cap above — confirmed not submitted, no Enter sent.
					return classifySubmit(OutcomeNotSubmitted, fmt.Errorf("command was not submitted to tmux session %q: a composer clear left a partial payload behind and press %d changed nothing, so no Enter was sent: %w", sessionName, press, errSubmissionPending))
				}
				held = after
				continue
			}
			if mutilated {
				// The sample agrees with a baseline that is not a reading of the intact
				// payload — either because a press provably cut into it (a killed
				// payload does not grow back, so this is a stale screen replaying the
				// pre-clear pane) or because the box already held a fragment when this
				// retry first looked. Neither is evidence the box holds the payload, so
				// the same loud verdict as the stuck case above.
				return classifySubmit(OutcomeNotSubmitted, fmt.Errorf("command was not submitted to tmux session %q: the composer holds a partial payload and press %d changed nothing, so no Enter was sent: %w", sessionName, press, errSubmissionPending))
			}
			// Evidenced POSITIVELY: the authoritative sample renders exactly the
			// baseline, so the box still holds the INTACT payload. This is the
			// ordinary swallowed-Enter case, so submit what is already there WITHOUT
			// re-delivering — a second copy would concatenate.
		}
		// Every surviving path is ready for the Enter below; the only branch that
		// presses again is the mutilated one, which continues above.
		break
	}
	if enterErr := c.sendEnter(ctx, sessionName); enterErr != nil {
		return classifySubmit(OutcomeNotSubmitted, enterErr)
	}
	retryOutcome, retryErr := c.waitForSubmission(ctx, sessionName, payload, opts.submitVerifyWait, opts.submitVerifyTick)
	if retryOutcome == OutcomeQueued {
		// The re-delivered payload landed in the agent's queue rather than being
		// consumed — the agent was mid-turn when the retry's Enter took. Keep the
		// queued verdict and its own wording: the generic wrapper below asserts
		// "not submitted", which is the opposite of what happened and would tell
		// the caller to retry a message the agent already holds.
		return classifySubmit(OutcomeQueued, retryErr)
	}
	if retryErr != nil {
		return classifySubmit(retryOutcome, fmt.Errorf("payload not submitted to tmux session %q after an Enter retry (delivery state: %s): %w", sessionName, retryOutcome, retryErr))
	}
	return nil
}

// composerVanishedErr is the verdict for a pane whose live input box is gone
// after the composer clear: the box can disappear because the agent accepted the
// payload after all, so the state is UNKNOWN rather than pending, and neither
// typing nor Enter may follow. Shared by the two places in verifyWithEnterRetry
// that can observe it so they cannot drift apart in wording or classification.
//
// Like every UNCONFIRMED text here it states the OBSERVATION and its consequence
// — never the verdict "submission could not be verified". The outcome travels
// beside the error (OutcomeUnconfirmed, and delivery_state on the wire), and the
// surfaces that render it label the state themselves, so a verdict clause here
// only makes the operator read the same fact twice in one line.
func composerVanishedErr(sessionName string) error {
	return classifySubmit(OutcomeUnconfirmed, fmt.Errorf("the live composer on tmux session %q disappeared after the composer clear, so the payload's state is unknown", sessionName))
}

// waitForSubmission polls capture-pane until the payload is confirmed submitted
// or the wait budget elapses, at which point it returns a loud error. The
// confirmation predicate is shape-aware but shares one wrapping-robust submission
// signal: a single-line payload is submitted when it has left the live input box
// (lineStillAtPrompt) and a multi-line payload — which never sits as one matchable
// row — when multiLineSubmitted reads a positive signal off the pane. Neither
// depends on matching the payload text against a single captured row, so a payload
// wider than the pane (which wraps across rows) is not mistaken for submitted
// (BOS-489). Both fail toward "still pending" so a delivery that loaded but never
// executed surfaces as an error rather than a silent no-op.
//
// The returned SubmitOutcome names which of the exits was taken, so the caller
// does not have to re-derive it from the error: OutcomeSubmitted with a nil
// error, OutcomeQueued for a payload that left the composer into the agent's
// visible message queue (errSubmissionQueued — a delivered variant of
// submitted, not of pending), OutcomeNotSubmitted for the elapsed budget
// (errSubmissionPending), OutcomeUnconfirmed for a capture failure, a cancelled
// context, or a budget that elapsed with no input box drawn at all — the cases
// where the pane state is genuinely unknown.
//
// The no-input-box case matters because the two shape predicates disagree about
// it: lineStillAtPrompt treats an absent prompt marker as "submitted" (the agent
// is working full-screen), while multiLineSubmitted fails toward "still pending".
// So a multi-line payload sent into a booting agent or behind a full-screen
// overlay reaches the deadline as "pending" without a single positive
// observation. Reporting that as OutcomeNotSubmitted would license
// verifyWithEnterRetry to fire C-u and re-paste the whole plan into a pane it
// cannot see — precisely the double-submit BOS-598 exists to prevent — so it is
// classified as unconfirmed and errSubmissionPending is NOT attached.
func (c *Client) waitForSubmission(ctx context.Context, sessionName, payload string, waitFor, tickEvery time.Duration) (SubmitOutcome, error) {
	if tickEvery <= 0 {
		tickEvery = 100 * time.Millisecond
	}

	trimmed := strings.TrimSpace(payload)
	multiLine := strings.ContainsAny(trimmed, "\r\n")

	deadline := time.NewTimer(waitFor)
	defer deadline.Stop()

	ticker := time.NewTicker(tickEvery)
	defer ticker.Stop()

	// Whether the most recent capture showed a live input box. It is the state
	// the retry would act on, so the deadline branch reads the LAST observation
	// rather than "any observation".
	composerDrawn := false

	for {
		pane, err := c.CapturePane(ctx, sessionName)
		if err != nil {
			// The pane could not be read, so nothing is known about whether the
			// payload was submitted — report it as unconfirmed rather than as a
			// failure to submit.
			return OutcomeUnconfirmed, fmt.Errorf("verify command submission: %w", err)
		}
		composerDrawn = bottomMostPromptMarkerIdx(strings.Split(pane, "\n")) != -1
		submitted := false
		if multiLine {
			submitted = multiLineSubmitted(pane, payload)
		} else {
			submitted = !lineStillAtPrompt(pane, trimmed)
		}
		if submitted {
			// The payload has left the composer, so it IS delivered. The only
			// question left is which kind of delivered, and the agent answers it
			// on the pane: a message consumed by an idle agent leaves no queue
			// behind, while one accepted mid-turn leaves the composer showing the
			// agent's queued-messages hint with the payload echoed above it.
			//
			// This refinement is safe by construction, and the property is the
			// whole reason the QUEUED verdict lives here rather than on the
			// timeout path: it can only relabel an ALREADY-POSITIVE submitted
			// verdict, never turn a negative into "delivered". A queued marker
			// that rots (the hint text is a placeholder, and placeholders rotate
			// — BOS-597) degrades to plain OutcomeSubmitted, which is still
			// "delivered, do not resend". Nothing is lost but the label.
			if paneShowsPayloadQueued(pane, payload) {
				return OutcomeQueued, fmt.Errorf("%q left the composer on tmux session %q into the agent's queued-message list, so it was accepted behind the running turn: %w", trimmed, sessionName, errSubmissionQueued)
			}
			return OutcomeSubmitted, nil
		}

		select {
		case <-ctx.Done():
			return OutcomeUnconfirmed, ctx.Err()
		case <-deadline.C:
			if !composerDrawn {
				// The observation, not the verdict: see composerVanishedErr.
				return OutcomeUnconfirmed, fmt.Errorf("no live composer was drawn on tmux session %q within the verify budget, so the payload's state is unknown", sessionName)
			}
			// A live composer still rendering the payload is NOT delivered, and
			// this branch says so unconditionally. That is a correction: an
			// earlier revision consulted an agent working-probe here and, when it
			// answered "mid-turn", called the payload QUEUED — delivered, do not
			// resend. Real captures refuted the premise that made that sound
			// (testdata/panes/claude_*_real.txt, claude 2.1.220; see capture.sh):
			//
			//   - A genuinely queued message CLEARS the composer. It never
			//     reaches this branch at all — it is caught above as submitted on
			//     the first poll, and relabelled QUEUED from the agent's own
			//     queued-message rendering.
			//   - The composer still holding the payload therefore has exactly
			//     ONE population left: the swallowed Enter this retry exists to
			//     recover. "The agent is working" never meant "the agent took my
			//     payload" — a send into a chat already mid-turn on an EARLIER
			//     request reads as working while holding an unsent payload.
			//
			// So the probe's queued verdict had an empty true-positive population
			// and a live false-positive one: it converted precisely the
			// recoverable case into "delivered", skipped the clear-and-re-deliver
			// recovery (errSubmissionQueued is deliberately not
			// errSubmissionPending), and silently dropped the message. Falling
			// through to OutcomeNotSubmitted is what the captures support.
			//
			// The captures also removed the reason to consult the probe at all
			// here, which is why no agent round-trip remains on this path: on
			// every real mid-turn pane captured for this ticket
			// statusdetect.HasWorkingIndicator answered FALSE, because claude
			// 2.1.220 renders its spinner as "✳ Blanching… (1m 37s · ↓ 244
			// tokens)" with no "esc to interrupt" clause for the probe's primary
			// regex to match — 0 of 50 polls across a full turn saw one. A
			// delivery-correctness decision must not rest on a signal that is
			// absent from the very panes it is meant to classify.
			return OutcomeNotSubmitted, fmt.Errorf("command was not submitted; %q is still present at the tmux prompt: %w", trimmed, errSubmissionPending)
		case <-ticker.C:
		}
	}
}

// composerAfterClear names what a captured pane shows once clearComposer has
// run. The retry acts on it, so it distinguishes the two ways "the composer is
// not holding the payload" can arise: an input box drawn and empty (the clear
// took) versus no input box at all (the payload may have been accepted and the
// agent gone full-screen — unknowable, and NOT a licence to press Enter).
type composerAfterClear int

const (
	// composerClearUnknown: no live input box on screen after the clear.
	composerClearUnknown composerAfterClear = iota
	// composerClearTook: input box drawn, no longer holding the payload.
	composerClearTook
	// composerClearMissed: input box drawn, still holding the payload.
	composerClearMissed
)

// classifyComposerAfterClear reads the three states off one captured pane.
//
// The "took" test is composerEmptyOf, NOT the submission oracle
// (multiLineSubmitted). The two questions look alike but are not: "was this
// submitted?" is satisfied by agent activity below the marker, and C-u cannot
// produce agent activity, so activity is never evidence a CLEAR worked. Reusing
// the wider oracle meant a pane that demonstrably still held the payload but had
// an activity glyph below the marker classified as composerClearTook — the one
// state that licenses the retry to re-type and press Enter, concatenating a
// second copy onto the first.
func classifyComposerAfterClear(pane, payload string) composerAfterClear {
	if bottomMostPromptMarkerIdx(strings.Split(pane, "\n")) == -1 {
		return composerClearUnknown
	}
	if composerEmptyOf(pane, payload) {
		return composerClearTook
	}
	return composerClearMissed
}

// composerEmptyOf reports whether the live input box is proven to no longer hold
// the payload: a prompt marker is drawn, its row does not hold the payload
// (composerHoldsPayload, which covers a bare glyph and a rotating placeholder
// hint alike), and no row below it is one of the payload's own lines.
//
// It is deliberately strictly narrower than multiLineSubmitted, which is the
// submission oracle and carries an agent-activity disjunct that is correct for
// that job and wrong for this one. Ambiguity here resolves to "not empty", so
// the retry loops or errors rather than re-typing onto a composer that may still
// hold the user's text.
func composerEmptyOf(pane, payload string) bool {
	lines := strings.Split(pane, "\n")
	markerIdx := bottomMostPromptMarkerIdx(lines)
	if markerIdx == -1 {
		return false
	}
	if composerHoldsPayload(strings.TrimSpace(lines[markerIdx]), payload) {
		return false
	}
	// A composer that renders the glyph on its own row ABOVE the pasted
	// continuation lines still holds the payload, however empty the marker row
	// reads.
	payloadLines := make(map[string]struct{})
	for _, l := range strings.Split(payload, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			payloadLines[t] = struct{}{}
		}
	}
	for _, l := range lines[markerIdx+1:] {
		if _, isPayload := payloadLines[strings.TrimSpace(l)]; isPayload {
			return false
		}
	}
	return true
}

// maxComposerClearPresses bounds how many times the Enter-retry will press C-u
// trying to empty a composer that keeps surrendering only part of the payload.
// It exists because C-u is line-scoped: a multi-line plan legitimately needs
// more than one press, and converging is the outcome that recovers the send.
const maxComposerClearPresses = 3

// baselineFootprintPolls is how many looks the baseline poll gets inside its
// window. A capture that draws no input box is a repaint frame — the box is
// erased and redrawn, so it clears within a frame or two — and a couple of extra
// looks is the whole difference between outlasting one and failing a healthy
// delivery over it. The window, not this count, is what bounds the wait.
const baselineFootprintPolls = 4

// composerFootprint is the payload AS THE LIVE INPUT BOX IS CURRENTLY RENDERING
// IT: the marker row's content, followed by every below-marker row that is one
// of the payload's own lines. Comparing it across a C-u is what tells a press
// that did NOTHING (footprint identical, so the held payload is intact and safe
// to submit as-is) from one that killed PART of the payload (footprint changed
// while the box still reads as holding it, so what is on screen is mutilated).
// The content predicates cannot make that distinction themselves: they are
// prefix-based for wrapping immunity (BOS-489), so a truncated payload and an
// intact one both read as "still holding".
//
// It is deliberately narrower than "every row below the marker". The statusline
// and footer chrome render BELOW the live input box — see
// testdata/panes/codex_pending.txt, where "gpt-5.6-terra high · … · Context 100%
// left" sits two rows under the composer — and the two captures are a whole
// clear-verify slice apart. A ticking counter, a rotating hint, or a one-row
// vertical shift would flip a whole-region comparison to "changed" and
// manufacture a hard failure out of a healthy retry. Keying on the payload's own
// text makes benign repaints invisible: a killed row stops matching the payload,
// which is the signal, while chrome never matched to begin with. Returns "" when
// no input box is drawn.
//
// A row counts as the composer's when it is any CONTIGUOUS SLICE of one of the
// payload's lines, not only a whole line. That is what a wrapped payload looks
// like: a line wider than the pane is broken across several physical rows, and
// none of those rows equals a whole payload line. Requiring equality made every
// such row invisible, so a C-u that killed the tail of an over-wide line left
// the footprint identical on both sides of the clear — the press read as a
// no-op, the box read as still holding the payload intact, and the bare Enter
// submitted the truncated remainder as a success. Over-wide lines are the normal
// case for the multi-line payloads this retry path serves.
//
// The scan stops at the first non-payload row rather than skipping it, which is
// what keeps chrome out now that a short row can match by slice: continuation
// rows always abut the marker row, so anything past the first foreign row is
// chrome by construction. Blank rows are stepped over, since a payload with a
// blank line renders one. A row that does slip in can only ever ADD a difference
// across the clear, and a spurious difference fails loudly as not-submitted —
// never the silent submit this predicate exists to prevent.
func composerFootprint(pane, payload string) string {
	lines := strings.Split(pane, "\n")
	markerIdx := bottomMostPromptMarkerIdx(lines)
	if markerIdx == -1 {
		return ""
	}

	payloadLines := payloadTextLines(payload)

	content, _ := composerContent(strings.TrimSpace(lines[markerIdx]))
	held := []string{content}
	for _, l := range lines[markerIdx+1:] {
		trimmed := strings.TrimSpace(l)
		if trimmed == "" {
			continue
		}
		if !isPayloadSlice(trimmed, payloadLines) {
			break
		}
		held = append(held, trimmed)
	}
	return strings.Join(held, "\n")
}

// payloadTextLines is the payload's non-blank lines, trimmed — the units a
// composer renders and the ones the row predicates match against.
func payloadTextLines(payload string) []string {
	var lines []string
	for _, l := range strings.Split(payload, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			lines = append(lines, t)
		}
	}
	return lines
}

// isPayloadSlice reports whether a trimmed below-marker row is the composer
// rendering some contiguous part of the payload — a whole line, or one wrapped
// slice of a line too wide for the pane.
func isPayloadSlice(trimmed string, payloadLines []string) bool {
	for _, line := range payloadLines {
		if strings.Contains(line, trimmed) {
			return true
		}
	}
	return false
}

// footprintRendersWholePayload reports whether a footprint accounts for the
// ENTIRE payload rather than some leading part of it.
//
// It compares with every space removed, because where the terminal chose to
// wrap, and how far it indented the continuation rows, are facts about the pane
// geometry and not about what the composer holds. Collapsing them is what lets
// one payload have many equally-correct renderings.
//
// This is the check that makes the baseline mean something. Every mutilation
// decision in verifyWithEnterRetry is taken by COMPARING against the baseline,
// which establishes only that nothing changed — worthless if the composer was
// already holding a truncated payload when the retry first looked at it, since
// that reading is perfectly stable and would license a bare Enter onto the
// fragment. The content predicates cannot catch this on their own: they are
// prefix-based for wrapping immunity (BOS-489), so a truncated payload reads as
// "still holding" exactly like an intact one.
func footprintRendersWholePayload(footprint, payload string) bool {
	return withoutSpace(footprint) == withoutSpace(payload)
}

// withoutSpace strips every space from s, so two renderings of the same text
// that wrapped at different columns compare equal.
func withoutSpace(s string) string {
	return strings.Join(strings.Fields(s), "")
}

// awaitComposerFootprint polls capture-pane until the composer renders a
// READABLE footprint, and returns the first one it sees — the baseline every
// mutilation decision in verifyWithEnterRetry is taken against.
//
// It polls rather than taking one capture because a single sample can land
// mid-repaint on a pane that draws no input box at all, and an absent footprint
// is not a fact about the payload: it says only that this instant was not
// readable as a composer. The caller has already PROVEN the payload is sitting
// in the composer, so a marker-less capture here is a lagging repaint to be
// outlasted, not an answer. An empty return means the composer never became
// readable, which the caller treats as "no key may be pressed" rather than as
// evidence either way.
//
// The bound is a fixed number of looks spaced across waitFor, not a deadline
// checked after each one. Both are bounded, but a deadline can expire during the
// FIRST capture on a loaded machine and reduce this poll to the single sample it
// exists not to take — the guarantee that matters here is that an unreadable
// frame is always looked past, and a look count is what states it.
func (c *Client) awaitComposerFootprint(ctx context.Context, sessionName, payload string, waitFor, tickEvery time.Duration) (string, error) {
	// Space the looks across waitFor rather than at the caller's tick, which is
	// sized for the clear-verify windows and would put every look after this
	// poll's whole slice of the budget had already elapsed.
	if pollTick := waitFor / baselineFootprintPolls; tickEvery <= 0 || tickEvery > pollTick {
		tickEvery = pollTick
	}
	if tickEvery < 0 {
		tickEvery = 0
	}

	for look := 1; ; look++ {
		pane, err := c.CapturePane(ctx, sessionName)
		if err != nil {
			return "", err
		}
		if footprint := composerFootprint(pane, payload); footprint != "" {
			return footprint, nil
		}
		if look == baselineFootprintPolls {
			return "", nil
		}

		wait := time.NewTimer(tickEvery)
		select {
		case <-ctx.Done():
			wait.Stop()
			return "", ctx.Err()
		case <-wait.C:
		}
	}
}

// awaitComposerCleared polls capture-pane until the live composer no longer
// holds the payload, and is the CHECKED postcondition of clearComposer. tmux's
// C-u is kill-to-start-of-line, which is not the same thing as emptying a
// composer holding a multi-line plan, and no fixture in this repo demonstrates
// that it is — so the retry confirms rather than assumes.
//
// It returns early only on composerClearTook, the one state that licenses the
// retry to type and submit; the other two are decided from the LAST capture at
// the deadline, so a composer that is slow to repaint is not misread. It errors
// only when the answer is unknowable (a capture failure or a cancelled context);
// the returned error is deliberately UNCLASSIFIED, because the delivery state
// that applies depends on what the caller had already proven — see
// verifyWithEnterRetry, which reaches this only after confirming the payload is
// still in the composer and therefore classifies these as OutcomeNotSubmitted.
// It also returns the LAST pane it captured, so the caller can compare the
// composer region across the clear.
func (c *Client) awaitComposerCleared(ctx context.Context, sessionName, payload string, waitFor, tickEvery time.Duration) (composerAfterClear, string, error) {
	if tickEvery <= 0 {
		tickEvery = 100 * time.Millisecond
	}

	deadline := time.NewTimer(waitFor)
	defer deadline.Stop()

	ticker := time.NewTicker(tickEvery)
	defer ticker.Stop()

	var (
		state composerAfterClear
		last  string
	)
	for {
		pane, err := c.CapturePane(ctx, sessionName)
		if err != nil {
			return composerClearUnknown, "", fmt.Errorf("verify composer cleared on tmux session %q: %w", sessionName, err)
		}
		state, last = classifyComposerAfterClear(pane, payload), pane
		if state == composerClearTook {
			return state, last, nil
		}

		select {
		case <-ctx.Done():
			return composerClearUnknown, "", ctx.Err()
		case <-deadline.C:
			return state, last, nil
		case <-ticker.C:
		}
	}
}

// multiLineSubmitted reports whether the multi-line payload pasted into the
// composer has been submitted. A multi-line plan never sits as one matchable
// prompt row, so instead of checking for its literal text we read two positive
// signals off the bottom-most prompt-marker row (the live input box): agent
// activity rendered below it (the agent accepted the paste and started working)
// OR a cleared composer (the input box no longer holds the payload, having
// emptied on submit — see composerHoldsPayload, which covers both a bare glyph
// and a placeholder hint). Absent either signal — or a prompt marker at all — it
// fails toward "still pending" so a paste that loaded but never executed
// surfaces as an error rather than a silent no-op.
//
// The agent-activity scan is payload-aware: a plan pasted into the composer but
// NOT submitted still shows its own lines below the marker, and a plan line that
// leads with an agent-activity glyph (e.g. a "•" or "·" bullet) would otherwise
// be mistaken for the agent's own activity — a false "submitted" that resurrects
// the silent no-op this guards against. So a below-marker row that matches one
// of the payload's own lines is treated as still-in-composer content, never as
// activity; only a distinct row the agent itself rendered qualifies.
func multiLineSubmitted(pane, payload string) bool {
	lines := strings.Split(pane, "\n")

	markerIdx := bottomMostPromptMarkerIdx(lines)
	if markerIdx == -1 {
		return false
	}

	// The payload's own trimmed, non-empty lines. A below-marker row matching
	// one of these is the pasted plan still sitting in the composer, not agent
	// output, so it must not be read as activity.
	payloadLines := make(map[string]struct{})
	for _, l := range strings.Split(payload, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			payloadLines[t] = struct{}{}
		}
	}

	// Agent activity below the marker: the paste was accepted and the agent is
	// responding, so the payload has left the prompt (mirrors lineStillAtPrompt).
	// Skip rows that are the payload's own lines echoed in the composer, but note
	// that the payload is still visible below the marker so the cleared-composer
	// signal below cannot fire on it.
	payloadStillBelowMarker := false
	for _, l := range lines[markerIdx+1:] {
		trimmed := strings.TrimSpace(l)
		if _, isPayload := payloadLines[trimmed]; isPayload {
			payloadStillBelowMarker = true
			continue
		}
		if isAgentActivity(trimmed) {
			return true
		}
	}

	// Cleared composer: the input box no longer holds the payload, having emptied
	// on submit. Only trust a cleared marker row when NONE of the payload's own
	// lines are still visible below it: a composer that renders the prompt glyph on
	// its own row ABOVE the pasted continuation lines (the "❯\nline one\nline two"
	// shape, or a payload with a blank leading line) is an unsubmitted composer,
	// not a cleared one — trusting the cleared row there would report a swallowed
	// paste as submitted and resurrect the silent no-op.
	if payloadStillBelowMarker {
		return false
	}
	return !composerHoldsPayload(strings.TrimSpace(lines[markerIdx]), payload)
}

// queuedMessagesHintRe matches the placeholder an agent renders IN its live
// composer while it holds messages queued behind a running turn — "Press up to
// edit queued messages" on the captured claude 2.1.220 pane
// (testdata/panes/claude_queued_midturn_real.txt).
//
// It is matched against the composer's own content, never the whole pane, so
// ordinary transcript text that happens to discuss queued messages cannot
// produce a queued verdict. The wording is a placeholder hint and placeholders
// rotate (BOS-597 caught a codex composer cycling through several), so this
// deliberately keys on the stable noun phrase rather than the full sentence,
// and every caller degrades safely if it rots — see paneShowsPayloadQueued.
var queuedMessagesHintRe = regexp.MustCompile(`(?i)queued messages?\b`)

// paneShowsPayloadQueued reports whether the pane shows the payload sitting in
// the agent's own queue rather than consumed by it. It is the QUEUED-specific
// discriminator this ticket went looking for, and unlike the working-probe
// verdict it replaced, it reads a shape that ONLY a genuinely accepted message
// produces.
//
// Both halves come from real captures (claude 2.1.220, see capture.sh):
//
//	  ❯ also update the changelog          <- the payload, echoed into the queue
//	  ❯ also update the changelog
//	 ─────────────────────────────
//	❯ Press up to edit queued messages     <- the live composer, CLEARED to a hint
//	 ─────────────────────────────
//
// versus the swallowed-Enter pane, where the live composer still holds the
// payload itself and no queue hint exists anywhere
// (testdata/panes/claude_swallowed_enter_midturn_real.txt).
//
// So it requires BOTH: the live composer cleared to the queue hint, AND the
// payload echoed ABOVE that composer. The second half is what ties the verdict
// to THIS payload rather than to some other message the agent had already
// queued.
//
// Every failure of either half degrades to plain OutcomeSubmitted — still
// "delivered, do not resend" — because the caller consults this only after the
// payload has been positively observed leaving the composer. It can refine a
// delivered verdict; it can never manufacture one.
//
// That bound is what makes the echo half's imprecision affordable. It inherits
// composerHoldsPayload's prefix matching, which a very short queued line can
// satisfy for the wrong payload, so it is corroboration rather than proof. Both
// directions of error cost only the label — SUBMITTED read as QUEUED or the
// reverse — and both of those already mean delivered. A stricter equality match
// would trade that for silence on any wrapped payload, which is the error that
// would actually mislead an operator.
func paneShowsPayloadQueued(pane, payload string) bool {
	lines := strings.Split(pane, "\n")
	markerIdx := bottomMostPromptMarkerIdx(lines)
	if markerIdx == -1 {
		return false
	}
	hint, ok := composerContent(strings.TrimSpace(lines[markerIdx]))
	if !ok || !queuedMessagesHintRe.MatchString(hint) {
		return false
	}
	return payloadEchoedAbove(lines[:markerIdx], payload)
}

// payloadEchoedAbove reports whether any row above the live composer carries the
// payload. The queue echo renders with its own prompt glyph and a leading indent
// ("  ❯ also update the changelog"), which is precisely the shape
// composerHoldsPayload already parses, so this reuses it rather than inventing a
// second notion of "this row holds the payload" — and inherits its wrapping
// tolerance for a payload wider than the pane (BOS-489) for free.
func payloadEchoedAbove(above []string, payload string) bool {
	for _, row := range above {
		if composerHoldsPayload(strings.TrimSpace(row), payload) {
			return true
		}
	}
	return false
}

// lineStillAtPrompt reports whether a single-line payload is still sitting in the
// live input box (not yet submitted). It shares the shape-agnostic submission
// signal used for multi-line payloads (multiLineSubmitted) rather than matching
// the payload against one captured pane row: a single line wider than the pane
// wraps across rows, so the bottom-most prompt row can never Contains the whole
// line — the historical text-match returned false for any wrapped payload and
// reported it as submitted, suppressing the swallowed-Enter retry (BOS-489). A
// wrapped line behaves exactly like a multi-line payload here — it is never one
// matchable row — so the same wrapping-immune signal (cleared composer OR agent
// activity below the marker) is the right predicate for both.
//
// The one single-line-specific rule preserved from the original: when no prompt
// marker is present at all (the agent is working full-screen with no input box
// drawn), the payload has left the prompt, so report submitted. multiLineSubmitted
// fails that case toward "still pending"; the single-line path must not, so the
// no-marker check stays here.
func lineStillAtPrompt(pane, line string) bool {
	if bottomMostPromptMarkerIdx(strings.Split(pane, "\n")) == -1 {
		return false
	}
	return !multiLineSubmitted(pane, line)
}

// promptMarkers are the leading glyphs an agent's input box renders. Shared by
// hasPromptMarker and composerContent so the list lives in one place.
var promptMarkers = []string{"❯", "›", ">"}

// bottomMostPromptMarkerIdx returns the index of the bottom-most row carrying an
// input-box prompt marker, or -1 if none is present. It is the live input box:
// everything below it is footer / statusline chrome, so submission checks scan
// from this row. Shared by lineStillAtPrompt and multiLineSubmitted.
func bottomMostPromptMarkerIdx(lines []string) int {
	for i := len(lines) - 1; i >= 0; i-- {
		if hasPromptMarker(strings.TrimSpace(lines[i])) {
			return i
		}
	}
	return -1
}

// hasPromptMarker reports whether a trimmed pane row begins with one of the
// input-box prompt indicators.
func hasPromptMarker(text string) bool {
	_, ok := composerContent(text)
	return ok
}

// composerContent returns the text a prompt-marker row carries after its glyph,
// and whether the row was a prompt-marker row at all.
func composerContent(trimmed string) (string, bool) {
	for _, marker := range promptMarkers {
		if rest, ok := strings.CutPrefix(trimmed, marker); ok {
			return strings.TrimSpace(rest), true
		}
	}
	return "", false
}

// composerHoldsPayload reports whether the live input box still holds the
// payload the verifier is waiting on. It replaces the former glyph-only check
// (promptRowIsEmpty, removed with this change) as the "composer cleared" signal,
// because a bare glyph is not the only shape a cleared composer takes (BOS-597).
//
// A real codex pane captured one second after a submitted send
// (testdata/panes/codex_submitted.txt, codex-cli 0.146.0) shows the live
// composer cleared to a *rotating placeholder hint* — "› Improve documentation
// in @filename" in that capture, "› Run /review on my current changes" in
// another — rather than to a bare "›". The glyph-only check therefore read the
// cleared box as still holding the payload, and because codex renders its
// working bullet ABOVE the input box the activity scan never saw it either.
// Both positive signals missed and a delivered message was reported unsubmitted.
//
// Matching the *payload* rather than enumerating placeholder strings is what
// makes this robust: the hints rotate between codex runs, so an allowlist would
// rot with every CLI release. It stays wrapping-immune (BOS-489) by treating a
// prefix as a hold — a payload wider than the pane leaves only its first slice
// on the marker row, and that is still an unsubmitted payload.
//
// Fail-safe direction is preserved: an ambiguous row (e.g. content that happens
// to share a prefix with the payload) resolves to "still holding", which keeps
// the verifier reporting still-pending and surfacing a swallowed Enter loudly
// rather than silently passing an unsubmitted payload (BOS-89 / BOS-489).
func composerHoldsPayload(trimmedMarkerRow, payload string) bool {
	content, isPromptRow := composerContent(trimmedMarkerRow)
	if !isPromptRow || content == "" {
		return false
	}
	first := ""
	for _, l := range strings.Split(payload, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			first = t
			break
		}
	}
	if first == "" {
		return false
	}
	return strings.HasPrefix(first, content) || strings.HasPrefix(content, first)
}

// agentActivityMarkers are the leading glyphs that mark a pane row as agent
// activity below the input box — an assistant/tool response, tool output, or a
// thinking/working spinner — rather than input-box footer or statusline chrome.
// The verifier runs against both Claude Code and Codex panes, so it covers
// both grammars: the Claude working/output markers statusdetect already trusts
// (lib/bossalib/statusdetect/question.go optionStopMarkers: ⎿ ⏺ · ✻) plus the
// "✽" spinner frame, and Codex's working bullet (plugins/bossd-plugin-codex/
// question.go codexWorking: "• Working (…)"). This lets the predicate recognise
// the activity row each agent renders immediately after accepting a line and
// before any response body appears.
var agentActivityMarkers = []string{
	"⏺", // Claude response / tool-use bullet (U+23FA)
	"⎿", // Claude tool-result branch (U+23BF)
	"·", // Claude working spinner (U+00B7)
	"✻", // Claude thinking spinner (U+273B)
	"✽", // Claude thinking spinner (U+273D)
	"•", // Codex working bullet (U+2022)
}

// isAgentActivity reports whether a trimmed pane row begins with an agent
// activity marker. Matching only the leading glyph keeps custom statusline rows
// safe (e.g. "◉ xhigh · /effort" has a mid-row "·" but does not start with
// one). It is deliberately conservative on unknown rows: misclassifying footer
// chrome as activity would let lineStillAtPrompt report a still-pending payload
// as submitted — the silent cron no-op this guards against — so only these
// distinctive glyphs qualify and arbitrary footer/statusline text never does.
func isAgentActivity(text string) bool {
	for _, marker := range agentActivityMarkers {
		if strings.HasPrefix(text, marker) {
			return true
		}
	}
	return false
}
