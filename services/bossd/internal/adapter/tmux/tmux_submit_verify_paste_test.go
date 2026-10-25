package tmux

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/kit/config"
)

// The fixtures this file asserts against are REAL captures produced by
// testdata/panes/capture_paste.sh (Claude Code 2.1.296, codex-cli 0.161.0,
// tmux 3.8). Both TUIs collapse a long payload into a placeholder — Claude's
// "[Pasted text #N]" / "[Pasted text #N +M lines]", codex's "[Pasted Content N
// chars]" — that does not prefix-match the payload, so a verifier that only
// recognised the payload's own text read a held composer as cleared and
// reported the send delivered (BOS-1506).

// pasteFixtureLongLine is the 2 KB single-line payload capture_paste.sh sends.
// It is rebuilt here exactly as the script builds it, because the verifier
// assertions below depend on the payload text.
func pasteFixtureLongLine() string {
	var b strings.Builder
	b.WriteString("Reply with only the single word OK and nothing else. Ignore the rest of this message, it is padding for a delivery test:")
	for i := 1; i <= 90; i++ {
		fmt.Fprintf(&b, " lorem ipsum padding %d", i)
	}
	return b.String()
}

// pasteFixtureMultiLine is the 11-line payload capture_paste.sh sends.
func pasteFixtureMultiLine() string {
	lines := []string{"Reply with only the single word OK and nothing else."}
	for i := 1; i <= 10; i++ {
		lines = append(lines, fmt.Sprintf("padding line %d of a multi-line delivery test", i))
	}
	return strings.Join(lines, "\n")
}

// waitOnPane runs waitForSubmission against a pane that never changes, so a
// pane the predicates read as held reaches the deadline branch.
func waitOnPane(t *testing.T, pane, payload string) (SubmitOutcome, error) {
	t.Helper()
	f := &sendPlanRecordingFactory{capturePaneOutputs: []string{pane}}
	c := NewClient(WithCommandFactory(f.factory))
	return c.waitForSubmission(context.Background(), "boss-test-paste", payload,
		40*time.Millisecond, 5*time.Millisecond)
}

func TestRealClaudeCollapsedPastesReadAsNotSubmitted(t *testing.T) {
	cases := []struct {
		name    string
		fixture string
		payload string
	}{
		// The pre-change delivery path: send-keys -l typed fast enough that Claude
		// read the burst as a paste and kept "[Pasted text #5][Pasted text
		// #6]<tail>" in the composer.
		{"typed long line", "claude_typed_long_line_stuck_real.txt", pasteFixtureLongLine()},
		{"pasted long line", "claude_paste_long_line_stuck_real.txt", pasteFixtureLongLine()},
		{"pasted multi-line", "claude_paste_multiline_stuck_real.txt", pasteFixtureMultiLine()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pane := readPaneFixture(t, tc.fixture)
			if hint := composerHintOf(t, pane); !strings.HasPrefix(hint, "[Pasted text #") {
				t.Fatalf("invalid fixture: the live composer shows %q, not a collapsed paste", hint)
			}

			outcome, err := waitOnPane(t, pane, tc.payload)
			if outcome != OutcomeNotSubmitted {
				t.Fatalf("outcome = %v, want %v: the live composer still holds the collapsed paste", outcome, OutcomeNotSubmitted)
			}
			if !errors.Is(err, errSubmissionPending) {
				t.Fatalf("a held composer must carry errSubmissionPending so the retry runs, got %v", err)
			}
			if composerEmptyOf(pane, tc.payload) {
				t.Fatal("composerEmptyOf = true: a C-u retry would re-deliver onto the held paste")
			}
		})
	}
}

func TestRealClaudeSubmittedPastesReadAsSubmitted(t *testing.T) {
	cases := []struct {
		name    string
		fixture string
		payload string
	}{
		{"pasted long line", "claude_paste_long_line_submitted_real.txt", pasteFixtureLongLine()},
		{"pasted multi-line", "claude_paste_multiline_submitted_real.txt", pasteFixtureMultiLine()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pane := readPaneFixture(t, tc.fixture)
			if hint := composerHintOf(t, pane); hint != "" {
				t.Fatalf("invalid fixture: the live composer is not cleared, it shows %q", hint)
			}
			outcome, err := waitOnPane(t, pane, tc.payload)
			if outcome != OutcomeSubmitted || err != nil {
				t.Fatalf("outcome = %v, err = %v; want submitted: the composer has cleared", outcome, err)
			}
		})
	}
}

