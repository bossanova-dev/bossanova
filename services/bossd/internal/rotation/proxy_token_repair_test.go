package rotation

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/recurser/bossalib/config"
)

// TestOnProxyTokenUnresolvedRespawnsWithoutProbe is the core of BOS-982 on the
// rotation side: a 401 the failover proxy minted itself must reach
// respawn-in-place directly, with NO account probe and NO two-healthy-probes
// streak in between. Probing here could only ever answer "healthy" — the account
// was never consulted for that 401 — which is exactly how the pre-BOS-982 route
// stalled.
func TestOnProxyTokenUnresolvedRespawnsWithoutProbe(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{}
	store := &lockedAuditStore{}
	r := f.authRotator(&now, store)

	r.OnProxyTokenUnresolved(testChatID)
	waitIdle(t, r)

	if n := f.authProbes(); n != 0 {
		t.Fatalf("auth probes = %d, want 0 — a proxy-minted 401 must never probe the account", n)
	}
	calls := f.switched()
	if len(calls) != 1 {
		t.Fatalf("switch calls = %d, want exactly 1 (the respawn)", len(calls))
	}
	c := calls[0]
	if !c.RespawnSameAccount {
		t.Fatal("proxy-token repair must set RespawnSameAccount=true")
	}
	if c.AccountID != "acct-capped" {
		t.Fatalf("respawn bound account = %q, want the chat's currently bound account", c.AccountID)
	}
	if c.AgentSessionID != testChatID || c.SessionID != "sess-1" || !c.Auto {
		t.Fatalf("respawn Switch addressed the wrong pane: %+v", c)
	}
	if got := f.decides(); got != 0 {
		t.Fatalf("proxy-token repair must NOT consult Decide, got %d decides", got)
	}
	if out := store.outcomes(); len(out) != 1 || out[0] != "ROTATION_OUTCOME_RESPAWNED_SAME_ACCOUNT" {
		t.Fatalf("outcomes = %v, want exactly [RESPAWNED_SAME_ACCOUNT]", out)
	}
	// The audit trigger cannot say "proxy token" (it is a proto enum name and this
	// change adds no enum entry), so the detail is the only column that separates a
	// self-inflicted-401 repair from a genuine credential rotation. If it stops
	// carrying the marker the new lane becomes invisible to the operator. (BOS-982)
	if d := store.details(); len(d) != 1 || !strings.HasPrefix(d[0], proxyRepairDetailPrefix) {
		t.Fatalf("audit details = %v, want the proxy-token lane marker %q", d, proxyRepairDetailPrefix)
	}
}

// TestOnProxyTokenUnresolvedConcurrentWithAuthFailed pins AC6: concurrent
// arrival of the proxy path and the pane-scrape path for one chat results in
// EXACTLY ONE respawn attempt.
//
// The seeding edge below is load-bearing. The auth lane needs two consecutive
// healthy probes to reach a respawn, so from a cold rotator it can never reach a
// Switch at all — an "at most one" assertion would then be satisfied by zero
// respawns and would stay green even if the two lanes stopped sharing a
// reservation, or if the losing signal were simply dropped. Seeding the streak to
// threshold-1 makes BOTH lanes able to respawn, so "exactly one" is a real
// constraint on the collapse: two means the reservation stopped collapsing them,
// zero means the loser's signal was dropped without a remedy.
func TestOnProxyTokenUnresolvedConcurrentWithAuthFailed(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{authFailed: true, authResult: AuthProbeHealthy}
	store := &lockedAuditStore{}
	r := f.authRotator(&now, store)

	r.OnAuthFailed(testChatID)
	waitIdle(t, r)
	if calls := f.switched(); len(calls) != 0 {
		t.Fatalf("seeding edge already respawned (switch calls = %d); the streak fixture no longer matches the threshold", len(calls))
	}
	// Clears the 10m per-chat rate limit while staying inside the 30m healthy-streak
	// TTL, so the next auth edge is the one that reaches respawnInPlace.
	now = now.Add(11 * time.Minute)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); r.OnProxyTokenUnresolved(testChatID) }()
	go func() { defer wg.Done(); r.OnAuthFailed(testChatID) }()
	wg.Wait()
	waitIdle(t, r)

	if calls := f.switched(); len(calls) != 1 {
		t.Fatalf("switch calls = %d, want exactly 1 — concurrent lanes must collapse to one respawn attempt, and neither signal may be dropped without one", len(calls))
	}
}

// TestOnProxyTokenUnresolvedQueuedBehindAuthLane pins the other half of "exactly
// one": the losing signal is not simply discarded.
//
// The two lanes share one per-chat reservation, and the auth lane routinely
// finishes WITHOUT touching the pane (a below-threshold healthy probe is the
// common case). Dropping the proxy signal there would strand the wedged pane for
// a whole ChatRotateMinInterval — the stall BOS-982 exists to remove — so the
// signal is queued while the reservation is held and drained on release, unless
// the winning lane already acted on the pane.
func TestOnProxyTokenUnresolvedQueuedBehindAuthLane(t *testing.T) {
	t.Run("winner applied no remedy: the queued repair is drained", func(t *testing.T) {
		now := time.Now()
		// One healthy probe only: below the two-probe threshold, so the auth lane
		// releases the reservation having done nothing to the pane.
		f := &fakeDeps{authFailed: true, authResult: AuthProbeHealthy}
		store := &lockedAuditStore{}
		r := f.authRotator(&now, store)

		entered, release := f.gateAuthProbe()
		go r.OnAuthFailed(testChatID)
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("auth lane never reached AuthProbe; the reservation was never held")
		}

		r.OnProxyTokenUnresolved(testChatID)
		if calls := f.switched(); len(calls) != 0 {
			t.Fatalf("switch calls = %d while the auth lane still holds the reservation, want 0", len(calls))
		}
		close(release)
		waitIdle(t, r)

		calls := f.switched()
		if len(calls) != 1 {
			t.Fatalf("switch calls = %d, want exactly 1 — the queued proxy repair must be drained once the auth lane releases", len(calls))
		}
		if !calls[0].RespawnSameAccount || calls[0].AccountID != "acct-capped" {
			t.Fatalf("drained repair is not a same-account respawn of the bound pane: %+v", calls[0])
		}
		if n := f.authProbes(); n != 1 {
			t.Fatalf("auth probes = %d, want 1 (the auth lane's own) — the drained repair must not probe", n)
		}
		var sawProxyRow bool
		for _, d := range store.details() {
			if strings.HasPrefix(d, proxyRepairDetailPrefix) {
				sawProxyRow = true
			}
		}
		if !sawProxyRow {
			t.Fatalf("audit details = %v, want one carrying the proxy-token lane marker %q", store.details(), proxyRepairDetailPrefix)
		}
	})

	t.Run("winner already respawned: the queued repair is dropped", func(t *testing.T) {
		now := time.Now()
		f := &fakeDeps{authFailed: true, authResult: AuthProbeHealthy}
		store := &lockedAuditStore{}
		r := f.authRotator(&now, store)

		// Seed the streak so the gated edge below is the one that respawns.
		r.OnAuthFailed(testChatID)
		waitIdle(t, r)
		now = now.Add(11 * time.Minute)

		entered, release := f.gateAuthProbe()
		go r.OnAuthFailed(testChatID)
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("auth lane never reached AuthProbe; the reservation was never held")
		}
		r.OnProxyTokenUnresolved(testChatID)
		close(release)
		waitIdle(t, r)

		if calls := f.switched(); len(calls) != 1 {
			t.Fatalf("switch calls = %d, want exactly 1 — a queued repair behind a lane that already respawned is the double respawn the shared reservation exists to prevent", len(calls))
		}
		for _, d := range store.details() {
			if strings.HasPrefix(d, proxyRepairDetailPrefix) {
				t.Fatalf("audit details = %v: the respawn was the auth lane's, so no proxy-token row may be recorded", store.details())
			}
		}
	})
}

