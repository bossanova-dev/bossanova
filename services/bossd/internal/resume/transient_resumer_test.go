package resume

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

const testChatID = "chat-1"

// fakeTimer is one armed callback recorded by the fake Schedule seam. Tests
// assert on delay and drive fn by hand so nothing in this file ever sleeps.
type fakeTimer struct {
	delay     time.Duration
	fn        func()
	cancelled bool
	fired     bool
}

// harness wires every TransientResumer seam to controllable state: a fake clock,
// a recording scheduler, and a recording deliverer. Everything is mutex-guarded
// so the suite is clean under -race even though the fake timers fire inline.
type harness struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer

	marker       bool
	auth         bool
	status       bossanovav1.ChatStatus
	lastOutputAt time.Time
	stateOK      bool
	resumable    bool

	delivered  []string
	deliverErr error
}

// newHarness returns a harness in the happy-path shape: marker set, chat IDLE,
// tracker fresh, session resumable, and last output far enough back that the
// settle window is already satisfied at fire time.
func newHarness() *harness {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	return &harness{
		now:          now,
		marker:       true,
		status:       bossanovav1.ChatStatus_CHAT_STATUS_IDLE,
		lastOutputAt: now.Add(-time.Hour),
		stateOK:      true,
		resumable:    true,
	}
}

func (h *harness) resumer() *TransientResumer {
	return NewTransientResumer(TransientResumerDeps{
		Logger: zerolog.Nop(),
		MarkerSet: func(string) bool {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.marker
		},
		AuthFailed: func(string) bool {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.auth
		},
		ChatState: func(string) (bossanovav1.ChatStatus, time.Time, bool) {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.status, h.lastOutputAt, h.stateOK
		},
		SessionResumable: func(context.Context, string) bool {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.resumable
		},
		Deliver: func(_ context.Context, _, message string) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.delivered = append(h.delivered, message)
			return h.deliverErr
		},
		Now: func() time.Time {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.now
		},
		Schedule: h.schedule,
	})
}

func (h *harness) schedule(d time.Duration, f func()) func() {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := &fakeTimer{delay: d, fn: f}
	h.timers = append(h.timers, t)
	return func() {
		h.mu.Lock()
		t.cancelled = true
		h.mu.Unlock()
	}
}

// delays returns every scheduled delay in arming order, cancelled ones included.
func (h *harness) delays() []time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]time.Duration, 0, len(h.timers))
	for _, t := range h.timers {
		out = append(out, t.delay)
	}
	return out
}

// armed counts timers that are neither cancelled nor already fired.
func (h *harness) armed() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, t := range h.timers {
		if !t.cancelled && !t.fired {
			n++
		}
	}
	return n
}

// fire runs the newest armed timer and reports whether one was found.
func (h *harness) fire() bool {
	h.mu.Lock()
	var target *fakeTimer
	for i := len(h.timers) - 1; i >= 0; i-- {
		if !h.timers[i].cancelled && !h.timers[i].fired {
			target = h.timers[i]
			break
		}
	}
	if target != nil {
		target.fired = true
	}
	h.mu.Unlock()
	if target == nil {
		return false
	}
	target.fn()
	return true
}

// lastTimer returns the most recently scheduled timer regardless of state.
func (h *harness) lastTimer() *fakeTimer {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.timers) == 0 {
		return nil
	}
	return h.timers[len(h.timers)-1]
}

func (h *harness) deliveries() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.delivered))
	copy(out, h.delivered)
	return out
}

func (h *harness) set(mutate func(h *harness)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	mutate(h)
}

func TestFirstAttemptWaitsSettleWindowThenDeliversOnce(t *testing.T) {
	h := newHarness()
	r := h.resumer()

	r.OnTransientAPIError(testChatID)

	if got := h.deliveries(); len(got) != 0 {
		t.Fatalf("delivered before the settle window elapsed: %v", got)
	}
	if got := h.delays(); len(got) != 1 || got[0] != DefaultSettleWindow {
		t.Fatalf("first scheduled delay = %v, want [%v]", got, DefaultSettleWindow)
	}

	if !h.fire() {
		t.Fatal("no timer armed after OnTransientAPIError")
	}
	got := h.deliveries()
	if len(got) != 1 {
		t.Fatalf("deliveries = %d, want exactly 1", len(got))
	}
	if got[0] != ResumeMessage {
		t.Fatalf("delivered message = %q, want %q", got[0], ResumeMessage)
	}
}

