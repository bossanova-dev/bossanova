package rotation

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/config"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
)

// proactiveFake wires a ChatRotator for the proactive pre-cap sweep. Each chat
// has its own bound account, probe snapshot, and re-check status, so tests can
// mix IDLE/WORKING panes in one sweep.
type proactiveFake struct {
	mu sync.Mutex

	cfg config.ManagedAccountsConfig

	live      map[string]bossanovav1.ChatStatus // LiveChatStatuses
	boundAcct map[string]string                 // agentSessionID → bound account
	repoID    map[string]string                 // agentSessionID → repo (opt-out)
	snaps     map[string]models.UsageSnapshot   // accountID → bound-probe snapshot
	recheck   map[string]bossanovav1.ChatStatus // CurrentStatus at re-check
	decision  ProactiveDecision                 // what ProactiveCandidate returns
	decideErr error

	probeCalls  map[string]int // accountID → probe calls
	candCalls   int            // ProactiveCandidate calls
	switchCalls []SwitchRequest
	switchErr   error
}

func newProactiveFake() *proactiveFake {
	return &proactiveFake{
		live:       map[string]bossanovav1.ChatStatus{},
		boundAcct:  map[string]string{},
		repoID:     map[string]string{},
		snaps:      map[string]models.UsageSnapshot{},
		recheck:    map[string]bossanovav1.ChatStatus{},
		probeCalls: map[string]int{},
	}
}

func (f *proactiveFake) rotator(now *time.Time) *ChatRotator {
	return NewChatRotator(ChatRotatorDeps{
		Logger:     zerolog.Nop(),
		LoadConfig: func() (config.ManagedAccountsConfig, error) { return f.cfg, nil },
		ChatContext: func(_ context.Context, id string) (ChatContext, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			repo := f.repoID[id]
			if repo == "" {
				repo = "repo-1"
			}
			return ChatContext{SessionID: "sess-" + id, RepoID: repo, Provider: "claude", AccountID: f.boundAcct[id]}, nil
		},
		CurrentStatus: func(id string) bossanovav1.ChatStatus {
			f.mu.Lock()
			defer f.mu.Unlock()
			if st, ok := f.recheck[id]; ok {
				return st
			}
			return bossanovav1.ChatStatus_CHAT_STATUS_IDLE
		},
		RateLimitProbe: func(_ context.Context, accountID string) (models.UsageSnapshot, error) {
			f.mu.Lock()
			f.probeCalls[accountID]++
			snap := f.snaps[accountID]
			f.mu.Unlock()
			if snap.FetchedAt == nil {
				fetched := now.UTC()
				snap.FetchedAt = &fetched
			}
			return snap, nil
		},
		LiveChatStatuses: func() map[string]bossanovav1.ChatStatus {
			f.mu.Lock()
			defer f.mu.Unlock()
			out := make(map[string]bossanovav1.ChatStatus, len(f.live))
			for id, st := range f.live {
				out[id] = st
			}
			return out
		},
		ProactiveCandidate: func(_ context.Context, _ ProactiveDecideRequest) (ProactiveDecision, error) {
			f.mu.Lock()
			f.candCalls++
			f.mu.Unlock()
			return f.decision, f.decideErr
		},
		Switch: func(_ context.Context, req SwitchRequest) (SwitchResult, error) {
			f.mu.Lock()
			f.switchCalls = append(f.switchCalls, req)
			f.mu.Unlock()
			return SwitchResult{SwitchedToLabel: req.AccountID}, f.switchErr
		},
		Now: func() time.Time { return *now },
	})
}

func (f *proactiveFake) switched() []SwitchRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]SwitchRequest, len(f.switchCalls))
	copy(out, f.switchCalls)
	return out
}

func (f *proactiveFake) probes(accountID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.probeCalls[accountID]
}

func (f *proactiveFake) candidateCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.candCalls
}

func enabled(b bool) *bool { return &b }

// util7dSnapshot builds a non-limited snapshot with the given 7-day utilization.
func util7dSnapshot(now time.Time, util7d float64) models.UsageSnapshot {
	fetched := now.UTC()
	return models.UsageSnapshot{Util7d: util7d, FetchedAt: &fetched}
}

func proactiveOnConfig() config.ManagedAccountsConfig {
	return config.ManagedAccountsConfig{Enabled: enabled(true), ProactiveRotation: enabled(true)}
}

