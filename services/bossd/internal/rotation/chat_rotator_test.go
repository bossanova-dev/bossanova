package rotation

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/config"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const testChatID = "agent-session-1"

type fakeDeps struct {
	mu          sync.Mutex
	decideCalls int
	decision    Decision
	decideErr   error
	lastDecide  DecideRequest
	probeCalls  int
	probeSnap   models.UsageSnapshot
	probeErr    error
	switchCalls []SwitchRequest
	switchErr   error

	status   bossanovav1.ChatStatus // what the re-check sees
	cfg      config.ManagedAccountsConfig
	repoID   string
	provider string

	// Auth-path (BOS-316) knobs, read by the authRotator seams.
	authFailed     bool  // what CurrentAuthFailed reports at dispatch time
	authConfirmed  bool  // what AuthProbe reports (true = typed 401 confirmed)
	authProbeErr   error // AuthProbe error (probe failed)
	authProbeCalls int
}

func (f *fakeDeps) rotator(now *time.Time) *ChatRotator {
	f.repoID, f.provider = "repo-1", "claude"
	return NewChatRotator(ChatRotatorDeps{
		Logger:     zerolog.Nop(),
		LoadConfig: func() (config.ManagedAccountsConfig, error) { return f.cfg, nil },
		ChatContext: func(_ context.Context, _ string) (ChatContext, error) {
			return ChatContext{SessionID: "sess-1", RepoID: f.repoID, Provider: f.provider, AccountID: "acct-capped"}, nil
		},
		CurrentStatus: func(_ string) bossanovav1.ChatStatus {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.status
		},
		Decide: func(_ context.Context, req DecideRequest) (Decision, error) {
			f.mu.Lock()
			f.decideCalls++
			f.lastDecide = req
			f.mu.Unlock()
			return f.decision, f.decideErr
		},
		RateLimitProbe: func(_ context.Context, _ string) (models.UsageSnapshot, error) {
			f.mu.Lock()
			f.probeCalls++
			f.mu.Unlock()
			if f.probeErr != nil {
				return models.UsageSnapshot{}, f.probeErr
			}
			if f.probeSnap.FetchedAt == nil && f.probeSnap.Status == "" && f.probeSnap.Util5h == 0 && f.probeSnap.Util7d == 0 {
				f.probeSnap = limitedSnapshot(now.Add(2 * time.Hour))
			}
			if f.probeSnap.FetchedAt == nil {
				fetched := now.UTC()
				f.probeSnap.FetchedAt = &fetched
			}
			return f.probeSnap, nil
		},
		Switch: func(_ context.Context, req SwitchRequest) (SwitchResult, error) {
			f.mu.Lock()
			f.switchCalls = append(f.switchCalls, req)
			f.mu.Unlock()
			return SwitchResult{SwitchedToLabel: "acct-b"}, f.switchErr
		},
		Now: func() time.Time { return *now },
	})
}

// authRotator builds a ChatRotator wired for the auth-invalidation path
// (BOS-316). store may be nil when the test asserts only on switch/decide calls.
func (f *fakeDeps) authRotator(now *time.Time, store AuditStore) *ChatRotator {
	f.repoID, f.provider = "repo-1", "claude"
	var rec *Recorder
	if store != nil {
		rec = NewRecorder(store, zerolog.Nop())
	}
	return NewChatRotator(ChatRotatorDeps{
		Logger:     zerolog.Nop(),
		Recorder:   rec,
		LoadConfig: func() (config.ManagedAccountsConfig, error) { return f.cfg, nil },
		ChatContext: func(_ context.Context, _ string) (ChatContext, error) {
			return ChatContext{SessionID: "sess-1", RepoID: f.repoID, Provider: f.provider, AccountID: "acct-capped"}, nil
		},
		CurrentStatus: func(_ string) bossanovav1.ChatStatus {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.status
		},
		CurrentAuthFailed: func(_ string) bool {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.authFailed
		},
		AuthProbe: func(_ context.Context, _ string) (bool, error) {
			f.mu.Lock()
			f.authProbeCalls++
			confirmed, err := f.authConfirmed, f.authProbeErr
			f.mu.Unlock()
			return confirmed, err
		},
		Decide: func(_ context.Context, req DecideRequest) (Decision, error) {
			f.mu.Lock()
			f.decideCalls++
			f.lastDecide = req
			f.mu.Unlock()
			return f.decision, f.decideErr
		},
		Switch: func(_ context.Context, req SwitchRequest) (SwitchResult, error) {
			f.mu.Lock()
			f.switchCalls = append(f.switchCalls, req)
			f.mu.Unlock()
			return SwitchResult{SwitchedToLabel: "acct-b"}, f.switchErr
		},
		Now: func() time.Time { return *now },
	})
}

func (f *fakeDeps) authProbes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authProbeCalls
}

func (f *fakeDeps) switched() []SwitchRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]SwitchRequest, len(f.switchCalls))
	copy(out, f.switchCalls)
	return out
}

func (f *fakeDeps) decides() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.decideCalls
}

func (f *fakeDeps) lastDecideRequest() DecideRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastDecide
}

func limitedSnapshot(reset time.Time) models.UsageSnapshot {
	fetched := reset.Add(-time.Hour)
	return models.UsageSnapshot{
		Util5h:    1,
		Reset5h:   &reset,
		Status:    "RATE_LIMIT_PLAN_STATUS_RATE_LIMITED",
		FetchedAt: &fetched,
	}
}

func waitIdle(t *testing.T, r *ChatRotator) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if r.idleForTest() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("rotator did not go idle")
}

