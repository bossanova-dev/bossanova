package tmux

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The composer-readiness wait used to get exactly one shot: if the agent TUI
// was still repainting when the budget expired, session start failed outright
// even though the pane became ready moments later (BOS-895). These tests cover
// the retry that fixes it — and, at least as importantly, the boundaries it
// must not cross. This project's standing rule is that a command is never
// re-sent after an ambiguous delivery; the retry is only sound because it is
// lexically confined to the phase BEFORE the first keystroke, so most of what
// follows is about proving that confinement rather than the retry itself.

const (
	// retryAttemptBudget is one attempt's readiness budget in these tests:
	// long enough that a poll loop runs several times, short enough that a
	// handful of exhausted attempts still finish in well under a second.
	//
	// The margins matter as much as the values: every poll forks a real
	// subprocess, so the gap between an attempt's end and retryReadyAt has to
	// absorb accumulated fork latency on a loaded box. At 200/300ms both
	// margins are 100ms — attempt one ends 100ms before the composer appears,
	// and attempt two still has 100ms left after it does.
	retryAttemptBudget = 200 * time.Millisecond
	// retryPollInterval is deliberately far below the production 100ms so an
	// attempt's span is set by its budget rather than by its poll granularity.
	retryPollInterval = 10 * time.Millisecond
	// retryReadyAt is when the pane starts drawing a composer: after attempt
	// one's budget has expired, before attempt two's would.
	retryReadyAt = 300 * time.Millisecond

	// retryPlanBody is deliberately MULTI-LINE. sendPlan dispatches on the
	// payload's shape — a single logical line is typed with send-keys -l, while
	// anything with a newline goes through load-buffer → paste-buffer — so the
	// tests that count deliveries must pick a shape and stay on it. Multi-line
	// is the session-start shape (a plan), which is the path BOS-895's retry
	// actually sits in front of.
	retryPlanBody = "plan line one\nplan line two\n"
)

// lateComposerFactory models the production failure: the pane is up and
// capturable from the first poll, but shows a booting TUI until retryReadyAt.
func lateComposerFactory() *sendPlanRecordingFactory {
	return &sendPlanRecordingFactory{
		capturePaneRules: []capturePaneRule{
			{after: 0, output: "Welcome to Claude — still booting\n"},
			{after: retryReadyAt, output: "some output\n❯ \n"},
		},
	}
}

// TestReadyRetry_LateComposerNeedsTheSecondAttempt is the headline behaviour,
// asserted as a PAIR so it cannot pass by accident. The same late-composer pane
// is delivered to twice: once with one attempt, which must fail, and once with
// two, which must succeed. Asserting only the success would leave the test
// green on a machine slow enough that attempt one happened to run past
// retryReadyAt; with the one-attempt case alongside it, that scheduling slop
// fails the test loudly instead of hiding the fact that nothing was retried.
func TestReadyRetry_LateComposerNeedsTheSecondAttempt(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timing-sensitive tmux test in -short; run make test-bossd for coverage")
	}
	tests := []struct {
		name     string
		attempts int
		wantErr  bool
	}{
		{name: "one attempt gives up before the composer appears", attempts: 1, wantErr: true},
		{name: "two attempts catch the composer on the second", attempts: 2, wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := lateComposerFactory()
			c := NewClient(WithCommandFactory(fake.factory))
			err := c.sendPlan(context.Background(), "boss-test-sess", retryPlanBody, sendPlanOpts{
				deadline:      retryAttemptBudget,
				pollInterval:  retryPollInterval,
				readyAttempts: tt.attempts,
				prefillOnly:   true,
			})
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected the single attempt to expire before the composer appeared, got nil")
				}
				// Nothing may have been typed: the wait never passed.
				assertNoDestructiveTmuxCalls(t, fake)
				return
			}
			if err != nil {
				t.Fatalf("delivery failed even though the composer appeared during attempt 2: %v", err)
			}
			// The retry must not have turned into a second DELIVERY. This is
			// the assertion the no-double-type rule actually cares about.
			assertDeliveredExactlyOnce(t, fake)
		})
	}
}

// assertDeliveredExactlyOnce fails unless the payload reached the pane exactly
// one time. A retry that re-ran any part of the delivery, rather than only the
// wait in front of it, shows up here as a second load-buffer or paste-buffer.
func assertDeliveredExactlyOnce(t *testing.T, fake *sendPlanRecordingFactory) {
	t.Helper()
	counts := map[string]int{}
	for _, call := range fake.callsCopy() {
		counts[call.subcommand]++
	}
	for _, sub := range []string{"load-buffer", "paste-buffer"} {
		if counts[sub] != 1 {
			t.Errorf("tmux %s ran %d times, want exactly 1 — the payload was delivered more than once", sub, counts[sub])
		}
	}
}

// TestReadyRetry_NeverTypesWhileWaiting pins the confinement directly: across a
// wait that exhausts every attempt, not one pane-mutating subcommand is
// emitted. Without this, a future refactor that hoisted part of the delivery
// above the wait would still pass every other test in this file.
func TestReadyRetry_NeverTypesWhileWaiting(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timing-sensitive tmux test in -short; run make test-bossd for coverage")
	}
	fake := neverReadyFactory()
	c := NewClient(WithCommandFactory(fake.factory))
	err := c.sendPlan(context.Background(), "boss-test-sess", "plan body", sendPlanOpts{
		deadline:      50 * time.Millisecond,
		pollInterval:  retryPollInterval,
		readyAttempts: 3,
	})
	if err == nil {
		t.Fatal("expected the readiness wait to exhaust its attempts, got nil")
	}
	assertNoDestructiveTmuxCalls(t, fake)
}

