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
	authFailed     bool            // what CurrentAuthFailed reports at dispatch time
	authResult     AuthProbeResult // what AuthProbe classifies (Unknown/Confirmed401/Healthy) (BOS-482)
	authProbeCalls int

	// Fake re-probe scheduler (BOS-482): captures scheduled funcs so a test can fire
	// them deterministically instead of waiting on a real timer.
	sched fakeScheduler
}

// fakeScheduler is a deterministic stand-in for the Schedule dep seam. It records
// scheduled callbacks (never invoking them synchronously) and lets a test fire the
// most-recent still-armed one. (BOS-482)
type fakeScheduler struct {
	mu      sync.Mutex
	pending []*scheduledCall
}

type scheduledCall struct {
	fn        func()
	cancelled bool
}

func (s *fakeScheduler) schedule(_ time.Duration, f func()) (cancel func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	call := &scheduledCall{fn: f}
	s.pending = append(s.pending, call)
	return func() {
		s.mu.Lock()
		call.cancelled = true
		s.mu.Unlock()
	}
}

// fire runs the most-recently scheduled callback that has not been cancelled, marking
// it cancelled so it fires at most once. Returns false when there is nothing armed.
func (s *fakeScheduler) fire() bool {
	s.mu.Lock()
	var call *scheduledCall
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

// pendingCount reports how many armed (not-yet-cancelled) callbacks remain.
func (s *fakeScheduler) pendingCount() int {
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
		AuthProbe: func(_ context.Context, _ string) AuthProbeResult {
			f.mu.Lock()
			f.authProbeCalls++
			res := f.authResult
			f.mu.Unlock()
			return res
		},
		Schedule: f.sched.schedule,
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

func (s *lockedAuditStore) details() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.inserted))
	for i, ev := range s.inserted {
		out[i] = ev.Detail
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
		{"no eligible account → status only", Decision{Kind: DecisionNoEligibleAccount}, "ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT"},
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

// TestChatRotator_RecordsNoEligibleAccount pins BOS-327: a DecisionNoEligibleAccount
// records the distinct ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT outcome
// with the actionable "boss account update ... --status active" detail, on BOTH
// the usage-limited and auth-invalidated rotation paths (and never switches).
func TestChatRotator_RecordsNoEligibleAccount(t *testing.T) {
	const wantOutcome = "ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT"
	const wantDetail = "no eligible account to rotate to — enable or re-authenticate one (e.g. `boss account update <id> --status active`)"

	t.Run("usage-limited path", func(t *testing.T) {
		now := time.Now()
		f := &fakeDeps{status: chatStatusLimited(), decision: Decision{Kind: DecisionNoEligibleAccount}}
		store := &lockedAuditStore{}
		r := rotatorWithRecorder(f, &now, store)
		r.OnChatStatus(testChatID, chatStatusLimited(), now.Add(2*time.Hour))
		waitIdle(t, r)
		if len(f.switched()) != 0 {
			t.Fatal("no-eligible path must not switch")
		}
		if out := store.outcomes(); len(out) != 1 || out[0] != wantOutcome {
			t.Fatalf("outcomes = %v, want one %s", out, wantOutcome)
		}
		if det := store.details(); len(det) != 1 || det[0] != wantDetail {
			t.Fatalf("details = %v, want one %q", det, wantDetail)
		}
	})

	t.Run("auth-invalidated path", func(t *testing.T) {
		now := time.Now()
		f := &fakeDeps{
			authFailed: true,
			authResult: AuthProbeConfirmed401,
			decision:   Decision{Kind: DecisionNoEligibleAccount},
		}
		store := &lockedAuditStore{}
		r := f.authRotator(&now, store)
		r.OnAuthFailed(testChatID)
		waitIdle(t, r)
		if len(f.switched()) != 0 {
			t.Fatal("no-eligible auth path must not switch")
		}
		if out := store.outcomes(); len(out) != 1 || out[0] != wantOutcome {
			t.Fatalf("outcomes = %v, want one %s", out, wantOutcome)
		}
		if det := store.details(); len(det) != 1 || det[0] != wantDetail {
			t.Fatalf("details = %v, want one %q", det, wantDetail)
		}
		if trig := store.triggers(); len(trig) != 1 || trig[0] != "ROTATION_TRIGGER_AUTH_INVALIDATED" {
			t.Fatalf("triggers = %v, want ROTATION_TRIGGER_AUTH_INVALIDATED", trig)
		}
	})
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
		authFailed: true,
		authResult: AuthProbeConfirmed401,
		decision:   Decision{Kind: DecisionSwitch, AccountID: "acct-b-id", Label: "acct-b"},
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
		name       string
		authFailed bool
		authResult AuthProbeResult
		wantProbe  bool
		wantDecide bool
	}{
		{
			name:       "healthy probe no-ops without decide",
			authFailed: true,
			authResult: AuthProbeHealthy,
			wantProbe:  true,
			wantDecide: false,
		},
		{
			name:       "inconclusive probe suppresses rotation",
			authFailed: true,
			authResult: AuthProbeUnknown,
			wantProbe:  true,
			wantDecide: false,
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
				authFailed: tt.authFailed,
				authResult: tt.authResult,
				decision:   Decision{Kind: DecisionSwitch, AccountID: "acct-b-id"},
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
		name       string
		authResult AuthProbeResult
	}{
		{name: "healthy probe records probe-unconfirmed", authResult: AuthProbeHealthy},
		{name: "inconclusive probe records probe-unconfirmed", authResult: AuthProbeUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now()
			f := &fakeDeps{
				authFailed: true,
				authResult: tt.authResult,
				decision:   Decision{Kind: DecisionSwitch, AccountID: "acct-b-id"},
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
		authFailed: true,
		authResult: AuthProbeConfirmed401,
		decision:   Decision{Kind: DecisionStatusOnly},
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
		authFailed: true,
		authResult: AuthProbeConfirmed401,
		decision:   Decision{Kind: DecisionAllExhausted, ResumeAt: now.Add(2 * time.Hour)},
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
	f := &fakeDeps{authFailed: true, authResult: AuthProbeConfirmed401, decision: Decision{Kind: DecisionSwitch, AccountID: "x"}}
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

// --- Respawn-in-place on healthy-probe auth wedge (BOS-482) ---

// TestChatRotator_HealthyProbeRespawnsInPlace pins the core BOS-482 behavior: a pane
// that is auth-failed while its bound account probes HEALTHY does not rotate; instead,
// after healthyRespawnThreshold consecutive Healthy confirmations it respawns in place
// on the SAME account (Switch with RespawnSameAccount=true) and records
// ROTATION_OUTCOME_RESPAWNED_SAME_ACCOUNT. The first (below-threshold) probe only
// records STATUS_ONLY_PROBE_UNCONFIRMED and arms a re-probe.
func TestChatRotator_HealthyProbeRespawnsInPlace(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{authFailed: true, authResult: AuthProbeHealthy}
	store := &lockedAuditStore{}
	r := f.authRotator(&now, store)

	// First auth-failed edge: streak → 1, below threshold, no switch, re-probe armed.
	r.OnAuthFailed(testChatID)
	waitIdle(t, r)
	if len(f.switched()) != 0 {
		t.Fatalf("below-threshold healthy probe must not switch, got %d", len(f.switched()))
	}
	if out := store.outcomes(); len(out) != 1 || out[0] != "ROTATION_OUTCOME_STATUS_ONLY_PROBE_UNCONFIRMED" {
		t.Fatalf("outcomes after first probe = %v, want one STATUS_ONLY_PROBE_UNCONFIRMED", out)
	}
	if n := f.sched.pendingCount(); n != 1 {
		t.Fatalf("pending re-probe timers = %d, want 1", n)
	}

	// Fire the re-probe: streak → 2 (threshold) → respawn-in-place on the same account.
	if !f.sched.fire() {
		t.Fatal("expected an armed re-probe to fire")
	}
	waitIdle(t, r)

	calls := f.switched()
	if len(calls) != 1 {
		t.Fatalf("switch calls = %d, want 1 (the respawn)", len(calls))
	}
	c := calls[0]
	if !c.RespawnSameAccount {
		t.Fatal("respawn Switch must set RespawnSameAccount=true")
	}
	if c.AccountID != "acct-capped" || !c.Auto {
		t.Fatalf("respawn Switch bound wrong account or not Auto: %+v", c)
	}
	if got := f.decides(); got != 0 {
		t.Fatalf("respawn-in-place must NOT consult Decide, got %d decides", got)
	}
	out := store.outcomes()
	if len(out) != 2 || out[1] != "ROTATION_OUTCOME_RESPAWNED_SAME_ACCOUNT" {
		t.Fatalf("outcomes = %v, want [..., RESPAWNED_SAME_ACCOUNT]", out)
	}
	if trig := store.triggers(); trig[1] != "ROTATION_TRIGGER_AUTH_INVALIDATED" {
		t.Fatalf("respawn audit trigger = %q, want ROTATION_TRIGGER_AUTH_INVALIDATED", trig[1])
	}
	// Success clears the streak and cancels any pending re-probe.
	if n := f.sched.pendingCount(); n != 0 {
		t.Fatalf("pending re-probe timers after respawn = %d, want 0", n)
	}
}

// TestChatRotator_InconclusiveProbeNeverRespawns pins that an Unknown (inconclusive)
// probe never advances the respawn streak: repeated re-probes that stay inconclusive
// keep recording STATUS_ONLY_PROBE_UNCONFIRMED and never respawn, no matter how many
// times the timer fires.
func TestChatRotator_InconclusiveProbeNeverRespawns(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{authFailed: true, authResult: AuthProbeUnknown}
	store := &lockedAuditStore{}
	r := f.authRotator(&now, store)

	r.OnAuthFailed(testChatID)
	waitIdle(t, r)
	// Fire the armed re-probe several times; each stays inconclusive and re-arms.
	for i := 0; i < 4; i++ {
		if !f.sched.fire() {
			t.Fatalf("iteration %d: expected an armed re-probe", i)
		}
		waitIdle(t, r)
	}
	if len(f.switched()) != 0 {
		t.Fatalf("inconclusive probe must never respawn/switch, got %d", len(f.switched()))
	}
	for _, o := range store.outcomes() {
		if o != "ROTATION_OUTCOME_STATUS_ONLY_PROBE_UNCONFIRMED" {
			t.Fatalf("unexpected outcome %q; every inconclusive probe must be STATUS_ONLY_PROBE_UNCONFIRMED", o)
		}
	}
}

// TestChatRotator_HealthyStreakTTLResets pins the streak TTL: two Healthy probes
// separated by more than healthyCountTTL do NOT reach the threshold (the stale first
// confirmation is discarded), so no respawn occurs.
func TestChatRotator_HealthyStreakTTLResets(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{authFailed: true, authResult: AuthProbeHealthy}
	store := &lockedAuditStore{}
	r := f.authRotator(&now, store)

	r.OnAuthFailed(testChatID) // streak → 1
	waitIdle(t, r)

	// Advance the clock past the TTL, then fire the re-probe: the stale count is reset
	// to 1 rather than incremented to 2, so no respawn.
	now = now.Add(healthyCountTTL + time.Minute)
	if !f.sched.fire() {
		t.Fatal("expected an armed re-probe")
	}
	waitIdle(t, r)

	if len(f.switched()) != 0 {
		t.Fatalf("TTL-expired streak must not respawn, got %d switches", len(f.switched()))
	}
	for _, o := range store.outcomes() {
		if o == "ROTATION_OUTCOME_RESPAWNED_SAME_ACCOUNT" {
			t.Fatal("TTL-expired streak respawned; the stale confirmation must be discarded")
		}
	}
}

// TestChatRotator_RespawnCapExhausted pins the per-chat respawn cap. With the Switch
// failing (so the healthy streak is preserved across re-probes) the pane keeps trying
// to respawn; the cap is charged BEFORE the Switch, so after respawnCap real attempts
// the next one records ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED and no further Switch is
// attempted.
func TestChatRotator_RespawnCapExhausted(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{authFailed: true, authResult: AuthProbeHealthy, switchErr: errors.New("respawn boom")}
	store := &lockedAuditStore{}
	r := f.authRotator(&now, store)

	r.OnAuthFailed(testChatID) // streak → 1 (no switch yet)
	waitIdle(t, r)
	// Fire re-probes until the cap is hit. Each fire advances the streak (already at/above
	// threshold) and attempts a respawn; the failing Switch keeps the streak alive.
	for i := 0; i < respawnCap+1; i++ {
		if !f.sched.fire() {
			t.Fatalf("iteration %d: expected an armed re-probe", i)
		}
		waitIdle(t, r)
	}

	if got := len(f.switched()); got != respawnCap {
		t.Fatalf("Switch attempts = %d, want respawnCap=%d (cap charged before Switch)", got, respawnCap)
	}
	out := store.outcomes()
	if last := out[len(out)-1]; last != "ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED" {
		t.Fatalf("final outcome = %q, want ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED", last)
	}
	// Cap-exhausted goes quiet: the re-probe is cancelled.
	if n := f.sched.pendingCount(); n != 0 {
		t.Fatalf("pending re-probe timers after cap-exhausted = %d, want 0", n)
	}
}

// TestChatRotator_RespawnAbortedIsFailSafe pins that an ErrSwitchAborted refusal (the
// chat went WORKING / mid-turn) is NOT a failure: no FAILED or RESPAWNED audit is
// written, the streak is preserved, and a re-probe stays armed to retry later.
func TestChatRotator_RespawnAbortedIsFailSafe(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{authFailed: true, authResult: AuthProbeHealthy, switchErr: ErrSwitchAborted}
	store := &lockedAuditStore{}
	r := f.authRotator(&now, store)

	r.OnAuthFailed(testChatID) // streak → 1
	waitIdle(t, r)
	if !f.sched.fire() { // streak → 2 → respawn attempt → aborted
		t.Fatal("expected an armed re-probe")
	}
	waitIdle(t, r)

	if len(f.switched()) != 1 {
		t.Fatalf("aborted respawn should still have attempted exactly one Switch, got %d", len(f.switched()))
	}
	for _, o := range store.outcomes() {
		if o == "ROTATION_OUTCOME_FAILED" || o == "ROTATION_OUTCOME_RESPAWNED_SAME_ACCOUNT" {
			t.Fatalf("aborted respawn must not record %q (fail-safe leaves chat as-is)", o)
		}
	}
	// Fail-safe re-arms a re-probe so the pane is retried once it is idle again.
	if n := f.sched.pendingCount(); n != 1 {
		t.Fatalf("pending re-probe timers after abort = %d, want 1", n)
	}
}

// TestChatRotator_RespawnGenericErrorRecordsFailed pins that a non-abort Switch error
// records exactly one ROTATION_OUTCOME_FAILED audit and re-arms a re-probe.
func TestChatRotator_RespawnGenericErrorRecordsFailed(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{authFailed: true, authResult: AuthProbeHealthy, switchErr: errors.New("boom")}
	store := &lockedAuditStore{}
	r := f.authRotator(&now, store)

	r.OnAuthFailed(testChatID) // streak → 1
	waitIdle(t, r)
	if !f.sched.fire() { // streak → 2 → respawn → generic error
		t.Fatal("expected an armed re-probe")
	}
	waitIdle(t, r)

	out := store.outcomes()
	if len(out) != 2 || out[1] != "ROTATION_OUTCOME_FAILED" {
		t.Fatalf("outcomes = %v, want [..., FAILED]", out)
	}
	if n := f.sched.pendingCount(); n != 1 {
		t.Fatalf("pending re-probe timers after FAILED = %d, want 1", n)
	}
}

// TestChatRotator_DeregisterCancelsReprobe pins that Deregister tears down the pending
// re-probe timer for a chat that is going away, so no orphaned timer can re-drive the
// auth path after the pane is gone.
func TestChatRotator_DeregisterCancelsReprobe(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{authFailed: true, authResult: AuthProbeHealthy}
	r := f.authRotator(&now, nil)

	r.OnAuthFailed(testChatID) // arms a re-probe
	waitIdle(t, r)
	if n := f.sched.pendingCount(); n != 1 {
		t.Fatalf("pending re-probe timers = %d, want 1", n)
	}
	r.Deregister(testChatID)
	if n := f.sched.pendingCount(); n != 0 {
		t.Fatalf("Deregister must cancel the pending re-probe; pending = %d, want 0", n)
	}
}
