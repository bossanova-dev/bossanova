package resume

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// newSeveredHarness is newHarness with the pane marker CLEARED. That is the
// whole point of the restart-sourced trigger (BOS-890): a stream cut by the
// daemon's own death leaves the pane parked mid-response with no banner, so
// there is no marker to see — not now and not on any later poll. Every test in
// this file therefore runs with marker=false, which means a regression that
// quietly re-routed the restart trigger through the banner gate would fail
// here rather than pass vacuously.
func newSeveredHarness() *harness {
	h := newHarness()
	h.marker = false
	return h
}

// TestResumeMessageNamesConnectionLossNotAStatusCode pins the delivered wording
// verbatim (BOS-890 criterion 5). The rest of the suite compares deliveries
// against the ResumeMessage constant, which is correct for those tests but
// makes the text itself unasserted — the constant could say anything at all and
// they would still pass. This is the one place the literal is checked, because
// the text is what a stalled agent actually acts on: it is the exact prompt
// verified by hand during the BOS-890 investigation (3/3 panes recovered), and
// the old "(5xx)" claim was simply false for both a connection-loss banner and
// a restart-severed stream.
func TestResumeMessageNamesConnectionLossNotAStatusCode(t *testing.T) {
	const want = "Transient API error detected (connection lost mid-response). " +
		"Resume the interrupted work: continue from committed state; do not restart completed work."
	if ResumeMessage != want {
		t.Fatalf("ResumeMessage = %q, want %q", ResumeMessage, want)
	}
}

// TestStreamSeveredArmsSettleWindowThenDeliversOnce is the core restart lane:
// marking a chat severed does NOT resume it, it starts the same settle-window
// cycle a banner would, and one attempt lands when the window elapses.
func TestStreamSeveredArmsSettleWindowThenDeliversOnce(t *testing.T) {
	h := newSeveredHarness()
	r := h.resumer()

	r.OnStreamSevered(testChatID)

	if got := h.deliveries(); len(got) != 0 {
		t.Fatalf("delivered immediately on marking; the settle window must come first: %v", got)
	}
	if got := h.delays(); len(got) != 1 || got[0] != DefaultSettleWindow {
		t.Fatalf("scheduled delays = %v, want [%v]", got, DefaultSettleWindow)
	}

	if !h.fire() {
		t.Fatal("no timer armed after OnStreamSevered")
	}
	got := h.deliveries()
	if len(got) != 1 {
		t.Fatalf("deliveries = %d, want exactly 1: %v", len(got), got)
	}
	if got[0] != ResumeMessage {
		t.Fatalf("delivered %q, want %q", got[0], ResumeMessage)
	}
}

// TestStreamSeveredDefersToAuthFailed proves the rotation lane still wins. A
// restart severs every stream at once, including panes whose credentials also
// expired, and typing a resume prompt into a pane rotation is about to respawn
// is noise. The cycle survives (bounded re-check) because rotation may hand
// back a chat that is still stalled.
func TestStreamSeveredDefersToAuthFailed(t *testing.T) {
	h := newSeveredHarness()
	h.set(func(h *harness) { h.auth = true })
	r := h.resumer()

	r.OnStreamSevered(testChatID)
	if !h.fire() {
		t.Fatal("no timer armed")
	}

	if got := h.deliveries(); len(got) != 0 {
		t.Fatalf("resumed an auth-failed chat: %v", got)
	}
	if h.armed() != 1 {
		t.Fatalf("armed timers = %d, want 1 (auth deference re-checks rather than abandoning)", h.armed())
	}
}

// TestStreamSeveredSkipsUnresumableSession proves a session a human ended stays
// ended. This gate is terminal by design, so the cycle must not re-arm either.
func TestStreamSeveredSkipsUnresumableSession(t *testing.T) {
	h := newSeveredHarness()
	h.set(func(h *harness) { h.resumable = false })
	r := h.resumer()

	r.OnStreamSevered(testChatID)
	if !h.fire() {
		t.Fatal("no timer armed")
	}

	if got := h.deliveries(); len(got) != 0 {
		t.Fatalf("resumed a non-resumable session: %v", got)
	}
	if h.armed() != 0 {
		t.Fatalf("armed timers = %d, want 0 (an archived/closed session never comes back)", h.armed())
	}
}