// TestReadyRetry_PostDeliveryFailureIsNotRetried is the other half of the
// confinement, and the one that would be a real incident: a failure AFTER the
// payload has been pasted must not re-run anything. The attempt count is 3, so
// a loop wrapped around the wrong scope would show up as three deliveries.
func TestReadyRetry_PostDeliveryFailureIsNotRetried(t *testing.T) {
	t.Parallel()
	fake := &sendPlanRecordingFactory{
		capturePaneOutputs: []string{"some output\n❯ \n"},
		failOnSubcommand:   map[string]int{"paste-buffer": 0},
	}
	c := NewClient(WithCommandFactory(fake.factory))
	err := c.sendPlan(context.Background(), "boss-test-sess", retryPlanBody, sendPlanOpts{
		deadline:      retryAttemptBudget,
		pollInterval:  retryPollInterval,
		readyAttempts: 3,
		prefillOnly:   true,
	})
	if err == nil {
		t.Fatal("expected the paste-buffer failure to surface, got nil")
	}
	counts := map[string]int{}
	for _, call := range fake.callsCopy() {
		counts[call.subcommand]++
	}
	if counts["load-buffer"] != 1 || counts["paste-buffer"] != 1 {
		t.Fatalf("delivery ran more than once after a post-readiness failure: load-buffer=%d paste-buffer=%d",
			counts["load-buffer"], counts["paste-buffer"])
	}
}

// TestReadyRetry_ExhaustionNamesTheAttemptCountFirst covers what the operator
// reads. The TUI renders this string through firstLine(), which cuts at the
// first newline, and the pane snapshot is multi-line — so the attempt count and
// elapsed time are only visible if they precede it.
func TestReadyRetry_ExhaustionNamesTheAttemptCountFirst(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timing-sensitive tmux test in -short; run make test-bossd for coverage")
	}
	fake := neverReadyFactory()
	c := NewClient(WithCommandFactory(fake.factory))
	err := c.sendPlan(context.Background(), "boss-test-sess", "plan body", sendPlanOpts{
		deadline:      50 * time.Millisecond,
		pollInterval:  retryPollInterval,
		readyAttempts: 3,
	})
	if err == nil {
		t.Fatal("expected the readiness wait to exhaust its attempts, got nil")
	}
	first := err.Error()
	if idx := strings.IndexByte(first, '\n'); idx != -1 {
		first = first[:idx]
	}
	if !strings.Contains(first, "after 3 attempts") {
		t.Errorf("first line does not name the attempt count: %q", first)
	}
	// The per-attempt budget stays visible too: it is the number an operator
	// changes in settings.json, and the retry must not hide it.
	if !strings.Contains(err.Error(), "within 50ms") {
		t.Errorf("exhaustion error no longer names the per-attempt budget: %v", err)
	}
	// And the pane the last attempt saw is still attached.
	if !strings.Contains(err.Error(), "still booting") {
		t.Errorf("exhaustion error dropped the pane snapshot: %v", err)
	}
}

// TestReadyRetry_SingleAttemptMessageIsUnchanged guards the send path, which
// stays at one attempt. Its timeout is what bosso surfaces to a chat, and it
// must read exactly as it did before the retry existed — no attempt count on a
// wait that was never retried.
func TestReadyRetry_SingleAttemptMessageIsUnchanged(t *testing.T) {
	t.Parallel()
	fake := neverReadyFactory()
	c := NewClient(WithCommandFactory(fake.factory))
	err := c.sendPlan(context.Background(), "boss-test-sess", "plan body", sendPlanOpts{
		deadline:      30 * time.Millisecond,
		pollInterval:  5 * time.Millisecond,
		readyAttempts: 1,
	})
	if err == nil {
		t.Fatal("expected a readiness timeout, got nil")
	}
	if !strings.HasPrefix(err.Error(), `ready marker "❯" not seen in pane "boss-test-sess" within 30ms`) {
		t.Errorf("single-attempt message changed shape: %v", err)
	}
	if strings.Contains(err.Error(), "attempts") {
		t.Errorf("single-attempt message mentions attempts: %v", err)
	}
}

// TestReadyRetry_UnspecifiedAttemptsMeansOne pins the floor. Every tmux test
// predating BOS-895 builds sendPlanOpts by hand and names no attempt count;
// defaulting the other way would silently double each of their wall clocks and
// change the errors they assert on.
func TestReadyRetry_UnspecifiedAttemptsMeansOne(t *testing.T) {
	t.Parallel()
	if got := resolveReadyAttemptsFloor(0); got != 1 {
		t.Errorf("zero attempts resolved to %d, want 1", got)
	}
	if got := resolveReadyAttemptsFloor(-3); got != 1 {
		t.Errorf("negative attempts resolved to %d, want 1", got)
	}
	if got := resolveReadyAttemptsFloor(4); got != 4 {
		t.Errorf("positive attempts was overridden: got %d", got)
	}
}