func TestBackoffScheduleIsBaseTimesTwoThenFour(t *testing.T) {
	h := newHarness()
	r := h.resumer()

	r.OnTransientAPIError(testChatID)
	if !h.fire() { // attempt 1
		t.Fatal("attempt 1 timer missing")
	}
	if !h.fire() { // attempt 2
		t.Fatal("attempt 2 timer missing")
	}

	want := []time.Duration{DefaultSettleWindow, 2 * DefaultBackoffBase, 4 * DefaultBackoffBase}
	got := h.delays()
	if len(got) != len(want) {
		t.Fatalf("scheduled delays = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("scheduled delays = %v, want %v", got, want)
		}
	}
}

func TestStopsAfterMaxAttempts(t *testing.T) {
	h := newHarness()
	r := h.resumer()

	r.OnTransientAPIError(testChatID)
	for i := 0; i < MaxAttempts; i++ {
		if !h.fire() {
			t.Fatalf("timer for attempt %d missing", i+1)
		}
	}
	if got := len(h.deliveries()); got != MaxAttempts {
		t.Fatalf("deliveries = %d, want %d", got, MaxAttempts)
	}
	if n := h.armed(); n != 0 {
		t.Fatalf("armed timers after the final attempt = %d, want 0", n)
	}
	if h.fire() {
		t.Fatal("a fourth attempt fired after MaxAttempts")
	}
	if got := len(h.deliveries()); got != MaxAttempts {
		t.Fatalf("deliveries after exhaustion = %d, want %d", got, MaxAttempts)
	}
}

// burnBudget spends every attempt in the chat's current budget.
func burnBudget(t *testing.T, h *harness, r *TransientResumer) {
	t.Helper()
	r.OnTransientAPIError(testChatID)
	for i := 0; i < MaxAttempts; i++ {
		if !h.fire() {
			t.Fatalf("timer for attempt %d missing", i+1)
		}
	}
	if got := len(h.deliveries()); got != MaxAttempts {
		t.Fatalf("deliveries = %d, want %d", got, MaxAttempts)
	}
}

// cycleMarker drives one clear→set transition of the pane marker, the edge pair
// the tracker hook delivers when the banner scrolls away and a new one lands.
func cycleMarker(h *harness, r *TransientResumer) {
	h.set(func(h *harness) { h.marker = false })
	r.OnTransientAPIError(testChatID)
	h.set(func(h *harness) { h.marker = true })
	r.OnTransientAPIError(testChatID)
}

// A marker clear is NOT evidence of recovery: delivering a resume prompt is
// itself pane output, so during a sustained outage the banner scrolls away
// seconds after every attempt. If that edge refilled the budget, MaxAttempts
// would bound nothing and the daemon would nag a wedged chat forever.
func TestMarkerClearWithinCooldownDoesNotRefillBudget(t *testing.T) {
	h := newHarness()
	r := h.resumer()

	burnBudget(t, h, r)

	// The banner scrolls out and straight back in, well inside the cooldown.
	h.set(func(h *harness) { h.now = h.now.Add(DefaultRefillCooldown - time.Minute) })
	cycleMarker(h, r)

	// Any timer that armed must find the budget already spent and deliver nothing.
	// Drain whatever armed; the assertion is below.
	for h.fire() {
	}
	if got := len(h.deliveries()); got != MaxAttempts {
		t.Fatalf("deliveries after an in-cooldown marker cycle = %d, want %d (budget must not refill)", got, MaxAttempts)
	}
}

