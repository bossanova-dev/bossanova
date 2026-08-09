package tmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/statusdetect"
)

// The fixtures this file asserts against are REAL tmux captures of Claude Code
// 2.1.220, produced by testdata/panes/capture.sh — not authored from a plan's
// prose. That distinction is the point of BOS-599's second pass: the previous
// revision reasoned about queued panes from prose, and every load-bearing claim
// it made turned out to be false the moment a real pane was captured.
//
// What the captures overturned, in the order it matters:
//
//  1. "A queued message and an unsent one render the same composer." FALSE.
//     Pressing Enter mid-turn CLEARS the composer: the payload is echoed into a
//     queued-message list ABOVE the input box, and the box itself is left
//     showing the placeholder hint "Press up to edit queued messages". A pane
//     holding the payload in the live composer is therefore never a queued
//     message.
//  2. "A real queued send reaches the verify timeout." FALSE. Because the
//     composer clears, the existing submit predicates read it as SUBMITTED on
//     the very first poll. It never reaches the deadline branch at all.
//  3. Consequently the deadline branch's old "payload still on screen + agent
//     is working ⇒ QUEUED" verdict had an EMPTY true-positive population and a
//     live false-positive one: the only pane that reaches it with the payload
//     still held is the swallowed Enter the retry exists to recover, which it
//     was relabelling "delivered, do not resend" — a silent drop.
//  4. "statusdetect.HasWorkingIndicator identifies a mid-turn claude pane."
//     FALSE for this CLI version. It answers false on every real mid-turn
//     capture here, because 2.1.220 renders "✳ Blanching… (1m 37s · ↓ 244
//     tokens)" with no "esc to interrupt" clause for the probe's primary regex
//     to match. TestRealMidTurnPanesCarryNoWorkingIndicator pins that finding so
//     a future change cannot quietly rebuild a delivery decision on it.
//
// The discriminator that replaced it — paneShowsPayloadQueued — reads the queue
// shape in (1) directly, and only ever refines an already-positive submitted
// verdict. It cannot manufacture "delivered" out of a pending pane, which is
// precisely what the old verdict could.
const (
	// realQueuedPane: mid-turn, Enter ACCEPTED. Composer cleared to the queue
	// hint, payload echoed above it.
	realQueuedPane = "claude_queued_midturn_real.txt"
	// realSwallowedEnterPane: mid-turn, Enter SWALLOWED. Composer still holds
	// the payload; no queue hint anywhere on the pane.
	realSwallowedEnterPane = "claude_swallowed_enter_midturn_real.txt"
	// realWorkingEmptyComposerPane: the same running turn as
	// realSwallowedEnterPane ten seconds later with the composer cleared (C-u),
	// captured to show the spinner wording is not an artefact of composer text.
	realWorkingEmptyComposerPane = "claude_working_empty_composer_real.txt"
	// queuedFixturePayload is the payload sent in every capture.
	queuedFixturePayload = "also update the changelog"
)

// composerHintOf returns the text the pane's live input box carries after its
// prompt glyph, failing the test if there is no input box at all. Tests read
// the composer through this rather than matching raw substrings so they stay
// immune to the exact whitespace the agent renders between glyph and text.
func composerHintOf(t *testing.T, pane string) string {
	t.Helper()
	lines := strings.Split(pane, "\n")
	idx := bottomMostPromptMarkerIdx(lines)
	if idx == -1 {
		t.Fatalf("invalid fixture: no live input box on the pane:\n%s", pane)
	}
	content, _ := composerContent(strings.TrimSpace(lines[idx]))
	return content
}

// verifyPane runs the submit verifier against a fixed pane and returns the
// outcome and error. The pane never changes across polls, so a pane the
// predicates read as pending reaches the deadline branch.
func verifyPane(t *testing.T, pane string) (SubmitOutcome, error) {
	t.Helper()
	f := &sendPlanRecordingFactory{capturePaneOutputs: []string{pane}}
	c := NewClient(WithCommandFactory(f.factory))
	return c.waitForSubmission(context.Background(), "boss-test-queued", queuedFixturePayload,
		40*time.Millisecond, 5*time.Millisecond)
}