func TestChatRotator_LimitedTriggersSwitch(t *testing.T) {
	now := time.Now()
	reset := now.Add(2 * time.Hour)
	f := &fakeDeps{
		status:    chatStatusLimited(),
		probeSnap: limitedSnapshot(reset),
		decision:  Decision{Kind: DecisionSwitch, AccountID: "acct-b-id"},
	}
	r := f.rotator(&now)

	r.OnChatStatus(testChatID, chatStatusLimited(), reset)
	waitIdle(t, r)

	calls := f.switched()
	if len(calls) != 1 {
		t.Fatalf("switch calls = %d, want 1", len(calls))
	}
	c := calls[0]
	if c.SessionID != "sess-1" || c.AgentSessionID != testChatID || c.AccountID != "acct-b-id" {
		t.Fatalf("unexpected switch request: %+v", c)
	}
	if !c.Auto || !c.PreviousResetAt.Equal(reset) {
		t.Fatalf("auto/reset not propagated: %+v", c)
	}
	if got := f.lastDecideRequest().AccountID; got != "acct-capped" {
		t.Fatalf("Decide AccountID = %q, want probed account acct-capped", got)
	}
}

func TestChatRotator_ProbeGate(t *testing.T) {
	now := time.Now()
	reset := now.Add(2 * time.Hour)
	tests := []struct {
		name       string
		snap       models.UsageSnapshot
		probeErr   error
		wantDecide bool
		wantReset  time.Time
	}{
		{
			name:       "limited uses probe reset",
			snap:       limitedSnapshot(reset),
			wantDecide: true,
			wantReset:  reset,
		},
		{
			name: "healthy suppresses loose trigger",
			snap: models.UsageSnapshot{
				Util5h:    0.08,
				Status:    "RATE_LIMIT_PLAN_STATUS_ACTIVE",
				FetchedAt: &now,
			},
		},
		{name: "probe error suppresses loose trigger", probeErr: errors.New("probe down")},
		{name: "auth failure suppresses loose trigger", probeErr: status.Error(codes.Unauthenticated, "auth invalidated")},
		{
			name: "unsupported suppresses loose trigger",
			snap: models.UsageSnapshot{
				Status:    "RATE_LIMIT_PLAN_STATUS_UNSUPPORTED",
				FetchedAt: &now,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotReset time.Time
			decideCalled := false
			f := &fakeDeps{
				status:    chatStatusLimited(),
				probeSnap: tt.snap,
				probeErr:  tt.probeErr,
				decision:  Decision{Kind: DecisionStatusOnly},
			}
			r := NewChatRotator(ChatRotatorDeps{
				Logger:     zerolog.Nop(),
				LoadConfig: func() (config.ManagedAccountsConfig, error) { return f.cfg, nil },
				ChatContext: func(_ context.Context, _ string) (ChatContext, error) {
					return ChatContext{SessionID: "sess-1", RepoID: "repo-1", Provider: "claude", AccountID: "acct-capped"}, nil
				},
				CurrentStatus: func(_ string) bossanovav1.ChatStatus { return chatStatusLimited() },
				RateLimitProbe: func(context.Context, string) (models.UsageSnapshot, error) {
					if f.probeErr != nil {
						return models.UsageSnapshot{}, f.probeErr
					}
					return f.probeSnap, nil
				},
				Decide: func(_ context.Context, req DecideRequest) (Decision, error) {
					decideCalled = true
					gotReset = req.ResetAt
					return f.decision, nil
				},
				Switch: func(context.Context, SwitchRequest) (SwitchResult, error) {
					return SwitchResult{}, nil
				},
				Now: func() time.Time { return now },
			})

			bannerReset := now.Add(60 * time.Minute)
			r.OnChatStatus(testChatID, chatStatusLimited(), bannerReset)
			waitIdle(t, r)

			if tt.wantDecide {
				if !decideCalled {
					t.Fatal("Decide was not called")
				}
				if gotReset.IsZero() || !gotReset.Equal(tt.wantReset) {
					t.Fatalf("Decide reset = %v, want probe reset %v", gotReset, tt.wantReset)
				}
			} else if decideCalled {
				t.Fatalf("Decide was called with reset %v, want no Decide call", gotReset)
			}
		})
	}
}

func TestChatRotator_NonLimitedStatusNeverRotates(t *testing.T) {
	// Regression guard: a WORKING (or any non-LIMITED) chat is never rotated.
	now := time.Now()
	f := &fakeDeps{status: bossanovav1.ChatStatus_CHAT_STATUS_WORKING, decision: Decision{Kind: DecisionSwitch, AccountID: "x"}}
	r := f.rotator(&now)

	for _, st := range []bossanovav1.ChatStatus{
		bossanovav1.ChatStatus_CHAT_STATUS_WORKING,
		bossanovav1.ChatStatus_CHAT_STATUS_IDLE,
		bossanovav1.ChatStatus_CHAT_STATUS_QUESTION,
		bossanovav1.ChatStatus_CHAT_STATUS_STOPPED,
		bossanovav1.ChatStatus_CHAT_STATUS_UNSPECIFIED,
	} {
		r.OnChatStatus(testChatID, st, time.Time{})
	}
	waitIdle(t, r)
	if f.decides() != 0 || len(f.switched()) != 0 {
		t.Fatalf("non-LIMITED status caused activity: decides=%d switches=%d", f.decides(), len(f.switched()))
	}
}