func TestSweepProactive_SwitchesIdleHighUtilChat(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	f := newProactiveFake()
	f.cfg = proactiveOnConfig()
	f.live = map[string]bossanovav1.ChatStatus{
		"chat-idle":    bossanovav1.ChatStatus_CHAT_STATUS_IDLE,
		"chat-working": bossanovav1.ChatStatus_CHAT_STATUS_WORKING,
	}
	f.boundAcct = map[string]string{"chat-idle": "acct-hi", "chat-working": "acct-hi"}
	f.snaps = map[string]models.UsageSnapshot{"acct-hi": util7dSnapshot(now, 0.9)}
	f.decision = ProactiveDecision{Kind: ProactiveSwitch, AccountID: "acct-lo", Label: "lo", CandidateUtil: 0.1}

	r := f.rotator(&now)
	if got := r.SweepProactive(context.Background()); got != 1 {
		t.Fatalf("SweepProactive() = %d, want 1", got)
	}
	sw := f.switched()
	if len(sw) != 1 {
		t.Fatalf("switch calls = %d, want 1", len(sw))
	}
	if sw[0].AgentSessionID != "chat-idle" || sw[0].AccountID != "acct-lo" || !sw[0].Auto {
		t.Errorf("switch req = %+v, want {AgentSessionID:chat-idle, AccountID:acct-lo, Auto:true}", sw[0])
	}
	// The WORKING chat is never probed and never switched.
	if n := f.probes("acct-hi"); n != 1 {
		t.Errorf("acct-hi probed %d times, want 1 (only the IDLE chat)", n)
	}
}

func TestSweepProactive_WorkingChatNeverRotated(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	f := newProactiveFake()
	f.cfg = proactiveOnConfig()
	f.live = map[string]bossanovav1.ChatStatus{"chat-working": bossanovav1.ChatStatus_CHAT_STATUS_WORKING}
	f.boundAcct = map[string]string{"chat-working": "acct-hi"}
	f.snaps = map[string]models.UsageSnapshot{"acct-hi": util7dSnapshot(now, 0.99)}
	f.decision = ProactiveDecision{Kind: ProactiveSwitch, AccountID: "acct-lo", CandidateUtil: 0.0}

	r := f.rotator(&now)
	if got := r.SweepProactive(context.Background()); got != 0 {
		t.Fatalf("SweepProactive() = %d, want 0", got)
	}
	if n := len(f.switched()); n != 0 {
		t.Errorf("switch calls = %d, want 0 for a WORKING chat", n)
	}
	if n := f.probes("acct-hi"); n != 0 {
		t.Errorf("acct-hi probed %d times, want 0 (WORKING chat never probed)", n)
	}
}

func TestSweepProactive_ConfigOffIsNoOp(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	f := newProactiveFake()
	// ProactiveRotation unset (nil) ⇒ default OFF.
	f.cfg = config.ManagedAccountsConfig{Enabled: enabled(true)}
	f.live = map[string]bossanovav1.ChatStatus{"chat-idle": bossanovav1.ChatStatus_CHAT_STATUS_IDLE}
	f.boundAcct = map[string]string{"chat-idle": "acct-hi"}
	f.snaps = map[string]models.UsageSnapshot{"acct-hi": util7dSnapshot(now, 0.99)}
	f.decision = ProactiveDecision{Kind: ProactiveSwitch, AccountID: "acct-lo", CandidateUtil: 0.0}

	r := f.rotator(&now)
	if got := r.SweepProactive(context.Background()); got != 0 {
		t.Fatalf("SweepProactive() = %d, want 0", got)
	}
	if n := len(f.switched()); n != 0 {
		t.Errorf("switch calls = %d, want 0 when proactive disabled", n)
	}
	if n := f.probes("acct-hi"); n != 0 {
		t.Errorf("acct-hi probed %d times, want 0 when proactive disabled (no probe)", n)
	}
	if n := f.candidateCalls(); n != 0 {
		t.Errorf("ProactiveCandidate called %d times, want 0 when proactive disabled", n)
	}
}

func TestSweepProactive_BelowTriggerSkips(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	f := newProactiveFake()
	f.cfg = proactiveOnConfig()
	f.live = map[string]bossanovav1.ChatStatus{"chat-idle": bossanovav1.ChatStatus_CHAT_STATUS_IDLE}
	f.boundAcct = map[string]string{"chat-idle": "acct-hi"}
	f.snaps = map[string]models.UsageSnapshot{"acct-hi": util7dSnapshot(now, 0.79)} // just below 0.8
	f.decision = ProactiveDecision{Kind: ProactiveSwitch, AccountID: "acct-lo", CandidateUtil: 0.0}

	r := f.rotator(&now)
	if got := r.SweepProactive(context.Background()); got != 0 {
		t.Fatalf("SweepProactive() = %d, want 0", got)
	}
	if n := len(f.switched()); n != 0 {
		t.Errorf("switch calls = %d, want 0 below the 0.8 trigger", n)
	}
	if n := f.candidateCalls(); n != 0 {
		t.Errorf("ProactiveCandidate called %d times, want 0 below the trigger", n)
	}
}

func TestSweepProactive_HysteresisBlocks(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	f := newProactiveFake()
	f.cfg = proactiveOnConfig()
	f.live = map[string]bossanovav1.ChatStatus{"chat-idle": bossanovav1.ChatStatus_CHAT_STATUS_IDLE}
	f.boundAcct = map[string]string{"chat-idle": "acct-hi"}
	f.snaps = map[string]models.UsageSnapshot{"acct-hi": util7dSnapshot(now, 0.9)}
	// Candidate 0.7 vs bound 0.9 ⇒ gap 0.2 < 0.25 hysteresis.
	f.decision = ProactiveDecision{Kind: ProactiveSwitch, AccountID: "acct-lo", CandidateUtil: 0.7}

	r := f.rotator(&now)
	if got := r.SweepProactive(context.Background()); got != 0 {
		t.Fatalf("SweepProactive() = %d, want 0", got)
	}
	if n := len(f.switched()); n != 0 {
		t.Errorf("switch calls = %d, want 0 when gap < hysteresis", n)
	}
}