// TestRealQueuedPaneIsDeliveredAsQueued is the assertion the discriminator
// exists for, and it fails without it: strip paneShowsPayloadQueued out of
// waitForSubmission and this pane resolves to OutcomeSubmitted, because the
// payload has genuinely left the composer.
func TestRealQueuedPaneIsDeliveredAsQueued(t *testing.T) {
	pane := readPaneFixture(t, realQueuedPane)

	// Fixture sanity, so the assertions below cannot pass vacuously against a
	// pane that no longer shows what it was captured to show. It reads the row
	// through composerContent rather than matching "❯ Press up…" literally,
	// because the real composer separates its glyph from the text with a
	// NON-BREAKING space (U+00A0) — one of the small truths a hand-authored
	// fixture gets wrong and a capture cannot.
	if hint := composerHintOf(t, pane); !strings.Contains(hint, "queued messages") {
		t.Fatalf("invalid fixture: the live composer no longer shows the queued-messages hint, it shows %q:\n%s", hint, pane)
	}
	if lineStillAtPrompt(pane, queuedFixturePayload) {
		t.Fatal("invalid fixture: a real queued pane has released the payload from the composer")
	}

	outcome, err := verifyPane(t, pane)

	if outcome != OutcomeQueued {
		t.Fatalf("outcome = %v, want %v: the payload left the composer into the agent's "+
			"queued-message list, which is delivery behind a running turn", outcome, OutcomeQueued)
	}
	if !errors.Is(err, errSubmissionQueued) {
		t.Fatalf("error must carry errSubmissionQueued, got %v", err)
	}
	// Load-bearing, not cosmetic: verifyWithEnterRetry gates its whole recovery
	// dance on errSubmissionPending. A queued payload must not match it, or the
	// retry would re-deliver a message the agent already holds.
	if errors.Is(err, errSubmissionPending) {
		t.Fatal("a queued payload must NOT carry errSubmissionPending: that sentinel " +
			"licenses the clear-and-re-deliver retry, which would queue a duplicate")
	}
}

// TestSubmitVerifierErrorsDoNotEchoPayload ensures verifier diagnostics remain
// safe to persist as transport errors while preserving their outcome classes.
func TestSubmitVerifierErrorsDoNotEchoPayload(t *testing.T) {
	cases := []struct {
		name        string
		payload     string
		pane        string
		wantOutcome SubmitOutcome
		wantCause   error
	}{
		{
			name:        "still pending",
			payload:     "TMUX-SECRET-PAYLOAD",
			pane:        "› TMUX-SECRET-PAYLOAD\n",
			wantOutcome: OutcomeNotSubmitted,
			wantCause:   errSubmissionPending,
		},
		{
			name:        "queued",
			payload:     queuedFixturePayload,
			pane:        readPaneFixture(t, realQueuedPane),
			wantOutcome: OutcomeQueued,
			wantCause:   errSubmissionQueued,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &sendPlanRecordingFactory{capturePaneOutputs: []string{tc.pane}}
			c := NewClient(WithCommandFactory(f.factory))
			outcome, err := c.waitForSubmission(context.Background(), "boss-test-redaction", tc.payload, 15*time.Millisecond, time.Millisecond)
			if outcome != tc.wantOutcome {
				t.Fatalf("outcome = %v, want %v", outcome, tc.wantOutcome)
			}
			if !errors.Is(err, tc.wantCause) {
				t.Fatalf("error = %v, want cause %v", err, tc.wantCause)
			}
			if strings.Contains(err.Error(), tc.payload) {
				t.Fatalf("submit-verifier error leaked payload: %q", err)
			}
		})
	}
}

// TestRealSwallowedEnterPaneIsNotSubmitted is the other half, and together with
// the test above it is the whole finding: the two panes were captured from the
// same agent, seconds apart, and they do NOT render alike. The swallowed-Enter
// pane must stay retryable, because its payload is genuinely still unsent.
func TestRealSwallowedEnterPaneIsNotSubmitted(t *testing.T) {
	pane := readPaneFixture(t, realSwallowedEnterPane)

	if hint := composerHintOf(t, pane); hint != queuedFixturePayload {
		t.Fatalf("invalid fixture: the composer holds %q, not the unsent payload:\n%s", hint, pane)
	}
	if strings.Contains(pane, "queued messages") {
		t.Fatal("invalid fixture: a swallowed-Enter pane must carry no queue hint — " +
			"that hint is the discriminator, and its presence here would void the comparison")
	}

	outcome, err := verifyPane(t, pane)

	if outcome != OutcomeNotSubmitted {
		t.Fatalf("outcome = %v, want %v: the payload is still in the live composer, "+
			"so it was never delivered and the Enter retry must run", outcome, OutcomeNotSubmitted)
	}
	if !errors.Is(err, errSubmissionPending) {
		t.Fatalf("error must carry errSubmissionPending so the Enter retry still runs, got %v", err)
	}
	if errors.Is(err, errSubmissionQueued) {
		t.Fatal("a swallowed Enter must never be reported as queued")
	}
}