// Elapsed quiet — and only elapsed quiet — earns a fresh budget, so a genuinely
// new incident hours later still gets the full ladder.
func TestBudgetRefillsAfterCooldownElapses(t *testing.T) {
	h := newHarness()
	r := h.resumer()

	burnBudget(t, h, r)

	h.set(func(h *harness) { h.now = h.now.Add(DefaultRefillCooldown + time.Minute) })
	cycleMarker(h, r)

	if !h.fire() {
		t.Fatal("no timer armed for the post-cooldown cycle")
	}
	if got := len(h.deliveries()); got != MaxAttempts+1 {
		t.Fatalf("deliveries after the cooldown elapsed = %d, want %d", got, MaxAttempts+1)
	}
}

// The cooldown is measured from the last CHARGED attempt, so a chat that never
// spent anything is never held back by it.
func TestUnchargedChatIsNeverHeldBackByCooldown(t *testing.T) {
	h := newHarness()
	r := h.resumer()

	cycleMarker(h, r)
	if !h.fire() {
		t.Fatal("no timer armed for an uncharged chat")
	}
	if got := len(h.deliveries()); got != 1 {
		t.Fatalf("deliveries = %d, want 1", got)
	}
}

func TestStatusSuppressesDelivery(t *testing.T) {
	cases := map[string]bossanovav1.ChatStatus{
		"working":  bossanovav1.ChatStatus_CHAT_STATUS_WORKING,
		"question": bossanovav1.ChatStatus_CHAT_STATUS_QUESTION,
		"limited":  bossanovav1.ChatStatus_CHAT_STATUS_LIMITED,
		"stopped":  bossanovav1.ChatStatus_CHAT_STATUS_STOPPED,
	}
	for name, st := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness()
			h.set(func(h *harness) { h.status = st })
			r := h.resumer()

			r.OnTransientAPIError(testChatID)
			if !h.fire() {
				t.Fatal("no timer armed")
			}
			if got := h.deliveries(); len(got) != 0 {
				t.Fatalf("delivered while %s: %v", name, got)
			}
			// A non-IDLE status is a poll-derived observation that can flap, so the
			// cycle re-checks rather than forfeiting — bounded by MaxGateRechecks.
			if n := h.armed(); n != 1 {
				t.Fatalf("armed timers after a status suppression = %d, want 1 re-check", n)
			}
			for i := 0; i < MaxGateRechecks; i++ {
				h.fire()
			}
			if got := h.deliveries(); len(got) != 0 {
				t.Fatalf("delivered while %s after re-checks: %v", name, got)
			}
			if n := h.armed(); n != 0 {
				t.Fatalf("armed timers after MaxGateRechecks re-checks = %d, want 0", n)
			}
		})
	}
}

// The tracker hook fires only on an EFFECTIVE marker flip, and the poller
// re-stamps the marker every tick while the banner is on screen — so a cycle
// that abandons on one unlucky status read has no way back, and a chat that
// reads WORKING for a beat would silently never be resumed.
func TestNonIdleAtFireTimeStillDeliversOnceTheChatGoesIdle(t *testing.T) {
	h := newHarness()
	h.set(func(h *harness) { h.status = bossanovav1.ChatStatus_CHAT_STATUS_WORKING })
	r := h.resumer()

	r.OnTransientAPIError(testChatID)
	if !h.fire() {
		t.Fatal("no timer armed")
	}
	if got := h.deliveries(); len(got) != 0 {
		t.Fatalf("delivered while WORKING: %v", got)
	}

	// The status flap passes; the banner is still on screen, so no marker flip
	// occurs and the re-check is the ONLY thing that can bring the cycle back.
	h.set(func(h *harness) { h.status = bossanovav1.ChatStatus_CHAT_STATUS_IDLE })
	if !h.fire() {
		t.Fatal("no re-check timer armed after the status suppression")
	}
	if got := h.deliveries(); len(got) != 1 {
		t.Fatalf("deliveries after the chat went idle = %d, want 1", len(got))
	}
}