// TestReadyRetry_VanishedPaneStopsImmediately is the reason the retry does not
// simply double the worst case. A tmux session that is definitively gone cannot
// come back, so waiting out a second full budget for it is pure delay on the
// session-start path. The budget here is 5s and three attempts; the assertion
// is that the whole thing is over in a fraction of one.
func TestReadyRetry_VanishedPaneStopsImmediately(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timing-sensitive tmux test in -short; run make test-bossd for coverage")
	}
	// Every attempt count, because the shortcut has to hold in both directions:
	// on the retrying path it is what keeps the second budget from being pure
	// added latency, and on the single-attempt path it must not have changed
	// anything about how a one-shot wait behaves.
	for _, attempts := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("attempts=%d", attempts), func(t *testing.T) {
			zero := 0
			fake := &sendPlanRecordingFactory{
				failCapturePaneFrom: &zero,
				failWithStderr:      map[string]string{"has-session": defaultCaptureFailStderr},
			}
			c := NewClient(WithCommandFactory(fake.factory))
			started := time.Now()
			err := c.sendPlan(context.Background(), "boss-test-sess", retryPlanBody, sendPlanOpts{
				deadline:      5 * time.Second,
				pollInterval:  retryPollInterval,
				readyAttempts: attempts,
			})
			elapsed := time.Since(started)
			if err == nil {
				t.Fatal("expected a vanished-pane error, got nil")
			}
			var vanished *paneVanishedError
			if !errors.As(err, &vanished) {
				t.Fatalf("vanished pane reported as an ordinary timeout: %v", err)
			}
			if vanished.sessionName != "boss-test-sess" {
				t.Errorf("vanished error names session %q, want %q", vanished.sessionName, "boss-test-sess")
			}
			if !strings.Contains(err.Error(), "has-session reports no such session") {
				t.Errorf("vanished error does not say how it knows: %v", err)
			}
			// Roughly one poll interval, against a 5s budget per attempt.
			if elapsed > time.Second {
				t.Errorf("waited %v for a session tmux said was gone", elapsed)
			}
			assertNoDestructiveTmuxCalls(t, fake)
		})
	}
}

// TestReadyRetry_IndeterminateProbeKeepsWaiting is the conservative half of the
// same probe, and the one that protects a healthy session: a has-session that
// itself fails says nothing about whether the pane exists, so the wait must
// continue rather than treat a tmux hiccup as a dead session. The stub fails
// has-session with EMPTY stderr, which is exactly the shape HasSessionStatus
// cannot classify.
func TestReadyRetry_IndeterminateProbeKeepsWaiting(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timing-sensitive tmux test in -short; run make test-bossd for coverage")
	}
	zero := 0
	fake := &sendPlanRecordingFactory{
		failCapturePaneFrom: &zero,
		failWithStderr:      map[string]string{"has-session": ""},
	}
	c := NewClient(WithCommandFactory(fake.factory))
	started := time.Now()
	err := c.sendPlan(context.Background(), "boss-test-sess", "plan body", sendPlanOpts{
		deadline:      60 * time.Millisecond,
		pollInterval:  retryPollInterval,
		readyAttempts: 2,
	})
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("expected a readiness timeout, got nil")
	}
	var vanished *paneVanishedError
	if errors.As(err, &vanished) {
		t.Fatalf("an unclassifiable has-session failure was treated as proof the pane is gone: %v", err)
	}
	if !strings.Contains(err.Error(), "no capture-pane call succeeded") {
		t.Errorf("expected the never-captured diagnostic, got: %v", err)
	}
	// It waited out both budgets rather than bailing on the first failed probe.
	if elapsed < 2*60*time.Millisecond {
		t.Errorf("returned after %v, before both attempts had spent their budgets", elapsed)
	}
}

// TestReadyRetry_ShortContextStillReportsThePane covers the clamp. A context
// smaller than one attempt's budget must still produce the diagnostic error —
// pane snapshot, capture accounting — rather than a bare "context deadline
// exceeded", which tells an operator nothing about what the agent was showing.
func TestReadyRetry_ShortContextStillReportsThePane(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timing-sensitive tmux test in -short; run make test-bossd for coverage")
	}
	fake := neverReadyFactory()
	c := NewClient(WithCommandFactory(fake.factory))
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	err := c.sendPlan(ctx, "boss-test-sess", "plan body", sendPlanOpts{
		deadline:      10 * time.Second,
		pollInterval:  retryPollInterval,
		readyAttempts: 2,
	})
	if err == nil {
		t.Fatal("expected a readiness timeout, got nil")
	}
	var timeout *readyMarkerTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("short context produced a bare context error instead of a readiness verdict: %v", err)
	}
	if timeout.pane == "" {
		t.Errorf("readiness error carries no pane snapshot: %v", err)
	}
	if timeout.sessionName != "boss-test-sess" || timeout.marker != sendPlanReadyMarker {
		t.Errorf("readiness error identifies the wrong wait: session=%q marker=%q", timeout.sessionName, timeout.marker)
	}
	if timeout.deadline <= 0 || timeout.deadline >= 10*time.Second {
		t.Errorf("attempt deadline %v was not clamped to the context", timeout.deadline)
	}
}

