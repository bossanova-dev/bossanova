package tmux

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"
)

// TestSendLine_RetryTreatsAnUnreadableSnapshotAsNoEvidence pins the cost of
// getting the UNREADABLE capture wrong. composerFootprint returns "" for a pane
// that drew no input box, and a repainting composer really does produce such a
// frame — the box is erased and redrawn, so a capture can land in between. That
// frame says nothing about what the composer holds, in EITHER direction: reading
// it as a cut latches mutilation, a one-way door that forbids every later Enter
// and fails a healthy delivery loudly as NOT_SUBMITTED with the payload sitting
// intact in the box.
//
// So the baseline is polled rather than sampled: the retry looks again until the
// box is readable, and only what it reads there licenses anything. The composer
// here survives C-u untouched, exactly as in the no-op case above, and only the
// first look is blank — the retry must still recover: one Enter onto the intact
// payload, no second copy typed.
func TestSendLine_RetryTreatsAnUnreadableSnapshotAsNoEvidence(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {

		// Capture 3 is the baseline poll's FIRST look: the ready-marker wait takes
		// capture 1 and the first submission verify takes capture 2, each of those
		// windows taking exactly one capture in this synctest bubble: slow forks
		// consume no fake time before the deadline beats the pollers' default tick.
		// Any other capture landing on it changes the outcome,
		// so a mis-count fails this test rather than passing it vacuously.
		pane := &composerPane{swallowFirstEnter: true, clearIsNoop: true, markerlessCaptureNo: 3}
		c := NewClient(WithCommandFactory(pane.factory))
		if err := c.sendLine(context.Background(), "boss-test-sess", "$boss-repair", sendPlanOpts{
			readyMarker:      "ready",
			submitVerifyWait: 30 * time.Millisecond,
		}); err != nil {
			t.Fatalf("sendLine retry: unexpected error when only the pre-clear snapshot was unreadable: %v", err)
		}

		want := []string{"literal", "enter", "clear", "enter"}
		if got := pane.submitSteps(); !slices.Equal(got, want) {
			t.Fatalf("submit sequence = %v, want %v (a blank snapshot is not proof the clear cut into the payload)", got, want)
		}
	})
}

// TestSendLine_RetryUnreadableSnapshotIsNotALicenceToEnterATruncatedPayload is
// the twin of the test above, and the reason the two must be pinned together:
// the SAME unreadable frame that must not be read as a cut must equally not be
// read as proof of an intact payload. Whichever way an absent footprint is
// resolved by default, one of these two deliveries is wrong — so neither may be
// decided from it, and the composer must be read again until it is legible.
//
// Here the pane is unreadable exactly when the clear truncates the payload, and
// then holds the fragment. Reading "no evidence of a cut" as "safe to Enter"
// submits the fragment, which leaves the composer, which the verifier reads as
// submitted — delivered=true for corrupted user text, silently, with every test
// green. Nothing is ever Entered onto a payload that could not be shown intact.
func TestSendLine_RetryUnreadableSnapshotIsNotALicenceToEnterATruncatedPayload(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {

		// Capture 3 is the retry's first look at the composer (see the capture count
		// above), and the press that follows it takes all but the first 9 bytes of the
		// payload; clearIsNoop then holds the fragment there, so the run can only end
		// loudly or by submitting mutilated text.
		pane := &composerPane{
			swallowFirstEnter:   true,
			markerlessCaptureNo: 3,
			clearKeepsBytes:     len("$boss-rep"),
			clearIsNoop:         true,
		}
		c := NewClient(WithCommandFactory(pane.factory))
		err := c.sendLine(context.Background(), "boss-test-sess", "$boss-repair", sendPlanOpts{
			readyMarker:      "ready",
			submitVerifyWait: 30 * time.Millisecond,
		})
		if err == nil {
			t.Fatal("sendLine retry: want a loud error when the clear truncated the payload behind an unreadable frame, got nil (mutilated text was submitted and reported as success)")
		}
		if got := OutcomeOf(err); got != OutcomeNotSubmitted {
			t.Fatalf("OutcomeOf(err) = %v, want %v (the payload provably never left the composer)", got, OutcomeNotSubmitted)
		}
		if !errors.Is(err, errSubmissionPending) {
			t.Fatalf("err = %v, want it to carry errSubmissionPending", err)
		}
		if steps := pane.stepsFromFirstClear(); slices.Contains(steps, "enter") {
			t.Fatalf("steps from the clear = %v, want no enter (the composer held a truncated payload)", steps)
		}
	})
}