func TestChatRotator_RateLimitPerChat(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{status: chatStatusLimited(), decision: Decision{Kind: DecisionSwitch, AccountID: "x"}}
	r := f.rotator(&now)

	r.OnChatStatus(testChatID, chatStatusLimited(), time.Time{})
	waitIdle(t, r)
	// Second LIMITED transition inside the window (banner flap) — suppressed.
	now = now.Add(1 * time.Minute)
	r.OnChatStatus(testChatID, chatStatusLimited(), time.Time{})
	waitIdle(t, r)
	if got := len(f.switched()); got != 1 {
		t.Fatalf("switches inside window = %d, want 1", got)
	}
	// After the window (default 10m) a new attempt is allowed.
	now = now.Add(10 * time.Minute)
	r.OnChatStatus(testChatID, chatStatusLimited(), time.Time{})
	waitIdle(t, r)
	if got := len(f.switched()); got != 2 {
		t.Fatalf("switches after window = %d, want 2", got)
	}
}

func TestChatRotator_ContextLookupFailureDoesNotConsumeRateLimit(t *testing.T) {
	now := time.Now()
	var mu sync.Mutex
	failContext := true
	var switchCalls []SwitchRequest

	r := NewChatRotator(ChatRotatorDeps{
		Logger:     zerolog.Nop(),
		LoadConfig: func() (config.ManagedAccountsConfig, error) { return config.ManagedAccountsConfig{}, nil },
		ChatContext: func(_ context.Context, _ string) (ChatContext, error) {
			mu.Lock()
			defer mu.Unlock()
			if failContext {
				return ChatContext{}, errors.New("temporary context lookup failure")
			}
			return ChatContext{SessionID: "sess-1", RepoID: "repo-1", Provider: "claude", AccountID: "acct-capped"}, nil
		},
		CurrentStatus: func(_ string) bossanovav1.ChatStatus { return chatStatusLimited() },
		RateLimitProbe: func(context.Context, string) (models.UsageSnapshot, error) {
			return limitedSnapshot(now.Add(2 * time.Hour)), nil
		},
		Decide: func(_ context.Context, _ DecideRequest) (Decision, error) {
			return Decision{Kind: DecisionSwitch, AccountID: "acct-b-id"}, nil
		},
		Switch: func(_ context.Context, req SwitchRequest) (SwitchResult, error) {
			mu.Lock()
			switchCalls = append(switchCalls, req)
			mu.Unlock()
			return SwitchResult{SwitchedToLabel: "acct-b"}, nil
		},
		Now: func() time.Time { return now },
	})

	r.OnChatStatus(testChatID, chatStatusLimited(), time.Time{})
	waitIdle(t, r)

	mu.Lock()
	failContext = false
	mu.Unlock()
	r.OnChatStatus(testChatID, chatStatusLimited(), time.Time{})
	waitIdle(t, r)

	mu.Lock()
	got := len(switchCalls)
	mu.Unlock()
	if got != 1 {
		t.Fatalf("switches after transient context failure = %d, want 1", got)
	}
}

func TestChatRotator_OptOutHonored(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }
	now := time.Now()

	t.Run("rotation enabled false kills chat rotation", func(t *testing.T) {
		f := &fakeDeps{status: chatStatusLimited(), decision: Decision{Kind: DecisionSwitch, AccountID: "x"},
			cfg: config.ManagedAccountsConfig{
				Enabled:                      boolPtr(false),
				AutoRotateChats:              boolPtr(true),
				AutoRotateChatsPerRepo:       map[string]bool{"repo-1": true},
				ChatRotateMinIntervalMinutes: 1,
			}}
		r := f.rotator(&now)
		r.OnChatStatus(testChatID, chatStatusLimited(), time.Time{})
		waitIdle(t, r)
		if f.decides() != 0 || len(f.switched()) != 0 {
			t.Fatalf("globally disabled rotation still rotated")
		}
	})
	t.Run("global off", func(t *testing.T) {
		f := &fakeDeps{status: chatStatusLimited(), decision: Decision{Kind: DecisionSwitch, AccountID: "x"},
			cfg: config.ManagedAccountsConfig{AutoRotateChats: boolPtr(false)}}
		r := f.rotator(&now)
		r.OnChatStatus(testChatID, chatStatusLimited(), time.Time{})
		waitIdle(t, r)
		if f.decides() != 0 || len(f.switched()) != 0 {
			t.Fatalf("opted-out chat rotated")
		}
	})
	t.Run("per-repo off overrides default-on", func(t *testing.T) {
		f := &fakeDeps{status: chatStatusLimited(), decision: Decision{Kind: DecisionSwitch, AccountID: "x"},
			cfg: config.ManagedAccountsConfig{AutoRotateChatsPerRepo: map[string]bool{"repo-1": false}}}
		r := f.rotator(&now)
		r.OnChatStatus(testChatID, chatStatusLimited(), time.Time{})
		waitIdle(t, r)
		if len(f.switched()) != 0 {
			t.Fatalf("per-repo opted-out chat rotated")
		}
	})
}

func TestChatRotator_AllExhaustedParksWithoutLoop(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{status: chatStatusLimited(), decision: Decision{Kind: DecisionAllExhausted, ResumeAt: now.Add(3 * time.Hour)}}
	r := f.rotator(&now)

	r.OnChatStatus(testChatID, chatStatusLimited(), time.Time{})
	waitIdle(t, r)
	if len(f.switched()) != 0 {
		t.Fatal("all-exhausted must not switch")
	}
	if f.decides() != 1 {
		t.Fatalf("decides = %d, want 1", f.decides())
	}
	// The tracker redraw keeps reporting LIMITED (same status = no tracker re-fire
	// in prod, but even direct re-calls are rate-limited): no loop.
	now = now.Add(1 * time.Minute)
	r.OnChatStatus(testChatID, chatStatusLimited(), time.Time{})
	waitIdle(t, r)
	if f.decides() != 1 {
		t.Fatalf("all-exhausted looped: decides = %d, want 1", f.decides())
	}
}

