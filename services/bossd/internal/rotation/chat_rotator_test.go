package rotation

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/config"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

const testChatID = "agent-session-1"

type fakeDeps struct {
	mu          sync.Mutex
	decideCalls int
	decision    Decision
	decideErr   error
	switchCalls []SwitchRequest
	switchErr   error

	status   bossanovav1.ChatStatus // what the re-check sees
	cfg      config.RotationConfig
	repoID   string
	provider string
}

func (f *fakeDeps) rotator(now *time.Time) *ChatRotator {
	f.repoID, f.provider = "repo-1", "claude"
	return NewChatRotator(ChatRotatorDeps{
		Logger:     zerolog.Nop(),
		LoadConfig: func() (config.RotationConfig, error) { return f.cfg, nil },
		ChatContext: func(_ context.Context, _ string) (ChatContext, error) {
			return ChatContext{SessionID: "sess-1", RepoID: f.repoID, Provider: f.provider}, nil
		},
		CurrentStatus: func(_ string) bossanovav1.ChatStatus {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.status
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
	f := &fakeDeps{status: chatStatusLimited(), decision: Decision{Kind: DecisionSwitch, AccountID: "acct-b-id"}}
	r := f.rotator(&now)
	reset := now.Add(2 * time.Hour)

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
		LoadConfig: func() (config.RotationConfig, error) { return config.RotationConfig{}, nil },
		ChatContext: func(_ context.Context, _ string) (ChatContext, error) {
			mu.Lock()
			defer mu.Unlock()
			if failContext {
				return ChatContext{}, errors.New("temporary context lookup failure")
			}
			return ChatContext{SessionID: "sess-1", RepoID: "repo-1", Provider: "claude"}, nil
		},
		CurrentStatus: func(_ string) bossanovav1.ChatStatus { return chatStatusLimited() },
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
			cfg: config.RotationConfig{
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
			cfg: config.RotationConfig{AutoRotateChats: boolPtr(false)}}
		r := f.rotator(&now)
		r.OnChatStatus(testChatID, chatStatusLimited(), time.Time{})
		waitIdle(t, r)
		if f.decides() != 0 || len(f.switched()) != 0 {
			t.Fatalf("opted-out chat rotated")
		}
	})
	t.Run("per-repo off overrides default-on", func(t *testing.T) {
		f := &fakeDeps{status: chatStatusLimited(), decision: Decision{Kind: DecisionSwitch, AccountID: "x"},
			cfg: config.RotationConfig{AutoRotateChatsPerRepo: map[string]bool{"repo-1": false}}}
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

func rotatorWithRecorder(f *fakeDeps, now *time.Time, store AuditStore) *ChatRotator {
	f.repoID, f.provider = "repo-1", "claude"
	return NewChatRotator(ChatRotatorDeps{
		Logger:     zerolog.Nop(),
		Recorder:   NewRecorder(store, zerolog.Nop()),
		LoadConfig: func() (config.RotationConfig, error) { return f.cfg, nil },
		ChatContext: func(_ context.Context, _ string) (ChatContext, error) {
			return ChatContext{SessionID: "sess-1", RepoID: f.repoID, Provider: f.provider}, nil
		},
		CurrentStatus: func(_ string) bossanovav1.ChatStatus {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.status
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

// TestChatRotator_RecordsDisabledWhenOptedOut pins the STATUS_ONLY_DISABLED
// audit event on the opt-out (kill-switch) path (BOS-176).
func TestChatRotator_RecordsDisabledWhenOptedOut(t *testing.T) {
	now := time.Now()
	off := false
	f := &fakeDeps{status: chatStatusLimited(), cfg: config.RotationConfig{AutoRotateChats: &off}}
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