// The re-check allowance is per cycle, not per attempt: a delivery resets it, so
// a flap before every attempt cannot stretch the cycle indefinitely.
func TestGateRecheckAllowanceResetsAfterADelivery(t *testing.T) {
	h := newHarness()
	h.set(func(h *harness) { h.stateOK = false })
	r := h.resumer()

	r.OnTransientAPIError(testChatID)
	if !h.fire() { // burns one re-check
		t.Fatal("no timer armed")
	}
	h.set(func(h *harness) { h.stateOK = true })
	if !h.fire() { // delivers, resetting the allowance
		t.Fatal("no re-check timer armed")
	}
	if got := len(h.deliveries()); got != 1 {
		t.Fatalf("deliveries = %d, want 1", got)
	}

	// The full allowance is available again for the next stall: MaxGateRechecks
	// re-arms, then the fire that exceeds it retires the cycle.
	h.set(func(h *harness) { h.stateOK = false })
	for i := 0; i <= MaxGateRechecks; i++ {
		if !h.fire() {
			t.Fatalf("re-check %d missing; allowance did not reset after the delivery", i+1)
		}
	}
	if n := h.armed(); n != 0 {
		t.Fatalf("armed timers after the refreshed allowance ran out = %d, want 0", n)
	}
}

func TestSuppressionsAtFireTime(t *testing.T) {
	cases := map[string]struct {
		mutate func(h *harness)
		// rechecks is how many further attempts the gate is allowed before the
		// cycle is abandoned: 0 for the terminal gates, MaxGateRechecks for the
		// ones that can plausibly flip back on their own.
		rechecks int
	}{
		"auth marker wins":      {func(h *harness) { h.auth = true }, MaxGateRechecks},
		"tracker not ok":        {func(h *harness) { h.stateOK = false }, MaxGateRechecks},
		"session not resumable": {func(h *harness) { h.resumable = false }, 0},
		"marker already clear":  {func(h *harness) { h.marker = false }, 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness()
			r := h.resumer()

			r.OnTransientAPIError(testChatID)
			h.set(tc.mutate) // the world changed between arming and firing
			if !h.fire() {
				t.Fatal("no timer armed")
			}
			for i := 0; i < tc.rechecks; i++ {
				if !h.fire() {
					t.Fatalf("re-check %d missing for %s", i+1, name)
				}
			}
			if got := h.deliveries(); len(got) != 0 {
				t.Fatalf("delivered despite %s: %v", name, got)
			}
			// Every gate must come to rest: no timer may outlive the allowance.
			if n := h.armed(); n != 0 {
				t.Fatalf("armed timers after %s ran out of re-checks = %d, want 0", name, n)
			}
		})
	}
}

func TestImmatureIdleReschedulesRemainingWindow(t *testing.T) {
	h := newHarness()
	r := h.resumer()

	// The pane went idle only 30s ago: 90s of the settle window remain.
	h.set(func(h *harness) { h.lastOutputAt = h.now.Add(-30 * time.Second) })

	r.OnTransientAPIError(testChatID)
	if !h.fire() {
		t.Fatal("no timer armed")
	}
	if got := h.deliveries(); len(got) != 0 {
		t.Fatalf("delivered before the pane settled: %v", got)
	}
	want := DefaultSettleWindow - 30*time.Second
	got := h.delays()
	if len(got) != 2 || got[1] != want {
		t.Fatalf("reschedule delays = %v, want second entry %v", got, want)
	}

	// Once the remainder elapses the attempt lands.
	h.set(func(h *harness) { h.now = h.now.Add(want) })
	if !h.fire() {
		t.Fatal("no reschedule timer armed")
	}
	if n := len(h.deliveries()); n != 1 {
		t.Fatalf("deliveries after the window matured = %d, want 1", n)
	}
}

func TestRepeatedTransitionsDoNotDoubleSchedule(t *testing.T) {
	h := newHarness()
	r := h.resumer()

	r.OnTransientAPIError(testChatID)
	r.OnTransientAPIError(testChatID)

	if got := h.delays(); len(got) != 1 {
		t.Fatalf("scheduled delays = %v, want exactly one timer", got)
	}
	if n := h.armed(); n != 1 {
		t.Fatalf("armed timers = %d, want 1", n)
	}
}