// TestStreamSeveredSkipsNonIdleChat proves a chat that is doing something else
// is left alone. WORKING here is the important shape for the restart lane: it
// is exactly what a pane looks like when it reconnected and resumed on its own.
func TestStreamSeveredSkipsNonIdleChat(t *testing.T) {
	h := newSeveredHarness()
	h.set(func(h *harness) { h.status = bossanovav1.ChatStatus_CHAT_STATUS_WORKING })
	r := h.resumer()

	r.OnStreamSevered(testChatID)
	if !h.fire() {
		t.Fatal("no timer armed")
	}

	if got := h.deliveries(); len(got) != 0 {
		t.Fatalf("resumed a non-idle chat: %v", got)
	}
}

// TestStreamSeveredSkipsUnknownChatState proves an unknown pane is never
// prompted. The tracker holds no fresh entry, so the daemon cannot say the chat
// is IDLE — or anything else about it — and a pane it has no current evidence
// about is left alone however convincing the severance record was. Note this is
// the STATUS half of the tracker: the restart lane's stalled predicate reads the
// liveness half (see TestStreamSeveredWithNoLivenessReadingIsNeverPrompted), and
// either one being unknown has to be enough on its own.
func TestStreamSeveredSkipsUnknownChatState(t *testing.T) {
	h := newSeveredHarness()
	h.set(func(h *harness) { h.stateOK = false })
	r := h.resumer()

	r.OnStreamSevered(testChatID)
	if !h.fire() {
		t.Fatal("no timer armed")
	}

	if got := h.deliveries(); len(got) != 0 {
		t.Fatalf("resumed a chat with no tracker evidence: %v", got)
	}
}

// TestStreamSeveredWithNoLivenessReadingIsNeverPrompted covers the fail-safe on
// the seam the restart lane actually gates on. No liveness reading means the
// poller has never looked at this pane at all — not that it looked and saw
// nothing — so there is no basis for claiming the chat is still stalled, and
// unknown reads as NOT stalled like every other uncertainty in this file.
//
// The fixture is the shape production can actually produce: main.go's
// ChatLiveness adapter synthesises ok=false from exactly `at.IsZero() &&
// !seeded`, so "no reading" is a ZERO stamp that is not seeded. Setting ok=false
// while leaving a nonzero stamp in place would be testing a triple the adapter
// cannot emit. On that reachable shape two guards in stillStalled independently
// refuse — the `ok` check and the zero-stamp check — so what this pins is the
// OUTCOME for a tracker holding no reading, not which of the two produced it.
// The `ok` branch stays regardless: it is a defensive check on an injected
// dependency seam, not on today's single adapter.
func TestStreamSeveredWithNoLivenessReadingIsNeverPrompted(t *testing.T) {
	h := newSeveredHarness()
	h.set(func(h *harness) {
		h.lastSubstantiveAt = time.Time{}
		h.livenessSeeded = false
		h.livenessOK = false
	})
	r := h.resumer()

	r.OnStreamSevered(testChatID)
	if !h.fire() {
		t.Fatal("no timer armed")
	}

	if got := h.deliveries(); len(got) != 0 {
		t.Fatalf("resumed a chat the poller has no liveness reading for: %v", got)
	}
	if h.armed() != 0 {
		t.Fatalf("armed timers = %d, want 0 (not-stalled ends the cycle)", h.armed())
	}
}