// TestReadyRetry_CancelledContextKeepsAnEarlierPane is the same concern one
// layer out. A context cancelled part-way through a later attempt yields an
// error with no pane of its own, but the question it leaves the operator with —
// what was on screen? — was already answered earlier in the wait.
func TestReadyRetry_CancelledContextKeepsAnEarlierPane(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timing-sensitive tmux test in -short; run make test-bossd for coverage")
	}
	fake := neverReadyFactory()
	c := NewClient(WithCommandFactory(fake.factory))
	// WithCancel, not WithTimeout: a deadline would trip the remaining-budget
	// guard below and stop the wait before a later attempt ever started.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(120 * time.Millisecond)
		cancel()
	}()
	err := c.sendPlan(ctx, "boss-test-sess", "plan body", sendPlanOpts{
		deadline:      50 * time.Millisecond,
		pollInterval:  5 * time.Millisecond,
		readyAttempts: 8,
	})
	if err == nil {
		t.Fatal("expected the cancellation to surface, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation was not preserved: %v", err)
	}
	// The cancel can be observed by the poll loop itself or by the retry loop
	// between attempts, and which one a given run takes is a scheduling detail.
	// Both now converge on the same shape — that convergence is the point of
	// readyMarkerContextErr, and asserting it here is what keeps the two paths
	// from drifting apart again.
	var timeout *readyMarkerTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("cancellation lost the readiness verdict: %#v", err)
	}
	if !strings.Contains(err.Error(), "the caller's context ended first") {
		t.Errorf("cancelled wait did not name the context as the reason it stopped: %v", err)
	}
	if !strings.Contains(err.Error(), "still booting") {
		t.Errorf("cancelled wait dropped every pane an earlier attempt had captured: %v", err)
	}
}

// TestReadyRetry_NoAttemptStartsItCannotFinish covers the remaining-budget
// guard. Starting an attempt the context will cut short trades a diagnostic
// timeout for a bare context error, so the loop stops instead — and says so,
// because "1 of 3" is the operator's clue that the ceiling above them, not the
// readiness budget, is what needs raising.
func TestReadyRetry_NoAttemptStartsItCannotFinish(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timing-sensitive tmux test in -short; run make test-bossd for coverage")
	}
	fake := neverReadyFactory()
	c := NewClient(WithCommandFactory(fake.factory))
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := c.sendPlan(ctx, "boss-test-sess", "plan body", sendPlanOpts{
		deadline:      100 * time.Millisecond,
		pollInterval:  retryPollInterval,
		readyAttempts: 3,
	})
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("expected a readiness timeout, got nil")
	}
	if !strings.Contains(err.Error(), "after 1 of 3 attempts") {
		t.Errorf("error does not report that the remaining budget stopped the retry: %v", err)
	}
	// One attempt, not three: the guard must fire before the second starts.
	if elapsed > 200*time.Millisecond {
		t.Errorf("ran for %v — a second attempt was started with no budget to finish it", elapsed)
	}
}

// TestReadyRetry_ModalRefusalIsNotRetried keeps BOS-600's verdict a verdict. A
// modal is not a slow boot: re-running the wait would spend the whole budget
// re-confirming a refusal the first attempt already reached.
func TestReadyRetry_ModalRefusalIsNotRetried(t *testing.T) {
	t.Parallel()
	// The refusal has two independent sites, and a retry loop can only be shown
	// to leave both alone by exercising both. They differ in whether the pane
	// ever drew something marker-shaped: an interactive menu whose highlighted
	// row happens to carry the marker glyph is caught mid-poll, while a modal
	// that draws no marker row at all is only recognised at the deadline, from
	// the last captured pane.
	tests := []struct {
		name string
		pane string
	}{
		{
			name: "recognised mid-poll from a marker-bearing row",
			pane: "Do you want to proceed?\n❯ 1. Yes\n",
		},
		{
			name: "recognised at the deadline from a pane with no marker row",
			pane: "Do you want to proceed?\n  1. Yes\n  2. No\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &sendPlanRecordingFactory{capturePaneOutputs: []string{tt.pane}}
			c := NewClient(WithCommandFactory(fake.factory))
			// The budget is deliberately generous. Only the second case can be
			// decided at the deadline, so it always spends one whole budget;
			// the assertion below has to separate that from three of them, and
			// a budget short enough to keep the test snappy leaves that gap
			// narrower than the fixed overhead a -race build adds.
			const modalBudget = 200 * time.Millisecond
			started := time.Now()
			err := c.sendPlan(context.Background(), "boss-test-sess", retryPlanBody, sendPlanOpts{
				deadline:      modalBudget,
				pollInterval:  retryPollInterval,
				readyAttempts: 3,
				modalDetector: func(context.Context, []byte) (bool, error) { return true, nil },
			})
			elapsed := time.Since(started)
			if !errors.Is(err, ErrBlockedByModal) {
				t.Fatalf("expected a modal refusal, got %v", err)
			}
			if OutcomeOf(err) != OutcomeBlockedByModal {
				t.Errorf("modal refusal lost its outcome through the retry loop: %v", OutcomeOf(err))
			}
			// One attempt's worth at most: a retried refusal would spend three
			// whole budgets re-reaching the same verdict.
			if elapsed > 2*modalBudget {
				t.Errorf("took %v — the refusal was retried instead of returned", elapsed)
			}
			if strings.Contains(err.Error(), "attempts") {
				t.Errorf("refusal was wrapped in the retry loop's exhaustion message: %v", err)
			}
			assertNoDestructiveTmuxCalls(t, fake)
		})
	}
}