// TestSendLine_RetryPressesNoKeyWhileTheComposerStaysUnreadable pins the floor
// under the two tests above. They both rest on there being a readable reading of
// the composer to judge later ones against; this is the case where there is
// none — the box never renders inside the retry's budget.
//
// Every key available here is unsafe without one. Enter would submit whatever
// the invisible box holds, which may already be a fragment, and C-u would keep
// cutting into a payload no one can see. The only correct move is to press
// nothing and say so loudly: the payload is confirmed still pending (the caller
// proved that before the retry began, and no key has been sent since), so the
// operator gets an accurate NOT_SUBMITTED they can retry rather than a silent
// corruption or a "may already have been submitted" they cannot act on.
func TestSendLine_RetryPressesNoKeyWhileTheComposerStaysUnreadable(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {

		// Captures 1 and 2 are the ready-marker wait and the first submission verify,
		// which must see the payload sitting in the composer for the retry to run at
		// all; the box is gone from capture 3 on, so every look the retry takes is
		// unreadable.
		pane := &composerPane{swallowFirstEnter: true, markerlessFromCaptureNo: 3}
		c := NewClient(WithCommandFactory(pane.factory))
		err := c.sendLine(context.Background(), "boss-test-sess", "$boss-repair", sendPlanOpts{
			readyMarker:      "ready",
			submitVerifyWait: 30 * time.Millisecond,
		})
		if err == nil {
			t.Fatal("sendLine retry: want a loud error when the composer never became readable, got nil")
		}
		if got := OutcomeOf(err); got != OutcomeNotSubmitted {
			t.Fatalf("OutcomeOf(err) = %v, want %v (nothing was pressed, so nothing was submitted)", got, OutcomeNotSubmitted)
		}
		if !errors.Is(err, errSubmissionPending) {
			t.Fatalf("err = %v, want it to carry errSubmissionPending", err)
		}
		// The literal and the swallowed Enter are the original delivery; the retry
		// itself adds nothing — not even the C-u it would otherwise open with.
		want := []string{"literal", "enter"}
		if got := pane.submitSteps(); !slices.Equal(got, want) {
			t.Fatalf("submit sequence = %v, want %v (no key may be pressed into a composer that cannot be read)", got, want)
		}
	})
}

// An operator may change the box at any of the Enter retry's reads. Whatever
// they leave, the retry presses nothing into it, types nothing after it and
// submits nothing, and the box keeps it. Each case is one way the operator
// sweeps (TestOperatorSweep_*) found a send erasing or submitting onto an
// operator's text, pinned at the one read where it happened.
func TestSendLine_RetryLeavesAnOperatorsEditAlone(t *testing.T) {
	t.Parallel()

	const foreign = "please hold off, I am reviewing"
	const wide = "$boss-repair fix the failing check on pull request 1754 and push the result"
	const paneWidth = 24
	firstWindow := func(p *composerPane) bool { return p.enters == 1 && p.clears == 0 && p.capturesSinceKey == 0 }
	baseline := func(p *composerPane) bool { return p.enters == 1 && p.clears == 0 && p.capturesSinceKey == 1 }
	pressWindow := func(p *composerPane) bool { return p.clears == 1 && p.capturesSinceKey == 0 }
	for _, tc := range []struct {
		name             string
		payload          string
		paneWidth, keeps int
		at               func(*composerPane) bool
		edit             func(held string) string
		want             []string
	}{
		{"a prefix typed in the first verify window", "$boss-repair", 0, 0, firstWindow,
			func(string) string { return "$boss" }, []string{"literal", "enter"}},
		{"other text typed before the baseline read", "$boss-repair", 0, 0, baseline,
			func(string) string { return foreign }, []string{"literal", "enter"}},
		{"other text typed in a press window", "$boss-repair", 0, 0, pressWindow,
			func(string) string { return foreign }, []string{"literal", "enter", "clear"}},
		{"two more rows cut in a press window", wide, paneWidth, 3 * paneWidth, pressWindow,
			func(held string) string { return held[:paneWidth] }, []string{"literal", "enter", "clear"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				var left string
				pane := &composerPane{swallowFirstEnter: true, paneWidth: tc.paneWidth, clearKeepsBytes: tc.keeps}
				pane.operator = func(p *composerPane) {
					if left == "" && tc.at(p) {
						left = tc.edit(p.held)
						p.held = left
					}
				}
				c := NewClient(WithCommandFactory(pane.factory))
				err := c.sendLine(context.Background(), "boss-test-sess", tc.payload, sendPlanOpts{
					readyMarker:      "ready",
					submitVerifyWait: 30 * time.Millisecond,
				})
				if left == "" {
					t.Fatalf("the send ended %v before the operator's read; steps %v", err, pane.submitSteps())
				}
				if OutcomeOf(err) != OutcomeNotSubmitted {
					t.Fatalf("err = %v (outcome %v), want not submitted", err, OutcomeOf(err))
				}
				if got := pane.submitSteps(); !slices.Equal(got, tc.want) {
					t.Fatalf("submit sequence = %v, want %v (no key into, or after, the operator's text)", got, tc.want)
				}
				if pane.held != left || pane.didSubmit() {
					t.Fatalf("the composer holds %q (submitted %v), want the operator's %q kept", pane.held, pane.didSubmit(), left)
				}
			})
		})
	}
}

