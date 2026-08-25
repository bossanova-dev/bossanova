package rotation_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/config"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/rotation"
	"github.com/recurser/bossd/internal/status"
)

// TestChatRotator_TrackerTransitionDrivesRotation mirrors the main.go wiring: a
// real status.Tracker → chained on-update hook → ChatRotator, with fakes only at
// the decide/switch seams. It proves the edge-triggered trigger end to end: a
// WORKING chat is never rotated, a transition to LIMITED drives exactly one
// rotation, and a same-status redraw does not re-fire the tracker hook.
//
// It lives in the external rotation_test package because status → db → rotation,
// so a white-box (package rotation) test importing status would form an import
// cycle; a black-box test breaks it.
func TestChatRotator_TrackerTransitionDrivesRotation(t *testing.T) {
	tracker := status.NewTracker()

	var mu sync.Mutex
	var switches []rotation.SwitchRequest
	rotator := rotation.NewChatRotator(rotation.ChatRotatorDeps{
		Logger:     zerolog.Nop(),
		LoadConfig: func() (config.ManagedAccountsConfig, error) { return config.ManagedAccountsConfig{}, nil },
		ChatContext: func(_ context.Context, _ string) (rotation.ChatContext, error) {
			return rotation.ChatContext{SessionID: "sess-1", RepoID: "repo-1", Provider: "claude", AccountID: "acct-capped"}, nil
		},
		CurrentStatus: func(id string) bossanovav1.ChatStatus {
			if e := tracker.Get(id); e != nil {
				return e.Status
			}
			return bossanovav1.ChatStatus_CHAT_STATUS_UNSPECIFIED
		},
		RateLimitProbe: func(context.Context, string) (models.UsageSnapshot, error) {
			fetched := time.Now().UTC()
			reset := fetched.Add(2 * time.Hour)
			return models.UsageSnapshot{
				Util5h:    1,
				Reset5h:   &reset,
				Status:    "RATE_LIMIT_PLAN_STATUS_RATE_LIMITED",
				FetchedAt: &fetched,
			}, nil
		},
		Decide: func(_ context.Context, _ rotation.DecideRequest) (rotation.Decision, error) {
			return rotation.Decision{Kind: rotation.DecisionSwitch, AccountID: "acct-b-id"}, nil
		},
		Switch: func(_ context.Context, req rotation.SwitchRequest) (rotation.SwitchResult, error) {
			mu.Lock()
			defer mu.Unlock()
			switches = append(switches, req)
			return rotation.SwitchResult{SwitchedToLabel: "acct-b"}, nil
		},
	})
	tracker.SetOnUpdate(func(agentSessionID string) {
		if e := tracker.Get(agentSessionID); e != nil {
			rotator.OnChatStatus(agentSessionID, e.Status, e.ResetAt)
		}
	})

	// WORKING → no rotation (false-positive regression guard).
	tracker.Update("chat-1", bossanovav1.ChatStatus_CHAT_STATUS_WORKING, time.Now())
	// Transition to LIMITED → exactly one rotation.
	tracker.UpdateLimited("chat-1", time.Time{}, time.Now())
	// Same-status redraw → tracker hook does not re-fire (edge trigger).
	tracker.UpdateLimited("chat-1", time.Time{}, time.Now())

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(switches)
		mu.Unlock()
		if n >= 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Settle: let any (erroneous) second dispatch land before asserting exactly one.
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(switches) != 1 {
		t.Fatalf("switches = %d, want exactly 1", len(switches))
	}
	if switches[0].AgentSessionID != "chat-1" || !switches[0].Auto {
		t.Fatalf("unexpected switch: %+v", switches[0])
	}
}

// authEpisodeScheduler is a deterministic stand-in for the ChatRotator's Schedule
// seam. It records the callbacks the rotator arms (never running them inline) so a
// test can fire the pending re-probe on demand instead of waiting five real minutes.
type authEpisodeScheduler struct {
	mu      sync.Mutex
	pending []*armedCall
}

type armedCall struct {
	fn        func()
	cancelled bool
}

func (s *authEpisodeScheduler) schedule(_ time.Duration, f func()) (cancel func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	call := &armedCall{fn: f}
	s.pending = append(s.pending, call)
	return func() {
		s.mu.Lock()
		call.cancelled = true
		s.mu.Unlock()
	}
}

// fire runs the most recently armed callback that has not been cancelled, marking it
// cancelled so it fires at most once. Returns false when nothing is armed — which is
// itself the assertion that matters here: an episode the rotator ended has no timer.
func (s *authEpisodeScheduler) fire() bool {
	s.mu.Lock()
	var call *armedCall
	for i := len(s.pending) - 1; i >= 0; i-- {
		if !s.pending[i].cancelled {
			call = s.pending[i]
			call.cancelled = true
			break
		}
	}
	s.mu.Unlock()
	if call == nil {
		return false
	}
	call.fn()
	return true
}