// TestReadyRetry_TimeoutStaysUnclassified pins the boundary between the two
// error vocabularies in this package. A readiness timeout means nothing was
// ever typed; tagging it with a SubmitOutcome would let a caller switching on
// OutcomeOf read a submission verdict out of a delivery that never reached the
// composer.
func TestReadyRetry_TimeoutStaysUnclassified(t *testing.T) {
	t.Parallel()
	fake := neverReadyFactory()
	c := NewClient(WithCommandFactory(fake.factory))
	for _, attempts := range []int{1, 2} {
		err := c.sendPlan(context.Background(), "boss-test-sess", "plan body", sendPlanOpts{
			deadline:      20 * time.Millisecond,
			pollInterval:  5 * time.Millisecond,
			readyAttempts: attempts,
		})
		if err == nil {
			t.Fatalf("attempts=%d: expected a readiness timeout, got nil", attempts)
		}
		if got := OutcomeOf(err); got != OutcomeUnclassified {
			t.Errorf("attempts=%d: readiness timeout classified as %v, want OutcomeUnclassified", attempts, got)
		}
	}
}

// TestReadyRetry_OnlyTheSessionStartPathRetries is the cross-leak assertion, in
// the same shape as the deadline pair BOS-893 added. The established-send path
// runs inside an RPC bosso relays under a 30s command deadline; a second wait
// there buys little and risks overrunning the relay.
func TestReadyRetry_OnlyTheSessionStartPathRetries(t *testing.T) {
	t.Parallel()
	c := NewClient()
	if got := c.startDeliveryOpts(sendPlanReadyMarker, true, nil).readyAttempts; got != sessionStartReadyAttempts {
		t.Errorf("startDeliveryOpts attempts = %d, want %d", got, sessionStartReadyAttempts)
	}
	if got := c.sendDeliveryOpts(sendPlanReadyMarker, true, nil).readyAttempts; got != sendReadyAttempts {
		t.Errorf("sendDeliveryOpts attempts = %d, want %d", got, sendReadyAttempts)
	}
	if sendReadyAttempts != 1 {
		t.Errorf("the established-send path must stay at one attempt, got %d", sendReadyAttempts)
	}
	if sessionStartReadyAttempts < 2 {
		t.Errorf("the session-start path must retry at least once, got %d attempts", sessionStartReadyAttempts)
	}
}

// TestReadyRetry_PublicWrappersCarryTheirOwnAttemptCount is the end-to-end half
// the builder table cannot reach: that the wiring survives all the way from a
// public entry point to the wait that actually runs. It reads the count back
// out of elapsed time and the error text, which is where it is observable.
func TestReadyRetry_PublicWrappersCarryTheirOwnAttemptCount(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timing-sensitive tmux test in -short; run make test-bossd for coverage")
	}
	// This budget cannot be shrunk to match the other timing tests in this
	// file, and the reason is the point of the test: these are the PUBLIC
	// wrappers, which take no pollInterval override, so the wait polls at the
	// production sendPlanDefaultPollInterval (100ms). A budget near that
	// interval makes an attempt's real duration a rounding-up to the next poll
	// plus three real subprocess spawns — at 120ms a single attempt naturally
	// runs ~200ms against a 240ms upper bound, which -race under load eats.
	// Several whole poll intervals is what separates "one attempt" from "two"
	// by a margin the machine cannot close.
	const budget = 500 * time.Millisecond

	t.Run("session start retries", func(t *testing.T) {
		c := NewClient(
			WithCommandFactory(neverReadyFactory().factory),
			WithSessionStartReadyDeadline(budget),
		)
		started := time.Now()
		err := c.SendPlanWithReadyMarker(context.Background(), "boss-test-sess", "plan body", sendPlanReadyMarker)
		elapsed := time.Since(started)
		if err == nil {
			t.Fatal("expected a readiness timeout, got nil")
		}
		if !strings.Contains(err.Error(), "after 2 attempts") {
			t.Errorf("session-start timeout does not report the retry: %v", err)
		}
		if elapsed < 2*budget {
			t.Errorf("returned after %v, before two %v budgets had elapsed", elapsed, budget)
		}
	})

	t.Run("established send does not", func(t *testing.T) {
		c := NewClient(
			WithCommandFactory(neverReadyFactory().factory),
			WithSendReadyDeadline(budget),
		)
		started := time.Now()
		err := c.SendMessage(context.Background(), "boss-test-sess", "hello", true, sendPlanReadyMarker)
		elapsed := time.Since(started)
		if err == nil {
			t.Fatal("expected a readiness timeout, got nil")
		}
		if strings.Contains(err.Error(), "attempts") {
			t.Errorf("send-path timeout mentions attempts, so the wait was retried: %v", err)
		}
		if elapsed >= 2*budget {
			t.Errorf("send path took %v — it spent more than its single %v budget", elapsed, budget)
		}
	})
}