// A capture failure says nothing about the composer, however a previous poll
// read it: it stays unconfirmed, which never licenses the clear-and-redeliver
// retry.
func TestCollapsedPasteCaptureFailureIsUnconfirmed(t *testing.T) {
	failFrom := 1
	f := &sendPlanRecordingFactory{
		capturePaneOutputs:  []string{readPaneFixture(t, "claude_paste_long_line_stuck_real.txt")},
		failCapturePaneFrom: &failFrom,
	}
	c := NewClient(WithCommandFactory(f.factory))
	outcome, err := c.waitForSubmission(context.Background(), "boss-test-paste", pasteFixtureLongLine(),
		time.Second, 5*time.Millisecond)
	if outcome != OutcomeUnconfirmed {
		t.Fatalf("outcome = %v, want %v", outcome, OutcomeUnconfirmed)
	}
	if errors.Is(err, errSubmissionPending) {
		t.Fatalf("a capture failure must not carry errSubmissionPending, got %v", err)
	}
}

// The settle-wait tests below drive the wait's MECHANISM, not a TUI's grammar,
// so their panes are synthetic frames built inline: a Claude-shaped input box
// between two rules, a spinner row above it and a footer below. The paste
// grammar itself is asserted against the real captures above.
const (
	settleRule   = "────────────────────────────────────────"
	settleFooter = "  Opus 5.5 (1M context) · footer"
)

// settleFrame renders one synthetic Claude pane: a spinner row, then the
// composer holding content, then the footer.
func settleFrame(spinner, content string) string {
	return spinner + "\n" + settleRule + "\n❯ " + content + "\n" + settleRule + "\n" + settleFooter + "\n"
}

// claudePasteOpts is a hand-built Claude send with the paste routing and the
// settle wait on, and budgets small enough for a unit test.
func claudePasteOpts(settle time.Duration) sendPlanOpts {
	return sendPlanOpts{
		deadline:         time.Second,
		pollInterval:     5 * time.Millisecond,
		readyMarker:      sendPlanReadyMarker,
		submitVerifyWait: 200 * time.Millisecond,
		submitVerifyTick: 5 * time.Millisecond,
		pasteFreeText:    true,
		pasteSettleWait:  settle,
	}
}

// callIndex returns the index of the first call matching subcommand whose last
// argument is lastArg (any last argument when lastArg is empty), or -1.
func callIndex(calls []sendPlanCall, subcommand, lastArg string) int {
	for i, call := range calls {
		if call.subcommand != subcommand {
			continue
		}
		if lastArg == "" || (len(call.args) > 0 && call.args[len(call.args)-1] == lastArg) {
			return i
		}
	}
	return -1
}

// TestClaudePasteSettleWaitsForPasteThenTwoEqualReads: the composer still shows
// the pre-paste rows for two reads (the paste has not landed), then the
// collapsed paste twice. Enter must follow the second identical paste read and
// nothing but capture-pane may run between the paste and the Enter.
func TestClaudePasteSettleWaitsForPasteThenTwoEqualReads(t *testing.T) {
	empty := settleFrame("✻ Thinking", "")
	pasted := settleFrame("✻ Thinking", "[Pasted text #1]")
	fake := &sendPlanRecordingFactory{capturePaneOutputs: []string{
		empty,  // readiness
		empty,  // pre-paste snapshot
		empty,  // settle: paste not landed yet
		empty,  // settle: still not landed
		pasted, // settle: changed
		pasted, // settle: equal to the previous read -> settled
		empty,  // verifier: composer cleared -> submitted
	}}
	c := NewClient(WithCommandFactory(fake.factory))

	if err := c.sendPlan(context.Background(), "boss-test-paste", pasteFixtureLongLine(), claudePasteOpts(2*time.Second)); err != nil {
		t.Fatalf("sendPlan: %v", err)
	}

	calls := fake.callsCopy()
	pasteIdx, enterIdx := callIndex(calls, "paste-buffer", ""), callIndex(calls, "send-keys", "Enter")
	if pasteIdx == -1 || enterIdx == -1 || enterIdx < pasteIdx {
		t.Fatalf("want a paste-buffer followed by an Enter, calls = %+v", calls)
	}
	if lit := callIndex(calls, "send-keys", ""); lit != enterIdx {
		t.Fatalf("a Claude free-text payload must not be typed literally, calls = %+v", calls)
	}
	for _, call := range calls[pasteIdx+1 : enterIdx] {
		if call.subcommand != "capture-pane" {
			t.Fatalf("the settle wait ran %q; it may only capture the pane", call.subcommand)
		}
	}
	if n := enterIdx - pasteIdx - 1; n != 4 {
		t.Fatalf("settle wait took %d captures before Enter, want 4 (two pre-paste reads, then two equal paste reads)", n)
	}
}