// TestOnProxyTokenUnresolvedRateLimited pins that a second repair for the same
// chat, while the first is still reserved, does not dispatch again. A wedged
// pane retries every 15s, so this is the steady-state case, not an edge one.
func TestOnProxyTokenUnresolvedRateLimited(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{}
	r := f.authRotator(&now, store())

	r.OnProxyTokenUnresolved(testChatID)
	r.OnProxyTokenUnresolved(testChatID)
	waitIdle(t, r)

	if calls := f.switched(); len(calls) != 1 {
		t.Fatalf("switch calls = %d, want 1 — a repeat repair for a live episode must not re-dispatch", len(calls))
	}
	if n := f.authProbes(); n != 0 {
		t.Fatalf("auth probes = %d, want 0", n)
	}
}

// TestOnProxyTokenUnresolvedGuards pins the fail-safe edges: an empty chat id is
// a no-op, and an opted-out repo leaves the pane alone rather than respawning it.
func TestOnProxyTokenUnresolvedGuards(t *testing.T) {
	t.Run("empty chat id", func(t *testing.T) {
		now := time.Now()
		f := &fakeDeps{}
		r := f.authRotator(&now, store())
		r.OnProxyTokenUnresolved("")
		waitIdle(t, r)
		if calls := f.switched(); len(calls) != 0 {
			t.Fatalf("switch calls = %d, want 0", len(calls))
		}
	})

	t.Run("auto-rotation opted out", func(t *testing.T) {
		now := time.Now()
		off := false
		f := &fakeDeps{cfg: config.ManagedAccountsConfig{AutoRotateChats: &off}}
		s := &lockedAuditStore{}
		r := f.authRotator(&now, s)
		r.OnProxyTokenUnresolved(testChatID)
		waitIdle(t, r)
		if calls := f.switched(); len(calls) != 0 {
			t.Fatalf("switch calls = %d, want 0 for an opted-out repo", len(calls))
		}
		if out := s.outcomes(); len(out) != 1 || out[0] != "ROTATION_OUTCOME_STATUS_ONLY_DISABLED" {
			t.Fatalf("outcomes = %v, want exactly [STATUS_ONLY_DISABLED]", out)
		}
		if d := s.details(); len(d) != 1 || !strings.HasPrefix(d[0], proxyRepairDetailPrefix) {
			t.Fatalf("audit details = %v, want the proxy-token lane marker %q", d, proxyRepairDetailPrefix)
		}
	})
}

// store is a tiny helper so the tests above that do not assert on audit rows
// still exercise the recorder path rather than the nil-recorder one.
func store() *lockedAuditStore { return &lockedAuditStore{} }

// TestOnProxyTokenUnresolvedIncompleteRespawnStaysOutOfTheProbeLane is the
// probe-skipping contract on the paths that do NOT reach a successful respawn.
//
// respawnInPlace was written for the auth lane, where an incomplete respawn is
// handed back to the re-probe timer; reprobeAuth runs rotateAuth, which is the
// AuthProbe + account-rotation lane this entry exists to bypass. The mid-turn
// abort is the likely outcome here, not an exotic one: the pane produced this
// 401 precisely because it was making an API request. Worse, a pane that reaches
// rotateAuth this way was typically never flagged auth-failed — no login banner
// has been scraped yet — so rotateAuth would take its recovered branch and record
// ROTATION_OUTCOME_STATUS_ONLY_RECOVERED for a pane that is still wedged.
func TestOnProxyTokenUnresolvedIncompleteRespawnStaysOutOfTheProbeLane(t *testing.T) {
	cases := []struct {
		name        string
		switchErr   error
		wantOutcome []string
	}{
		{
			name:        "chat mid-turn (switch aborted)",
			switchErr:   ErrSwitchAborted,
			wantOutcome: nil, // fail-safe: no audit row for a deliberate non-interruption
		},
		{
			name:        "switch failed",
			switchErr:   errors.New("tmux respawn failed"),
			wantOutcome: []string{"ROTATION_OUTCOME_FAILED"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			// authFailed is deliberately false: the pane never got a scraped banner,
			// so if this lane hands the chat to rotateAuth the recovered branch fires.
			f := &fakeDeps{switchErr: tc.switchErr, authResult: AuthProbeHealthy}
			store := &lockedAuditStore{}
			r := f.authRotator(&now, store)

			r.OnProxyTokenUnresolved(testChatID)
			waitIdle(t, r)

			if calls := f.switched(); len(calls) != 1 {
				t.Fatalf("switch calls = %d, want 1 (the attempted respawn)", len(calls))
			}
			// fire reports whether anything is still armed. An armed timer here IS the
			// bug: it runs reprobeAuth -> rotateAuth, probe included.
			if f.sched.fire() {
				t.Fatal("proxy-token repair armed an auth re-probe; that re-enters the probing lane this entry exists to skip")
			}
			waitIdle(t, r)
			if n := f.authProbes(); n != 0 {
				t.Fatalf("auth probes = %d, want 0 on every exit of the proxy-token lane", n)
			}
			got := store.outcomes()
			if len(got) != len(tc.wantOutcome) {
				t.Fatalf("outcomes = %v, want %v", got, tc.wantOutcome)
			}
			for i, want := range tc.wantOutcome {
				if got[i] != want {
					t.Fatalf("outcomes = %v, want %v", got, tc.wantOutcome)
				}
			}
			for _, d := range store.details() {
				if !strings.HasPrefix(d, proxyRepairDetailPrefix) {
					t.Fatalf("audit detail %q lacks the proxy-token lane marker %q", d, proxyRepairDetailPrefix)
				}
			}
			// The false-recovery row is what an accidental re-entry would leave behind.
			for _, o := range got {
				if o == "ROTATION_OUTCOME_STATUS_ONLY_RECOVERED" {
					t.Fatal("proxy-token repair recorded a RECOVERED row for a pane it never observed recovering")
				}
			}
		})
	}
}