func (s *authEpisodeScheduler) armed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.pending {
		if !c.cancelled {
			n++
		}
	}
	return n
}

// recordingAuditStore is a thread-safe rotation.AuditStore for asserting which
// outcomes the rotator recorded from its dispatched goroutines.
type recordingAuditStore struct {
	mu       sync.Mutex
	inserted []rotation.AuditEvent
}

func (s *recordingAuditStore) Insert(_ context.Context, ev rotation.AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inserted = append(s.inserted, ev)
	return nil
}

func (s *recordingAuditStore) RecentBySession(_ context.Context, _ string, _ int) ([]rotation.AuditEvent, error) {
	return nil, nil
}

func (s *recordingAuditStore) outcomes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.inserted))
	for i, ev := range s.inserted {
		out[i] = ev.Outcome
	}
	return out
}

// TestChatRotator_SingleCleanPollDoesNotEndAuthFailedEpisode is BOS-980's regression
// proof, asserted end to end through a REAL status.Tracker rather than a CurrentAuthFailed
// stub — because the defect lives in the interaction between the tracker's falling-edge
// clear and the rotator's re-probe, and a stub cannot reproduce it.
//
// The wedged-pane flap: Claude Code's "Retrying in 15s · attempt N/10" countdown redraws
// the login banner out of statusdetect's 20-line tail for a tick, so one poll reads clean.
// status.Tracker.SetAuthFailed deletes the marker outright on that poll (deliberately —
// see the BOS-808 note on tracker.go), and the very next auth-failed poll only rebuilds
// the rise-debounce to 1 of AuthFailedConsecutivePollsRequired, so Tracker.AuthFailed is
// still false. Before BOS-980 a re-probe landing in that window read "recovered", dropped
// the healthy streak and cancelled the pending re-probe — 18 spurious
// ROTATION_OUTCOME_STATUS_ONLY_RECOVERED rows in the live daemon log, on panes that had
// not recovered at all.
//
// After BOS-980 the rotator latches the episode: a clean reading is held, not believed,
// until the pane has been SUSTAINEDLY clear, so the streak and its re-probe survive the
// flap and the healer reaches its threshold.
func TestChatRotator_SingleCleanPollDoesNotEndAuthFailedEpisode(t *testing.T) {
	const chatID = "chat-flap"
	tracker := status.NewTracker()
	sched := &authEpisodeScheduler{}
	store := &recordingAuditStore{}

	var mu sync.Mutex
	var switches []rotation.SwitchRequest
	var probes int

	rotator := rotation.NewChatRotator(rotation.ChatRotatorDeps{
		Logger:     zerolog.Nop(),
		Recorder:   rotation.NewRecorder(store, zerolog.Nop()),
		LoadConfig: func() (config.ManagedAccountsConfig, error) { return config.ManagedAccountsConfig{}, nil },
		ChatContext: func(_ context.Context, _ string) (rotation.ChatContext, error) {
			return rotation.ChatContext{SessionID: "sess-1", RepoID: "repo-1", Provider: "claude", AccountID: "acct-bound"}, nil
		},
		CurrentStatus: func(id string) bossanovav1.ChatStatus {
			if e := tracker.Get(id); e != nil {
				return e.Status
			}
			return bossanovav1.ChatStatus_CHAT_STATUS_UNSPECIFIED
		},
		// The seams under test: both are fed by the real tracker, exactly as main.go wires them.
		CurrentAuthFailed: tracker.AuthFailed,
		AuthFailedSince:   tracker.AuthFailedSince,
		AuthProbe: func(context.Context, string) rotation.AuthProbeResult {
			mu.Lock()
			probes++
			mu.Unlock()
			// The bound credential is fine; the pane's proxy token is what is wedged.
			return rotation.AuthProbeHealthy
		},
		Schedule: sched.schedule,
		Decide: func(context.Context, rotation.DecideRequest) (rotation.Decision, error) {
			return rotation.Decision{Kind: rotation.DecisionStatusOnly}, nil
		},
		Switch: func(_ context.Context, req rotation.SwitchRequest) (rotation.SwitchResult, error) {
			mu.Lock()
			defer mu.Unlock()
			switches = append(switches, req)
			return rotation.SwitchResult{SwitchedToLabel: "acct-bound", Fresh: true}, nil
		},
	})
	tracker.SetOnAuthChange(func(agentSessionID string) {
		if tracker.AuthFailed(agentSessionID) {
			rotator.OnAuthFailed(agentSessionID)
		}
	})

	probeCount := func() int {
		mu.Lock()
		defer mu.Unlock()
		return probes
	}
	switchCount := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(switches)
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}

	// 1. The pane wedges: two consecutive login-required polls cross the rise debounce,
	//    the tracker's auth hook fires, and the rotator opens the episode at streak 1.
	tracker.SetAuthFailed(chatID, true)
	tracker.SetAuthFailed(chatID, true)
	waitFor("the first auth probe", func() bool { return probeCount() >= 1 })
	waitFor("the first re-probe to be armed", func() bool { return sched.armed() == 1 })
	if switchCount() != 0 {
		t.Fatalf("a single healthy probe must not respawn yet, got %d switches", switchCount())
	}

	// 2. The flap: ONE clean poll (the retry countdown redrew the banner out of the tail),
	//    then the banner returns. The tracker deleted the marker on the clean poll, so the
	//    single auth-failed poll that follows has only rebuilt the debounce to 1 — AuthFailed
	//    still reports false even though the pane is visibly wedged.
	tracker.SetAuthFailed(chatID, false)
	tracker.SetAuthFailed(chatID, true)
	if tracker.AuthFailed(chatID) {
		t.Fatal("precondition: one auth-failed poll after a clean poll must NOT yet report AuthFailed")
	}

	// 3. The re-probe lands inside that window. It must NOT end the episode.
	if !sched.fire() {
		t.Fatal("expected the pending re-probe to still be armed")
	}
	// Settle on either observable outcome so the assertion below reports the real
	// difference rather than a bare timeout: the latch re-arms the re-probe, while the
	// pre-BOS-980 rotator writes a RECOVERED row and cancels the timer.
	waitFor("the flapped re-probe to settle", func() bool {
		return sched.armed() == 1 || len(store.outcomes()) > 0
	})
	for _, o := range store.outcomes() {
		if o == "ROTATION_OUTCOME_STATUS_ONLY_RECOVERED" {
			t.Fatalf("a single clean poll ended the episode as RECOVERED (%v); the pane had not recovered", store.outcomes())
		}
	}
	if sched.armed() != 1 {
		t.Fatalf("armed re-probes = %d, want 1: the held episode must keep its pending re-probe", sched.armed())
	}

	// 4. The pane re-establishes the marker on the next poll, and the still-armed re-probe
	//    fires: the preserved streak reaches the threshold and the healer respawns in place.
	tracker.SetAuthFailed(chatID, true)
	if !tracker.AuthFailed(chatID) {
		t.Fatal("precondition: the pane should be reporting auth-failed again")
	}
	if !sched.fire() {
		t.Fatal("expected the episode's re-probe to still be armed after the flap")
	}
	waitFor("the respawn", func() bool { return switchCount() >= 1 })

	mu.Lock()
	defer mu.Unlock()
	if len(switches) != 1 {
		t.Fatalf("switch calls = %d, want exactly 1 (the respawn-in-place)", len(switches))
	}
	if !switches[0].RespawnSameAccount {
		t.Fatalf("healer must respawn in place on the same account, got %+v", switches[0])
	}
}