func TestDeregisterCancelsPendingTimer(t *testing.T) {
	h := newHarness()
	r := h.resumer()

	r.OnTransientAPIError(testChatID)
	timer := h.lastTimer()
	if timer == nil {
		t.Fatal("no timer armed")
	}

	r.Deregister(testChatID)

	h.mu.Lock()
	cancelled := timer.cancelled
	h.mu.Unlock()
	if !cancelled {
		t.Fatal("Deregister did not invoke the pending timer's cancel func")
	}
	// Even a timer that races the cancel and fires anyway must deliver nothing.
	timer.fn()
	if got := h.deliveries(); len(got) != 0 {
		t.Fatalf("delivered after Deregister: %v", got)
	}
}

func TestDeliverErrorStillChargesAttemptAndRetries(t *testing.T) {
	h := newHarness()
	h.set(func(h *harness) { h.deliverErr = errors.New("chat unreachable") })
	r := h.resumer()

	r.OnTransientAPIError(testChatID)
	if !h.fire() {
		t.Fatal("no timer armed")
	}
	if got := len(h.deliveries()); got != 1 {
		t.Fatalf("delivery attempts = %d, want 1", got)
	}
	got := h.delays()
	if len(got) != 2 || got[1] != 2*DefaultBackoffBase {
		t.Fatalf("delays after a failed delivery = %v, want retry at %v", got, 2*DefaultBackoffBase)
	}

	// The failed attempt was charged: only two more may run.
	h.fire()
	h.fire()
	if n := len(h.deliveries()); n != MaxAttempts {
		t.Fatalf("total delivery attempts = %d, want %d", n, MaxAttempts)
	}
	if h.fire() {
		t.Fatal("a fourth attempt fired after a failed first delivery")
	}
}

func TestNilSeamsAreInert(t *testing.T) {
	var scheduled int
	r := NewTransientResumer(TransientResumerDeps{
		Logger: zerolog.Nop(),
		Schedule: func(time.Duration, func()) func() {
			scheduled++
			return func() {}
		},
	})

	r.OnTransientAPIError(testChatID)
	r.Deregister(testChatID)

	if scheduled != 0 {
		t.Fatalf("scheduled %d timers with nil seams, want 0", scheduled)
	}
}

func TestPartialNilSeamsDoNotPanicAtFireTime(t *testing.T) {
	h := newHarness()
	// Deliver is missing: the resumer must reach the fire path and bail out
	// rather than panic, so a daemon without the capability stays healthy.
	r := NewTransientResumer(TransientResumerDeps{
		Logger:    zerolog.Nop(),
		MarkerSet: func(string) bool { return true },
		ChatState: func(string) (bossanovav1.ChatStatus, time.Time, bool) {
			return bossanovav1.ChatStatus_CHAT_STATUS_IDLE, h.now.Add(-time.Hour), true
		},
		SessionResumable: func(context.Context, string) bool { return true },
		Now:              func() time.Time { return h.now },
		Schedule:         h.schedule,
	})

	r.OnTransientAPIError(testChatID)
	h.fire()
	if got := h.deliveries(); len(got) != 0 {
		t.Fatalf("delivered without a Deliver seam: %v", got)
	}
}

func TestDefaultsAreApplied(t *testing.T) {
	r := NewTransientResumer(TransientResumerDeps{Logger: zerolog.Nop()})
	if r == nil {
		t.Fatal("NewTransientResumer returned nil")
	}
	// A zero-valued Schedule/Now must be filled in so the resumer never
	// dereferences a nil func on a live daemon.
	if got := r.settleWindow(); got != DefaultSettleWindow {
		t.Fatalf("settleWindow = %v, want %v", got, DefaultSettleWindow)
	}
	if got := r.backoffBase(); got != DefaultBackoffBase {
		t.Fatalf("backoffBase = %v, want %v", got, DefaultBackoffBase)
	}
}