// TestRespawnInPlaceAuthLaneStillReprobes is the other half of the lane split:
// the BOS-482 auth lane must keep re-arming its re-probe timer on an incomplete
// respawn, because there the timer IS the retry driver. Without this the fix
// above could satisfy its own test by disabling the re-probe for everyone.
func TestRespawnInPlaceAuthLaneStillReprobes(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{authFailed: true, authResult: AuthProbeHealthy, switchErr: ErrSwitchAborted}
	r := f.authRotator(&now, &lockedAuditStore{})

	// Two auth-failed edges reach the healthy-streak threshold and therefore
	// respawnInPlace; the rate limit is cleared between them by advancing the clock.
	r.OnAuthFailed(testChatID)
	waitIdle(t, r)
	now = now.Add(11 * time.Minute)
	r.OnAuthFailed(testChatID)
	waitIdle(t, r)

	if calls := f.switched(); len(calls) != 1 {
		t.Fatalf("switch calls = %d, want 1 (the threshold respawn)", len(calls))
	}
	if !f.sched.fire() {
		t.Fatal("the auth lane must still re-arm its re-probe after an aborted respawn")
	}
	waitIdle(t, r)
}

// TestDispatchProxyRepairHandsTheSignalToANewHolder pins that the drain is
// total: a queued proxy-token repair is never lost, only ever deferred.
//
// releaseInFlight MUST delete proxyRepairPending before it calls
// dispatchProxyRepair — it drops the lock in between, and a drained repair takes
// the in-flight guard for itself. That leaves a window in which another trigger
// claims the chat: dispatchProxyRepair then finds inFlight set, and the flag it
// was carrying exists nowhere, so the new holder's own release has nothing to
// drain. The new holder is very often a lane that touches no pane at all (a
// below-threshold healthy probe, an exhausted pool, a usage-limit rotation with
// no eligible account), so dropping the signal there strands the wedged pane
// with nothing scheduled — the round-1 defect in a narrower window.
//
// The reservation is taken directly rather than raced for: the window is a few
// instructions wide, so a scheduler-decided version of this test would assert
// nothing most runs.
func TestDispatchProxyRepairHandsTheSignalToANewHolder(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{}
	store := &lockedAuditStore{}
	r := f.authRotator(&now, store)

	// Stand in for the trigger that claimed the chat in the gap.
	r.mu.Lock()
	r.chatLocked(testChatID).inFlight = true
	r.mu.Unlock()

	r.dispatchProxyRepair(testChatID)
	waitIdle(t, r)
	if calls := f.switched(); len(calls) != 0 {
		t.Fatalf("switch calls = %d, want 0 — the drain must not run while another trigger holds the reservation", len(calls))
	}

	r.mu.Lock()
	requeued := r.chats[testChatID] != nil && r.chats[testChatID].proxyRepairPending
	r.mu.Unlock()
	if !requeued {
		t.Fatal("the drained signal was dropped: nothing holds it, so the new holder's release has nothing to drain and the wedged pane has nothing scheduled")
	}

	// The new holder finishes without touching the pane — the ordinary auth-lane
	// outcome — and the handed-back signal is what repairs the pane.
	r.releaseInFlight(testChatID)
	waitIdle(t, r)

	calls := f.switched()
	if len(calls) != 1 {
		t.Fatalf("switch calls = %d, want exactly 1 — the handed-back signal must be drained by the new holder's release", len(calls))
	}
	if !calls[0].RespawnSameAccount || calls[0].AccountID != "acct-capped" {
		t.Fatalf("drained repair is not a same-account respawn of the bound pane: %+v", calls[0])
	}
	if n := f.authProbes(); n != 0 {
		t.Fatalf("auth probes = %d, want 0 — the drained repair must not probe", n)
	}
}

// TestOnProxyTokenUnresolvedIncompleteRespawnReopensTheLane pins the backstop
// proxyTokenRespawnLane's retryNote promises — and the fact that the two
// pane-untouched outcomes reach it on DIFFERENT timelines.
//
// This lane arms no re-probe on purpose (that would re-enter the AuthProbe path
// it exists to skip), so its ONLY route back after an attempt that did not touch
// the pane is "the pane's next unresolved-token 401 re-enters this lane". A
// wedged pane produces those every few seconds — but OnProxyTokenUnresolved
// charged the rate limiter on the way in, and reserve()'s rate-limit branch
// refuses without queueing anything.
//
// A Switch that ERRORED releases that slot: the fault may not repeat, so the
// next 401 is worth letting straight back in, and without the release the one
// interleaving this branch was built for — repair attempted, pane untouched —
// degrades to zero repairs for a whole ChatRotateMinInterval.
//
// A Switch ABORTED because the chat is mid-turn does NOT, and that asymmetry is
// the point. A mid-turn pane stays mid-turn, so an immediate re-entry meets the
// same abort — while still charging the respawn cap, which chargeRespawn takes
// before the Switch. Releasing there spends the whole 2/hour cap inside a minute
// and leaves the pane with nothing at all for the rest of the window. Holding
// the slot makes the retry wait out ChatRotateMinInterval, which is the throttle
// the limiter exists to provide, and the retryNote still holds: the 401 after
// that window re-enters the lane.
func TestOnProxyTokenUnresolvedIncompleteRespawnReopensTheLane(t *testing.T) {
	for _, tc := range []struct {
		name string
		// switchErr is what the Switch seam returns on every attempt: a wedged pane
		// keeps failing the same way.
		switchErr error
		// wantInWindow is the cumulative respawn count after the pane's SECOND 401,
		// still inside the rate-limit window. wantAfterWindow is the count after a
		// third 401 delivered once the window has elapsed — where the respawn cap
		// (2 per hour, charged before every Switch) becomes the binding constraint
		// for whichever variant already spent it.
		wantInWindow    int
		wantAfterWindow int
	}{
		{"switch failed", errors.New("tmux respawn failed"), 2, 2},
		{"chat mid-turn (switch aborted)", ErrSwitchAborted, 1, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			f := &fakeDeps{switchErr: tc.switchErr}
			r := f.authRotator(&now, store())

			r.OnProxyTokenUnresolved(testChatID)
			waitIdle(t, r)
			if calls := f.switched(); len(calls) != 1 {
				t.Fatalf("switch calls = %d after the first 401, want 1", len(calls))
			}

			// The pane's next 401, well inside the 10m rate-limit window. The clock is
			// deliberately NOT advanced.
			r.OnProxyTokenUnresolved(testChatID)
			waitIdle(t, r)

			if calls := f.switched(); len(calls) != tc.wantInWindow {
				t.Fatalf("switch calls inside the rate-limit window = %d, want %d", len(calls), tc.wantInWindow)
			}
			// Once the window has elapsed the limiter no longer suppresses anything, so
			// the lane is reachable again — that is what retryNote promises. What each
			// variant gets there is decided by the respawn cap, and the contrast is the
			// whole finding: the mid-turn variant still has an attempt left because it
			// did not spend the cap racing its own 401s, while the released variant is
			// already at 2/2 and goes quiet for the rest of the hour.
			now = now.Add(11 * time.Minute)
			r.OnProxyTokenUnresolved(testChatID)
			waitIdle(t, r)
			if calls := f.switched(); len(calls) != tc.wantAfterWindow {
				t.Fatalf("switch calls after the window elapsed = %d, want %d", len(calls), tc.wantAfterWindow)
			}
			if n := f.authProbes(); n != 0 {
				t.Fatalf("auth probes = %d, want 0 on every exit of the proxy-token lane", n)
			}
		})
	}
}