// TestReadyRetry_TheRetriedRegionIssuesNoPaneMutation pins the confinement on
// the REGION rather than on its callers.
//
// Every other confinement test in this file drives sendPlan or sendLine and
// asserts that nothing was typed. That proves the property for the two callers
// that exist today, and says nothing about a third — or about a "cheap" nudge
// (send-keys Escape to dismiss an interstitial, say) added INSIDE the wait,
// where it would be inside the retried region without any caller-level test
// noticing. This drives waitForReadyMarkerWithAttempts directly and asserts a
// whitelist: across an exhausted multi-attempt wait, the only tmux subcommands
// the region may issue are the read-only ones it is allowed to issue.
//
// The whitelist, not a blacklist of the three known-mutating subcommands, is
// the point: a new tmux call added inside the wait fails this test by default
// and has to be justified by editing the list.
func TestReadyRetry_TheRetriedRegionIssuesNoPaneMutation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timing-sensitive tmux test in -short; run make test-bossd for coverage")
	}
	// Fail capture-pane from the first call so the region takes its liveness
	// branch too — has-session is the one non-capture call it legitimately
	// makes, and a whitelist that never saw it would not be pinning much.
	zero := 0
	fake := &sendPlanRecordingFactory{
		failCapturePaneFrom: &zero,
		failWithStderr:      map[string]string{"has-session": ""},
	}
	c := NewClient(WithCommandFactory(fake.factory))
	err := c.waitForReadyMarkerWithAttempts(context.Background(), "boss-test-sess", sendPlanOpts{
		deadline:      40 * time.Millisecond,
		pollInterval:  retryPollInterval,
		readyMarker:   sendPlanReadyMarker,
		readyAttempts: 3,
	})
	if err == nil {
		t.Fatal("expected the readiness wait to exhaust its attempts, got nil")
	}

	readOnly := map[string]bool{"capture-pane": true, "has-session": true}
	seen := map[string]bool{}
	for _, call := range fake.callsCopy() {
		if !readOnly[call.subcommand] {
			t.Errorf("the retried readiness region issued tmux %s %v — every call it makes must be read-only, "+
				"or the retry is no longer confined to the phase before the first keystroke",
				call.subcommand, call.args)
		}
		seen[call.subcommand] = true
	}
	// Guard against the assertion above passing vacuously.
	if !seen["capture-pane"] || !seen["has-session"] {
		t.Errorf("the wait made no capture-pane and/or no has-session call, so the whitelist proved nothing: %v", seen)
	}
}

// TestReadyRetry_SendLineNeverTypesWhileWaiting is the sendLine half of
// TestReadyRetry_NeverTypesWhileWaiting.
//
// sendLine is not a variant of sendPlan for this purpose: it delivers with
// send-keys -l rather than load-buffer/paste-buffer, it is what
// SendLineWithReadyMarker and PrefillLineWithReadyMarker call, and via
// startDeliveryOpts it inherits the SAME two-attempt production count. Its
// failures reach the operator under the `send command failed: ` prefix. Leaving
// it uncovered would mean half the callers of the property this change rests on
// were unpinned.
func TestReadyRetry_SendLineNeverTypesWhileWaiting(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timing-sensitive tmux test in -short; run make test-bossd for coverage")
	}
	fake := neverReadyFactory()
	c := NewClient(WithCommandFactory(fake.factory))
	err := c.sendLine(context.Background(), "boss-test-sess", "/status", sendPlanOpts{
		deadline:      50 * time.Millisecond,
		pollInterval:  retryPollInterval,
		readyAttempts: 3,
	})
	if err == nil {
		t.Fatal("expected the readiness wait to exhaust its attempts, got nil")
	}
	if !strings.Contains(err.Error(), "after 3 attempts") {
		t.Errorf("sendLine's exhaustion error does not report the retry: %v", err)
	}
	assertNoDestructiveTmuxCalls(t, fake)
}

// TestReadyRetry_SendLineLateComposerNeedsTheSecondAttempt mirrors the headline
// pair for the literal-keystroke path, and asserts the delivery happened
// exactly once on the attempt that succeeded.
func TestReadyRetry_SendLineLateComposerNeedsTheSecondAttempt(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timing-sensitive tmux test in -short; run make test-bossd for coverage")
	}
	tests := []struct {
		name     string
		attempts int
		wantErr  bool
	}{
		{name: "one attempt gives up before the composer appears", attempts: 1, wantErr: true},
		{name: "two attempts catch the composer on the second", attempts: 2, wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := lateComposerFactory()
			c := NewClient(WithCommandFactory(fake.factory))
			err := c.sendLine(context.Background(), "boss-test-sess", "/status", sendPlanOpts{
				deadline:      retryAttemptBudget,
				pollInterval:  retryPollInterval,
				readyAttempts: tt.attempts,
				prefillOnly:   true,
			})
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected the single attempt to expire before the composer appeared, got nil")
				}
				assertNoDestructiveTmuxCalls(t, fake)
				return
			}
			if err != nil {
				t.Fatalf("delivery failed even though the composer appeared during attempt 2: %v", err)
			}
			if n := literalSendKeysCount(fake); n != 1 {
				t.Errorf("send-keys -l ran %d times, want exactly 1 — the line was delivered more than once", n)
			}
		})
	}
}

// literalSendKeysCount reports how many recorded send-keys calls carried the
// literal flag, i.e. how many times sendLine actually typed its payload.
func literalSendKeysCount(fake *sendPlanRecordingFactory) int {
	n := 0
	for _, call := range fake.callsCopy() {
		if call.subcommand != "send-keys" {
			continue
		}
		for _, a := range call.args {
			if a == "-l" {
				n++
				break
			}
		}
	}
	return n
}