// TestStreamSeveredWaitsOutSettleRemainder proves the settle window is a real
// quiet requirement, not just an initial delay: a chat that produced output
// recently (but still before the severance mark) waits out the remainder
// without spending an attempt.
func TestStreamSeveredWaitsOutSettleRemainder(t *testing.T) {
	h := newSeveredHarness()
	r := h.resumer()

	r.OnStreamSevered(testChatID)
	// Output 30s ago: still older than the severance stamp (so the chat reads as
	// stalled) but well inside the 2m settle window.
	h.set(func(h *harness) { h.lastOutputAt = h.now.Add(-30 * time.Second) })

	if !h.fire() {
		t.Fatal("no timer armed")
	}
	if got := h.deliveries(); len(got) != 0 {
		t.Fatalf("delivered before the pane settled: %v", got)
	}
	last := h.lastTimer()
	if last == nil || last.delay != DefaultSettleWindow-30*time.Second {
		t.Fatalf("re-armed delay = %v, want the %v remainder", last, DefaultSettleWindow-30*time.Second)
	}
}

// TestSelfRecoveredChatAfterRestartIsNeverPrompted is BOS-890 criterion 4 and
// the single most important test in this file. A chat that reconnected on its
// own during the settle window — via the pane-token re-adoption sweep, an
// internal retry, or a human — has produced pane output SINCE the severance was
// marked, and that is the pane-independent evidence the restart lane gates on.
// It must end the cycle silently: prompting a chat that already recovered would
// inject a stray instruction into live work.
func TestSelfRecoveredChatAfterRestartIsNeverPrompted(t *testing.T) {
	h := newSeveredHarness()
	r := h.resumer()

	r.OnStreamSevered(testChatID)

	// The pane came back and printed something a minute into the settle window.
	// Substantive output, not a spinner frame, and a real observation rather than
	// the bootstrap seed — that combination is the only thing the restart lane
	// accepts as proof of recovery.
	h.set(func(h *harness) {
		h.now = h.now.Add(time.Minute)
		h.lastOutputAt = h.now
		h.lastSubstantiveAt = h.now
		h.livenessSeeded = false
	})
	// ...and then went quiet again long enough to satisfy the settle window, so
	// the ONLY thing standing between this chat and a prompt is the stalled
	// predicate itself.
	h.set(func(h *harness) { h.now = h.now.Add(DefaultSettleWindow) })

	if !h.fire() {
		t.Fatal("no timer armed")
	}
	if got := h.deliveries(); len(got) != 0 {
		t.Fatalf("prompted a chat that had already recovered: %v", got)
	}
	if h.armed() != 0 {
		t.Fatalf("armed timers = %d, want 0 (a recovered chat ends the cycle)", h.armed())
	}
}

// deliveryProducesOutput models the side effect a real delivery has and the
// harness would otherwise omit: a prompt that LANDS is typed into the pane, so
// the pane repaints with substantive content. The restart lane's stalled
// predicate reads exactly that stamp, so this is not decoration — it is the
// difference between the contract this lane has and the contract a harness with
// an inert Deliver appears to have.
//
// Time advances by a second first because the stamp is compared against the
// severance instant with !After: writing the same nanosecond back would read as
// "no output since", which is the one thing a landed prompt is not.
func deliveryProducesOutput(h *harness) {
	h.now = h.now.Add(time.Second)
	h.lastOutputAt = h.now
	h.lastSubstantiveAt = h.now
	h.livenessSeeded = false
}