// TestDrainedProxyRepairLeavesTheWinnersLimiterCharged pins the drain path's half
// of the limiter asymmetry.
//
// dispatchProxyRepair deliberately does NOT charge the rate limiter — the lane
// that won the reservation already did, for a signal that produced no remedy.
// The stamp sitting in lastAttempt therefore belongs to that lane, so the drained
// repair's error exits must not delete it: doing so silently un-charges the
// winner and lets the chat's next CHAT_STATUS_LIMITED or auth edge rotate
// immediately instead of waiting out ChatRotateMinInterval.
func TestDrainedProxyRepairLeavesTheWinnersLimiterCharged(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{chatCtxErr: errors.New("chat context unavailable")}
	r := f.authRotator(&now, store())

	// Stand in for the winning lane's charge.
	r.mu.Lock()
	r.chatLocked(testChatID).lastAttempt = now
	r.mu.Unlock()

	r.dispatchProxyRepair(testChatID)
	waitIdle(t, r)

	r.mu.Lock()
	stillCharged := !lastAttemptOf(r, testChatID).IsZero()
	r.mu.Unlock()
	if !stillCharged {
		t.Fatal("the drained repair cleared a rate-limit charge it never made; the winning lane's guard is now gone")
	}

	// The mirror: an entry that DID charge the limiter must release it, or the
	// pane's next 401 is refused for the whole window.
	f2 := &fakeDeps{chatCtxErr: errors.New("chat context unavailable")}
	r2 := f2.authRotator(&now, store())
	r2.OnProxyTokenUnresolved(testChatID)
	waitIdle(t, r2)
	r2.mu.Lock()
	held := !lastAttemptOf(r2, testChatID).IsZero()
	r2.mu.Unlock()
	if held {
		t.Fatal("a charged entry that failed before the chat context kept its rate-limit slot; the pane's next 401 is suppressed for the whole window")
	}
}

// TestProxyRepairDoesNotChainOnAStandingNoOp pins the drain path against a
// decision that cannot change.
//
// The drain deliberately charges no rate limit — the lane that won the
// reservation already charged it — so the limiter, which is what normally bounds
// a repeating no-op to one pass per window, is not in the loop here at all. That
// leaves nothing between a wedged pane and an unbounded cycle: repair runs, the
// pane's next 401 arrives mid-run and is queued, release drains it, the drained
// repair re-derives the same standing decision, its own release drains the next
// one, forever — one LoadConfig + ChatContext (and, for the opt-out, one
// STATUS_ONLY_DISABLED audit row) per pane retry, for as long as the pane stays
// wedged.
//
// The hook below fires exactly one such mid-run 401, so a chain shows up as a
// second entry into the repair rather than as a hang.
func TestProxyRepairDoesNotChainOnAStandingNoOp(t *testing.T) {
	off := false
	for _, tc := range []struct {
		name string
		deps func() *fakeDeps
	}{
		{"repo opted out of automatic rotation", func() *fakeDeps {
			return &fakeDeps{cfg: config.ManagedAccountsConfig{AutoRotateChats: &off}}
		}},
		{"chat has no bound account", func() *fakeDeps {
			return &fakeDeps{noBoundAccount: true}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			f := tc.deps()
			r := f.authRotator(&now, store())

			// The pane's next unresolved-token 401, delivered while the first repair still
			// holds the reservation: reserve() refuses it and queues it for the drain.
			f.chatCtxHook = func() { r.OnProxyTokenUnresolved(testChatID) }

			r.OnProxyTokenUnresolved(testChatID)
			waitIdle(t, r)

			f.mu.Lock()
			entries := f.chatCtxCalls
			f.mu.Unlock()
			if entries != 1 {
				t.Fatalf("repairProxyPane ran %d times, want 1 — a decision that reads the same on every pass must settle the reservation, not drain into a re-derivation of itself", entries)
			}
			if calls := f.switched(); len(calls) != 0 {
				t.Fatalf("switch calls = %d, want 0", len(calls))
			}
			if n := f.authProbes(); n != 0 {
				t.Fatalf("auth probes = %d, want 0 on every exit of the proxy-token lane", n)
			}
		})
	}
}

// seedAuthLaneState drives one auth-failed edge on a healthy account, which is
// what puts the BOS-482 auth lane's state on a chat: a consecutive-Healthy
// streak in healthy[] and an armed re-probe in reprobeCancel[]. It returns with
// the rotator idle and the clock advanced past the rate-limit window, so the
// caller's own trigger is not suppressed by this seeding.
//
// The streak is deliberately left BELOW the respawn threshold: at threshold the
// auth lane would respawn and clear its own state, which is not the fixture any
// caller here wants.
func seedAuthLaneState(t *testing.T, r *ChatRotator, f *fakeDeps, now *time.Time) {
	t.Helper()
	r.OnAuthFailed(testChatID)
	waitIdle(t, r)

	r.mu.Lock()
	cs := r.chatLocked(testChatID)
	streak := cs.healthy.count
	armed := cs.reprobeCancel != nil
	r.mu.Unlock()
	if streak == 0 || !armed {
		t.Fatalf("auth-lane fixture did not take: healthy streak = %d, reprobe armed = %v — the tests below would assert nothing", streak, armed)
	}
	if calls := f.switched(); len(calls) != 0 {
		t.Fatalf("seeding edge already respawned (switch calls = %d); the fixture no longer matches the threshold", len(calls))
	}
	*now = now.Add(11 * time.Minute)
}

// authLaneState reports the BOS-482 respawn state the auth lane owns.
func authLaneState(r *ChatRotator) (streak int, reprobeArmed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cs := r.lookupChatLocked(testChatID)
	if cs == nil {
		return 0, false
	}
	return cs.healthy.count, cs.reprobeCancel != nil
}