func TestChatRotator_EvictsStaleRateLimitEntries(t *testing.T) {
	// The lastAttempt rate-limit map must not grow unbounded: an entry older than
	// the rate-limit window is evicted on the next transition (it can no longer
	// suppress anything), so a busy daemon does not accumulate a permanent entry
	// per chat that ever hit LIMITED.
	now := time.Now()
	f := &fakeDeps{status: chatStatusLimited(), decision: Decision{Kind: DecisionSwitch, AccountID: "x"}}
	r := f.rotator(&now)

	r.OnChatStatus("chat-A", chatStatusLimited(), time.Time{})
	waitIdle(t, r)
	// Advance past the default 10m window, then a *different* chat transitions.
	now = now.Add(11 * time.Minute)
	r.OnChatStatus("chat-B", chatStatusLimited(), time.Time{})
	waitIdle(t, r)

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.lastAttempt["chat-A"]; ok {
		t.Error("stale chat-A rate-limit entry was not evicted after its window elapsed")
	}
	if _, ok := r.lastAttempt["chat-B"]; !ok {
		t.Error("chat-B rate-limit entry missing")
	}
}

func TestChatRotator_StatusOnlyDecisionDoesNothing(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{status: chatStatusLimited(), decision: Decision{Kind: DecisionStatusOnly}}
	r := f.rotator(&now)
	r.OnChatStatus(testChatID, chatStatusLimited(), time.Time{})
	waitIdle(t, r)
	if len(f.switched()) != 0 {
		t.Fatal("status-only decision must not switch")
	}
}

func TestChatRotator_RecheckAbortsWhenChatRecovered(t *testing.T) {
	// Race guard: transition fired LIMITED but by dispatch time the pane redrew
	// and the chat is WORKING again — abort, never rotate.
	now := time.Now()
	f := &fakeDeps{status: bossanovav1.ChatStatus_CHAT_STATUS_WORKING, decision: Decision{Kind: DecisionSwitch, AccountID: "x"}}
	r := f.rotator(&now)
	r.OnChatStatus(testChatID, chatStatusLimited(), time.Time{})
	waitIdle(t, r)
	if f.decides() != 0 || len(f.switched()) != 0 {
		t.Fatal("recovered chat was rotated")
	}
}

func TestChatRotator_ConcurrentSignalsSingleAttempt(t *testing.T) {
	// -race gate: N concurrent LIMITED signals for the same chat ⇒ ≤1 switch.
	now := time.Now()
	f := &fakeDeps{status: chatStatusLimited(), decision: Decision{Kind: DecisionSwitch, AccountID: "x"}}
	r := f.rotator(&now)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.OnChatStatus(testChatID, chatStatusLimited(), time.Time{})
		}()
	}
	wg.Wait()
	waitIdle(t, r)
	if got := len(f.switched()); got != 1 {
		t.Fatalf("concurrent signals produced %d switches, want 1", got)
	}
}

// TestRotate_UnboundSession_TrustsBannerAndSwitches pins the unbound-session
// path (BOS-320): a LIMITED chat with no bound account (AccountID=="") has no
// account to probe, so the rotator trusts the detected banner and rotates onto
// an eligible managed account via the Auto switch path — even with no
// RateLimitProbe wired.
func TestRotate_UnboundSession_TrustsBannerAndSwitches(t *testing.T) {
	now := time.Now()
	reset := now.Add(90 * time.Minute)
	var mu sync.Mutex
	var switchCalls []SwitchRequest

	r := NewChatRotator(ChatRotatorDeps{
		Logger:     zerolog.Nop(),
		LoadConfig: func() (config.ManagedAccountsConfig, error) { return config.ManagedAccountsConfig{}, nil },
		ChatContext: func(context.Context, string) (ChatContext, error) {
			return ChatContext{SessionID: "s1", RepoID: "r1", Provider: "claude", AccountID: ""}, nil // unbound
		},
		CurrentStatus: func(string) bossanovav1.ChatStatus { return chatStatusLimited() },
		Decide: func(_ context.Context, req DecideRequest) (Decision, error) {
			// An unbound session passes AccountID=="" through as CappedAccountID.
			if req.AccountID != "" {
				t.Errorf("Decide AccountID = %q, want empty for unbound session", req.AccountID)
			}
			return Decision{Kind: DecisionSwitch, AccountID: "b", Label: "acct-b"}, nil
		},
		Switch: func(_ context.Context, req SwitchRequest) (SwitchResult, error) {
			mu.Lock()
			switchCalls = append(switchCalls, req)
			mu.Unlock()
			return SwitchResult{SwitchedToLabel: "acct-b"}, nil
		},
		RateLimitProbe: nil, // no bound account to probe — must not block the unbound path
		Now:            func() time.Time { return now },
	})

	r.OnChatStatus("chat-1", chatStatusLimited(), reset)
	waitIdle(t, r)

	mu.Lock()
	defer mu.Unlock()
	if len(switchCalls) != 1 {
		t.Fatalf("switch calls = %d, want 1 Auto switch for the unbound LIMITED chat", len(switchCalls))
	}
	c := switchCalls[0]
	if !c.Auto || c.AccountID != "b" {
		t.Errorf("Switch req = %+v, want Auto to acct b", c)
	}
	if !c.PreviousResetAt.Equal(reset) {
		t.Errorf("Switch PreviousResetAt = %v, want banner reset %v", c.PreviousResetAt, reset)
	}
}

