package tmux

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// codexReceiptOpts is a codex send (typed literally, no paste settle) whose
// receipt records every turn, as the server wires a Codex rollout receipt.
func codexReceiptOpts(submitted func() bool, required bool) sendPlanOpts {
	return sendPlanOpts{
		deadline:         time.Second,
		pollInterval:     5 * time.Millisecond,
		readyMarker:      "›",
		submitVerifyWait: 150 * time.Millisecond,
		submitVerifyTick: 5 * time.Millisecond,
		submitted:        submitted,
		receiptRequired:  required,
	}
}

// codexEmptyComposer is the real codex pane with the payload gone from the
// composer: what Codex draws while it holds a typed burst undrawn.
func codexEmptyComposer(t *testing.T) string {
	t.Helper()
	pane := readPaneFixture(t, "codex_pending.txt")
	empty := strings.Replace(pane, "› "+codexFixturePayload, "›", 1)
	if empty == pane {
		t.Fatal("invalid fixture: the pending pane does not hold the payload")
	}
	return empty
}

func typedCount(calls []sendPlanCall, payload string) int {
	n := 0
	for _, call := range calls {
		if call.subcommand == "send-keys" && len(call.args) > 0 && call.args[len(call.args)-1] == payload {
			n++
		}
	}
	return n
}

// TestRequiredReceiptHeldBurstIsUnconfirmed is the BOS-3068 incident: Codex
// holds the typed burst undrawn, so every capture after the Enter shows an empty
// composer, and the agent records no turn. An empty composer alone must not
// confirm the send. Nothing is cleared or retyped, because an empty composer
// gives the retry nothing to clear and the payload may still land.
func TestRequiredReceiptHeldBurstIsUnconfirmed(t *testing.T) {
	empty := codexEmptyComposer(t)
	fake := &sendPlanRecordingFactory{capturePaneOutputs: []string{empty}}
	c := NewClient(WithCommandFactory(fake.factory))

	err := c.sendPlan(context.Background(), "boss-test-codex", codexFixturePayload,
		codexReceiptOpts(func() bool { return false }, true))
	if OutcomeOf(err) != OutcomeUnconfirmed {
		t.Fatalf("outcome = %v (%v), want %v: an empty composer without a receipt proves nothing", OutcomeOf(err), err, OutcomeUnconfirmed)
	}
	calls := fake.callsCopy()
	if callIndex(calls, "send-keys", "C-u") != -1 {
		t.Fatal("the composer was cleared on an unconfirmed send")
	}
	if n := typedCount(calls, codexFixturePayload); n != 1 {
		t.Fatalf("payload typed %d times, want 1", n)
	}
}

// TestRequiredReceiptConfirmsOverEmptyComposer: the rollout records the turn,
// so the send is submitted.
func TestRequiredReceiptConfirmsOverEmptyComposer(t *testing.T) {
	empty := codexEmptyComposer(t)
	fake := &sendPlanRecordingFactory{capturePaneOutputs: []string{empty}}
	c := NewClient(WithCommandFactory(fake.factory))
	var reads atomic.Int32
	receipt := func() bool { return reads.Add(1) > 3 }

	if err := c.sendPlan(context.Background(), "boss-test-codex", codexFixturePayload, codexReceiptOpts(receipt, true)); err != nil {
		t.Fatalf("a recorded turn must confirm the send, got %v (outcome %v)", err, OutcomeOf(err))
	}
	if callIndex(fake.callsCopy(), "send-keys", "C-u") != -1 {
		t.Fatal("the composer was cleared after the receipt confirmed the send")
	}
}

// TestRequiredReceiptRetriesABurstThatFlushesIntoTheComposer: the composer reads
// empty right after the Enter, then Codex flushes the held burst and the
// payload reappears, still unsubmitted. With the rollout receipt required, the
// early empty reads do not end the wait, so the verifier sees the payload held
// and runs its clear-and-redeliver retry. Without it, the first empty read is
// taken as submitted and the message is stranded, which is the bug.
func TestRequiredReceiptRetriesABurstThatFlushesIntoTheComposer(t *testing.T) {
	empty := codexEmptyComposer(t)
	held := readPaneFixture(t, "codex_pending.txt")
	frames := []string{empty, empty, empty, held}

	t.Run("receipt required", func(t *testing.T) {
		fake := &sendPlanRecordingFactory{capturePaneOutputs: frames}
		c := NewClient(WithCommandFactory(fake.factory))

		err := c.sendPlan(context.Background(), "boss-test-codex", codexFixturePayload,
			codexReceiptOpts(func() bool { return false }, true))
		if OutcomeOf(err) != OutcomeNotSubmitted {
			t.Fatalf("outcome = %v (%v), want %v", OutcomeOf(err), err, OutcomeNotSubmitted)
		}
		// The fake pane never changes, so the retry's clear proves nothing and
		// it stops before retyping; the C-u is the evidence the retry ran.
		if callIndex(fake.callsCopy(), "send-keys", "C-u") == -1 {
			t.Fatal("the held payload was never cleared: the retry did not run")
		}
	})

	t.Run("pane only", func(t *testing.T) {
		fake := &sendPlanRecordingFactory{capturePaneOutputs: frames}
		c := NewClient(WithCommandFactory(fake.factory))

		if err := c.sendPlan(context.Background(), "boss-test-codex", codexFixturePayload, codexReceiptOpts(nil, false)); err != nil {
			t.Fatalf("pane-only verification reads the first empty composer as submitted, got %v", err)
		}
	})
}