// lastAttemptOf reports the chat's reactive rate-limit stamp. The zero time is
// what an absent entry carries, which is the same "no charge stands" the old
// per-lane lastAttempt map expressed by having no key. Caller holds r.mu.
func lastAttemptOf(r *ChatRotator, agentSessionID string) time.Time {
	cs := r.lookupChatLocked(agentSessionID)
	if cs == nil {
		return time.Time{}
	}
	return cs.lastAttempt
}

// respawnCharges reports how much of the per-chat respawn cap is spent.
func respawnCharges(r *ChatRotator) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	cs := r.lookupChatLocked(testChatID)
	if cs == nil {
		return 0
	}
	return cs.respawns.count
}

// TestMidTurnProxyRepairDoesNotBurnTheRespawnCap is the mid-turn abort's own
// test, and it lives at the boundary the "exactly one respawn" tests are silent
// on.
//
// A pane that is mid-turn STAYS mid-turn: Switch keeps returning
// ErrSwitchAborted, and the proxy keeps minting unresolved-token 401s against it
// every few seconds. If that outcome hands the rate-limit slot back, each 401
// re-enters the lane immediately, so the pane is retried on its own 401 cadence
// rather than the limiter's.
//
// Two guards, not one. Holding the slot is the throttle; refundRespawn is what
// keeps the SHARED respawn budget intact, because Switch declined before touching
// the pane and a charge for a respawn that never happened is a slot the pane
// cannot spend once it finally goes idle.
//
// The second casualty is not in this lane at all. The cap-exhausted branch of
// respawnInPlace goes quiet by calling clearRespawnState, which deletes healthy[]
// and cancels reprobeCancel[] — state only the BOS-482 AUTH lane ever writes. So
// a proxy-lane cap burn also tears down the auth lane's healthy streak and its
// pending re-probe, silencing the slower backstop that was supposed to be behind
// this one.
//
// The loop below is the realistic cadence (a 401 every few seconds for two
// minutes), which is exactly the shape that walks past the cap edge.
func TestMidTurnProxyRepairDoesNotBurnTheRespawnCap(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{authFailed: true, authResult: AuthProbeHealthy, switchErr: ErrSwitchAborted}
	r := f.authRotator(&now, store())
	seedAuthLaneState(t, r, f, &now)

	// The wedged pane's 401 stream: one every four seconds for two minutes. All of
	// it sits inside both the 10m rate-limit window and the 1h respawn-cap window.
	for i := 0; i < 30; i++ {
		r.OnProxyTokenUnresolved(testChatID)
		waitIdle(t, r)
		now = now.Add(4 * time.Second)
	}

	if calls := f.switched(); len(calls) != 1 {
		t.Fatalf("switch calls = %d, want 1 — a mid-turn pane must be throttled by the rate limiter, not retried on its own 401 cadence", len(calls))
	}
	if spent := respawnCharges(r); spent != 0 {
		t.Fatalf("respawn cap spend = %d/%d after two minutes of 401s, want 0 — a mid-turn abort never touched the pane, so its charge must be refunded", spent, respawnCap)
	}
	streak, armed := authLaneState(r)
	if streak == 0 {
		t.Fatal("the proxy lane destroyed the auth lane's healthy streak; the BOS-482 backstop now starts from zero")
	}
	if !armed {
		t.Fatal("the proxy lane cancelled the auth lane's pending re-probe; the BOS-482 backstop is no longer scheduled at all")
	}
	if n := f.authProbes(); n != 1 {
		t.Fatalf("auth probes = %d, want 1 (the seeding edge only) — the proxy lane must never probe", n)
	}
}

// TestRespawnCapExhaustionIsLaneGated pins finding 1b directly: which lane may
// tear down the BOS-482 respawn state when the cap is exhausted.
//
// healthy[] and reprobeCancel[] are written only by the auth lane. On the
// cap-exhausted branch NOTHING happens to the pane, so a proxy-token lane that
// cleared them would cancel a re-probe and reset a streak it neither built nor
// invalidated — and the two lanes share one cap, so a wedged pane reaches that
// branch through the proxy lane in ordinary operation. The auth lane clearing its
// OWN state there is the BOS-482 behaviour and must survive this fix.
func TestRespawnCapExhaustionIsLaneGated(t *testing.T) {
	for _, tc := range []struct {
		name string
		// trigger drives the lane under test into respawnInPlace with the cap already
		// spent.
		trigger func(r *ChatRotator, now *time.Time)
		// wantCleared says whether the auth lane's state should be gone afterwards.
		wantCleared bool
	}{
		{
			name:        "proxy-token lane leaves the auth lane's state alone",
			trigger:     func(r *ChatRotator, _ *time.Time) { r.OnProxyTokenUnresolved(testChatID) },
			wantCleared: false,
		},
		{
			name: "auth lane still clears its own state",
			trigger: func(r *ChatRotator, _ *time.Time) {
				// A second auth-failed edge takes the streak to the threshold, which is
				// what reaches respawnInPlace on this lane.
				r.OnAuthFailed(testChatID)
			},
			wantCleared: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			f := &fakeDeps{authFailed: true, authResult: AuthProbeHealthy}
			s := store()
			r := f.authRotator(&now, s)
			seedAuthLaneState(t, r, f, &now)

			// Spend the shared per-chat respawn cap, so whichever lane arrives next
			// takes the cap-exhausted branch without touching the pane.
			r.mu.Lock()
			r.chatLocked(testChatID).respawns = respawnWindow{count: respawnCap, windowStart: now}
			r.mu.Unlock()

			tc.trigger(r, &now)
			waitIdle(t, r)

			if calls := f.switched(); len(calls) != 0 {
				t.Fatalf("switch calls = %d, want 0 — the cap-exhausted branch must not touch the pane", len(calls))
			}
			if out := s.outcomes(); len(out) == 0 || out[len(out)-1] != outcomeRespawnCapExhausted {
				t.Fatalf("outcomes = %v, want the last one to be %s; this test is not exercising the capped branch", out, outcomeRespawnCapExhausted)
			}
			streak, armed := authLaneState(r)
			if tc.wantCleared {
				if streak != 0 || armed {
					t.Fatalf("auth-lane cap exhaustion left streak = %d, reprobe armed = %v; BOS-482 requires it to go quiet", streak, armed)
				}
				return
			}
			if streak == 0 || !armed {
				t.Fatalf("proxy-lane cap exhaustion cleared state it does not own: streak = %d, reprobe armed = %v", streak, armed)
			}
		})
	}
}