// chatStatusLimited returns the LIMITED enum value added by BOS-166.
func chatStatusLimited() bossanovav1.ChatStatus {
	return bossanovav1.ChatStatus_CHAT_STATUS_LIMITED
}

// lockedAuditStore is a thread-safe AuditStore for asserting recorder emission
// from the rotate goroutine.
type lockedAuditStore struct {
	mu       sync.Mutex
	inserted []AuditEvent
}

func (s *lockedAuditStore) Insert(_ context.Context, ev AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inserted = append(s.inserted, ev)
	return nil
}

func (s *lockedAuditStore) RecentBySession(_ context.Context, _ string, _ int) ([]AuditEvent, error) {
	return nil, nil
}

func (s *lockedAuditStore) outcomes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.inserted))
	for i, ev := range s.inserted {
		out[i] = ev.Outcome
	}
	return out
}

func (s *lockedAuditStore) triggers() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.inserted))
	for i, ev := range s.inserted {
		out[i] = ev.Trigger
	}
	return out
}

func rotatorWithRecorder(f *fakeDeps, now *time.Time, store AuditStore) *ChatRotator {
	f.repoID, f.provider = "repo-1", "claude"
	return NewChatRotator(ChatRotatorDeps{
		Logger:     zerolog.Nop(),
		Recorder:   NewRecorder(store, zerolog.Nop()),
		LoadConfig: func() (config.ManagedAccountsConfig, error) { return f.cfg, nil },
		ChatContext: func(_ context.Context, _ string) (ChatContext, error) {
			return ChatContext{SessionID: "sess-1", RepoID: f.repoID, Provider: f.provider, AccountID: "acct-capped"}, nil
		},
		CurrentStatus: func(_ string) bossanovav1.ChatStatus {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.status
		},
		RateLimitProbe: func(context.Context, string) (models.UsageSnapshot, error) {
			return limitedSnapshot(now.Add(2 * time.Hour)), nil
		},
		Decide: func(_ context.Context, _ DecideRequest) (Decision, error) {
			f.mu.Lock()
			f.decideCalls++
			f.mu.Unlock()
			return f.decision, f.decideErr
		},
		Switch: func(_ context.Context, req SwitchRequest) (SwitchResult, error) {
			f.mu.Lock()
			f.switchCalls = append(f.switchCalls, req)
			f.mu.Unlock()
			return SwitchResult{SwitchedToLabel: "acct-b"}, f.switchErr
		},
		Now: func() time.Time { return *now },
	})
}

// TestChatRotator_RecordsAuditPerOutcome pins that every live rotator decision
// path records exactly one audit event with the right outcome (BOS-176).
func TestChatRotator_RecordsAuditPerOutcome(t *testing.T) {
	cases := []struct {
		name   string
		decide Decision
		want   string
	}{
		{"switch → rotated", Decision{Kind: DecisionSwitch, AccountID: "acct-b-id", Label: "acct-b"}, "ROTATION_OUTCOME_ROTATED"},
		{"all cooling → exhausted", Decision{Kind: DecisionAllExhausted, ResumeAt: time.Now().Add(time.Hour)}, "ROTATION_OUTCOME_EXHAUSTED"},
		{"no capability → status only", Decision{Kind: DecisionStatusOnly}, "ROTATION_OUTCOME_STATUS_ONLY_NO_CAPABILITY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			f := &fakeDeps{status: chatStatusLimited(), decision: tc.decide}
			store := &lockedAuditStore{}
			r := rotatorWithRecorder(f, &now, store)
			r.OnChatStatus(testChatID, chatStatusLimited(), now.Add(2*time.Hour))
			waitIdle(t, r)
			got := store.outcomes()
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("want exactly one %s audit event, got %v", tc.want, got)
			}
		})
	}
}

func TestChatRotator_RecordsProbeGateAudit(t *testing.T) {
	now := time.Now()
	healthy := models.UsageSnapshot{
		Util5h:    0.08,
		Status:    "RATE_LIMIT_PLAN_STATUS_ACTIVE",
		FetchedAt: &now,
	}
	tests := []struct {
		name  string
		probe RateLimitProbe
	}{
		{
			name: "healthy",
			probe: func(context.Context, string) (models.UsageSnapshot, error) {
				return healthy, nil
			},
		},
		{
			name: "error",
			probe: func(context.Context, string) (models.UsageSnapshot, error) {
				return models.UsageSnapshot{}, errors.New("probe down")
			},
		},
		{name: "missing probe"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &lockedAuditStore{}
			decideCalled := false
			r := NewChatRotator(ChatRotatorDeps{
				Logger:   zerolog.Nop(),
				Recorder: NewRecorder(store, zerolog.Nop()),
				LoadConfig: func() (config.ManagedAccountsConfig, error) {
					return config.ManagedAccountsConfig{}, nil
				},
				ChatContext: func(context.Context, string) (ChatContext, error) {
					return ChatContext{SessionID: "sess-1", RepoID: "repo-1", Provider: "claude", AccountID: "acct-capped"}, nil
				},
				CurrentStatus:  func(string) bossanovav1.ChatStatus { return chatStatusLimited() },
				RateLimitProbe: tt.probe,
				Decide: func(context.Context, DecideRequest) (Decision, error) {
					decideCalled = true
					return Decision{}, nil
				},
				Switch: func(context.Context, SwitchRequest) (SwitchResult, error) {
					return SwitchResult{}, nil
				},
				Now: func() time.Time { return now },
			})
			r.OnChatStatus(testChatID, chatStatusLimited(), now.Add(time.Hour))
			waitIdle(t, r)
			if decideCalled {
				t.Fatal("Decide was called")
			}
			got := store.outcomes()
			if len(got) != 1 || got[0] != "ROTATION_OUTCOME_STATUS_ONLY_PROBE_UNCONFIRMED" {
				t.Fatalf("want one probe-gate audit event, got %v", got)
			}
		})
	}
}