// TestAuthLatchConstantsMirrorTheTracker pins the BOS-980 latch constants against the
// status package values they duplicate. rotation cannot import status (status → db →
// rotation would be an import cycle), so the module-boundary convention is to duplicate
// the constants — which is only safe if a test fails when the original moves.
func TestAuthLatchConstantsMirrorTheTracker(t *testing.T) {
	if rotation.AuthPollIntervalForTest != status.PollInterval {
		t.Fatalf("authPollInterval = %v, want status.PollInterval = %v", rotation.AuthPollIntervalForTest, status.PollInterval)
	}
	if rotation.AuthRisePollsForTest != status.AuthFailedConsecutivePollsRequired {
		t.Fatalf("authRisePolls = %d, want status.AuthFailedConsecutivePollsRequired = %d",
			rotation.AuthRisePollsForTest, status.AuthFailedConsecutivePollsRequired)
	}
	// The retry cycle is Claude Code's, not the tracker's, but they are equal today and
	// the derivation's justification leans on that: a window shorter than StaleThreshold
	// could expire while the tracker still considers the last observation fresh.
	if rotation.AuthRetryCycleForTest != status.StaleThreshold {
		t.Fatalf("authRetryCycle = %v, want status.StaleThreshold = %v", rotation.AuthRetryCycleForTest, status.StaleThreshold)
	}
	// The grace window must outlast one retry cycle AND the cost of re-establishing a
	// cleared marker, or a flapping pane escapes the latch through its own trough. It
	// has to beat that budget STRICTLY, not merely meet it: at exactly the budget the
	// window expires in the same instant the second rise poll lands, so the clear races
	// the re-assertion and jitter decides which wins.
	worstCase := status.StaleThreshold + status.PollInterval*status.AuthFailedConsecutivePollsRequired
	if rotation.AuthClearGraceWindowForTest <= worstCase {
		t.Fatalf("authClearGraceWindow = %v, want strictly more than the worst-case re-assertion budget %v",
			rotation.AuthClearGraceWindowForTest, worstCase)
	}
}