// codexQueuedPane is a real codex-cli 0.161.0 pane captured 0.7s after Enter on
// codexFixturePayload while a turn was running (capture_codex_queued.sh): codex
// holds it under its "Messages to be submitted" header and its rollout records
// no turn for it until the running turn ends.
const codexQueuedPane = "codex_queued_midturn_real.txt"

// withoutCodexQueueBlock removes the queue header and its items, leaving the
// same pane with a placeholder composer and no sign of where the payload went.
func withoutCodexQueueBlock(t *testing.T, pane string) string {
	t.Helper()
	var kept []string
	for _, row := range strings.Split(pane, "\n") {
		if strings.Contains(row, "Messages to be submitted") || strings.Contains(row, codexQueuedItemGlyph) {
			continue
		}
		kept = append(kept, row)
	}
	stripped := strings.Join(kept, "\n")
	if stripped == pane {
		t.Fatal("invalid fixture: the queued pane has no queue block to remove")
	}
	return stripped
}

// TestRequiredReceiptCodexQueuedSendIsQueued: a send into a busy codex chat is
// accepted into codex's queue, and the receipt cannot arrive until the running
// turn ends. The queue on the pane must classify it QUEUED, so the caller
// records it delivered and never sends it again.
func TestRequiredReceiptCodexQueuedSendIsQueued(t *testing.T) {
	pane := readPaneFixture(t, codexQueuedPane)
	if !strings.Contains(pane, "↳ "+codexFixturePayload) {
		t.Fatalf("invalid fixture: the payload is not in codex's queue:\n%s", pane)
	}
	if lineStillAtPrompt(pane, codexFixturePayload) {
		t.Fatal("invalid fixture: the payload is still in the live composer")
	}
	fake := &sendPlanRecordingFactory{capturePaneOutputs: []string{pane}}
	c := NewClient(WithCommandFactory(fake.factory))

	err := c.sendPlan(context.Background(), "boss-test-codex", codexFixturePayload,
		codexReceiptOpts(func() bool { return false }, true))
	if OutcomeOf(err) != OutcomeQueued {
		t.Fatalf("outcome = %v (%v), want %v", OutcomeOf(err), err, OutcomeQueued)
	}
	calls := fake.callsCopy()
	if callIndex(calls, "send-keys", "C-u") != -1 {
		t.Fatal("the composer was cleared on a queued send")
	}
	if n := typedCount(calls, codexFixturePayload); n != 1 {
		t.Fatalf("payload typed %d times, want 1", n)
	}
}

// TestRequiredReceiptVerdictsOnRealPanes pins what a receipt-required send
// reports for each real pane shape.
func TestRequiredReceiptVerdictsOnRealPanes(t *testing.T) {
	never := func() bool { return false }

	cases := []struct {
		name    string
		pane    func(t *testing.T) string
		payload string
		receipt func() bool
		want    SubmitOutcome
	}{
		{
			name:    "codex queue holds the payload",
			pane:    func(t *testing.T) string { return readPaneFixture(t, codexQueuedPane) },
			payload: codexFixturePayload,
			receipt: never,
			want:    OutcomeQueued,
		},
		{
			name:    "claude queue holds the payload",
			pane:    func(t *testing.T) string { return readPaneFixture(t, realQueuedPane) },
			payload: queuedFixturePayload,
			receipt: never,
			want:    OutcomeQueued,
		},
		{
			name:    "codex submitted and the rollout records the turn",
			pane:    func(t *testing.T) string { return readPaneFixture(t, "codex_submitted.txt") },
			payload: codexFixturePayload,
			receipt: func() func() bool {
				var reads atomic.Int32
				return func() bool { return reads.Add(1) > 3 }
			}(),
			want: OutcomeSubmitted,
		},
		{
			name: "codex placeholder composer with no queue and no receipt",
			pane: func(t *testing.T) string {
				return withoutCodexQueueBlock(t, readPaneFixture(t, codexQueuedPane))
			},
			payload: codexFixturePayload,
			receipt: never,
			want:    OutcomeUnconfirmed,
		},
		{
			name:    "codex blank composer with no queue and no receipt",
			pane:    codexEmptyComposer,
			payload: codexFixturePayload,
			receipt: never,
			want:    OutcomeUnconfirmed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &sendPlanRecordingFactory{capturePaneOutputs: []string{tc.pane(t)}}
			c := NewClient(WithCommandFactory(fake.factory))

			got, err := c.waitForSubmissionOrReceipt(context.Background(), "boss-test-codex", tc.payload,
				60*time.Millisecond, 5*time.Millisecond, tc.receipt, true)
			if got != tc.want {
				t.Fatalf("outcome = %v (%v), want %v", got, err, tc.want)
			}
		})
	}
}