// TestChatRotator_RecordsRecoveredWhenChatRecovered pins that the "chat no
// longer limited; aborting" early-return records exactly one recovered audit
// row (BOS-315: previously silent).
func TestChatRotator_RecordsRecoveredWhenChatRecovered(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{status: bossanovav1.ChatStatus_CHAT_STATUS_WORKING, decision: Decision{Kind: DecisionSwitch, AccountID: "x"}}
	store := &lockedAuditStore{}
	r := rotatorWithRecorder(f, &now, store)
	r.OnChatStatus(testChatID, chatStatusLimited(), now.Add(2*time.Hour))
	waitIdle(t, r)
	if f.decides() != 0 || len(f.switched()) != 0 {
		t.Fatal("recovered chat was rotated")
	}
	got := store.outcomes()
	if len(got) != 1 || got[0] != "ROTATION_OUTCOME_STATUS_ONLY_RECOVERED" {
		t.Fatalf("want one STATUS_ONLY_RECOVERED audit event, got %v", got)
	}
}

// TestChatRotator_RecordsFailedWhenDecideErrors pins that the engine-Decide
// failure early-return records exactly one FAILED audit row (BOS-315).
func TestChatRotator_RecordsFailedWhenDecideErrors(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{status: chatStatusLimited(), decideErr: errors.New("engine unavailable")}
	store := &lockedAuditStore{}
	r := rotatorWithRecorder(f, &now, store)
	r.OnChatStatus(testChatID, chatStatusLimited(), now.Add(2*time.Hour))
	waitIdle(t, r)
	if len(f.switched()) != 0 {
		t.Fatal("decide-failed path must not switch")
	}
	got := store.outcomes()
	if len(got) != 1 || got[0] != "ROTATION_OUTCOME_FAILED" {
		t.Fatalf("want one FAILED audit event, got %v", got)
	}
}

// TestChatRotator_RecordsFailedOnUnknownDecisionKind pins that the default
// unknown-decision-kind case records exactly one FAILED audit row (BOS-315).
func TestChatRotator_RecordsFailedOnUnknownDecisionKind(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{status: chatStatusLimited(), decision: Decision{Kind: DecisionKind(0)}}
	store := &lockedAuditStore{}
	r := rotatorWithRecorder(f, &now, store)
	r.OnChatStatus(testChatID, chatStatusLimited(), now.Add(2*time.Hour))
	waitIdle(t, r)
	if len(f.switched()) != 0 {
		t.Fatal("unknown-decision-kind path must not switch")
	}
	got := store.outcomes()
	if len(got) != 1 || got[0] != "ROTATION_OUTCOME_FAILED" {
		t.Fatalf("want one FAILED audit event, got %v", got)
	}
}

// TestChatRotator_ProbeDisagreementLogsWarn pins that the probe-disagreement
// path (banner tripped LIMITED but the authoritative probe disagrees) surfaces
// at warn level and still records exactly one PROBE_UNCONFIRMED audit row
// (BOS-315).
func TestChatRotator_ProbeDisagreementLogsWarn(t *testing.T) {
	now := time.Now()
	var buf bytes.Buffer
	store := &lockedAuditStore{}
	healthy := models.UsageSnapshot{
		Util5h:    0.08,
		Status:    "RATE_LIMIT_PLAN_STATUS_ACTIVE",
		FetchedAt: &now,
	}
	r := NewChatRotator(ChatRotatorDeps{
		Logger:     zerolog.New(&buf),
		Recorder:   NewRecorder(store, zerolog.Nop()),
		LoadConfig: func() (config.ManagedAccountsConfig, error) { return config.ManagedAccountsConfig{}, nil },
		ChatContext: func(context.Context, string) (ChatContext, error) {
			return ChatContext{SessionID: "sess-1", RepoID: "repo-1", Provider: "claude", AccountID: "acct-capped"}, nil
		},
		CurrentStatus:  func(string) bossanovav1.ChatStatus { return chatStatusLimited() },
		RateLimitProbe: func(context.Context, string) (models.UsageSnapshot, error) { return healthy, nil },
		Decide: func(context.Context, DecideRequest) (Decision, error) {
			t.Fatal("Decide must not be called when probe disagrees")
			return Decision{}, nil
		},
		Switch: func(context.Context, SwitchRequest) (SwitchResult, error) { return SwitchResult{}, nil },
		Now:    func() time.Time { return now },
	})
	r.OnChatStatus(testChatID, chatStatusLimited(), now.Add(time.Hour))
	waitIdle(t, r)

	logOut := buf.String()
	if !strings.Contains(logOut, `"level":"warn"`) || !strings.Contains(logOut, "usage probe says account is not limited") {
		t.Fatalf("probe-disagreement line not at warn level: %s", logOut)
	}
	got := store.outcomes()
	if len(got) != 1 || got[0] != "ROTATION_OUTCOME_STATUS_ONLY_PROBE_UNCONFIRMED" {
		t.Fatalf("want one PROBE_UNCONFIRMED audit event, got %v", got)
	}
}