func TestSweepProactive_ProactiveNoneSkips(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	f := newProactiveFake()
	f.cfg = proactiveOnConfig()
	f.live = map[string]bossanovav1.ChatStatus{"chat-idle": bossanovav1.ChatStatus_CHAT_STATUS_IDLE}
	f.boundAcct = map[string]string{"chat-idle": "acct-hi"}
	f.snaps = map[string]models.UsageSnapshot{"acct-hi": util7dSnapshot(now, 0.9)}
	f.decision = ProactiveDecision{Kind: ProactiveNone}

	r := f.rotator(&now)
	if got := r.SweepProactive(context.Background()); got != 0 {
		t.Fatalf("SweepProactive() = %d, want 0", got)
	}
	if n := len(f.switched()); n != 0 {
		t.Errorf("switch calls = %d, want 0 for ProactiveNone", n)
	}
}

func TestSweepProactive_IdleFlippedToWorkingAtRecheckSkips(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	f := newProactiveFake()
	f.cfg = proactiveOnConfig()
	f.live = map[string]bossanovav1.ChatStatus{"chat-idle": bossanovav1.ChatStatus_CHAT_STATUS_IDLE}
	f.boundAcct = map[string]string{"chat-idle": "acct-hi"}
	f.snaps = map[string]models.UsageSnapshot{"acct-hi": util7dSnapshot(now, 0.9)}
	f.decision = ProactiveDecision{Kind: ProactiveSwitch, AccountID: "acct-lo", CandidateUtil: 0.1}
	// The pane started a turn between enumeration and the pre-switch re-check.
	f.recheck = map[string]bossanovav1.ChatStatus{"chat-idle": bossanovav1.ChatStatus_CHAT_STATUS_WORKING}

	r := f.rotator(&now)
	if got := r.SweepProactive(context.Background()); got != 0 {
		t.Fatalf("SweepProactive() = %d, want 0", got)
	}
	if n := len(f.switched()); n != 0 {
		t.Errorf("switch calls = %d, want 0 when chat flipped to WORKING at re-check", n)
	}
}

func TestSweepProactive_SkipsWhenReactiveRotationInFlight(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	f := newProactiveFake()
	f.cfg = proactiveOnConfig()
	f.live = map[string]bossanovav1.ChatStatus{"chat-idle": bossanovav1.ChatStatus_CHAT_STATUS_IDLE}
	f.boundAcct = map[string]string{"chat-idle": "acct-hi"}
	f.snaps = map[string]models.UsageSnapshot{"acct-hi": util7dSnapshot(now, 0.9)}
	f.decision = ProactiveDecision{Kind: ProactiveSwitch, AccountID: "acct-lo", Label: "lo", CandidateUtil: 0.1}

	r := f.rotator(&now)
	// Simulate a reactive (usage-limit / auth) rotation already running for this
	// chat: it holds the shared inFlight marker. SwitchAccount has no internal
	// per-chat serialization, so the proactive sweep must NOT switch concurrently.
	r.mu.Lock()
	r.inFlight["chat-idle"] = true
	r.mu.Unlock()

	if got := r.SweepProactive(context.Background()); got != 0 {
		t.Fatalf("SweepProactive() = %d, want 0 (reactive rotation in flight)", got)
	}
	if n := len(f.switched()); n != 0 {
		t.Errorf("switch calls = %d, want 0 while a reactive rotation holds inFlight", n)
	}
}

func TestSweepProactive_PerChatLimiterSuppressesSecondSwitch(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	f := newProactiveFake()
	f.cfg = proactiveOnConfig()
	f.live = map[string]bossanovav1.ChatStatus{"chat-idle": bossanovav1.ChatStatus_CHAT_STATUS_IDLE}
	f.boundAcct = map[string]string{"chat-idle": "acct-hi"}
	f.snaps = map[string]models.UsageSnapshot{"acct-hi": util7dSnapshot(now, 0.9)}
	f.decision = ProactiveDecision{Kind: ProactiveSwitch, AccountID: "acct-lo", CandidateUtil: 0.1}

	r := f.rotator(&now)
	if got := r.SweepProactive(context.Background()); got != 1 {
		t.Fatalf("first SweepProactive() = %d, want 1", got)
	}
	// A second sweep within the interval must not re-switch the same chat.
	if got := r.SweepProactive(context.Background()); got != 0 {
		t.Fatalf("second SweepProactive() = %d, want 0 (per-chat limiter)", got)
	}
	if n := len(f.switched()); n != 1 {
		t.Errorf("total switch calls = %d, want 1 (limiter suppresses the second)", n)
	}
}