// TestSendLine_RetryReReadsThePaneBeforeABareEnter pins the loss mode hiding
// inside the unchanged-footprint shortcut. "The two captures agree" is what a
// no-op C-u looks like — and equally what a composer that DID empty looks like
// before it repaints, since both captures are taken inside one clear slice. On
// the second reading a bare Enter lands in an empty box, and waitForSubmission
// then reads that cleared composer as "submitted": delivered=true for a payload
// that was silently discarded.
//
// So an unchanged footprint may not be acted on blind. A second, later reading
// separates the two: if the box is in fact empty the retry must re-deliver
// before its Enter, exactly as it does when the clear is observed to take.
//
// The pane owes exactly ONE repaint (staleCaptures: 1), and the sequence rather
// than host speed is what makes this deterministic inside the synctest bubble:
// slow forks consume no fake time, so each window's deadline beats the
// pollers' 100ms default tick and every window takes exactly one capture. Capture 1 after the clear is the stale one the clear-verify window
// sees; capture 2 is the re-read, and the first sight of the truth.
func TestSendLine_RetryReReadsThePaneBeforeABareEnter(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {

		pane := &composerPane{swallowFirstEnter: true, staleCaptures: 1}
		c := NewClient(WithCommandFactory(pane.factory))
		if err := c.sendLine(context.Background(), "boss-test-sess", "$boss-repair", sendPlanOpts{
			readyMarker:      "ready",
			submitVerifyWait: 30 * time.Millisecond,
		}); err != nil {
			t.Fatalf("sendLine retry: unexpected error for a slow-repainting composer: %v", err)
		}

		// The re-delivery between the clear and the final Enter is half the
		// assertion: a bare Enter there would submit an empty composer and the
		// verifier would report that as success. The other half is the SECOND capture
		// before it — the re-read this test exists for. A retry that decided on the
		// clear-verify window's reading alone reaches the same keystrokes as the
		// ordinary observed-clear path (TestSendLine_RetryClearsComposerBeforeSecondEnter),
		// so the keystrokes alone cannot tell the two apart; the extra look can.
		want := []string{"clear", "capture", "capture", "literal", "enter", "capture"}
		if got := pane.stepsFromFirstClear(); !slices.Equal(got, want) {
			t.Fatalf("steps from the clear = %v, want %v (look again before acting on an unchanged composer, then re-deliver into it)", got, want)
		}
	})
}

// TestSendLine_RetryLooksAgainBeforeEnteringAMutilatedRemainder pins the loss
// mode the re-read itself can introduce. The re-read exists because an unchanged
// footprint may be a stale screen rather than a no-op C-u — but "the pane
// repainted" and "the payload is intact" are different claims. C-u is
// line-scoped, so the repaint can reveal a TRUNCATED payload, and the content
// predicates are prefix-based (BOS-489) so the fragment still reads as "the
// composer holds the payload". Acting on that reading alone sends a bare Enter
// onto mutilated user text; the fragment then leaves the composer, which
// waitForSubmission reads as submitted, and the RPC reports delivered=true for
// corrupted text.
//
// The second reading must therefore be compared against the baseline exactly as
// the first one is: a footprint that is anything OTHER than what the box held
// before a key was pressed is a cut, so mutilation latches and the loop presses
// again instead of Entering the remainder. Here the composer truncates on press
// 1 and then survives every later press, so the run ends where a stuck partial
// clear must — loudly, with no Enter.
func TestSendLine_RetryLooksAgainBeforeEnteringAMutilatedRemainder(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {

		pane := &composerPane{
			swallowFirstEnter: true,
			clearKeepsBytes:   len("$boss-rep"),
			paneWidth:         len("$boss-rep"),
			clearIsNoop:       true,
			staleCaptures:     1,
		}
		c := NewClient(WithCommandFactory(pane.factory))
		err := c.sendLine(context.Background(), "boss-test-sess", "$boss-repair", sendPlanOpts{
			readyMarker:      "ready",
			submitVerifyWait: 30 * time.Millisecond,
		})
		if err == nil {
			t.Fatal("sendLine retry: want a loud error when the repaint reveals a truncated payload, got nil (mutilated text was submitted and reported as success)")
		}
		if got := OutcomeOf(err); got != OutcomeNotSubmitted {
			t.Fatalf("OutcomeOf(err) = %v, want %v (the payload provably never left the composer)", got, OutcomeNotSubmitted)
		}
		if !errors.Is(err, errSubmissionPending) {
			t.Fatalf("err = %v, want it to carry errSubmissionPending", err)
		}
		// No Enter after any clear: the truncated payload is never submitted. Each
		// press costs its post-clear window plus the re-read; the reading they are
		// judged against is the ONE baseline taken before the first clear, so no press
		// pays for a pre-clear capture of its own.
		want := []string{"clear", "capture", "capture", "clear", "capture", "capture"}
		if got := pane.stepsFromFirstClear(); !composerClearReadsMatch(got, want) {
			t.Fatalf("steps from the clear = %v, want %v with optional extra captures (no Enter onto a mutilated payload)", got, want)
		}
	})
}