// TestClaudePasteSettleIgnoresSpinnerOutsideComposer: a spinner above the input
// box changes on every capture, which must not hold the wait open.
func TestClaudePasteSettleIgnoresSpinnerOutsideComposer(t *testing.T) {
	fake := &sendPlanRecordingFactory{capturePaneOutputs: []string{
		settleFrame("✻ Thinking (1s)", ""),
		settleFrame("✻ Thinking (1s)", ""),
		settleFrame("✻ Thinking (2s)", "[Pasted text #1]"),
		settleFrame("✶ Thinking (3s)", "[Pasted text #1]"),
		settleFrame("✻ Thinking (4s)", ""),
	}}
	c := NewClient(WithCommandFactory(fake.factory))
	// The slice is far longer than the four captures this send needs, so a wait
	// held open by the spinner is unmistakable: it spends the whole slice.
	const settleSlice = 5 * time.Second

	start := time.Now()
	if err := c.sendPlan(context.Background(), "boss-test-paste", pasteFixtureLongLine(), claudePasteOpts(settleSlice)); err != nil {
		t.Fatalf("sendPlan: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= settleSlice {
		t.Fatalf("send took %v: a spinner outside the composer held the %v settle wait open", elapsed, settleSlice)
	}
	calls := fake.callsCopy()
	if n := callIndex(calls, "send-keys", "Enter") - callIndex(calls, "paste-buffer", "") - 1; n != 2 {
		t.Fatalf("settle wait took %d captures, want 2: the composer rows agreed on the second read", n)
	}
}

// TestClaudePasteSettleProceedsWhenSliceExpires: the composer never changes, so
// the wait spends its slice and then proceeds to Enter and the verifier — with
// no second paste.
func TestClaudePasteSettleProceedsWhenSliceExpires(t *testing.T) {
	held := settleFrame("✻ Thinking", "")
	fake := &sendPlanRecordingFactory{capturePaneOutputs: []string{held}}
	c := NewClient(WithCommandFactory(fake.factory))
	const slice = 150 * time.Millisecond
	opts := claudePasteOpts(slice)

	start := time.Now()
	_ = c.sendPlan(context.Background(), "boss-test-paste", pasteFixtureLongLine(), opts)
	calls := fake.callsCopy()

	enterIdx := callIndex(calls, "send-keys", "Enter")
	if enterIdx == -1 {
		t.Fatalf("the settle wait never proceeded to Enter, calls = %+v", calls)
	}
	pastes := 0
	for _, call := range calls[:enterIdx] {
		if call.subcommand == "paste-buffer" {
			pastes++
		}
	}
	if pastes != 1 {
		t.Fatalf("pasted %d times before Enter, want exactly 1: the settle wait must never re-paste", pastes)
	}
	if elapsed := time.Since(start); elapsed < slice {
		t.Fatalf("send finished in %v, before the %v settle slice expired", elapsed, slice)
	}
}

// TestClaudePrefillPastesWithoutSettle: a prefill sends no Enter, so there is
// nothing to settle before — it pastes and returns without reading the pane.
func TestClaudePrefillPastesWithoutSettle(t *testing.T) {
	fake := &sendPlanRecordingFactory{capturePaneOutputs: []string{settleFrame("", "")}}
	c := NewClient(WithCommandFactory(fake.factory))

	if err := c.SendMessage(context.Background(), "boss-test-paste", "run the thing", false, sendPlanReadyMarker); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	calls := fake.callsCopy()
	if callIndex(calls, "paste-buffer", "") == -1 || callIndex(calls, "send-keys", "") != -1 {
		t.Fatalf("a Claude prefill must paste and send no keys, calls = %+v", calls)
	}
	if n := len(calls) - callIndex(calls, "paste-buffer", "") - 1; n != 0 {
		t.Fatalf("a prefill ran %d calls after the paste, want 0", n)
	}
}

// TestReceiptConfirmsSubmissionOverAHeldPane: the pane keeps showing the
// collapsed paste, but the caller's receipt says the agent took it. The send
// reports submitted and never clears the composer — a C-u and re-delivery
// past a receipt would submit a second copy.
func TestReceiptConfirmsSubmissionOverAHeldPane(t *testing.T) {
	held := readPaneFixture(t, "claude_paste_long_line_stuck_real.txt")
	fake := &sendPlanRecordingFactory{capturePaneOutputs: []string{held}}
	c := NewClient(WithCommandFactory(fake.factory))
	opts := claudePasteOpts(20 * time.Millisecond)
	opts.submitted = func() bool { return true }

	if err := c.sendPlan(context.Background(), "boss-test-paste", pasteFixtureLongLine(), opts); err != nil {
		t.Fatalf("a receipt must confirm the send, got %v (outcome %v)", err, OutcomeOf(err))
	}
	if idx := callIndex(fake.callsCopy(), "send-keys", "C-u"); idx != -1 {
		t.Fatal("the composer was cleared after a receipt confirmed the send")
	}
}

// TestNoReceiptHeldPaneIsNotSubmitted is the other half: no receipt and a
// composer still holding the collapsed paste is a loud not-submitted.
func TestNoReceiptHeldPaneIsNotSubmitted(t *testing.T) {
	held := readPaneFixture(t, "claude_paste_long_line_stuck_real.txt")
	fake := &sendPlanRecordingFactory{capturePaneOutputs: []string{held}}
	c := NewClient(WithCommandFactory(fake.factory))
	opts := claudePasteOpts(20 * time.Millisecond)
	opts.submitted = func() bool { return false }

	err := c.sendPlan(context.Background(), "boss-test-paste", pasteFixtureLongLine(), opts)
	if OutcomeOf(err) != OutcomeNotSubmitted {
		t.Fatalf("outcome = %v (%v), want %v", OutcomeOf(err), err, OutcomeNotSubmitted)
	}
}

// bossoCommandDeadline mirrors commandDeadline in
// services/bosso/internal/server/proxy.go: the relay ceiling every established
// send runs under. It is restated because that module cannot be imported here.
const bossoCommandDeadline = 30 * time.Second

// establishedSendTail is the worst-case time an established send can spend
// after its readiness wait: the settle wait on the first delivery, the first
// verify, the retry's clear presses (carved out of one verify budget), the
// settle wait on the re-delivery, and the re-verify.
func establishedSendTail(opts sendPlanOpts) time.Duration {
	return 2*max(opts.pasteSettleWait, typedDrawBudget(opts)) + 3*opts.submitVerifyWait
}

// TestEstablishedSendTailFitsRelayReservation: the readiness wait may grow to
// config.SendReadyDeadlineMax, and everything else an established Claude send
// does must fit in what the relay ceiling leaves over (BOS-1506 added the
// settle wait to that tail).
func TestEstablishedSendTailFitsRelayReservation(t *testing.T) {
	opts := NewClient().sendDeliveryOpts(sendPlanReadyMarker, true, nil)
	if !opts.pasteFreeText || opts.pasteSettleWait <= 0 {
		t.Fatalf("a Claude send must paste and settle: pasteFreeText=%v pasteSettleWait=%v", opts.pasteFreeText, opts.pasteSettleWait)
	}
	reservation := bossoCommandDeadline - config.SendReadyDeadlineMax
	if tail := establishedSendTail(opts); tail >= reservation {
		t.Fatalf("established-send tail %v does not fit the %v the relay leaves after a %v readiness wait",
			tail, reservation, config.SendReadyDeadlineMax)
	}
}

// TestWithPasteSettleWaitBoundsTheClaudeSettle: the option replaces the
// production settle bound on both delivery paths, and a non-positive value
// leaves the production bound in place.
func TestWithPasteSettleWaitBoundsTheClaudeSettle(t *testing.T) {
	c := NewClient(WithPasteSettleWait(20 * time.Millisecond))
	for name, opts := range map[string]sendPlanOpts{
		"send":  c.sendDeliveryOpts(sendPlanReadyMarker, true, nil),
		"start": c.startDeliveryOpts(sendPlanReadyMarker, true, nil),
	} {
		if opts.pasteSettleWait != 20*time.Millisecond {
			t.Errorf("%s: pasteSettleWait = %v, want 20ms", name, opts.pasteSettleWait)
		}
	}
	for _, d := range []time.Duration{0, -time.Second} {
		if got := NewClient(WithPasteSettleWait(d)).sendDeliveryOpts(sendPlanReadyMarker, true, nil).pasteSettleWait; got != pasteSettleWait {
			t.Errorf("WithPasteSettleWait(%v): pasteSettleWait = %v, want the production %v", d, got, pasteSettleWait)
		}
	}
}

// TestHeldPasteSendStaysInsideItsTail measures the bound above on a real run:
// a Claude send whose composer keeps holding the paste finishes within its
// readiness wait plus establishedSendTail.
func TestHeldPasteSendStaysInsideItsTail(t *testing.T) {
	held := readPaneFixture(t, "claude_paste_long_line_stuck_real.txt")
	fake := &sendPlanRecordingFactory{capturePaneOutputs: []string{held}}
	c := NewClient(WithCommandFactory(fake.factory))
	opts := claudePasteOpts(100 * time.Millisecond)

	start := time.Now()
	err := c.sendPlan(context.Background(), "boss-test-paste", pasteFixtureLongLine(), opts)
	elapsed := time.Since(start)
	if OutcomeOf(err) != OutcomeNotSubmitted {
		t.Fatalf("outcome = %v (%v), want %v", OutcomeOf(err), err, OutcomeNotSubmitted)
	}
	// The readiness wait returns on its first capture here; the slack covers
	// process spawns for the fake tmux commands.
	if bound := establishedSendTail(opts) + 500*time.Millisecond; elapsed > bound {
		t.Fatalf("held send took %v, beyond its tail bound %v", elapsed, bound)
	}
}