// TestStreamSeveredStopsAfterOnePromptThatLands pins the restart lane's REAL
// terminal condition, which is not the attempt budget.
//
// The lane gates on "has this chat produced substantive output since the
// severance?", and a prompt that lands IS substantive output. So the delivery
// that succeeds is the very evidence that ends the cycle: one prompt, then the
// next rung sees a recovered chat and abandons silently. That is the desired
// behaviour — a chat that took the prompt and started working must not be
// nagged twice more on a ladder — but it is only visible in a test whose Deliver
// models the side effect. An earlier version of this file asserted three
// deliveries here, which production cannot produce when the prompt lands.
func TestStreamSeveredStopsAfterOnePromptThatLands(t *testing.T) {
	h := newSeveredHarness()
	h.set(func(h *harness) { h.onDeliver = deliveryProducesOutput })
	r := h.resumer()

	r.OnStreamSevered(testChatID)
	// Walk the FULL ladder's worth of timers: if the cycle wrongly continued, the
	// extra prompts would land here rather than going unobserved.
	for i := 0; i < MaxAttempts; i++ {
		if !h.fire() && i == 0 {
			t.Fatalf("no timer armed for attempt %d", i+1)
		}
	}

	if got := h.deliveries(); len(got) != 1 {
		t.Fatalf("deliveries = %d, want exactly 1 (a landed prompt is itself the output that ends the cycle): %v", len(got), got)
	}
	if h.armed() != 0 {
		t.Fatalf("armed timers = %d, want 0 (a recovered chat ends the cycle)", h.armed())
	}
}

// TestStreamSeveredWalksTheLadderWhenDeliveryFails is the other half of the same
// contract, and the case in which the shared attempt budget genuinely applies. A
// delivery that ERRORS types nothing into the pane, so the chat is still parked
// exactly where the severed stream left it and retrying is right. The ladder
// runs to MaxAttempts on the shared backoff rungs and then gives up loudly — the
// give-up Warn is the only escalation signal this lane has, so it is asserted
// rather than assumed.
func TestStreamSeveredWalksTheLadderWhenDeliveryFails(t *testing.T) {
	h := newSeveredHarness()
	h.set(func(h *harness) {
		h.deliverErr = errors.New("tmux send-keys failed")
		// The side effect is wired too, and deliberately NOT invoked by a failed
		// delivery: that asymmetry is the whole reason the ladder is reachable here
		// and unreachable in the test above.
		h.onDeliver = nil
	})
	h.logs = &bytes.Buffer{}
	r := h.resumer()

	r.OnStreamSevered(testChatID)
	for i := 0; i < MaxAttempts; i++ {
		if !h.fire() {
			t.Fatalf("no timer armed for attempt %d", i+1)
		}
	}

	if got := h.deliveries(); len(got) != MaxAttempts {
		t.Fatalf("deliveries = %d, want %d", len(got), MaxAttempts)
	}
	if h.armed() != 0 {
		t.Fatalf("armed timers = %d, want 0 after the budget is spent", h.armed())
	}
	want := []time.Duration{DefaultSettleWindow, DefaultBackoffBase << 1, DefaultBackoffBase << 2}
	got := h.delays()
	if len(got) != len(want) {
		t.Fatalf("scheduled delays = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("scheduled delays = %v, want %v", got, want)
		}
	}
	if logs := h.logs.String(); !strings.Contains(logs, "max attempts reached") {
		t.Fatalf("give-up escalation Warn missing from the resumer's logs: %s", logs)
	}
}

// TestSeededLivenessStampAfterSeveranceStillReadsAsStalled is the BOS-890
// regression that a last-output comparison cannot survive.
//
// The poller's Bootstrap seeds every restored pane's stamp with a synthetic
// `now - IdleThreshold - 1s`, and that bootstrap runs AFTER the severance is
// marked, with several unbounded startup sweeps in between (orphan advancement,
// headless resume, a display backfill under a 30s timeout). On any busy
// workspace that gap exceeds the ~6s margin, so the synthetic seed lands after
// the severance instant. Comparing raw stamps therefore reads EVERY severed chat
// as recovered and the whole lane no-ops — under exactly the load that makes a
// restart likely. The seeded flag is what makes the stamp's value irrelevant.
func TestSeededLivenessStampAfterSeveranceStillReadsAsStalled(t *testing.T) {
	h := newSeveredHarness()
	r := h.resumer()

	r.OnStreamSevered(testChatID)

	// Reproduce the production shape exactly. Bootstrap runs 30s after the
	// severance is marked and seeds BOTH tracker stamps with its `now -
	// IdleThreshold - 1s` placeholder — 6s before it ran, so 24s AFTER the
	// severance instant. Nothing else touches the pane, so the poller carries that
	// same placeholder forward while it ages, and by the time the attempt fires it
	// is older than the settle window (the gate below is satisfied) yet still
	// dated after severedAt. Gating on it reads "recovered" and abandons; gating
	// on the seeded flag reads "nothing observed" and delivers.
	seed := h.now.Add(30 * time.Second).Add(-6 * time.Second)
	h.set(func(h *harness) {
		h.now = h.now.Add(DefaultSettleWindow + 30*time.Second)
		h.lastSubstantiveAt = seed
		h.livenessSeeded = true
		h.lastOutputAt = seed
	})

	if !h.fire() {
		t.Fatal("no timer armed")
	}
	if got := h.deliveries(); len(got) != 1 {
		t.Fatalf("deliveries = %d, want 1: a bootstrap SEED dated after the severance is not evidence of recovery: %v", len(got), got)
	}
}