// TestPayloadHeldInComposerIsNeverQueued is the regression test for the Critical
// this pass closed. The old code turned "payload still in the composer + the
// agent is working" into QUEUED — delivered, do not resend — which silently
// dropped exactly the message the retry existed to recover.
//
// The adversarial pane is DERIVED from the real swallowed-Enter capture rather
// than authored: it is those bytes with the working marker the probe used to
// trust spliced in above the composer. So it asserts against the strongest
// input the old code could have been handed, without inventing a rendering
// claim — the splice is explicitly a synthetic worst case, not a statement that
// claude renders this.
func TestPayloadHeldInComposerIsNeverQueued(t *testing.T) {
	real := readPaneFixture(t, realSwallowedEnterPane)

	lines := strings.Split(real, "\n")
	markerIdx := bottomMostPromptMarkerIdx(lines)
	if markerIdx == -1 {
		t.Fatalf("the swallowed-Enter fixture no longer has a live composer row to splice above:\n%s", real)
	}
	spliced := append([]string{}, lines[:markerIdx]...)
	spliced = append(spliced, "✳ Cooking… (18s · ↓ 1.4k tokens · esc to interrupt)")
	adversarial := strings.Join(append(spliced, lines[markerIdx:]...), "\n")

	// The splice must actually be the worst case, or the assertion is vacuous:
	// the working signal the old verdict rested on has to read true here.
	if !statusdetect.HasWorkingIndicator([]byte(adversarial)) {
		t.Fatalf("the spliced pane does not read as working, so it does not exercise the "+
			"hazard this test exists for:\n%s", adversarial)
	}
	if paneShowsPayloadQueued(adversarial, queuedFixturePayload) {
		t.Fatal("the spliced pane must not show a queue shape; it is the swallowed-Enter case")
	}

	outcome, err := verifyPane(t, adversarial)

	if outcome == OutcomeQueued {
		t.Fatal("a pane whose live composer still HOLDS the payload must never be reported " +
			"as queued, however loudly the agent reports it is working: 'the agent is working' " +
			"is a fact about the agent's PREVIOUS turn, not a receipt for this payload, and a " +
			"queued verdict here skips the recovery and silently drops the message")
	}
	if outcome != OutcomeNotSubmitted {
		t.Fatalf("outcome = %v, want %v", outcome, OutcomeNotSubmitted)
	}
	if !errors.Is(err, errSubmissionPending) {
		t.Fatalf("error must carry errSubmissionPending so the retry runs, got %v", err)
	}
}

// TestRealMidTurnPanesCarryNoWorkingIndicator records a finding that is wider
// than this ticket and deliberately pinned here: on claude 2.1.220 the
// production working probe answers FALSE for a genuinely mid-turn pane.
//
// Both fixtures were captured during one running turn — one with the payload
// typed into the composer, one ten seconds later with it cleared — so composer
// text is ruled out as the cause. The spinner simply renders as
// "✳ Blanching… (1m 37s · ↓ 244 tokens)", with no "esc to interrupt" clause for
// statusdetect's primary regex; a 50-poll sweep across a full turn saw the
// clause zero times.
//
// This test does not assert that the probe is wrong to be built the way it is —
// it pins the observation, so that anyone tempted to rebuild a delivery verdict
// on that signal (as BOS-599's first pass did) hits a failing test that shows
// the signal is absent from the panes it would classify.
func TestRealMidTurnPanesCarryNoWorkingIndicator(t *testing.T) {
	for _, name := range []string{realSwallowedEnterPane, realWorkingEmptyComposerPane, realQueuedPane} {
		t.Run(name, func(t *testing.T) {
			pane := readPaneFixture(t, name)
			if statusdetect.HasWorkingIndicator([]byte(pane)) {
				t.Fatalf("statusdetect.HasWorkingIndicator now reports WORKING for %s. That is a "+
					"behaviour change worth understanding, not a test to relax: this fixture was "+
					"captured mid-turn and the probe read false, which is why delivery no longer "+
					"depends on it.", name)
			}
		})
	}
}