// TestSendLine_RetryComposerVanishingOnTheReReadIsUnconfirmed is the vanishing
// twin of the re-read. The box can disappear on the repaint the re-read is there
// to wait for — the agent accepted the payload after all and went full-screen —
// and the second reading must classify that exactly as the first one does. A
// pane with no input box is unknowable, not cleared: re-delivering into it
// submits the message twice, and Entering into it is the same double-submit.
func TestSendLine_RetryComposerVanishingOnTheReReadIsUnconfirmed(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {

		pane := &composerPane{swallowFirstEnter: true, markerGoneAfterClear: true, staleCaptures: 1}
		c := NewClient(WithCommandFactory(pane.factory))
		err := c.sendLine(context.Background(), "boss-test-sess", "$boss-repair", sendPlanOpts{
			readyMarker:      "ready",
			submitVerifyWait: 30 * time.Millisecond,
		})
		if err == nil {
			t.Fatal("sendLine retry: want an error when the composer vanishes on the re-read, got nil")
		}
		if got := OutcomeOf(err); got != OutcomeUnconfirmed {
			t.Fatalf("OutcomeOf(err) = %v, want %v (the payload may have been accepted)", got, OutcomeUnconfirmed)
		}
		if errors.Is(err, errSubmissionPending) {
			t.Fatalf("err = %v, must NOT claim the payload is confirmed pending", err)
		}
		// The stale capture hides the vanishing from the clear-verify window, so the
		// re-read is the capture that sees it: two looks, then a stop.
		want := []string{"clear", "capture", "capture"}
		if got := pane.stepsFromFirstClear(); !composerClearReadsMatch(got, want) {
			t.Fatalf("steps from the clear = %v, want %v with optional extra captures (no keystroke into a pane with no input box)", got, want)
		}
	})
}

func TestSendLine_CaptureCountsHoldWhenEveryCaptureIsSlowerThanTheTick(t *testing.T) {
	for _, tc := range []struct {
		name string
		pane *composerPane
		want []string
	}{
		{"unreadable snapshot", &composerPane{swallowFirstEnter: true, clearIsNoop: true, markerlessCaptureNo: 3}, []string{"literal", "enter", "clear", "enter"}},
		{"stale frame", &composerPane{swallowFirstEnter: true, staleCaptures: 1}, []string{"literal", "enter", "clear", "literal", "enter"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tc.pane.captureDelay = 150 * time.Millisecond
				c := NewClient(WithCommandFactory(tc.pane.factory))
				if err := c.sendLine(context.Background(), "boss-test-sess", "$boss-repair", sendPlanOpts{readyMarker: "ready", submitVerifyWait: 30 * time.Millisecond}); err != nil {
					t.Fatalf("sendLine retry with slow captures: %v", err)
				}
				if got := tc.pane.submitSteps(); !slices.Equal(got, tc.want) {
					t.Fatalf("submit sequence = %v, want %v", got, tc.want)
				}
				if tc.name == "stale frame" {
					want := []string{"clear", "capture", "capture", "literal", "enter", "capture"}
					if got := tc.pane.stepsFromFirstClear(); !slices.Equal(got, want) {
						t.Fatalf("steps from clear = %v, want %v", got, want)
					}
				}
			})
		})
	}
}