// TestReleaseAttemptRestoresRatherThanDeletes pins finding 3: the reactive
// rate-limit stamp is SHARED by every lane, so giving a reservation back must
// undo that reservation's charge and nothing else.
//
// forgetAttempt — a bare delete(lastAttempt, id) — cannot express that. It wipes
// whatever the entry was carrying, so a proxy-token repair that released its slot
// would also un-suppress the auth and usage-limit lanes and let their next
// trigger rotate immediately, inside a window they had already spent.
//
// The unit-level drive is deliberate. reserve() refuses while a live stamp
// stands, so today a proxy reservation can only ever succeed on top of an absent
// entry — the restore is the invariant that keeps the release exact rather than
// merely correct-by-accident, and it is checkable only here.
func TestReleaseAttemptRestoresRatherThanDeletes(t *testing.T) {
	base := time.Now()
	other := base.Add(-2 * time.Minute) // another lane's still-live charge
	mine := base                        // what this reservation stamped

	for _, tc := range []struct {
		name    string
		seed    *time.Time // what lastAttempt holds when the release runs
		stamp   attemptStamp
		wantVal *time.Time // nil ⇒ no charge may stand
		// wantEntry says whether r.chats must still hold an entry for the chat.
		//
		// This is a SEPARATE claim from wantVal and both matter. lastAttemptOf returns
		// the zero time for an absent entry and for a present all-zero husk alike, so
		// asserting only on the stamp cannot tell "the entry was reclaimed" from "the
		// entry is still there carrying nothing" — which is the whole content of the
		// "do not resurrect it" case below, and of releaseAttempt's gcChatLocked call.
		wantEntry bool
	}{
		{
			// The entry now carries what an absent entry would, so releaseAttempt must
			// reclaim it rather than leave an all-zero husk behind.
			name:      "no prior entry: the lane's own charge is cleared",
			seed:      &mine,
			stamp:     attemptStamp{stamped: mine},
			wantVal:   nil,
			wantEntry: false,
		},
		{
			name:      "prior entry: another lane's suppression is put back, not dropped",
			seed:      &mine,
			stamp:     attemptStamp{stamped: mine, prior: other, hadPrior: true},
			wantVal:   &other,
			wantEntry: true,
		},
		{
			name:      "another writer moved the stamp: leave it alone",
			seed:      &base,
			stamp:     attemptStamp{stamped: mine.Add(-time.Hour)},
			wantVal:   &base,
			wantEntry: true,
		},
		{
			// "Resurrect" is a claim about the ENTRY, not about the charge: a release
			// that reads through a get-or-create materialises a chatState for a chat no
			// lane is tracking, which is exactly the unbounded growth gcChatLocked
			// exists to prevent — and it leaves the stamp zero either way.
			name:      "entry already gone: do not resurrect it",
			seed:      nil,
			stamp:     attemptStamp{stamped: mine, prior: other, hadPrior: true},
			wantVal:   nil,
			wantEntry: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := base
			f := &fakeDeps{}
			r := f.authRotator(&now, store())

			r.mu.Lock()
			if tc.seed != nil {
				r.chatLocked(testChatID).lastAttempt = *tc.seed
			}
			r.mu.Unlock()

			r.releaseAttempt(testChatID, tc.stamp)

			r.mu.Lock()
			got := lastAttemptOf(r, testChatID)
			_, haveEntry := r.chats[testChatID]
			r.mu.Unlock()
			if haveEntry != tc.wantEntry {
				if tc.wantEntry {
					t.Fatalf("r.chats entry for %s is gone; releasing one lane's charge must not destroy the entry", testChatID)
				}
				t.Fatalf("r.chats still holds an entry for %s; it carries nothing, so gcChatLocked must have reclaimed it (an all-zero husk is the unbounded growth, and it reads identically to an absent entry through lastAttemptOf)", testChatID)
			}
			// A zero stamp is what an absent entry carries: the lane holds no charge.
			if tc.wantVal == nil {
				if !got.IsZero() {
					t.Fatalf("lastAttempt = %v, want no charge held", got)
				}
				return
			}
			if got.IsZero() {
				t.Fatalf("lastAttempt charge was cleared; want it left at %v", *tc.wantVal)
			}
			if !got.Equal(*tc.wantVal) {
				t.Fatalf("lastAttempt = %v, want %v", got, *tc.wantVal)
			}
		})
	}
}

// TestProxyRepairReleaseClearsOnlyItsOwnCharge is the end-to-end half of finding
// 3: a real proxy-token repair that gives its slot back must leave lastAttempt
// exactly as it found it — here, absent, because reserve() only ever grants a
// reservation when nothing live is stamped.
//
// The lane's release runs on the Switch-error outcome, so the fixture makes the
// Switch fail rather than abort (an abort deliberately keeps the slot).
func TestProxyRepairReleaseClearsOnlyItsOwnCharge(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{switchErr: errors.New("tmux respawn failed")}
	r := f.authRotator(&now, store())

	r.OnProxyTokenUnresolved(testChatID)
	waitIdle(t, r)

	if calls := f.switched(); len(calls) != 1 {
		t.Fatalf("switch calls = %d, want 1", len(calls))
	}
	r.mu.Lock()
	got := lastAttemptOf(r, testChatID)
	r.mu.Unlock()
	if !got.IsZero() {
		t.Fatalf("lastAttempt = %v after a released reservation, want no entry — the map must read exactly as it did before the repair ran", got)
	}
}

// TestChatStateIsReclaimedAfterAFullLifecycle is the regression guard for the
// failure mode that folding the parallel per-chat maps into one chatState
// creates.
//
// Every lane used to own its own map, so a lane that forgot to delete its key
// leaked one entry in one map and nothing else could see it. Now the lanes share
// an entry, which is what makes a chat's lifecycle checkable in one place — but
// only if each lane zeroes its own field AND runs the reclaim, because deleting
// the shared entry outright would destroy the other lanes' state. A lane that
// zeroes without reclaiming leaves an all-zero husk behind, one per chat that
// ever hit a rotation trigger, forever: exactly the unbounded growth the
// stale-entry eviction comments in reserve and proactiveSuppressed promise does
// not happen.
//
// The chat here goes through the whole cycle — a reservation taken and released,
// a proxy-token repair queued behind it and drained, a respawn charged and its
// streak cleared, then deregistration — and the map must be empty at the end.
func TestChatStateIsReclaimedAfterAFullLifecycle(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{authFailed: true, authResult: AuthProbeHealthy}
	r := f.authRotator(&now, store())

	// Reservation taken and released, leaving the auth lane's state on the chat: a
	// below-threshold healthy streak, an armed re-probe, and a rate-limit charge.
	r.OnAuthFailed(testChatID)
	waitIdle(t, r)
	if streak, armed := authLaneState(r); streak == 0 || !armed {
		t.Fatalf("auth-lane fixture did not take: streak = %d, reprobe armed = %v", streak, armed)
	}
	r.mu.Lock()
	charged := !lastAttemptOf(r, testChatID).IsZero()
	r.mu.Unlock()
	if !charged {
		t.Fatal("the auth edge did not charge the rate limiter; this test is not exercising that field")
	}

	// Past the rate-limit window, so the reservation below runs the stale-entry
	// eviction over the charge above rather than being suppressed by it.
	now = now.Add(11 * time.Minute)

	// A proxy-token repair queued behind another trigger's reservation, then
	// drained by that trigger's release. The drain respawns the pane in place,
	// which charges the respawn window and clears the auth lane's streak.
	r.mu.Lock()
	r.chatLocked(testChatID).inFlight = true
	r.mu.Unlock()
	r.OnProxyTokenUnresolved(testChatID)
	r.mu.Lock()
	queued := r.chats[testChatID] != nil && r.chats[testChatID].proxyRepairPending
	r.mu.Unlock()
	if !queued {
		t.Fatal("the proxy repair was not queued behind the held reservation; the drain below asserts nothing")
	}
	r.releaseInFlight(testChatID)
	waitIdle(t, r)

	if calls := f.switched(); len(calls) != 1 || !calls[0].RespawnSameAccount {
		t.Fatalf("switch calls = %+v, want exactly one same-account respawn from the drained repair", calls)
	}
	if got := respawnCharges(r); got != 1 {
		t.Fatalf("respawn charges = %d, want 1; the respawn-cap field was never written", got)
	}
	if streak, armed := authLaneState(r); streak != 0 || armed {
		t.Fatalf("after a completed respawn: streak = %d, reprobe armed = %v, want 0/false", streak, armed)
	}
	// The respawn-cap window deliberately outlives the streak, so the entry is still
	// live here — the reclaim must be driven by the field going to zero, not by the
	// last lane to touch the chat.
	r.mu.Lock()
	_, stillTracked := r.chats[testChatID]
	r.mu.Unlock()
	if !stillTracked {
		t.Fatal("the chats entry was dropped while the respawn-cap window was still charged")
	}

	// Deregistration is the last lane to let go.
	r.Deregister(testChatID)

	r.mu.Lock()
	defer r.mu.Unlock()
	if n := len(r.chats); n != 0 {
		t.Fatalf("len(chats) = %d after a full chat lifecycle, want 0; state = %+v", n, r.chats[testChatID])
	}
}