// TestReadyRetry_SendLineModalRefusalIsNotRetried mirrors the modal cases for
// the literal-keystroke path. A refusal is a verdict on either path, and the
// two paths reach the refusal through separate call sites.
func TestReadyRetry_SendLineModalRefusalIsNotRetried(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		pane string
	}{
		{
			name: "recognised mid-poll from a marker-bearing row",
			pane: "Do you want to proceed?\n❯ 1. Yes\n",
		},
		{
			name: "recognised at the deadline from a pane with no marker row",
			pane: "Do you want to proceed?\n  1. Yes\n  2. No\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &sendPlanRecordingFactory{capturePaneOutputs: []string{tt.pane}}
			c := NewClient(WithCommandFactory(fake.factory))
			const modalBudget = 200 * time.Millisecond
			started := time.Now()
			err := c.sendLine(context.Background(), "boss-test-sess", "/status", sendPlanOpts{
				deadline:      modalBudget,
				pollInterval:  retryPollInterval,
				readyAttempts: 3,
				modalDetector: func(context.Context, []byte) (bool, error) { return true, nil },
			})
			elapsed := time.Since(started)
			if !errors.Is(err, ErrBlockedByModal) {
				t.Fatalf("expected a modal refusal, got %v", err)
			}
			if elapsed > 2*modalBudget {
				t.Errorf("took %v — the refusal was retried instead of returned", elapsed)
			}
			if strings.Contains(err.Error(), "attempts") {
				t.Errorf("refusal was wrapped in the retry loop's exhaustion message: %v", err)
			}
			assertNoDestructiveTmuxCalls(t, fake)
		})
	}
}

// TestReadyRetry_SingleAttemptContextMessageIsHistoricallyExact covers the one
// message shape the single-attempt guard does NOT reach: a wait cut short by
// its CALLER's context rather than by its own budget.
//
// Reaching it takes a CANCELLED context rather than a short one. A context with
// a deadline is what clampAttemptDeadline exists for: it shrinks the attempt's
// own budget to fire first, so a short-context wait lands on the readiness
// timeout (pane snapshot and all) instead of here. Cancellation has no deadline
// to clamp against, so this is the branch that answers it.
//
// The assertion is EQUALITY, not a prefix check, and that is the point. The
// acceptance criterion for the established-send wrapper is that its returned
// error message is unchanged from what it was before the retry existed — and a
// prefix check passes just as happily against a message with a paragraph of new
// diagnostic appended to it, which is exactly what the first cut of this change
// shipped. Only equality can fail on that.
//
// The retried path's enrichment is pinned by its own sibling below; the two
// tests together say the enrichment exists AND is confined.
func TestReadyRetry_SingleAttemptContextMessageIsHistoricallyExact(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timing-sensitive tmux test in -short; run make test-bossd for coverage")
	}
	fake := neverReadyFactory()
	c := NewClient(WithCommandFactory(fake.factory))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	err := c.sendPlan(ctx, "boss-test-sess", "plan body", sendPlanOpts{
		deadline:      10 * time.Second,
		pollInterval:  retryPollInterval,
		readyAttempts: 1,
	})
	if err == nil {
		t.Fatal("expected the cancellation to cut the wait short, got nil")
	}
	const want = `wait for ready marker on "boss-test-sess": context canceled`
	if err.Error() != want {
		t.Errorf("single-attempt context message changed:\n got: %s\nwant: %s", err.Error(), want)
	}
	// errors.Is still answers the shutdown-vs-failure question.
	if !errors.Is(err, context.Canceled) {
		t.Errorf("context cause was lost: %v", err)
	}
	assertNoDestructiveTmuxCalls(t, fake)
}

// TestReadyRetry_RetriedContextErrorCarriesTheDiagnostic is the other half of
// the pair above: the confinement must not have thrown the diagnostic away, only
// moved it to the path that is allowed to have it.
//
// The session-start path is the one that runs unattended under cron, where the
// caller kills the tmux session as failure cleanup and this snapshot is often
// the only evidence anyone will ever have of what the agent was showing.
func TestReadyRetry_RetriedContextErrorCarriesTheDiagnostic(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timing-sensitive tmux test in -short; run make test-bossd for coverage")
	}
	fake := neverReadyFactory()
	c := NewClient(WithCommandFactory(fake.factory))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	err := c.sendPlan(ctx, "boss-test-sess", "plan body", sendPlanOpts{
		deadline:      10 * time.Second,
		pollInterval:  retryPollInterval,
		readyAttempts: 2,
	})
	if err == nil {
		t.Fatal("expected the cancellation to cut the wait short, got nil")
	}
	// The retried path wraps its inner error with the attempt accounting, so the
	// historical text is contained rather than leading. What matters is that the
	// enrichment APPENDS to that text rather than rewriting it: the same phrase
	// the frozen path returns whole is still in here, intact.
	if !strings.Contains(err.Error(), `wait for ready marker on "boss-test-sess": context canceled;`) {
		t.Errorf("retried context message lost the shared prefix: %v", err)
	}
	if !strings.Contains(err.Error(), "of 2 attempts") {
		t.Errorf("retried context message does not report the attempt accounting: %v", err)
	}
	if !strings.Contains(err.Error(), "capture-pane:") {
		t.Errorf("retried context error carries no capture accounting: %v", err)
	}
	if !strings.Contains(err.Error(), "still booting") {
		t.Errorf("retried context error carries no pane snapshot: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("context cause was lost: %v", err)
	}
	assertNoDestructiveTmuxCalls(t, fake)
}

