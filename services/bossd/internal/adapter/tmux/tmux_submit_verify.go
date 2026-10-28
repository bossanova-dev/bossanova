package tmux

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/recurser/bossd/internal/domain"
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
type SubmitOutcome = domain.SubmitOutcome

const (
	OutcomeUnclassified     = domain.OutcomeUnclassified
	OutcomeSubmitted        = domain.OutcomeSubmitted
	OutcomeNotSubmitted     = domain.OutcomeNotSubmitted
	OutcomeUnconfirmed      = domain.OutcomeUnconfirmed
	OutcomeQueued           = domain.OutcomeQueued
	OutcomeBlockedByModal   = domain.OutcomeBlockedByModal
	OutcomeComposerOccupied = domain.OutcomeComposerOccupied
)

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

func (e *submitError) SubmitOutcome() domain.SubmitOutcome { return e.outcome }

// OutcomeOf reports the submit outcome an error carries. A nil error is
// OutcomeSubmitted; an error from anywhere other than the verifier is
// OutcomeUnclassified, so a caller that maps outcomes to behaviour keeps its
// existing handling for every non-verifier failure.
func OutcomeOf(err error) SubmitOutcome { return domain.OutcomeOf(err) }

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
func (c *Client) verifyWithEnterRetry(ctx context.Context, sessionName string, drawn composerDrawing, opts sendPlanOpts, redeliver func(context.Context) error) error {
	payload := drawn.text
	outcome, err := c.waitForSubmissionOrReceipt(ctx, sessionName, payload, opts.submitVerifyWait, opts.submitVerifyTick, opts.submitted, opts.receiptRequired)
	if errors.Is(err, errSubmissionPending) && opts.submitted != nil && opts.receiptGrace > 0 {
		// No keys during the grace: a late hook or repaint can still prove that
		// the original Enter took, before clearing could disguise that submission.
		outcome, err = c.waitForSubmissionOrReceipt(ctx, sessionName, payload, opts.receiptGrace, opts.submitVerifyTick, opts.submitted, opts.receiptRequired)
	}
	if err == nil {
		return nil
	}
	// A receipt that lands as the budget expires is still proof the payload
	// was taken: never clear and re-deliver a second copy past it (BOS-1506).
	// A queued verdict is already delivered and keeps its more precise label.
	if outcome != OutcomeQueued && receiptConfirms(opts.submitted) {
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
	if !drawn.renderedBy(baseline) {
		// The box holds something other than the whole payload or the
		// placeholder this send's paste drew: an operator who cleared bossd's
		// text and typed a message of their own, whatever it begins with, or a
		// copy that lost part of itself, which bossd cannot tell apart from the
		// first. A C-u would erase whichever it is, so no key is sent.
		return classifySubmit(OutcomeNotSubmitted, fmt.Errorf("command was not submitted to tmux session %q: the composer holds text other than the whole payload, which may be an operator's own message, so no key was sent: %w", sessionName, errSubmissionPending))
	}
	// mutilated latches once the payload on screen is PROVABLY not the intact
	// one. It guards the one way a post-clear sample can match the baseline and
	// still be worthless: agreeing with a baseline that was never a reading of
	// the whole payload. Once that is so, a reading that agrees with the baseline
	// is either a stale screen replaying the pre-clear pane or a faithful reading
	// of a fragment — never evidence the box holds the payload intact.
	//
	// It starts latched when the baseline is the paste's placeholder rather
	// than the payload's text. Comparing against the baseline can only ever
	// establish that NOTHING CHANGED, and a placeholder that survives a press
	// says nothing about whether the payload behind it is intact, so only a
	// clear that PROVES the box empty re-delivers and submits.
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
		case composerClearUnproven:
			// The payload is gone but the box is not proven empty: it holds text
			// an operator may have typed after the press. Typing the payload
			// after it and pressing Enter would submit both as one message.
			return classifySubmit(OutcomeNotSubmitted, fmt.Errorf("command was not submitted to tmux session %q: after composer clear %d the box holds text other than the payload, which may be an operator's own message, so no further key was sent: %w", sessionName, press, errSubmissionPending))
		case composerClearTook:
			if receiptConfirms(opts.submitted) {
				return nil
			}
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
				// not license an Enter.
				mutilated = true
				if after != held && !clearLeft(held, after, ctrlUKillOf(opts.readyMarker)) {
					// Not what this press leaves of the read before it, so someone
					// else may have changed the box, and another press could erase
					// their text. Stop with no further key.
					return classifySubmit(OutcomeNotSubmitted, fmt.Errorf("command was not submitted to tmux session %q: the composer changed in a way composer clear %d cannot explain, so no further key was sent: %w", sessionName, press, errSubmissionPending))
				}
				if press == maxComposerClearPresses {
					return classifySubmit(OutcomeNotSubmitted, &partialPayload{held: after, err: fmt.Errorf("command was not submitted to tmux session %q: %d composer clears left a partial payload behind, so no Enter was sent: %w", sessionName, press, errSubmissionPending)})
				}
				if after == held {
					// A stuck partial clear: the box holds a fragment and this press
					// changed nothing, so it will not empty. Fail exactly as loudly as
					// the press cap above — confirmed not submitted, no Enter sent.
					return classifySubmit(OutcomeNotSubmitted, &partialPayload{held: after, err: fmt.Errorf("command was not submitted to tmux session %q: a composer clear left a partial payload behind and press %d changed nothing, so no Enter was sent: %w", sessionName, press, errSubmissionPending)})
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
				return classifySubmit(OutcomeNotSubmitted, &partialPayload{held: after, err: fmt.Errorf("command was not submitted to tmux session %q: the composer holds a partial payload and press %d changed nothing, so no Enter was sent: %w", sessionName, press, errSubmissionPending)})
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
	// The clear or re-delivery can race a receipt, including an intact but
	// stale composer in the bare-Enter arm. Never press Enter past that proof.
	if receiptConfirms(opts.submitted) {
		return nil
	}
	if enterErr := c.sendEnter(ctx, sessionName); enterErr != nil {
		return classifySubmit(OutcomeNotSubmitted, enterErr)
	}
	retryOutcome, retryErr := c.waitForSubmissionOrReceipt(ctx, sessionName, payload, opts.submitVerifyWait, opts.submitVerifyTick, opts.submitted, opts.receiptRequired)
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

// ctrlUKill is how much of a wrapped composer line one C-u removes.
type ctrlUKill int

const (
	// killsOneRow is Claude Code's ctrl+u, which kills back to the start of
	// the cursor's wrapped row (pressLeft). An agent this package has not
	// measured is held to it too, as the narrower of the two rules.
	killsOneRow ctrlUKill = iota
	// killsLogicalLine is Codex's ctrl+u (kill_line_start, which runs
	// TextArea::kill_to_beginning_of_line in codex-rs/tui). It kills back to
	// the last newline before the cursor, so every wrapped row of a long line
	// goes in one press.
	killsLogicalLine
)

// ctrlUKillOf is the C-u rule of the agent whose composer readyMarker names.
func ctrlUKillOf(readyMarker string) ctrlUKill {
	if readyMarker == codexReadyMarker {
		return killsLogicalLine
	}
	return killsOneRow
}

// clearLeft reports whether after, a composer footprint read after a C-u, is
// what that press can leave of held, the footprint read before it: held less
// its last row, or, for an agent that kills a whole logical line, less one or
// more whole rows from its end. Anything else, such as an operator's own trim
// of several rows or a box they cleared and typed into, does not pass, even
// when what is left begins as the payload does.
func clearLeft(held, after string, kill ctrlUKill) bool {
	h, a := strings.Split(held, "\n"), strings.Split(after, "\n")
	if len(a) >= len(h) || (kill == killsOneRow && len(a) != len(h)-1) {
		return false
	}
	for i, row := range a {
		if withoutSpace(row) != withoutSpace(h[i]) {
			return false
		}
	}
	return true
}

// partialPayload is how verifyWithEnterRetry ends when its C-u presses left
// part of the payload in the composer and it sent no Enter. held is that part,
// as the composer footprint its last read showed it (composerFootprint), and
// each read after a press was what that press leaves of the one before
// (clearLeft). It is
// the only composer text other than the whole payload that the withdrawal may
// clear (composerDrawing.leftByRetry): any other prefix of the payload can be
// an operator's own message that begins the same way.
type partialPayload struct {
	held string
	err  error
}

func (p *partialPayload) Error() string { return p.err.Error() }

func (p *partialPayload) Unwrap() error { return p.err }

// clearComposerCheckWindow / clearComposerCheckTick bound the exported
// ClearComposer's per-press postcondition check. They are deliberately small:
// the caller is on a failure path whose own budget has usually just been spent,
// and the question here — "did that C-u land?" — is answered by a repaint or
// two, not by waiting out a slow composer.
const (
	clearComposerCheckWindow = 300 * time.Millisecond
	clearComposerCheckTick   = 100 * time.Millisecond
)

// ClearComposer removes from a live composer a payload the submit verifier has
// already proven was NOT submitted, so nothing is left sitting at the prompt for
// the next Enter — whoever presses it — to send.
//
// It is the exported counterpart of the clear half of verifyWithEnterRetry, and
// it carries the SAME precondition: call it only with positive evidence that a
// composer is drawn and holds the payload, i.e. an OutcomeNotSubmitted verdict.
// C-u into a working full-screen pane is an unwanted keystroke, and after any
// other outcome the payload may have been delivered — clearing then would erase
// nothing useful and could cut into text this code did not put there.
//
// That verdict LICENSES the attempt but does not establish the precondition, so
// this REVALIDATES it against the live pane before the first press: an
// OutcomeNotSubmitted can be reached through an infrastructure error that never
// read a composer at all (sendEnter can fail after tmux already delivered the
// key, classifySubmit above), leaving the payload running. Only a drawn box that
// still holds the payload earns the keystroke.
//
// It presses C-u up to maxComposerClearPresses times, checking the postcondition
// after each press rather than trusting the press, for the same line-scoped
// reason the retry does. It succeeds when no live composer holds the payload any
// more — including the case where the input box is no longer drawn at all, which
// is not something a further press could improve. It errors when the payload is
// still held after the last press, or when the pane could not be read; both mean
// the caller must assume the text is still there.
func (c *Client) ClearComposer(ctx context.Context, sessionName, payload string) error {
	if sessionName == "" {
		return fmt.Errorf("clear composer: empty tmux session name")
	}
	// Read BEFORE pressing. composerClearMissed — box drawn and still holding the
	// payload — is the only state that earns a destructive key. The other two are
	// already this function's success case after a press (the loop below returns
	// nil on both), so skipping them changes no caller-visible result; it only
	// withholds a keystroke that could strike a running pane or a user's own text.
	pane, err := c.CapturePane(ctx, sessionName)
	if err != nil {
		return fmt.Errorf("read composer before clear on tmux session %q: %w", sessionName, err)
	}
	if classifyComposerAfterClear(pane, payload) != composerClearMissed {
		return nil
	}
	for press := 1; press <= maxComposerClearPresses; press++ {
		if err := c.clearComposer(ctx, sessionName); err != nil {
			return err
		}
		state, _, err := c.awaitComposerCleared(ctx, sessionName, payload, clearComposerCheckWindow, clearComposerCheckTick)
		if err != nil {
			return fmt.Errorf("clear composer on tmux session %q: %w", sessionName, err)
		}
		if state != composerClearMissed {
			return nil
		}
	}
	return fmt.Errorf("the composer on tmux session %q still holds the payload after %d clears",
		sessionName, maxComposerClearPresses)
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
	return c.waitForSubmissionOrReceipt(ctx, sessionName, payload, waitFor, tickEvery, nil, false)
}

// receiptConfirms reports whether the caller's submission receipt has arrived.
func receiptConfirms(submitted func() bool) bool {
	return submitted != nil && submitted()
}

// waitForSubmissionOrReceipt is waitForSubmission with the caller's receipt
// consulted first on every poll: a receipt is authoritative proof of
// submission (Claude's UserPromptSubmit hook), so it decides before the pane is
// read, and the pane classification below is the fallback for an agent or a
// pane without one. A nil receipt is exactly waitForSubmission.
//
// receiptRequired marks a receipt that records every turn the agent takes
// (Codex's rollout), so a composer that merely reads empty does not confirm a
// send on its own: Codex can hold a typed burst undrawn, which reads as an empty
// composer while the payload is still unsubmitted. The wait keeps polling for
// the receipt instead, and a budget that ends on an empty composer with no
// receipt is unconfirmed. The queued signal still decides, because a message
// queued behind a running turn is recorded only when that turn ends.
func (c *Client) waitForSubmissionOrReceipt(ctx context.Context, sessionName, payload string, waitFor, tickEvery time.Duration, submitted func() bool, receiptRequired bool) (SubmitOutcome, error) {
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
	// Whether the most recent capture read the payload as gone from the
	// composer while receiptRequired withheld that from confirming the send.
	clearedWithoutReceipt := false

	for {
		if receiptConfirms(submitted) {
			return OutcomeSubmitted, nil
		}
		pane, err := c.CapturePane(ctx, sessionName)
		if err != nil {
			// The pane could not be read, so nothing is known about whether the
			// payload was submitted — report it as unconfirmed rather than as a
			// failure to submit.
			return OutcomeUnconfirmed, fmt.Errorf("verify command submission: %w", err)
		}
		composerDrawn = composerMarkerIdx(strings.Split(pane, "\n")) != -1
		left := false
		if multiLine {
			left = multiLineSubmitted(pane, payload)
		} else {
			left = !lineStillAtPrompt(pane, trimmed)
		}
		clearedWithoutReceipt = false
		if left {
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
			// "delivered, do not resend". Nothing is lost but the label. With
			// receiptRequired it is more than a label: a queued payload has no
			// receipt until the running turn ends, so this check is what keeps
			// it from reading unconfirmed.
			if paneShowsPayloadQueued(pane, payload) {
				return OutcomeQueued, fmt.Errorf("a payload left the composer on tmux session %q into the agent's queued-message list, so it was accepted behind the running turn: %w", sessionName, errSubmissionQueued)
			}
			if !receiptRequired {
				return OutcomeSubmitted, nil
			}
			clearedWithoutReceipt = true
		}

		select {
		case <-ctx.Done():
			return OutcomeUnconfirmed, ctx.Err()
		case <-deadline.C:
			if !composerDrawn {
				// The observation, not the verdict: see composerVanishedErr.
				return OutcomeUnconfirmed, fmt.Errorf("no live composer was drawn on tmux session %q within the verify budget, so the payload's state is unknown", sessionName)
			}
			if clearedWithoutReceipt {
				return OutcomeUnconfirmed, fmt.Errorf("the composer on tmux session %q reads empty but the agent recorded no new turn within the verify budget, so the payload's state is unknown", sessionName)
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
			// The working probe answered FALSE on these 2.1.220 spinner rows
			// before BOS-1995. It now recognizes them, but working still does not
			// prove Claude accepted this payload, so no agent round-trip belongs
			// on this delivery-correctness path.
			return OutcomeNotSubmitted, fmt.Errorf("command was not submitted; a payload is still present at the tmux prompt: %w", errSubmissionPending)
		case <-ticker.C:
		}
	}
}

// composerRowsUnread is the snapshot taken when the pre-paste capture failed.
// It cannot equal any capture's composer rows, so the first readable capture
// after the paste counts as the change the settle wait looks for.
const composerRowsUnread = "\x00unread"

// composerRows returns the live composer's own rows: the bottom-most
// prompt-marker row and the rows below it up to the next horizontal rule.
// Claude draws its input box between two "────" rules, with the working
// spinner above the top rule and the footer below the bottom one, so this
// excludes both — a ticking spinner or footer must not keep the composer from
// reading as settled. Returns "" when no composer is drawn.
func composerRows(pane string) string {
	lines := strings.Split(pane, "\n")
	markerIdx := composerMarkerIdx(lines)
	if markerIdx == -1 {
		return ""
	}
	rows := []string{strings.TrimRight(lines[markerIdx], " ")}
	for _, l := range lines[markerIdx+1:] {
		if isHorizontalRule(strings.TrimSpace(l)) {
			break
		}
		rows = append(rows, strings.TrimRight(l, " "))
	}
	return strings.Join(rows, "\n")
}

// isHorizontalRule reports whether a trimmed row is a box-drawing rule.
func isHorizontalRule(trimmed string) bool {
	if trimmed == "" {
		return false
	}
	for _, r := range trimmed {
		if r != '─' && r != '━' {
			return false
		}
	}
	return true
}

// composerRowsSnapshot captures the composer rows before a paste, for
// awaitComposerSettled to compare against. A failed capture yields
// composerRowsUnread rather than an error: the settle wait is advisory.
func (c *Client) composerRowsSnapshot(ctx context.Context, sessionName string) string {
	pane, err := c.CapturePane(ctx, sessionName)
	if err != nil {
		return composerRowsUnread
	}
	return composerRows(pane)
}

// awaitComposerSettled is the read-only wait between pasting a Claude payload
// and pressing Enter (BOS-1506). An Enter that lands while Claude is still
// ingesting the paste becomes a newline in the composer instead of a submit,
// so it waits for the composer rows to DIFFER from the pre-paste snapshot and
// then for two consecutive captures of those rows to agree.
//
// It only ever runs capture-pane — never a key, never a re-paste — and it has
// no failure mode: a capture error is skipped, and when waitFor expires it
// simply returns, so the caller proceeds to Enter and the submit verifier
// decides what happened. Only the composer rows are compared (composerRows),
// so a working spinner above the input box cannot hold the wait open. It
// returns the rows the composer settled on, or "" when it did not settle.
func (c *Client) awaitComposerSettled(ctx context.Context, sessionName, before string, waitFor, tickEvery time.Duration) string {
	if waitFor <= 0 {
		return ""
	}
	if tickEvery <= 0 {
		tickEvery = sendPlanDefaultPollInterval
	}
	deadline := time.NewTimer(waitFor)
	defer deadline.Stop()
	ticker := time.NewTicker(tickEvery)
	defer ticker.Stop()

	changed := false
	var last string
	for {
		if pane, err := c.CapturePane(ctx, sessionName); err == nil {
			rows := composerRows(pane)
			switch {
			case !changed && rows != before:
				changed, last = true, rows
			case changed && rows == last:
				return rows
			case changed:
				last = rows
			}
		}
		select {
		case <-ctx.Done():
			return ""
		case <-deadline.C:
			return ""
		case <-ticker.C:
		}
	}
}

// placeholderIn returns the collapsed-paste placeholder that composer rows
// (see composerRows) hold for a paste of text, such as
// "[Pasted text #3 +4 lines]", or "" when they hold anything else.
func placeholderIn(rows, text string) string {
	first, rest, _ := strings.Cut(rows, "\n")
	content, isPrompt := composerContent(strings.TrimSpace(first))
	if !isPrompt || strings.TrimSpace(rest) != "" || !claudePlaceholderOf(content, text) {
		return ""
	}
	return strings.TrimSpace(content)
}

type composerDraw struct {
	drawn       bool
	rows        string
	prefixLen   int
	growing     bool
	partialSeen bool
}

// awaitComposerDraws requires two matching full captures before submission.
// Progress is measured against the prefix carried from the previous window,
// so an unchanged partial line cannot buy another window indefinitely.
func (c *Client) awaitComposerDraws(ctx context.Context, sessionName, payload string, waitFor, tickEvery time.Duration, initialPrefix int) (draw composerDraw, err error) {
	if tickEvery <= 0 {
		tickEvery = sendPlanDefaultPollInterval
	}
	deadline := time.NewTimer(waitFor)
	defer deadline.Stop()
	ticker := time.NewTicker(tickEvery)
	defer ticker.Stop()

	last := ""
	longest := initialPrefix
	want := withoutSpace(payload)
	for {
		pane, err := c.CapturePane(ctx, sessionName)
		if err != nil {
			return draw, err
		}
		draw.rows = typedComposerRows(pane, payload)
		text := composerTypedText(draw.rows)
		draw.prefixLen = 0
		if text != "" && strings.HasPrefix(want, text) {
			draw.prefixLen = len(text)
			draw.partialSeen = draw.partialSeen || len(text) < len(want)
			if len(text) > longest {
				draw.growing = true
				longest = len(text)
			}
		}
		if composerShowsPayload(draw.rows, payload) {
			if draw.rows == last {
				draw.drawn = true
				return draw, nil
			}
			last = draw.rows
		} else {
			last = ""
		}
		select {
		case <-ctx.Done():
			return draw, ctx.Err()
		case <-deadline.C:
			return draw, nil
		case <-ticker.C:
		}
	}
}

// composerRowsForError renders composer rows on one line for an error message,
// cut to a length a log line can carry.
func composerRowsForError(rows string) string {
	const maxRunes = 160
	text := strings.Join(strings.Fields(rows), " ")
	if text == "" {
		return "no input box"
	}
	if r := []rune(text); len(r) > maxRunes {
		return strconv.Quote(string(r[:maxRunes])) + " (truncated)"
	}
	return strconv.Quote(text)
}

// composerShowsPayload reports whether composer rows (see typedComposerRows)
// show the payload this send just typed: exactly its text, or a
// collapsed-paste placeholder, which cannot be read and so is taken as it.
func composerShowsPayload(rows, payload string) bool {
	return composerHoldsCollapsedPaste(rows) || composerHoldsExactly(rows, payload)
}

// composerHoldsExactly reports whether composer rows (see typedComposerRows)
// hold exactly text, ignoring wrap and indent. Exact rather than containment,
// so a box that also holds a draft or an earlier copy is not entered: the
// Enter would submit that extra text with it. A shell-mode box has no prompt
// marker; its "!" glyph is the text's own.
func composerHoldsExactly(rows, text string) bool {
	want := withoutSpace(text)
	return want != "" && composerTypedText(rows) == want
}

// composerTypedText normalizes typed input consistently for full and partial
// matches, including shell-mode boxes whose leading ! belongs to the payload.
func composerTypedText(rows string) string {
	first, rest, _ := strings.Cut(rows, "\n")
	content, isPrompt := composerContent(strings.TrimSpace(first))
	if !isPrompt {
		content = first
	}
	return withoutSpace(content + " " + rest)
}

// composerHoldsCollapsedPaste reports whether composer rows lead with the
// placeholder an agent draws in place of a long paste.
func composerHoldsCollapsedPaste(rows string) bool {
	first, _, _ := strings.Cut(rows, "\n")
	content, isPrompt := composerContent(strings.TrimSpace(first))
	return isPrompt && collapsedPastePlaceholderRe.MatchString(content)
}

// typedComposerRows returns the composer rows a typed line is drawn into. A
// shell-mode line is drawn behind a "!" glyph rather than a prompt marker, so
// its rows are the box between the bottom two rules; the "! for shell mode"
// hint below the bottom rule is outside it
// (testdata/panes/claude_typed_shell_mode_real.txt).
func typedComposerRows(pane, line string) string {
	if !strings.HasPrefix(line, shellModePrefix) {
		return composerRows(pane)
	}
	lines := strings.Split(pane, "\n")
	top, bottom, ok := inputBoxRules(lines)
	if !ok {
		return ""
	}
	box := make([]string, 0, bottom-top-1)
	for _, l := range lines[top+1 : bottom] {
		box = append(box, strings.TrimRight(l, " "))
	}
	return strings.Join(box, "\n")
}

// inputBoxRules returns the indexes of the bottom two horizontal rules, which
// bound Claude's input box, and whether the pane has two.
func inputBoxRules(lines []string) (top, bottom int, ok bool) {
	bottom = -1
	for i := len(lines) - 1; i >= 0; i-- {
		if !isHorizontalRule(strings.TrimSpace(lines[i])) {
			continue
		}
		if bottom != -1 {
			return i, bottom, true
		}
		bottom = i
	}
	return -1, -1, false
}

// LaunchComposerState describes what a captured launch pane shows in its live
// input box. It does not establish whether the agent accepted an earlier input.
type LaunchComposerState string

const (
	LaunchComposerEmpty      LaunchComposerState = "empty"
	LaunchComposerHoldsInput LaunchComposerState = "holds-input"
	LaunchComposerHoldsOther LaunchComposerState = "holds-other"
	LaunchComposerNoComposer LaunchComposerState = "no-composer"
)

// ClassifyLaunchComposer reads a launch pane without sending keys. Unstyled
// nonempty text is conservatively treated as a draft unless it is the known
// Codex launch placeholder; styled captures can prove other dim placeholders.
func ClassifyLaunchComposer(pane, payload string) LaunchComposerState {
	lines := strings.Split(plainText(pane), "\n")
	marker := composerMarkerIdx(lines)
	if marker == -1 {
		return LaunchComposerNoComposer
	}
	if strings.TrimSpace(payload) != "" && composerBlockHoldsPayload(lines, marker, payload) {
		return LaunchComposerHoldsInput
	}
	switch readComposer(pane) {
	case composerBlank, composerPlaceholder:
		return LaunchComposerEmpty
	case composerTyped:
		return LaunchComposerHoldsOther
	case composerUndrawn, composerUnstyled:
	}
	content, _ := composerContent(strings.TrimSpace(lines[marker]))
	if content == "Ask Codex to do anything" {
		if marker+1 == len(lines) || strings.TrimSpace(lines[marker+1]) == "" || isHorizontalRule(strings.TrimSpace(lines[marker+1])) {
			return LaunchComposerEmpty
		}
	}
	return LaunchComposerHoldsOther
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
	// composerClearUnproven: input box drawn without the payload, but holding
	// text that is not a placeholder, or drawn in a capture that cannot tell
	// the two apart. Only awaitComposerCleared returns it.
	composerClearUnproven
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
	if composerMarkerIdx(strings.Split(pane, "\n")) == -1 {
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
//
// A box that no longer holds the payload may hold an operator's text instead,
// and plain text cannot tell that from a placeholder. awaitComposerCleared
// therefore calls a clear took only once the box also reads blank, or holds
// nothing but a placeholder in a styled capture (confirmComposerEmpty).
func composerEmptyOf(pane, payload string) bool {
	lines := strings.Split(pane, "\n")
	markerIdx := composerMarkerIdx(lines)
	if markerIdx == -1 {
		return false
	}
	if composerBlockHoldsPayload(lines, markerIdx, payload) {
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
	markerIdx := composerMarkerIdx(lines)
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
			if state, err = c.confirmComposerEmpty(ctx, sessionName, pane); err != nil {
				return composerClearUnknown, "", fmt.Errorf("verify composer cleared on tmux session %q: %w", sessionName, err)
			}
			if state == composerClearTook {
				return state, last, nil
			}
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

// confirmComposerEmpty is composerClearTook when the box in pane, which no
// longer holds the payload, holds nothing else either: it reads blank, or a
// styled capture shows only a placeholder. Otherwise it is
// composerClearUnproven, so nothing is typed after text an operator may have
// typed into the box.
func (c *Client) confirmComposerEmpty(ctx context.Context, sessionName, pane string) (composerAfterClear, error) {
	if readComposer(pane) == composerBlank {
		return composerClearTook, nil
	}
	styled, err := c.captureVisibleStyled(ctx, sessionName)
	if err != nil {
		return composerClearUnknown, err
	}
	switch readComposer(styled) {
	case composerBlank, composerPlaceholder:
		return composerClearTook, nil
	default:
		return composerClearUnproven, nil
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

	markerIdx := composerMarkerIdx(lines)
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
	return !composerBlockHoldsPayload(lines, markerIdx, payload)
}

// composerBlockHoldsPayload reports whether the live composer holds the payload
// anywhere in its block: the marker row plus the continuation rows below it, up
// to the first blank row or horizontal rule.
//
// The marker row alone is not enough once the composer already held text.
// Codex draws only the FIRST line of a multi-line draft after its "›" glyph and
// every later line as an indented continuation row, and typed text lands at the
// cursor, at the end of the draft. A payload typed into a composer still holding
// earlier unsent lines therefore sits at the bottom of the block — on a row of
// its own, or appended to the last row — while the marker row shows the oldest
// line, which never prefix-matches it (testdata/panes/
// codex_stuck_composer_appended_real.txt). Reading only that row reported the
// send submitted, and a verify chat collected ten unsent commands that way.
//
// A block with continuation rows is never a placeholder, which is always one
// row, so its text is matched as a suffix of the whole block with whitespace
// removed — the wrap column and indent are pane geometry, not content. A
// single-row composer keeps composerHoldsPayload's prefix rule alone: a rotating
// placeholder such as "Ask Codex to do anything" ends in ordinary words, so a
// suffix match there would read a short payload as held after a real submit.
func composerBlockHoldsPayload(lines []string, markerIdx int, payload string) bool {
	markerRow := strings.TrimSpace(lines[markerIdx])
	if composerHoldsPayload(markerRow, payload) {
		return true
	}
	content, _ := composerContent(markerRow)
	block := []string{content}
	for _, l := range lines[markerIdx+1:] {
		trimmed := strings.TrimSpace(l)
		if trimmed == "" || isHorizontalRule(trimmed) {
			break
		}
		block = append(block, trimmed)
	}
	want := withoutSpace(payload)
	return len(block) > 1 && want != "" && strings.HasSuffix(withoutSpace(strings.Join(block, "")), want)
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
// Codex marks its queue differently: a header row and "↳" items above a
// composer cleared to a placeholder (codexQueueHoldsPayload).
//
// Every failure of either half degrades to plain OutcomeSubmitted — still
// "delivered, do not resend" — because the caller consults this only after the
// payload has been positively observed leaving the composer. It can refine a
// delivered verdict; it can never manufacture one. The exception is a send whose
// receipt is required (Codex): a queued message reaches the rollout only when
// the running turn ends, so a queue this misses reads UNCONFIRMED, and the
// codex shape must be recaptured when the codex TUI changes it.
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
	markerIdx := composerMarkerIdx(lines)
	if markerIdx == -1 {
		return false
	}
	composer := strings.TrimSpace(lines[markerIdx])
	hint, ok := composerContent(composer)
	if !ok {
		return false
	}
	if queuedMessagesHintRe.MatchString(hint) {
		return payloadEchoedAbove(lines[:markerIdx], payload)
	}
	return !composerHoldsPayload(composer, payload) && codexQueueHoldsPayload(lines[:markerIdx], payload)
}

// codexQueueHeaderRe matches the header codex draws over the messages it holds
// behind a running turn. On codex-cli 0.161.0 the header reads "• Messages to be
// submitted after next tool call (press esc to interrupt and send immediately)",
// each held message follows on an indented "↳ <message>" row, and the composer
// below is cleared to a placeholder such as "› Ask Codex to do anything"
// (testdata/panes/codex_queued_midturn_real.txt, from capture_codex_queued.sh).
var codexQueueHeaderRe = regexp.MustCompile(`(?i)^•\s+messages? to be submitted\b`)

// codexQueuedItemGlyph leads each message in codex's queue block.
const codexQueuedItemGlyph = "↳"

// codexQueueHoldsPayload reports whether the rows directly above codex's live
// composer are its queue block with the payload as one of its items. Walking up
// from the composer, only blank rows, "↳" items and their indented wrap rows may
// sit between it and the header, so the header phrase quoted anywhere else in
// the transcript cannot produce a queued verdict.
func codexQueueHoldsPayload(above []string, payload string) bool {
	held := false
	for i := len(above) - 1; i >= 0; i-- {
		row := strings.TrimSpace(above[i])
		switch {
		case codexQueueHeaderRe.MatchString(row):
			return held
		case row == "":
		case strings.HasPrefix(row, codexQueuedItemGlyph):
			item := strings.TrimSpace(strings.TrimPrefix(row, codexQueuedItemGlyph))
			held = held || contentHoldsPayload(item, payload)
		case row != above[i]:
		default:
			return false
		}
	}
	return false
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
	if composerMarkerIdx(strings.Split(pane, "\n")) == -1 {
		return false
	}
	return !multiLineSubmitted(pane, line)
}

// promptMarkers are the leading glyphs an agent's input box renders. Shared by
// hasPromptMarker and composerContent so the list lives in one place.
var promptMarkers = []string{"❯", "›", "┃", ">"}

// composerMarkerIdx returns the index of the live input box's prompt-marker
// row, or -1 if none is present. Everything below it is footer, statusline or
// menu chrome, so submission checks scan from this row. Shared by
// lineStillAtPrompt and multiLineSubmitted.
//
// A Claude prompt row opening the box between the bottom two rules is the input
// box. Claude draws its slash-command menu below that box and marks the
// selected entry with the same "❯" (testdata/panes/claude_typed_slash_menu_real.txt),
// so the bottom-most marker row is the input box only when no such box is drawn.
func composerMarkerIdx(lines []string) int {
	if top, bottom, ok := inputBoxRules(lines); ok && top+1 < bottom && strings.HasPrefix(strings.TrimSpace(lines[top+1]), sendPlanReadyMarker) {
		return top + 1
	}
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

// collapsedPastePlaceholderRe matches a composer whose content LEADS with the
// placeholder an agent renders in place of a long paste, taken from real
// captures (testdata/panes/capture_paste.sh):
//
//	❯ [Pasted text #1]                            claude 2.1.296, a 2 KB line
//	❯ [Pasted text #2 +10 lines]                  claude, an 11-line payload
//	❯ [Pasted text #5][Pasted text #6]lorem …     claude, the same line TYPED
//	› [Pasted Content 2181 chars]                 codex-cli 0.161.0
//
// It is anchored at the start of the composer content and matched only on the
// live composer row, so transcript text that merely mentions a paste cannot
// trip it. It is deliberately the placeholder and not "any non-empty composer":
// a cleared codex composer renders a rotating hint, and reading that as held
// would misreport every codex send.
var collapsedPastePlaceholderRe = regexp.MustCompile(`^\[(?:Pasted text #\d+|Pasted Content\b)`)

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
//
// A collapsed paste is the one shape that cannot be matched against the payload
// at all, so it is recognised positively instead (BOS-1506): both TUIs replace
// a long payload in the composer with a placeholder — see
// collapsedPastePlaceholderRe — and that placeholder holds the payload exactly
// as much as its text would.
func composerHoldsPayload(trimmedMarkerRow, payload string) bool {
	content, isPromptRow := composerContent(trimmedMarkerRow)
	if !isPromptRow || content == "" {
		return false
	}
	if collapsedPastePlaceholderRe.MatchString(content) {
		return true
	}
	return contentHoldsPayload(content, payload)
}

// contentHoldsPayload reports whether a row's text, with its glyph already
// stripped, is the payload's first line or a wrapped prefix of it.
func contentHoldsPayload(content, payload string) bool {
	first := ""
	for _, l := range strings.Split(payload, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			first = t
			break
		}
	}
	if content == "" || first == "" {
		return false
	}
	return strings.HasPrefix(first, content) || strings.HasPrefix(content, first)
}

// agentActivityMarkers are the leading glyphs that mark a pane row as agent
// activity below the input box — an assistant/tool response, tool output, or a
// thinking/working spinner — rather than input-box footer or statusline chrome.
// The verifier runs against both Claude Code and Codex panes, so it covers
// both grammars: the Claude working/output markers statusdetect already trusts
// (lib/bossalib/contract/plugin/statusdetect/question.go optionStopMarkers: ⎿ ⏺ · ✻) plus the
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

// ErrComposerOccupied is the cause carried by an OutcomeComposerOccupied
// refusal. A caller that only sees the error, such as the callback worker
// behind the SendChatMessage RPC, matches it with errors.Is.
var ErrComposerOccupied = errors.New("the live composer already holds unsubmitted text")

// refuseOccupiedComposer refuses a submit into a Codex or Claude composer that
// already holds a draft, before a single key is typed. Prefill types without
// submitting, so the composer owner sees the result and it is not refused.
//
// Typing into a draft appends: the payload lands after the draft, and an Enter
// either submits both as one message or, when the draft is a pile of earlier
// sends Codex never took, leaves the payload stuck behind them where only the
// draft's oldest line shows on the marker row. That is how a verify chat
// collected ten unsent "$boss-verify" lines while every send read submitted.
// In Claude a typed line is entered only once the box holds exactly that line,
// so a draft makes every attempt fail and the give-up clear erases the draft.
//
// It refuses rather than clears or submits, whatever the draft says. The pane
// cannot say who wrote it: it may be earlier automation that never ran, or an
// operator's half-typed message, even one whose text equals an earlier send's,
// and C-u or Enter would act on the second as readily as the first. A send
// that ends not submitted withdraws its own payload before it returns
// (withdrawUnsubmitted), so a retry does not meet it here. A refusal costs one
// loud OutcomeComposerOccupied; a clear could silently erase a person's work.
// The operator clears the composer and the next send goes through.
//
// It reads a styled capture because the plain text cannot tell a draft from an
// empty composer's placeholder (Codex's "Ask Codex to do anything", Claude's
// `Try "…"`, both of which rotate): only SGR attributes separate them (see
// composerHoldsDraft). A capture that fails holds no evidence either way, so
// the send proceeds and the submit verifier remains the check. OpenCode is not
// checked: it draws its placeholder and status rail in plain colour, not dim.
func (c *Client) refuseOccupiedComposer(ctx context.Context, sessionName string, opts sendPlanOpts) error {
	if opts.prefillOnly || (opts.readyMarker != codexReadyMarker && !isClaudeComposer(opts.readyMarker)) {
		return nil
	}
	styled, err := c.captureVisibleStyled(ctx, sessionName)
	if err != nil || !composerHoldsDraft(styled) {
		return nil
	}
	return classifySubmit(OutcomeComposerOccupied, fmt.Errorf(
		"refused to type into tmux session %q: the live composer already holds text this send did not type, so the payload would be appended to it; nothing was typed and no Enter was sent; clear the composer and resend: %w",
		sessionName, ErrComposerOccupied))
}

// composerHoldsDraft reports whether a styled capture (capture-pane -e) shows
// the live composer holding text rather than an empty box or a placeholder.
func composerHoldsDraft(styled string) bool {
	return readComposer(styled) == composerTyped
}

// composerReading is what a capture shows in the live composer.
type composerReading int

const (
	// composerUndrawn: no live composer.
	composerUndrawn composerReading = iota
	// composerBlank: nothing visible but the prompt glyph.
	composerBlank
	// composerPlaceholder: only dim or inverse characters, in a capture that
	// carries style beyond the cursor.
	composerPlaceholder
	// composerTyped: a character that is neither dim nor inverse, in a capture
	// that carries style beyond the cursor.
	composerTyped
	// composerUnstyled: visible characters in a capture with no style to tell
	// a placeholder from typed text.
	composerUnstyled
)

// readComposer reads the live composer off a capture, styled (capture-pane
// -e) or plain. The composer is the live prompt-marker row (composerMarkerIdx)
// and the rows below it up to the first blank row or horizontal rule; any
// visible character in it that is neither dim nor inverse, other than the
// marker glyph itself, is typed text. Inverse is the cursor Claude paints,
// which an empty composer puts on its placeholder's first letter: Claude Code
// 2.1.302 renders a placeholder as inverse(p[0]) + dim(p[1:]). Codex draws its
// placeholder dim (testdata/panes/codex_placeholder_styled_real.txt). The IDE
// selection pill Claude may draw ahead of the text is not typed text either
// (afterIDESelectionPill).
//
// A composer styled with nothing but inverse cells holds no evidence, so it
// reads composerUnstyled. That is a plain capture, and also Claude under
// NO_COLOR: the colour level 0 it sets strips the dim from the placeholder,
// while the cursor stays inverse, so an empty box draws as an inverse first
// letter followed by plain text, which reads exactly like a draft.
func readComposer(styled string) composerReading {
	rows := strings.Split(styled, "\n")
	cellRows := styledRows(styled)
	plain := make([]string, len(cellRows))
	for i, cells := range cellRows {
		plain[i] = cellText(cells)
	}
	markerIdx := composerMarkerIdx(plain)
	if markerIdx == -1 || markerIdx >= len(rows) {
		return composerUndrawn
	}
	end := markerIdx + 1
	for end < len(plain) {
		if trimmed := strings.TrimSpace(plain[end]); trimmed == "" || isHorizontalRule(trimmed) {
			break
		}
		end++
	}
	visible, typed := false, false
	for i := markerIdx; i < end; i++ {
		cells := cellRows[i]
		if i == markerIdx {
			cells = afterIDESelectionPill(afterMarkerGlyph(cells))
		}
		for _, cell := range cells {
			if unicode.IsSpace(cell.r) {
				continue
			}
			visible = true
			if !cell.dim && !cell.inverse {
				typed = true
			}
		}
	}
	switch {
	case !visible:
		return composerBlank
	case !styledBeyondCursor(rows[markerIdx:min(end, len(rows))]):
		return composerUnstyled
	case typed:
		return composerTyped
	default:
		return composerPlaceholder
	}
}

// sgrSequenceRe matches one SGR escape sequence and captures its parameters.
var sgrSequenceRe = regexp.MustCompile("\x1b\\[([0-9;:]*)m")

// styledBeyondCursor reports whether styled rows set any SGR attribute other
// than inverse, the one attribute Claude draws whatever the colour level.
func styledBeyondCursor(rows []string) bool {
	for _, row := range rows {
		for _, m := range sgrSequenceRe.FindAllStringSubmatch(row, -1) {
			for _, param := range strings.Split(m[1], ";") {
				switch param {
				case "", "0", "7", "27":
				default:
					return true
				}
			}
		}
	}
	return false
}

// unreadableCell stands in for a cell typedText cannot read. No payload holds
// it, so a match against one fails.
const unreadableCell = '\uFFFD'

// typedText is a styled capture as plain text, reduced to what may be typed
// text. A dim cell is blanked: it is a placeholder, a suggestion or a
// completion Claude draws after what was typed. A non-blank inverse cell is
// Claude's cursor sitting on a character, which may be typed or the first
// letter of a suggestion, since Claude renders a suggestion as inverse(s[0]) +
// dim(s[1:]); it reads as unreadableCell.
func typedText(styled string) string {
	cells := styledCells(styled)
	out := make([]rune, len(cells))
	for i, cell := range cells {
		switch {
		case cell.r == '\n':
			out[i] = cell.r
		case cell.dim:
			out[i] = ' '
		case cell.inverse && !unicode.IsSpace(cell.r):
			out[i] = unreadableCell
		default:
			out[i] = cell.r
		}
	}
	return string(out)
}

// composerDrawing is how the agent draws a send's payload in its composer:
// the text itself, or, for a paste the agent collapses, one placeholder.
// placeholder is the one this send's own paste drew, read from the rows the
// composer settled on after the paste (placeholderIn). It stays empty when
// that read did not show one, and then nothing is withdrawn.
type composerDrawing struct {
	text        string
	collapsed   bool
	placeholder string
}

// fills reports whether composer text (see ruledComposerText) is exactly the
// drawing and nothing else. A collapsed drawing matches only the placeholder
// its own paste drew, paste id included, because an operator's paste draws the
// same kind of placeholder and may hold as many lines.
func (d composerDrawing) fills(box string) bool {
	if d.collapsed {
		return d.placeholder != "" && strings.TrimSpace(box) == d.placeholder
	}
	want := withoutSpace(d.text)
	return want != "" && withoutSpace(box) == want
}

// partOf reports whether composer text is what C-u presses can have left of
// the drawing. Claude Code 2.1.302's ctrl+u kills a newline just before the
// cursor on its own, or else the text back to the start of the cursor's
// wrapped row, so what presses leave is a prefix of the drawing's characters,
// compared here ignoring whitespace. Text an operator appends is not such a
// prefix unless it repeats the payload's own next words. A placeholder is one
// row, so nothing is left of it.
func (d composerDrawing) partOf(box string) bool {
	have := withoutSpace(box)
	return !d.collapsed && have != "" && strings.HasPrefix(withoutSpace(d.text), have)
}

// leftByRetry reports whether composer text is exactly the part of the
// drawing that the Enter retry's presses left behind, which sendErr carries
// (partialPayload). Only that reading is taken on the withdrawal's first read:
// a box holding some other prefix, after a retry that left the whole payload or
// none, may hold an operator's message that begins as the payload does.
func (d composerDrawing) leftByRetry(box string, sendErr error) bool {
	var left *partialPayload
	return errors.As(sendErr, &left) && d.partOf(box) && withoutSpace(box) == withoutSpace(left.held)
}

// renderedBy reports whether a composer footprint (composerFootprint) shows
// the drawing whole: all of its text, or the placeholder standing in for it.
// Only this send can have left that reading, so it is the only one the Enter
// retry presses C-u into. A Claude placeholder must be the one this send's own
// paste drew, paste id included, since an operator's paste draws the same
// kind. A Codex "[Pasted Content N chars]" must count the payload's
// characters, as it does for capture_paste.sh's 2181-character line.
func (d composerDrawing) renderedBy(footprint string) bool {
	if footprintRendersWholePayload(footprint, d.text) {
		return true
	}
	if d.placeholder != "" {
		return strings.TrimSpace(footprint) == d.placeholder
	}
	m := codexPastePlaceholderRe.FindStringSubmatch(strings.TrimSpace(footprint))
	return m != nil && m[1] == strconv.Itoa(utf8.RuneCountInString(d.text))
}

// codexPastePlaceholderRe matches a footprint that is exactly the placeholder
// Codex draws for a long paste, capturing its character count.
var codexPastePlaceholderRe = regexp.MustCompile(`^\[Pasted Content (\d+) chars\]$`)

// pressLeft reports whether next, read after a C-u, is what that one press
// leaves of prev, read before it. Claude Code 2.1.302's ctrl+u kills a newline
// just before the cursor, which leaves the same characters on fewer rows, or
// else the cursor's wrapped row, which leaves prev less its last row. A cursor
// at the start of an empty last row kills back over the row above, so empty
// last rows are dropped before the last row is. Anything else, such as a box
// an operator cleared and typed into after the press, does not pass.
func pressLeft(prev, next string) bool {
	have := withoutSpace(next)
	if have == withoutSpace(prev) {
		return composerRowCount(next) < composerRowCount(prev)
	}
	rows := strings.Split(prev, "\n")
	for len(rows) > 1 && strings.TrimSpace(rows[len(rows)-1]) == "" {
		rows = rows[:len(rows)-1]
	}
	return have == withoutSpace(strings.Join(rows[:len(rows)-1], "\n"))
}

// pressesToEmpty is how many C-u presses can empty box, the first read of a
// composer holding the drawing: one per row it draws, and one per newline,
// since a newline just before the cursor takes a press of its own.
func (d composerDrawing) pressesToEmpty(box string) int {
	if d.collapsed {
		return composerRowCount(box)
	}
	return composerRowCount(box) + strings.Count(d.text, "\n")
}

// composerShape is composer text with each row's trailing blanks cut, so two
// reads of an unchanged box compare equal while a press that kills only a
// newline still shows as a change.
func composerShape(text string) string {
	rows := strings.Split(text, "\n")
	for i, r := range rows {
		rows[i] = strings.TrimRightFunc(r, unicode.IsSpace)
	}
	return strings.Join(rows, "\n")
}

// composerRowCount is how many rows composer text spans.
func composerRowCount(text string) int {
	return strings.Count(text, "\n") + 1
}

// claudePastePlaceholderRe matches composer text that is exactly one
// placeholder Claude Code 2.1.302 draws for a collapsed paste, capturing its
// line count.
var claudePastePlaceholderRe = regexp.MustCompile(`^\[Pasted text #\d+(?: \+(\d+) lines)?\]$`)

// claudePlaceholderOf reports whether composer text is the placeholder Claude
// draws for a paste of text: "[Pasted text #N]" for one line, else
// "[Pasted text #N +M lines]" where M counts its newlines.
func claudePlaceholderOf(box, text string) bool {
	m := claudePastePlaceholderRe.FindStringSubmatch(strings.TrimSpace(box))
	if m == nil {
		return false
	}
	newlines := strings.Count(claudePasteText(text), "\n")
	if newlines == 0 {
		return m[1] == ""
	}
	return m[1] == strconv.Itoa(newlines)
}

// claudePasteText is a paste as Claude Code 2.1.302 stores it: CR and CRLF
// become LF and a tab becomes four spaces.
func claudePasteText(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return strings.ReplaceAll(text, "\t", "    ")
}

// claudeCollapsesPaste reports whether Claude Code 2.1.302 draws a paste of
// text as one placeholder. It collapses a paste longer than 800 UTF-16 units
// or holding more than min(rows-10, 2) newlines, rows being the terminal's
// height. The height only lowers that bound, below 12 rows, so taking 2 never
// reports a paste collapsed that Claude draws as text. Codex collapses a long
// paste into "[Pasted Content N chars]", but a Codex composer is never
// withdrawn from (withdrawUnsubmitted), so its rule is not modelled here.
func claudeCollapsesPaste(text string) bool {
	text = claudePasteText(text)
	return len(utf16.Encode([]rune(text))) > 800 || strings.Count(text, "\n") > 2
}

// ruledComposerText returns what may be typed text in Claude's input box: the
// rows between the bottom two rules, read through typedText, with the prompt
// glyph cut from the first. ruled is false unless those rules bound the live
// composer, whose first row carries the prompt glyph, or a shell-mode payload's
// "!" row. A suggestion Claude draws after the text is dim, so it reads blank,
// and a cursor on a character reads as unreadableCell.
func ruledComposerText(styled, payload string) (text string, ruled bool) {
	lines := strings.Split(typedText(styled), "\n")
	top, bottom, ok := inputBoxRules(lines)
	if !ok {
		return "", false
	}
	if bottom-top < 2 {
		return "", false
	}
	box := lines[top+1 : bottom]
	first := strings.TrimSpace(box[0])
	content, isPrompt := composerContent(first)
	switch {
	case isPrompt && composerMarkerIdx(lines) == top+1:
	case !isPrompt && strings.HasPrefix(payload, shellModePrefix):
		content = first
	default:
		return "", false
	}
	return strings.Join(append([]string{content}, box[1:]...), "\n"), true
}

// composerTextIsEmpty reports whether composer text holds nothing typed. An
// empty Claude box draws its placeholder as the cursor on a first letter,
// followed by dim text, which reads as one unreadableCell.
func composerTextIsEmpty(text string) bool {
	t := withoutSpace(text)
	return t == "" || t == string(unreadableCell)
}

// composerHoldsOnlyPayload reports whether a styled capture shows Claude's
// ruled input box holding exactly the drawing and nothing else.
func composerHoldsOnlyPayload(styled string, drawn composerDrawing) bool {
	box, ruled := ruledComposerText(styled, drawn.text)
	return ruled && drawn.fills(box)
}

// afterMarkerGlyph returns a marker row's cells after its prompt glyph.
func afterMarkerGlyph(cells []styledCell) []styledCell {
	for i, cell := range cells {
		if !unicode.IsSpace(cell.r) {
			return cells[i+1:]
		}
	}
	return nil
}

// ideSelectionPillOpen is how the IDE selection pill starts: Claude Code
// 2.1.302 labels it "[⧉ 3 lines from main.go] " or "[⧉ In main.go] ".
const ideSelectionPillOpen = "[⧉"

// afterIDESelectionPill returns the composer cells after the IDE selection
// pill, or all of them when the row does not lead with one. Claude draws the
// pill, while an IDE is connected with a selection, ahead of the placeholder
// or the text, in its "ide" colour rather than dim, so without this an empty
// composer reads as a draft. Every cell of the pill must carry a foreground
// colour: text an operator types is drawn in the default one.
func afterIDESelectionPill(cells []styledCell) []styledCell {
	start := 0
	for start < len(cells) && unicode.IsSpace(cells[start].r) {
		start++
	}
	open := []rune(ideSelectionPillOpen)
	if len(cells)-start < len(open) {
		return cells
	}
	for i, r := range open {
		if cells[start+i].r != r {
			return cells
		}
	}
	for i := start; i < len(cells); i++ {
		if !cells[i].coloured {
			return cells
		}
		if cells[i].r == ']' {
			return cells[i+1:]
		}
	}
	return cells
}

// styledCell is one visible character of a styled row and the SGR attributes
// in force when it was drawn.
type styledCell struct {
	r rune
	sgrAttrs
}

// sgrAttrs are the SGR attributes the draft reading uses. coloured is any
// foreground colour other than the terminal default.
type sgrAttrs struct {
	dim      bool
	inverse  bool
	coloured bool
}

// styledRows splits a styled capture into rows of cells. The attributes carry
// across rows: capture-pane -e emits a sequence only where the style changes,
// so a dim placeholder wrapped onto a second row starts that row with none.
func styledRows(styled string) [][]styledCell {
	rows := [][]styledCell{nil}
	for _, cell := range styledCells(styled) {
		if cell.r == '\n' {
			rows = append(rows, nil)
			continue
		}
		rows[len(rows)-1] = append(rows[len(rows)-1], cell)
	}
	return rows
}

// styledCells walks text captured with escape sequences, tracking sgrAttrs
// through SGR sequences and dropping every escape sequence (CSI and OSC) from
// the output.
func styledCells(row string) []styledCell {
	var cells []styledCell
	var attrs sgrAttrs
	runes := []rune(row)
	for i := 0; i < len(runes); i++ {
		if runes[i] != '\x1b' {
			cells = append(cells, styledCell{r: runes[i], sgrAttrs: attrs})
			continue
		}
		if i+1 >= len(runes) {
			break
		}
		switch runes[i+1] {
		case '[':
			end := i + 2
			for end < len(runes) && (runes[end] < 0x40 || runes[end] > 0x7e) {
				end++
			}
			if end < len(runes) && runes[end] == 'm' {
				attrs = applySGR(string(runes[i+2:end]), attrs)
			}
			i = end
		case ']':
			end := i + 2
			for end < len(runes) && runes[end] != '\a' && (runes[end] != '\x1b' || end+1 >= len(runes) || runes[end+1] != '\\') {
				end++
			}
			if end < len(runes) && runes[end] == '\x1b' {
				end++
			}
			i = end
		default:
			i++
		}
	}
	return cells
}

// applySGR returns the attributes after one SGR parameter list: 2 sets dim and
// 22 clears it, 7 sets inverse and 27 clears it, 30-37, 90-97 and 38 set a
// foreground colour and 39 clears it, 0 or an empty list clears all of them,
// and the arguments of an extended colour (38/48/58 followed by 5;n or
// 2;r;g;b) are skipped so a "2" or "7" among them is not read as an attribute.
func applySGR(params string, attrs sgrAttrs) sgrAttrs {
	fields := strings.Split(params, ";")
	for i := 0; i < len(fields); i++ {
		if n, err := strconv.Atoi(fields[i]); err == nil && (30 <= n && n <= 37 || 90 <= n && n <= 97) {
			attrs.coloured = true
			continue
		}
		switch fields[i] {
		case "", "0":
			attrs = sgrAttrs{}
		case "2":
			attrs.dim = true
		case "22":
			attrs.dim = false
		case "7":
			attrs.inverse = true
		case "27":
			attrs.inverse = false
		case "39":
			attrs.coloured = false
		case "38", "48", "58":
			if fields[i] == "38" {
				attrs.coloured = true
			}
			if i+1 < len(fields) {
				switch fields[i+1] {
				case "5":
					i += 2
				case "2":
					i += 4
				}
			}
		}
	}
	return attrs
}

// plainText is styled text with its escape sequences removed.
func plainText(styled string) string {
	return cellText(styledCells(styled))
}

// cellText is the characters of cells.
func cellText(cells []styledCell) string {
	out := make([]rune, len(cells))
	for i, cell := range cells {
		out[i] = cell.r
	}
	return string(out)
}