// respawnWindowOf reports the chat's whole respawn-cap window, not just its count,
// so a test can tell "uncharged" from "charged but in a window that rolled over".
func respawnWindowOf(r *ChatRotator) respawnWindow {
	r.mu.Lock()
	defer r.mu.Unlock()
	cs := r.lookupChatLocked(testChatID)
	if cs == nil {
		return respawnWindow{}
	}
	return cs.respawns
}

// TestMidTurnAbortsNeverSpendTheRespawnCap pins refundRespawn: a chat that is
// mid-turn every single time the lane reaches it must still have its FULL
// respawn budget, however many times that happens.
//
// chargeRespawn runs BEFORE the Switch on purpose — a respawn that may have
// touched the pane has to count, or a pane that can never respawn loops (BOS-482).
// ErrSwitchAborted is the one outcome that provably did not reach the pane, and it
// is the likely one on this lane: the pane minted the 401 by making an API request,
// so it is mid-turn almost by construction. Without the refund each abort spends one
// of respawnCap, the budget is gone after two, and the pane cannot be repaired even
// once it goes idle — nor can the auth lane repair it, because handleHealthyAuthProbe
// converges on this same window.
//
// The loop runs well past respawnCap, one repair per rate-limit window, and the
// clock stays inside respawnCapWindow throughout so the test cannot pass by the
// window rolling over instead of by the refund.
func TestMidTurnAbortsNeverSpendTheRespawnCap(t *testing.T) {
	start := time.Now()
	now := start
	f := &fakeDeps{switchErr: ErrSwitchAborted}
	s := store()
	r := f.authRotator(&now, s)

	// One rate-limit window per repair plus a minute of slack: the abort path
	// deliberately HOLDS the limiter slot, so this is the pane's real retry cadence.
	// Derived from the configured interval rather than hard-coded, because a step
	// shorter than it makes reserve() refuse the next repair and the loop below then
	// reports a cap that is not the cause.
	step := f.cfg.ChatRotateMinInterval() + time.Minute

	const aborts = respawnCap + 2
	// The scenario needs `aborts` limiter windows to fit inside ONE respawnCapWindow,
	// or the window rolls and grants the budget the refund is supposed to be granting.
	// Checked up front so a config change is diagnosed here rather than as a confusing
	// mid-loop failure.
	if budget := time.Duration(aborts) * step; budget >= respawnCapWindow {
		t.Fatalf("%d aborts at %v apart span %v, which does not fit inside respawnCapWindow (%v) — this test can no longer distinguish the refund from a window rollover; shorten the scenario or revisit the constants",
			aborts, step, budget, respawnCapWindow)
	}
	for i := 0; i < aborts; i++ {
		r.OnProxyTokenUnresolved(testChatID)
		waitIdle(t, r)
		if calls := f.switched(); len(calls) != i+1 {
			t.Fatalf("abort %d: switch calls = %d, want %d — the repair never reached Switch: either the cap has closed the lane or the reactive rate limiter (%v, step %v) refused the entry",
				i, len(calls), i+1, f.cfg.ChatRotateMinInterval(), step)
		}
		if w := respawnWindowOf(r); w.count != 0 {
			t.Fatalf("abort %d: respawn cap spend = %d/%d, want 0 — a Switch that declined before touching the pane must not hold a slot", i, w.count, respawnCap)
		}
		now = now.Add(step)
	}

	if w := respawnWindowOf(r); w.count != 0 || !w.windowStart.IsZero() {
		t.Fatalf("respawn window after %d mid-turn aborts = %+v, want fully uncharged", aborts, w)
	}
	for _, o := range s.outcomes() {
		if o == outcomeRespawnCapExhausted {
			t.Fatalf("outcomes = %v — mid-turn aborts exhausted the cap", s.outcomes())
		}
	}

	// The pane finally goes idle. The whole point of not spending the cap on the
	// aborts is that a genuine repair is still available here.
	if elapsed := now.Sub(start); elapsed >= respawnCapWindow {
		t.Fatalf("test clock advanced %v, past respawnCapWindow (%v) — a fresh window would grant this repair even without the refund", elapsed, respawnCapWindow)
	}
	f.mu.Lock()
	f.switchErr = nil
	f.mu.Unlock()

	r.OnProxyTokenUnresolved(testChatID)
	waitIdle(t, r)

	if calls := f.switched(); len(calls) != aborts+1 {
		t.Fatalf("switch calls = %d, want %d — the repair that could actually land was refused", len(calls), aborts+1)
	}
	out := s.outcomes()
	if len(out) == 0 || out[len(out)-1] != outcomeRespawnedSameAccount {
		t.Fatalf("outcomes = %v, want the genuine repair to record %s", out, outcomeRespawnedSameAccount)
	}
	if n := f.authProbes(); n != 0 {
		t.Fatalf("auth probes = %d, want 0 on every exit of the proxy-token lane", n)
	}
}