// TestVerifyQueuedSkipsEnterRetry asserts the safety property the distinct
// sentinel exists for: a queued payload is already in the agent's queue, so the
// verifier must NOT clear the composer, re-type it, and press Enter again —
// that would queue a second copy of the user's message. The redeliver callback
// is the observable proof, since it is the step that re-types the payload.
func TestVerifyQueuedSkipsEnterRetry(t *testing.T) {
	pane := readPaneFixture(t, realQueuedPane)
	f := &sendPlanRecordingFactory{capturePaneOutputs: []string{pane}}
	c := NewClient(WithCommandFactory(f.factory))

	redelivered := false
	err := c.verifyWithEnterRetry(context.Background(), "boss-test-queued", queuedFixturePayload,
		sendPlanOpts{
			submitVerifyWait: 40 * time.Millisecond,
			submitVerifyTick: 5 * time.Millisecond,
		},
		func(context.Context) error {
			redelivered = true
			return nil
		})

	if redelivered {
		t.Fatal("a queued payload must never be re-delivered: the agent already holds it, " +
			"so re-typing and pressing Enter again queues a SECOND copy of the message")
	}
	if got := OutcomeOf(err); got != OutcomeQueued {
		t.Fatalf("OutcomeOf(err) = %v, want %v — the caller must see the queued verdict, "+
			"not a generic submit failure that would tell it to retry", got, OutcomeQueued)
	}
	for _, call := range f.callsCopy() {
		if call.subcommand == "send-keys" {
			t.Fatalf("no keys may be sent after a queued verdict, got send-keys %v", call.args)
		}
	}
}

// TestPaneShowsPayloadQueued covers the discriminator directly, including the
// ways it must refuse to fire. Both halves are required — the queue hint in the
// live composer AND this payload echoed above it — so that a queued verdict is
// never produced from an unrelated agent queue or from transcript prose.
func TestPaneShowsPayloadQueued(t *testing.T) {
	const payload = "also update the changelog"
	const hint = "❯ Press up to edit queued messages"

	cases := []struct {
		name string
		pane string
		want bool
	}{
		{
			name: "queue hint in the composer with the payload echoed above",
			pane: "⏺ Working…\n\n  ❯ " + payload + "\n\n" + hint + "\n  ? for shortcuts\n",
			want: true,
		},
		{
			name: "queue hint but a different message queued",
			pane: "⏺ Working…\n\n  ❯ run the tests again\n\n" + hint + "\n",
			want: false,
		},
		{
			// The swallowed Enter: the payload is IN the composer, so there is no
			// queue at all. This is the case the old verdict got wrong.
			name: "payload held in the live composer",
			pane: "⏺ Working…\n\n❯ " + payload + "\n  ? for shortcuts\n",
			want: false,
		},
		{
			// The nastiest shape, and the reason the hint is read off the composer
			// rather than the pane: an EARLIER message is genuinely queued while
			// THIS payload sits unsent. Typed text replaces the placeholder, so the
			// hint is not on the composer row and the verdict stays pending — which
			// is what lets the retry recover it.
			name: "another message queued while this payload is still unsent",
			pane: "⏺ Working…\n\n  ❯ an earlier queued message\n\n❯ " + payload + "\n  ? for shortcuts\n",
			want: false,
		},
		{
			name: "cleared composer with no queue hint",
			pane: "⏺ Done.\n\n  ❯ " + payload + "\n\n❯\n  ? for shortcuts\n",
			want: false,
		},
		{
			// Matched against the composer's own content only, so transcript text
			// discussing queued messages cannot fabricate a queued verdict.
			name: "queue wording in the transcript rather than the composer",
			pane: "⏺ I will edit queued messages later.\n\n  ❯ " + payload + "\n\n❯\n",
			want: false,
		},
		{
			name: "no live composer at all",
			pane: "⏺ Working full-screen with no input box drawn.\n",
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := paneShowsPayloadQueued(tc.pane, payload); got != tc.want {
				t.Fatalf("paneShowsPayloadQueued = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestQueuedFixturesAreCommitted guards the fixtures themselves: the ticket
// requires the captures to be committed, and a test that silently skipped on a
// missing file would make that vacuous in CI.
func TestQueuedFixturesAreCommitted(t *testing.T) {
	for _, name := range []string{realQueuedPane, realSwallowedEnterPane, realWorkingEmptyComposerPane, "capture.sh"} {
		if _, err := os.Stat(filepath.Join("testdata", "panes", name)); err != nil {
			t.Fatalf("required pane artefact %s is missing: %v", name, err)
		}
	}
}