// TestReadyMarkerRetryVerdictsAreCarriedByTheType pins the discriminator the
// retry loop reads, at the two constructors rather than through the loop.
//
// Going through the loop would prove only that today's two errors are handled
// today. The property worth keeping is narrower and longer-lived: whichever
// constructor mints a readiness timeout has to answer whether it may be re-run,
// and the two existing ones answer differently. A future third that copies the
// wrong neighbour fails here, at the mint, rather than in production as a
// second full budget spent on a pane that was never coming back.
func TestReadyMarkerRetryVerdictsAreCarriedByTheType(t *testing.T) {
	budget := readyMarkerTimeoutErr("boss-test-sess", "❯", time.Second, "pane", "msg")
	var asTimeout *readyMarkerTimeoutError
	if !errors.As(budget, &asTimeout) {
		t.Fatalf("budget expiry is not a readyMarkerTimeoutError: %T", budget)
	}
	if !asTimeout.mayRetryReadiness() {
		t.Error("the wait's own budget expiring must be retryable: another look is exactly what can fix it")
	}

	cut := readyMarkerContextErr("boss-test-sess", sendPlanOpts{readyMarker: "❯", deadline: time.Second},
		context.Canceled, 1, 0, time.Second, nil, "pane")
	var asCut *readyMarkerTimeoutError
	if !errors.As(cut, &asCut) {
		t.Fatalf("context cut is not a readyMarkerTimeoutError: %T", cut)
	}
	if asCut.mayRetryReadiness() {
		t.Error("a wait the caller's context cut short must never be retried: the budget that ran out is not ours to remint")
	}
}

// TestReadyRetry_ClampedBudgetNamesBothNumbers pins the one thing the clamp
// must not do quietly: present the budget it shortened as the budget that was
// configured.
//
// An operator reading "ready marker not seen within 290ms" against a 10s
// setting has two candidate faults with opposite fixes — the setting is too
// small, or something upstream imposed a ceiling under it — and the message as
// first shipped gave no way to tell them apart. It reported the clamped number
// in the slot where the configured one had always been.
func TestReadyRetry_ClampedBudgetNamesBothNumbers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timing-sensitive tmux test in -short; run make test-bossd for coverage")
	}
	fake := neverReadyFactory()
	c := NewClient(WithCommandFactory(fake.factory))
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := c.sendPlan(ctx, "boss-test-sess", "plan body", sendPlanOpts{
		deadline:      10 * time.Second,
		pollInterval:  retryPollInterval,
		readyAttempts: 2,
	})
	if err == nil {
		t.Fatal("expected the clamped attempt to time out, got nil")
	}
	if !strings.Contains(err.Error(), "shortened from 10s to stay inside the caller's context") {
		t.Errorf("clamped budget is reported as the configured one: %v", err)
	}
	assertNoDestructiveTmuxCalls(t, fake)
}

// TestReadyRetry_UnclampedBudgetSaysNothingAboutClamping is the negative half:
// the clause is evidence that something shortened this attempt, so it must be
// absent when nothing did. A clause that always appears carries no information.
func TestReadyRetry_UnclampedBudgetSaysNothingAboutClamping(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timing-sensitive tmux test in -short; run make test-bossd for coverage")
	}
	fake := neverReadyFactory()
	c := NewClient(WithCommandFactory(fake.factory))
	// No deadline on the context: there is nothing to clamp against.
	err := c.sendPlan(context.Background(), "boss-test-sess", "plan body", sendPlanOpts{
		deadline:      100 * time.Millisecond,
		pollInterval:  retryPollInterval,
		readyAttempts: 2,
	})
	if err == nil {
		t.Fatal("expected the readiness wait to time out, got nil")
	}
	if strings.Contains(err.Error(), "shortened from") {
		t.Errorf("unclamped attempt claims it was shortened: %v", err)
	}
	if !strings.Contains(err.Error(), "within 100ms") {
		t.Errorf("unclamped attempt does not report its own budget: %v", err)
	}
	assertNoDestructiveTmuxCalls(t, fake)
}

// TestWithSessionStartReadyAttempts covers the seam an out-of-package caller
// needs to pin what a doomed session start COSTS.
//
// That cost is a product — attempts × budget — and until this Option existed
// only the budget half was reachable from outside the package. A caller pinning
// the cost had to halve the budget to compensate for an attempt count it could
// not see, which silently stopped meaning what it said the moment either factor
// moved. The test harness is the caller that does this.
func TestWithSessionStartReadyAttempts(t *testing.T) {
	pinned := NewClient(WithSessionStartReadyAttempts(5))
	if got := pinned.startDeliveryOpts("❯", true, nil).readyAttempts; got != 5 {
		t.Errorf("session-start attempts not injected: got %d, want 5", got)
	}
	// The send path is a different budget for a different hazard; pinning one
	// must never move the other.
	if got := pinned.sendDeliveryOpts("❯", true, nil).readyAttempts; got != sendReadyAttempts {
		t.Errorf("send attempts moved with the session-start pin: got %d, want %d", got, sendReadyAttempts)
	}
	// Non-positive is ignored rather than stored, matching the deadline
	// Options: zero would mean "never wait", which no caller should be able to
	// express by accident.
	for _, n := range []int{0, -1} {
		c := NewClient(WithSessionStartReadyAttempts(n))
		if got := c.startDeliveryOpts("❯", true, nil).readyAttempts; got != sessionStartReadyAttempts {
			t.Errorf("WithSessionStartReadyAttempts(%d) was stored: got %d, want the package default %d",
				n, got, sessionStartReadyAttempts)
		}
	}
}