// TestRefundRespawnIsExact drives refundRespawn directly, because its exactness rules
// are not reachable from the lane tests: those only ever refund from count == 1, which
// makes "decrement by one" and "reset to zero" indistinguishable and leaves the
// windowStart rule unobserved. Both divergences are reachable in production — within one
// respawnCapWindow a first repair can take a genuine Switch error (charge kept, count 1)
// and a second can charge to 2 before aborting mid-turn — and both are silent: a
// reset-to-zero hands back the earlier attempt's charge, and re-stamping windowStart
// extends an hour-long budget window.
//
// The two guard branches with no lane-level coverage are pinned here too: the
// rolled-over-window skip, and the absent/uncharged entry (reachable when Deregister
// zeroes cs.respawns while an abort is in flight — Deregister does not clear inFlight).
func TestRefundRespawnIsExact(t *testing.T) {
	t0 := time.Now()
	rolled := t0.Add(-2 * respawnCapWindow)

	for _, tc := range []struct {
		name        string
		seed        *respawnWindow // nil: no chats entry at all
		now         time.Time      // clock when the refund lands
		want        respawnWindow
		wantNoEntry bool
	}{
		{
			// The rule the lane tests cannot see: an earlier charge in the same window
			// is not this call's to give back.
			name: "a second charge in a live window gives back exactly one",
			seed: &respawnWindow{count: 2, windowStart: t0},
			now:  t0.Add(time.Minute),
			want: respawnWindow{count: 1, windowStart: t0},
		},
		{
			// windowStart must not move forward: a refund may not extend the window.
			// Clearing it alongside the LAST charge is behaviour-neutral (chargeRespawn
			// ignores windowStart at count 0) and is what keeps the entry reclaimable.
			name: "the last charge clears the window stamp with it",
			seed: &respawnWindow{count: 1, windowStart: t0},
			now:  t0.Add(time.Minute),
			want: respawnWindow{},
		},
		{
			// The window has to be LIVE for this row to reach the floor at all: a
			// zero windowStart reads as rolled over (now.Sub(zero) >= respawnCapWindow)
			// and the rollover guard would return first, leaving the floor untested.
			// A negative count is not cosmetic — chargeRespawn would then hand out more
			// than respawnCap for the rest of the window.
			name: "an uncharged live window is floored, not driven negative",
			seed: &respawnWindow{windowStart: t0},
			now:  t0.Add(time.Minute),
			want: respawnWindow{windowStart: t0},
		},
		{
			name: "a rolled-over window is skipped, not decremented",
			seed: &respawnWindow{count: 2, windowStart: rolled},
			now:  t0,
			want: respawnWindow{count: 2, windowStart: rolled},
		},
		{
			name:        "an absent entry is not resurrected",
			seed:        nil,
			now:         t0.Add(time.Minute),
			want:        respawnWindow{},
			wantNoEntry: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := tc.now
			f := &fakeDeps{}
			r := f.authRotator(&now, nil)
			if tc.seed != nil {
				r.mu.Lock()
				r.chatLocked(testChatID).respawns = *tc.seed
				r.mu.Unlock()
			}

			r.refundRespawn(testChatID)

			got := respawnWindowOf(r)
			if got.count != tc.want.count {
				t.Errorf("respawns.count = %d, want %d", got.count, tc.want.count)
			}
			if !got.windowStart.Equal(tc.want.windowStart) {
				t.Errorf("respawns.windowStart = %v, want %v (a refund must never move the window)", got.windowStart, tc.want.windowStart)
			}
			if tc.wantNoEntry {
				r.mu.Lock()
				n := len(r.chats)
				r.mu.Unlock()
				if n != 0 {
					t.Errorf("len(chats) = %d after refunding a chat with no entry, want 0", n)
				}
			}
		})
	}
}

// TestProxyLaneNotAttemptedRefundsWithoutRotatingOrProbing covers the branch BOS-981's
// machinery added to this lane's error path: a Switch refused BEFORE it touched the pane
// (ErrSwitchNotAttempted). The refund is unconditional and applies here too — nothing was
// consumed — but everything BOS-981 does next is auth-lane machinery this lane must not
// run (respawnLane.handlesNotAttempted is false here).
//
// The refusal used is the ineligible-bound-account one, deliberately: that is the shape
// on which the auth lane rotates to a different account, so it is the shape on which a
// lane-blind merge of BOS-981 into this lane would silently start rotating accounts for a
// 401 the proxy minted itself — the exact action this ticket exists to prevent. The
// assertions therefore pin the two halves separately: the account is untouched (no
// switch to a different account, no AuthProbe), and the retry stays paced by the
// reactive rate limiter rather than by a not-attempted budget that would park the chat.
func TestProxyLaneNotAttemptedRefundsWithoutRotatingOrProbing(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{
		authFailed:       true,
		authResult:       AuthProbeHealthy,
		fromLabel:        "agent.yuki@kamik.ai",
		respawnSwitchErr: disabledTargetRefusal(),
		// An eligible alternate account is configured ON PURPOSE. Without one,
		// rotateToEligibleAccount finds nothing to rotate to and the account-rotation
		// assertion below can never fail — it would read as a guard while pinning
		// nothing. With it, a lane-blind merge of BOS-981 into this lane produces a real
		// switch to acct-b, which is what the assertion catches.
		decision: Decision{Kind: DecisionSwitch, AccountID: "acct-b", Label: "acct-b"},
	}
	r := f.authRotator(&now, store())
	seedAuthLaneState(t, r, f, &now)
	probesAfterSeed := f.authProbes()
	switchesAfterSeed := len(f.switched())

	r.OnProxyTokenUnresolved(testChatID)
	waitIdle(t, r)

	if spent := respawnCharges(r); spent != 0 {
		t.Fatalf("respawn cap spend = %d/%d, want 0 — a switch refused before the pane was touched consumed nothing, so its charge must be refunded", spent, respawnCap)
	}
	if spent := notAttemptCharges(r); spent != 0 {
		t.Fatalf("not-attempt budget spend = %d, want 0 — that budget is the auth lane's replacement for the cap it refunds; this lane is paced by the reactive rate limiter and must not park a chat the limiter is already holding back", spent)
	}
	if n := f.authProbes(); n != probesAfterSeed {
		t.Fatalf("auth probes = %d, want %d (the seeding edge only) — an ineligible bound account must not make this lane probe", n, probesAfterSeed)
	}
	for _, c := range f.switched()[switchesAfterSeed:] {
		if !c.RespawnSameAccount {
			t.Fatalf("this lane rotated the account (%+v); an ineligible bound account is not a reason to rotate a 401 this proxy minted itself", c)
		}
	}
	streak, armed := authLaneState(r)
	if streak == 0 {
		t.Fatal("the proxy lane destroyed the auth lane's healthy streak on a not-attempted refusal")
	}
	if !armed {
		t.Fatal("the proxy lane cancelled the auth lane's pending re-probe on a not-attempted refusal")
	}
}