// TestSubstantiveOutputAfterSeveranceEndsTheCycle is the seeded case's mirror,
// and the reason the flag is not simply ignored: once the poller has observed
// something real, the stamp is authoritative again and a chat that produced
// output after the severance is left completely alone.
func TestSubstantiveOutputAfterSeveranceEndsTheCycle(t *testing.T) {
	h := newSeveredHarness()
	r := h.resumer()

	r.OnStreamSevered(testChatID)
	h.set(func(h *harness) {
		h.now = h.now.Add(DefaultSettleWindow)
		h.lastSubstantiveAt = h.now.Add(-90 * time.Second) // after severedAt, before now
		h.livenessSeeded = false
		h.lastOutputAt = h.lastSubstantiveAt
	})

	if !h.fire() {
		t.Fatal("no timer armed")
	}
	if got := h.deliveries(); len(got) != 0 {
		t.Fatalf("prompted a chat the poller had observed producing real output: %v", got)
	}
	if h.armed() != 0 {
		t.Fatalf("armed timers = %d, want 0 (a recovered chat ends the cycle)", h.armed())
	}
}

// TestSpinnerRepaintDoesNotEndTheRestartCycle pins the other half of the clock
// choice, at unit level: given a fresh raw last-output stamp and a stale
// substantive one, stillStalled must read the substantive stamp. Revert it to
// ChatState.LastOutputAt and this test fails, which is exactly the mutant it
// exists to catch — a lane gated on the raw stamp abandons the cycle on the
// first cosmetic frame, before any prompt is ever delivered.
//
// It is a pin on the STAMP CHOICE in isolation, not a model of end-to-end
// spinner behaviour, and the fixture below is deliberately not a production
// shape. The poller derives status from the same raw capture (see
// status/tmux_poller.go: `case captureChanged: status = WORKING`), so a pane
// that is genuinely repainting reads WORKING, not the IDLE this harness holds;
// in production such a chat fails the resumer's IDLE gate on every recheck and
// the cycle is abandoned after MaxGateRechecks rather than delivering. What
// this seam still buys is real: it is what stops a SINGLE stale spinner frame,
// or any other cosmetic redraw, from being mistaken for recovery on a pane the
// status gate does let through.
func TestSpinnerRepaintDoesNotEndTheRestartCycle(t *testing.T) {
	h := newSeveredHarness()
	r := h.resumer()

	r.OnStreamSevered(testChatID)
	h.set(func(h *harness) {
		h.now = h.now.Add(DefaultSettleWindow)
		// A spinner frame: the raw pane-change stamp jumps to the present, the
		// substantive stamp does not move, and the reading is a real observation
		// (not a seed) so this cannot pass by way of the seeded branch.
		h.lastOutputAt = h.now.Add(-time.Second)
		h.lastSubstantiveAt = h.now.Add(-DefaultSettleWindow - time.Hour)
		h.livenessSeeded = false
	})

	// The repaint does postpone the attempt — the quiet-pane half of the settle
	// window reads the raw stamp — but postponing is not abandoning, and that is
	// the distinction under test.
	if !h.fire() {
		t.Fatal("no timer armed")
	}
	if got := h.deliveries(); len(got) != 0 {
		t.Fatalf("delivered while the pane was still repainting: %v", got)
	}
	if h.armed() != 1 {
		t.Fatalf("armed timers = %d, want 1 (a spinner frame must not end the cycle)", h.armed())
	}

	// The spinner stops and the pane goes quiet for a full settle window.
	h.set(func(h *harness) { h.now = h.now.Add(DefaultSettleWindow) })
	if !h.fire() {
		t.Fatal("the cycle did not survive the spinner repaint")
	}
	if got := h.deliveries(); len(got) != 1 {
		t.Fatalf("deliveries = %d, want 1: a cosmetic spinner repaint must not read as recovery: %v", len(got), got)
	}
}