// TestChatRotator_RecordsDisabledWhenOptedOut pins the STATUS_ONLY_DISABLED
// audit event on the opt-out (kill-switch) path (BOS-176).
func TestChatRotator_RecordsDisabledWhenOptedOut(t *testing.T) {
	now := time.Now()
	off := false
	f := &fakeDeps{status: chatStatusLimited(), cfg: config.ManagedAccountsConfig{AutoRotateChats: &off}}
	store := &lockedAuditStore{}
	r := rotatorWithRecorder(f, &now, store)
	r.OnChatStatus(testChatID, chatStatusLimited(), now.Add(2*time.Hour))
	waitIdle(t, r)
	got := store.outcomes()
	if len(got) != 1 || got[0] != "ROTATION_OUTCOME_STATUS_ONLY_DISABLED" {
		t.Fatalf("want one STATUS_ONLY_DISABLED audit event, got %v", got)
	}
	if f.decides() != 0 {
		t.Fatalf("opted-out rotator must not call Decide, got %d", f.decides())
	}
}

// --- Auth-invalidation path (BOS-316) ---

// TestChatRotator_AuthFailedConfirmedRotates pins the happy path: a pane that is
// still auth-failed and whose probe confirms a typed 401 rotates exactly once via
// the Auto switch path, with the audit stamped
// ROTATION_TRIGGER_AUTH_INVALIDATED / ROTATION_OUTCOME_ROTATED and the engine
// driven with the AuthInvalidated signal kind.
func TestChatRotator_AuthFailedConfirmedRotates(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{
		authFailed:    true,
		authConfirmed: true,
		decision:      Decision{Kind: DecisionSwitch, AccountID: "acct-b-id", Label: "acct-b"},
	}
	store := &lockedAuditStore{}
	r := f.authRotator(&now, store)

	r.OnAuthFailed(testChatID)
	waitIdle(t, r)

	calls := f.switched()
	if len(calls) != 1 {
		t.Fatalf("switch calls = %d, want 1", len(calls))
	}
	c := calls[0]
	if c.SessionID != "sess-1" || c.AgentSessionID != testChatID || c.AccountID != "acct-b-id" || !c.Auto {
		t.Fatalf("unexpected switch request: %+v", c)
	}
	if got := f.lastDecideRequest().Kind; got != AuthInvalidated {
		t.Fatalf("Decide Kind = %v, want AuthInvalidated", got)
	}
	trig := store.triggers()
	if len(trig) != 1 || trig[0] != "ROTATION_TRIGGER_AUTH_INVALIDATED" {
		t.Fatalf("audit triggers = %v, want one ROTATION_TRIGGER_AUTH_INVALIDATED", trig)
	}
	if out := store.outcomes(); len(out) != 1 || out[0] != "ROTATION_OUTCOME_ROTATED" {
		t.Fatalf("audit outcomes = %v, want one ROTATION_OUTCOME_ROTATED", out)
	}
	// Surface the audit trigger in -v output as the auto-rotate-on-401 proof token.
	t.Logf("auth-rotation audit trigger = %s", trig[0])
}

// TestChatRotator_AuthProbeGate pins the fail-safe gate: the auth path proceeds
// to Decide/Switch ONLY when the probe authoritatively confirms a typed 401. A
// healthy probe, a probe error, or a pane that recovered before dispatch all
// leave the chat as-is with no Switch and no false-positive Decide.
func TestChatRotator_AuthProbeGate(t *testing.T) {
	tests := []struct {
		name          string
		authFailed    bool
		authConfirmed bool
		authProbeErr  error
		wantProbe     bool
		wantDecide    bool
	}{
		{
			name:          "healthy probe no-ops without decide",
			authFailed:    true,
			authConfirmed: false,
			wantProbe:     true,
			wantDecide:    false,
		},
		{
			name:         "probe error suppresses rotation",
			authFailed:   true,
			authProbeErr: status.Error(codes.Unauthenticated, "probe transport failed"),
			wantProbe:    true,
			wantDecide:   false,
		},
		{
			name:       "pane recovered before dispatch aborts",
			authFailed: false,
			wantProbe:  false,
			wantDecide: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now()
			f := &fakeDeps{
				authFailed:    tt.authFailed,
				authConfirmed: tt.authConfirmed,
				authProbeErr:  tt.authProbeErr,
				decision:      Decision{Kind: DecisionSwitch, AccountID: "acct-b-id"},
			}
			r := f.authRotator(&now, nil)

			r.OnAuthFailed(testChatID)
			waitIdle(t, r)

			if got := len(f.switched()); got != 0 {
				t.Fatalf("switch calls = %d, want 0", got)
			}
			if got := f.decides(); (got > 0) != tt.wantDecide {
				t.Fatalf("decides = %d, want wantDecide=%v", got, tt.wantDecide)
			}
			if got := f.authProbes() > 0; got != tt.wantProbe {
				t.Fatalf("authProbe called = %v, want %v", got, tt.wantProbe)
			}
		})
	}
}