// TestSeveranceDoesNotStampACycleItDidNotArm pins the coupling between the
// stamp and the pending-slot claim. The stamp is what swaps a cycle's stalled
// predicate for the pane-independent one, so writing it onto a cycle this
// trigger did not arm loosens the gate for a chat that has real on-screen
// evidence — the wrong direction for a lane that must fail toward NOT resuming.
//
// The interleaving is a banner cycle claiming the slot first (the common case
// after a restart: some panes DO carry a banner) and the restart sweep arriving
// second. Stamping before arming would leave that window open; arming and
// stamping under one lock acquisition closes it. The banner's marker is cleared
// below so the two predicates disagree — without that, the test would pass
// whichever predicate ran.
func TestSeveranceDoesNotStampACycleItDidNotArm(t *testing.T) {
	h := newHarness() // marker set: a genuine banner cycle
	r := h.resumer()

	r.OnTransientAPIError(testChatID) // the banner claims the pending slot
	r.OnStreamSevered(testChatID)     // ...and the restart sweep loses the race

	// The banner scrolled away. A banner cycle must abandon here; a cycle wearing
	// a severance stamp would instead read the hour-old lastOutputAt as "still
	// stalled" and prompt.
	h.set(func(h *harness) { h.marker = false })

	if !h.fire() {
		t.Fatal("no timer armed")
	}
	if got := h.deliveries(); len(got) != 0 {
		t.Fatalf("a severance stamp leaked onto a banner cycle and resumed it: %v", got)
	}
	if h.armed() != 0 {
		t.Fatalf("armed timers = %d, want 0", h.armed())
	}
}

// TestRepeatedSeveranceArmsExactlyOneLadder pins the restart lane as one-shot
// per chat. MarkSevered dedupes its input, but the recorder is written by many
// goroutines and read back as a list, so "the same chat named twice" is a state
// the resumer must absorb rather than one the caller can be trusted to prevent.
// Two ladders for one chat would spend two attempt budgets and deliver up to
// 2*MaxAttempts prompts into a single pane.
func TestRepeatedSeveranceArmsExactlyOneLadder(t *testing.T) {
	h := newSeveredHarness()
	// Failing deliveries, because a landed prompt ends a restart cycle after one
	// attempt (see TestStreamSeveredStopsAfterOnePromptThatLands) and a one-prompt
	// cycle cannot show whether a second budget was opened. A pane nothing can be
	// typed into is the state in which the full ladder legitimately runs, so it is
	// also the only state in which a duplicated ladder is observable.
	h.set(func(h *harness) { h.deliverErr = errors.New("tmux send-keys failed") })
	r := h.resumer()

	r.OnStreamSevered(testChatID)
	r.OnStreamSevered(testChatID)
	r.OnStreamSevered(testChatID)

	if got := h.delays(); len(got) != 1 || got[0] != DefaultSettleWindow {
		t.Fatalf("scheduled delays = %v, want exactly one %v settle window", got, DefaultSettleWindow)
	}

	// Spend the whole ladder and confirm the budget was a single one.
	for i := 0; i < MaxAttempts; i++ {
		if !h.fire() {
			t.Fatalf("no timer armed for attempt %d", i+1)
		}
	}
	if got := h.deliveries(); len(got) != MaxAttempts {
		t.Fatalf("deliveries = %d, want %d (a duplicate severance opened a second ladder)", len(got), MaxAttempts)
	}
	if h.armed() != 0 {
		t.Fatalf("armed timers = %d, want 0 after the budget is spent", h.armed())
	}
}