// TestChatRotator_AuthProbeGateRecordsAudit pins that the auth probe-gate
// no-op branches (probe says healthy, probe errored) still record exactly one
// audit row stamped ROTATION_TRIGGER_AUTH_INVALIDATED with the
// STATUS_ONLY_PROBE_UNCONFIRMED outcome — the acceptance-criterion invariant
// that EVERY audit event on the auth path carries the auth trigger, on the
// paths TestChatRotator_AuthProbeGate exercises with a nil store.
func TestChatRotator_AuthProbeGateRecordsAudit(t *testing.T) {
	tests := []struct {
		name          string
		authConfirmed bool
		authProbeErr  error
	}{
		{name: "healthy probe records probe-unconfirmed", authConfirmed: false},
		{name: "probe error records probe-unconfirmed", authProbeErr: status.Error(codes.Unauthenticated, "probe transport failed")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now()
			f := &fakeDeps{
				authFailed:    true,
				authConfirmed: tt.authConfirmed,
				authProbeErr:  tt.authProbeErr,
				decision:      Decision{Kind: DecisionSwitch, AccountID: "acct-b-id"},
			}
			store := &lockedAuditStore{}
			r := f.authRotator(&now, store)

			r.OnAuthFailed(testChatID)
			waitIdle(t, r)

			if len(f.switched()) != 0 {
				t.Fatal("probe-gate no-op must not switch")
			}
			if f.decides() != 0 {
				t.Fatalf("probe-gate no-op must not call Decide, got %d", f.decides())
			}
			if out := store.outcomes(); len(out) != 1 || out[0] != "ROTATION_OUTCOME_STATUS_ONLY_PROBE_UNCONFIRMED" {
				t.Fatalf("audit outcomes = %v, want one STATUS_ONLY_PROBE_UNCONFIRMED", out)
			}
			if trig := store.triggers(); len(trig) != 1 || trig[0] != "ROTATION_TRIGGER_AUTH_INVALIDATED" {
				t.Fatalf("audit triggers = %v, want ROTATION_TRIGGER_AUTH_INVALIDATED", trig)
			}
		})
	}
}

// TestChatRotator_AuthAllFailedParksWithoutLoop pins the no-rotate-loop
// safeguard: with every candidate account failed the engine returns
// DecisionStatusOnly, so the confirmed-401 pane records a status-only audit and
// does NOT switch — and an immediate repeat OnAuthFailed for the same chat is
// suppressed by the shared inFlight/rate limiter (one attempt, then park).
func TestChatRotator_AuthAllFailedParksWithoutLoop(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{
		authFailed:    true,
		authConfirmed: true,
		decision:      Decision{Kind: DecisionStatusOnly},
	}
	store := &lockedAuditStore{}
	r := f.authRotator(&now, store)

	r.OnAuthFailed(testChatID)
	waitIdle(t, r)
	if len(f.switched()) != 0 {
		t.Fatal("all-failed status-only must not switch")
	}
	if f.decides() != 1 {
		t.Fatalf("decides = %d, want 1", f.decides())
	}
	if out := store.outcomes(); len(out) != 1 || out[0] != "ROTATION_OUTCOME_STATUS_ONLY_NO_CAPABILITY" {
		t.Fatalf("audit outcomes = %v, want one status-only event", out)
	}
	if trig := store.triggers(); len(trig) != 1 || trig[0] != "ROTATION_TRIGGER_AUTH_INVALIDATED" {
		t.Fatalf("audit triggers = %v, want ROTATION_TRIGGER_AUTH_INVALIDATED", trig)
	}

	// Immediate redelivery inside the rate-limit window must not loop.
	now = now.Add(1 * time.Minute)
	r.OnAuthFailed(testChatID)
	waitIdle(t, r)
	if f.decides() != 1 {
		t.Fatalf("auth path looped: decides = %d, want 1", f.decides())
	}
	if len(f.switched()) != 0 {
		t.Fatal("auth path looped into a switch")
	}
}

// TestChatRotator_AuthExhaustedRecordsExhausted pins the DecisionAllExhausted
// outcome maps to a ROTATION_OUTCOME_EXHAUSTED audit on the auth trigger.
func TestChatRotator_AuthExhaustedRecordsExhausted(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{
		authFailed:    true,
		authConfirmed: true,
		decision:      Decision{Kind: DecisionAllExhausted, ResumeAt: now.Add(2 * time.Hour)},
	}
	store := &lockedAuditStore{}
	r := f.authRotator(&now, store)

	r.OnAuthFailed(testChatID)
	waitIdle(t, r)
	if len(f.switched()) != 0 {
		t.Fatal("all-exhausted must not switch")
	}
	if out := store.outcomes(); len(out) != 1 || out[0] != "ROTATION_OUTCOME_EXHAUSTED" {
		t.Fatalf("audit outcomes = %v, want one ROTATION_OUTCOME_EXHAUSTED", out)
	}
	if trig := store.triggers(); len(trig) != 1 || trig[0] != "ROTATION_TRIGGER_AUTH_INVALIDATED" {
		t.Fatalf("audit triggers = %v, want ROTATION_TRIGGER_AUTH_INVALIDATED", trig)
	}
}

// TestChatRotator_AuthConcurrentSignalsSingleAttempt is the -race gate for the
// auth path: N concurrent OnAuthFailed calls for one chat produce at most one
// switch (shared inFlight guard with the LIMITED path).
func TestChatRotator_AuthConcurrentSignalsSingleAttempt(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{authFailed: true, authConfirmed: true, decision: Decision{Kind: DecisionSwitch, AccountID: "x"}}
	r := f.authRotator(&now, nil)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.OnAuthFailed(testChatID)
		}()
	}
	wg.Wait()
	waitIdle(t, r)
	if got := len(f.switched()); got != 1 {
		t.Fatalf("concurrent auth signals produced %d switches, want 1", got)
	}
}