// TestBannerCycleStillRequiresTheMarker is BOS-890 criterion 8 at the unit
// level: the new trigger must not have relaxed the gate for the OLD one. A
// banner-sourced cycle whose marker cleared before the attempt is abandoned
// exactly as it always was — the severance stamp is what unlocks the substitute
// predicate, and a chat that was never marked severed does not have one.
func TestBannerCycleStillRequiresTheMarker(t *testing.T) {
	h := newHarness() // marker set: a genuine banner cycle
	r := h.resumer()

	r.OnTransientAPIError(testChatID)
	// The banner scrolled away, but the pane produced no output the tracker saw
	// (lastOutputAt is still an hour old) — the exact state the restart lane's
	// substitute predicate would read as "still stalled". A banner cycle must
	// NOT get that reading.
	h.set(func(h *harness) { h.marker = false })

	if !h.fire() {
		t.Fatal("no timer armed")
	}
	if got := h.deliveries(); len(got) != 0 {
		t.Fatalf("banner cycle resumed a chat whose marker had cleared: %v", got)
	}
	if h.armed() != 0 {
		t.Fatalf("armed timers = %d, want 0", h.armed())
	}
}

// TestStreamSeveredDoesNotWeakenALiveBannerCycle proves the two triggers cannot
// be combined to loosen a gate. A chat that is already in a banner cycle when
// the severance mark arrives keeps the stricter marker predicate: marking must
// not double-arm (which would compress the backoff) and must not silently
// upgrade the live cycle to the pane-independent predicate.
func TestStreamSeveredDoesNotWeakenALiveBannerCycle(t *testing.T) {
	h := newHarness() // marker set
	r := h.resumer()

	r.OnTransientAPIError(testChatID)
	r.OnStreamSevered(testChatID)

	if got := h.delays(); len(got) != 1 {
		t.Fatalf("scheduled delays = %v, want exactly 1 (marking must not double-arm)", got)
	}

	h.set(func(h *harness) { h.marker = false })
	if !h.fire() {
		t.Fatal("no timer armed")
	}
	if got := h.deliveries(); len(got) != 0 {
		t.Fatalf("a banner cycle overlapped by a severance mark resumed on a cleared marker: %v", got)
	}
}

// TestStreamSeveredStampIsDroppedWhenTheCycleEnds proves the severance stamp is
// scoped to its own cycle. If it leaked, a chat marked severed once would keep
// the pane-independent predicate forever, so a LATER banner cycle for that same
// chat could resume on evidence the banner lane is not allowed to use.
func TestStreamSeveredStampIsDroppedWhenTheCycleEnds(t *testing.T) {
	h := newSeveredHarness()
	h.set(func(h *harness) { h.resumable = false }) // terminal gate: ends the cycle
	r := h.resumer()

	r.OnStreamSevered(testChatID)
	if !h.fire() {
		t.Fatal("no timer armed")
	}

	// The session becomes resumable again and a genuine banner arrives later.
	h.set(func(h *harness) {
		h.resumable = true
		h.marker = true
	})
	r.OnTransientAPIError(testChatID)
	// Marker clears before the attempt, with no pane output the tracker saw.
	h.set(func(h *harness) { h.marker = false })
	if !h.fire() {
		t.Fatal("no timer armed for the later banner cycle")
	}

	if got := h.deliveries(); len(got) != 0 {
		t.Fatalf("a stale severance stamp leaked into a later banner cycle: %v", got)
	}
}
