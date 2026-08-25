package rotation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/config"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/detach/detachgate"
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
	loadErr     error
	chatCtxErr  error
	// noBoundAccount makes the ChatContext seam report a chat with no account
	// bound to it, the state repairProxyPane treats as terminal. (BOS-982)
	noBoundAccount bool

	// respawnSwitchErr, when non-nil, is returned INSTEAD of switchErr for a
	// RespawnSameAccount switch only. It lets a test make the respawn-in-place attempt
	// fail while a subsequent rotation to a different account still succeeds. (BOS-981)
	respawnSwitchErr error
	// fromLabel is the human label of the bound account reported by ChatContext, so a
	// test can assert an audit detail names it. (BOS-981)
	fromLabel string

	status   bossanovav1.ChatStatus // what the re-check sees
	cfg      config.ManagedAccountsConfig
	repoID   string
	provider string

	// Auth-path (BOS-316) knobs, read by the authRotator seams.
	authFailed     bool            // what CurrentAuthFailed reports at dispatch time
	authResult     AuthProbeResult // what AuthProbe classifies (Unknown/Confirmed401/Healthy) (BOS-482)
	authProbeCalls int
	authSince      time.Time // what AuthFailedSince reports as the episode anchor (BOS-980)

	// probeEntered/probeRelease gate the AuthProbe seam so a test can hold the auth
	// lane inside its dispatched goroutine — reservation held — while it delivers a
	// second, racing trigger. Without a gate the race is decided by the scheduler and
	// only one interleaving is ever exercised. (BOS-982)
	probeEntered chan struct{}
	probeRelease chan struct{}

	// chatCtxHook runs ONCE, inside the ChatContext dep seam of authRotator — i.e.
	// while the calling lane holds the chat's reservation. It lets a test deliver a
	// racing trigger at a known point instead of leaving the interleaving to the
	// scheduler. chatCtxCalls counts entries to that seam, which is one per
	// repairProxyPane/rotateAuth run. (BOS-982)
	chatCtxHook  func()
	chatCtxCalls int

	// Fake re-probe scheduler (BOS-482): captures scheduled funcs so a test can fire
	// them deterministically instead of waiting on a real timer.
	sched fakeScheduler
}

// gateAuthProbe arms the AuthProbe gate. The returned entered channel receives
// once the lane is inside AuthProbe (and therefore holds the chat's reservation);
// closing the returned release channel lets it continue.
func (f *fakeDeps) gateAuthProbe() (entered <-chan struct{}, release chan<- struct{}) {
	in := make(chan struct{}, 1)
	rel := make(chan struct{})
	f.mu.Lock()
	f.probeEntered, f.probeRelease = in, rel
	f.mu.Unlock()
	return in, rel
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

// armed reports how many scheduled callbacks are still pending.
func (s *fakeScheduler) armed() int {
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

// fireWhenArmed waits for a callback to be armed, then fires it.
//
// fire() returns false immediately when nothing is armed, so a test that
// reaches it via an observation the rotator emits *before* its own Schedule
// call races that call and fails spuriously. Waiting on the pending count
// closes the window without weakening the assertion: an arm that never
// happens still fails, just at the deadline rather than instantly.
func fireWhenArmed(t *testing.T, s *fakeScheduler) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for s.armed() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for a scheduled callback to be armed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !s.fire() {
		t.Fatal("scheduled callback was armed but did not fire")
	}
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
		LoadConfig: func() (config.ManagedAccountsConfig, error) { return f.cfg, f.loadErr },
		ChatContext: func(_ context.Context, _ string) (ChatContext, error) {
			if f.chatCtxErr != nil {
				return ChatContext{}, f.chatCtxErr
			}
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
		LoadConfig: func() (config.ManagedAccountsConfig, error) { return f.cfg, f.loadErr },
		ChatContext: func(_ context.Context, _ string) (ChatContext, error) {
			f.mu.Lock()
			f.chatCtxCalls++
			hook := f.chatCtxHook
			f.chatCtxHook = nil
			accountID := "acct-capped"
			if f.noBoundAccount {
				accountID = ""
			}
			f.mu.Unlock()
			if hook != nil {
				hook()
			}
			if f.chatCtxErr != nil {
				return ChatContext{}, f.chatCtxErr
			}
			return ChatContext{SessionID: "sess-1", RepoID: f.repoID, Provider: f.provider, AccountID: accountID, FromLabel: f.fromLabel}, nil
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
		AuthFailedSince: func(_ string) (time.Time, bool) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.authSince, f.authFailed
		},
		AuthProbe: func(_ context.Context, _ string) AuthProbeResult {
			f.mu.Lock()
			f.authProbeCalls++
			res := f.authResult
			entered, release := f.probeEntered, f.probeRelease
			f.probeEntered, f.probeRelease = nil, nil
			f.mu.Unlock()
			if entered != nil {
				entered <- struct{}{}
			}
			if release != nil {
				<-release
			}
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
			err := f.switchErr
			if req.RespawnSameAccount && f.respawnSwitchErr != nil {
				err = f.respawnSwitchErr
			}
			f.mu.Unlock()
			return SwitchResult{SwitchedToLabel: "acct-b"}, err
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
	// chat-A held nothing but its rate-limit stamp, so evicting that stamp also
	// reclaims its whole chats entry — the map does not keep a husk per chat either.
	if _, ok := r.chats["chat-A"]; ok {
		t.Error("stale chat-A rate-limit entry was not evicted after its window elapsed")
	}
	if lastAttemptOf(r, "chat-B").IsZero() {
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

func (s *lockedAuditStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inserted)
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

func TestChatRotator_OnAuthDecisionCompleteRunsAfterAuditRecord(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{
		authFailed: true,
		authResult: AuthProbeConfirmed401,
		decision:   Decision{Kind: DecisionStatusOnly},
	}
	store := &lockedAuditStore{}
	r := f.authRotator(&now, store)
	done := make(chan int, 1)
	r.deps.OnAuthDecisionComplete = func(string) {
		done <- store.count()
	}

	r.OnAuthFailed(testChatID)
	select {
	case got := <-done:
		if got != 1 {
			t.Fatalf("audit records visible to callback = %d, want 1", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for auth decision completion callback")
	}
	waitIdle(t, r)
}

func TestChatRotator_OnAuthDecisionCompleteRunsAfterScheduledReprobeAudit(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{
		authFailed: true,
		authResult: AuthProbeUnknown,
	}
	store := &lockedAuditStore{}
	r := f.authRotator(&now, store)
	done := make(chan int, 2)
	r.deps.OnAuthDecisionComplete = func(string) {
		done <- store.count()
	}

	r.OnAuthFailed(testChatID)
	select {
	case got := <-done:
		if got != 1 {
			t.Fatalf("initial callback audit records = %d, want 1", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for initial auth decision completion callback")
	}
	fireWhenArmed(t, &f.sched)
	select {
	case got := <-done:
		if got != 2 {
			t.Fatalf("reprobe callback audit records = %d, want 2", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for re-probe auth decision completion callback")
	}
	waitIdle(t, r)
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
	// BOS-981 added a suppression seam for the respawn healer only. A CONFIRMED 401
	// really does invalidate the account, so this path must keep benching its health.
	if f.lastDecideRequest().SuppressHealthFail {
		t.Errorf("confirmed-401 rotation must not suppress the health write")
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

func TestChatRotator_AuthTransientPreAuditFailuresScheduleReprobe(t *testing.T) {
	tests := []struct {
		name       string
		loadErr    error
		chatCtxErr error
	}{
		{name: "config load error", loadErr: errors.New("config unavailable")},
		{name: "chat context error", chatCtxErr: errors.New("chat context unavailable")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now()
			f := &fakeDeps{
				authFailed: true,
				loadErr:    tt.loadErr,
				chatCtxErr: tt.chatCtxErr,
			}
			store := &lockedAuditStore{}
			r := f.authRotator(&now, store)

			r.OnAuthFailed(testChatID)
			waitIdle(t, r)

			if got := store.count(); got != 0 {
				t.Fatalf("pre-audit failure audit records = %d, want 0", got)
			}
			if got := f.sched.pendingCount(); got != 1 {
				t.Fatalf("scheduled auth re-probes = %d, want 1", got)
			}
			if got := f.authProbes(); got != 0 {
				t.Fatalf("auth probes = %d, want 0 before context is available", got)
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

func TestChatRotator_AuthLimiterSuppressionSchedulesReprobe(t *testing.T) {
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
	if got := f.authProbes(); got != 1 {
		t.Fatalf("initial auth probes = %d, want 1", got)
	}
	if got := store.count(); got != 1 {
		t.Fatalf("initial audit records = %d, want 1", got)
	}

	f.mu.Lock()
	f.authResult = AuthProbeUnknown
	f.mu.Unlock()
	now = now.Add(time.Minute)
	r.OnAuthFailed(testChatID)
	waitIdle(t, r)
	if got := f.authProbes(); got != 1 {
		t.Fatalf("rate-limited auth edge probed immediately: probes = %d, want 1", got)
	}
	if n := f.sched.pendingCount(); n != 1 {
		t.Fatalf("rate-limited auth edge pending re-probes = %d, want 1", n)
	}

	if !f.sched.fire() {
		t.Fatal("expected limiter-suppressed auth edge to arm a re-probe")
	}
	waitIdle(t, r)
	if got := f.authProbes(); got != 2 {
		t.Fatalf("re-probe auth probes = %d, want 2", got)
	}
	if out := store.outcomes(); len(out) != 2 || out[1] != "ROTATION_OUTCOME_STATUS_ONLY_PROBE_UNCONFIRMED" {
		t.Fatalf("outcomes after re-probe = %v, want second STATUS_ONLY_PROBE_UNCONFIRMED", out)
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

// healthyStreakOf reports the chat's consecutive-Healthy streak, so a test can assert
// the auth lane really went quiet rather than merely stopped calling Switch.
func healthyStreakOf(r *ChatRotator) healthyStreak {
	r.mu.Lock()
	defer r.mu.Unlock()
	cs := r.lookupChatLocked(testChatID)
	if cs == nil {
		return healthyStreak{}
	}
	return cs.healthy
}

// TestChatRotator_AuthLaneMidTurnAbortsExhaustTheRespawnCap pins the counterpart of
// TestMidTurnAbortsNeverSpendTheRespawnCap: the BOS-982 refund is lane-gated, and the
// BOS-482 auth lane must NOT get it.
//
// The auth lane re-arms scheduleReprobe on every abort and reprobeAuth charges no
// reactive rate limiter, while healthyCountTTL (30m) far outlasts reprobeInterval (5m),
// so a streak at the threshold never decays between re-probes. The respawn cap is
// therefore the only termination condition this lane has. Refund the aborted charge here
// and the cap is never reached, the respawnCapped branch never runs, clearRespawnState
// never fires, and an auth-failed pane that probes Healthy while staying mid-turn polls
// forever at 5-minute cadence — one real upstream AuthProbe plus one Switch attempt per
// pass. That is the loop "a pane that can never respawn cannot loop" rules out.
func TestChatRotator_AuthLaneMidTurnAbortsExhaustTheRespawnCap(t *testing.T) {
	now := time.Now()
	// A non-zero episode anchor is not decoration: it is what makes healthyStreak's
	// episodeSince live at the teardown asserted at the end of this test. With the
	// zero value AuthFailedSince reports by default, a clearRespawnState that
	// preserved episodeSince would preserve a zero and the teardown assertion could
	// not tell the difference. (BOS-980)
	f := &fakeDeps{authFailed: true, authResult: AuthProbeHealthy, switchErr: ErrSwitchAborted, authSince: now.Add(-time.Hour)}
	store := &lockedAuditStore{}
	r := f.authRotator(&now, store)

	r.OnAuthFailed(testChatID) // streak → 1 (below threshold, no respawn yet)
	waitIdle(t, r)

	// The clock deliberately never advances: healthyCountTTL cannot decay the streak
	// between re-probes in production either, which is what makes the cap load-bearing.
	for i := 0; i < respawnCap; i++ {
		if !f.sched.fire() {
			t.Fatalf("abort %d: expected an armed re-probe", i)
		}
		waitIdle(t, r)
		if got := len(f.switched()); got != i+1 {
			t.Fatalf("abort %d: Switch attempts = %d, want %d", i, got, i+1)
		}
		if w := respawnWindowOf(r); w.count != i+1 {
			t.Fatalf("abort %d: respawn cap spend = %d, want %d — the auth lane must KEEP an aborted charge, or the cap it terminates on is never reached",
				i, w.count, i+1)
		}
	}

	// The budget is spent. The next re-probe must find the cap exhausted.
	if !f.sched.fire() {
		t.Fatal("expected an armed re-probe before the capped attempt")
	}
	waitIdle(t, r)

	if got := len(f.switched()); got != respawnCap {
		t.Fatalf("Switch attempts = %d, want respawnCap=%d — the capped attempt must not reach Switch", got, respawnCap)
	}
	out := store.outcomes()
	if len(out) == 0 || out[len(out)-1] != outcomeRespawnCapExhausted {
		t.Fatalf("outcomes = %v, want the last to be %s", out, outcomeRespawnCapExhausted)
	}
	// Going quiet is the point: OnAuthFailed is edge-triggered, so with the streak
	// dropped and no timer armed nothing re-enters the lane until a fresh edge.
	if n := f.sched.pendingCount(); n != 0 {
		t.Fatalf("pending re-probe timers after cap-exhausted = %d, want 0 — the auth lane must stop polling", n)
	}
	// Whole-struct, not field-by-field: healthyStreak is comparable, so this stays
	// correct as it grows. Three of its four fields are live here — count and at from
	// the confirmations above, episodeSince from the non-zero authSince this test sets
	// — so a clearRespawnState that preserved any one of them fails this compare.
	//
	// The fourth, clearedAt, is provably zero at EVERY cap-exhausted teardown rather
	// than merely at this one: only the auth lane clears respawn state on the capped
	// path (respawnInPlace gates it on lane.ownsRespawnState), and that lane always
	// arrives via handleHealthyAuthProbe, which zeroes clearedAt on the very
	// confirmation that reaches the threshold. Its teardown therefore cannot be pinned
	// from here; TestChatRotator_SustainedClearEndsEpisode pins it instead, on the
	// grace-window path. That is not the only teardown where clearedAt can be live:
	// the success path (clearRespawnState "respawned in place") is deliberately
	// ungated, and the opt-out (clearRespawnState "auto-rotation opted out") returns
	// before markEpisodeLive can clear a stamp an earlier pass's holdEpisode left
	// standing. The grace-window path is simply the one the suite pins it on. (BOS-980)
	if h := healthyStreakOf(r); h != (healthyStreak{}) {
		t.Fatalf("healthy streak after cap-exhausted = %+v, want torn down", h)
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
	// BOS-981: the abort path keeps its charge — a mid-turn refusal is deliberately NOT
	// treated as "never attempted", so the refund does not apply here.
	if got := respawnCharges(r); got != 1 {
		t.Fatalf("respawn charges after abort = %d, want 1 (abort keeps its charge)", got)
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
	// BOS-981: an error that is not tagged "refused before the pane was touched" is an
	// attempt that may have disturbed the chat, so it keeps its charge.
	if got := respawnCharges(r); got != 1 {
		t.Fatalf("respawn charges after a generic failure = %d, want 1 (a real attempt still charges)", got)
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

// --- BOS-980: auth-failed episode latch -------------------------------------------

// Test-only aliases so the external rotation_test package (which CAN import
// internal/status, unlike this in-package test file — status → db → rotation would be a
// cycle) can pin the duplicated tracker constants against their originals.
var (
	AuthPollIntervalForTest     = authPollInterval
	AuthRisePollsForTest        = authRisePolls
	AuthRetryCycleForTest       = authRetryCycle
	AuthClearGraceWindowForTest = authClearGraceWindow
)

// TestChatRotator_LatchedEpisodeSurvivesTransientClearReading is the in-package companion
// to the end-to-end proof: a re-probe that reads clean inside the grace window must keep
// the streak and re-arm, not record RECOVERED. (BOS-980 AC1)
func TestChatRotator_LatchedEpisodeSurvivesTransientClearReading(t *testing.T) {
	now := time.Now()
	episode := now
	f := &fakeDeps{authFailed: true, authResult: AuthProbeHealthy, authSince: episode}
	store := &lockedAuditStore{}
	r := f.authRotator(&now, store)

	r.OnAuthFailed(testChatID)
	waitIdle(t, r)
	if n := f.sched.pendingCount(); n != 1 {
		t.Fatalf("pending re-probe timers = %d, want 1", n)
	}

	// The pane flaps clean for a single reading, well inside the grace window.
	f.mu.Lock()
	f.authFailed = false
	f.mu.Unlock()
	now = now.Add(authPollInterval)
	if !f.sched.fire() {
		t.Fatal("expected an armed re-probe to fire")
	}
	waitIdle(t, r)

	for _, o := range store.outcomes() {
		if o == "ROTATION_OUTCOME_STATUS_ONLY_RECOVERED" {
			t.Fatalf("a clean reading inside the grace window ended the episode: %v", store.outcomes())
		}
	}
	if n := f.sched.pendingCount(); n != 1 {
		t.Fatalf("held episode must stay armed, pending timers = %d, want 1", n)
	}

	// The banner returns and the re-armed timer fires: the preserved streak reaches the
	// threshold and the healer respawns, which it could not do had the streak been reset.
	f.mu.Lock()
	f.authFailed = true
	f.mu.Unlock()
	now = now.Add(authPollInterval)
	if !f.sched.fire() {
		t.Fatal("expected the re-armed re-probe to fire")
	}
	waitIdle(t, r)

	calls := f.switched()
	if len(calls) != 1 || !calls[0].RespawnSameAccount {
		t.Fatalf("want exactly one respawn-in-place switch, got %+v", calls)
	}
}

// TestChatRotator_SustainedClearEndsEpisode is the other half of the latch: a pane that
// stays clear past authClearGraceWindow really has recovered, so the episode ends with
// STATUS_ONLY_RECOVERED and clearRespawnState runs (streak dropped, timer cancelled).
// (BOS-980 AC2)
func TestChatRotator_SustainedClearEndsEpisode(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{authFailed: true, authResult: AuthProbeHealthy, authSince: now}
	store := &lockedAuditStore{}
	r := f.authRotator(&now, store)

	r.OnAuthFailed(testChatID)
	waitIdle(t, r)

	// First clean reading: held, and re-armed at the grace window.
	f.mu.Lock()
	f.authFailed = false
	f.mu.Unlock()
	if !f.sched.fire() {
		t.Fatal("expected an armed re-probe to fire")
	}
	waitIdle(t, r)
	if n := f.sched.pendingCount(); n != 1 {
		t.Fatalf("first clean reading must re-arm, pending timers = %d, want 1", n)
	}

	// The grace window elapses with the pane still clear: this reading is believed.
	now = now.Add(authClearGraceWindow)
	if !f.sched.fire() {
		t.Fatal("expected the grace-window re-probe to fire")
	}
	waitIdle(t, r)

	out := store.outcomes()
	if len(out) == 0 || out[len(out)-1] != "ROTATION_OUTCOME_STATUS_ONLY_RECOVERED" {
		t.Fatalf("outcomes = %v, want a trailing STATUS_ONLY_RECOVERED", out)
	}
	// A grace-window EXPIRY must not be indistinguishable from a pane that read clean on
	// its very first reading: authClearGraceWindow is derived from a countdown bossd does
	// not control, so "the latch held and then let go" has to be greppable if the window
	// ever turns out to be too short. (BOS-980)
	det := store.details()
	if len(det) == 0 || det[len(det)-1] != "pane sustainedly clear for the auth-failed grace window" {
		t.Fatalf("details = %v, want a trailing grace-window-expiry detail", det)
	}
	if n := f.sched.pendingCount(); n != 0 {
		t.Fatalf("clearRespawnState must cancel the re-probe, pending timers = %d, want 0", n)
	}
	if len(f.switched()) != 0 {
		t.Fatalf("a recovered pane must not be respawned, got %+v", f.switched())
	}
	// Whole-struct teardown, asserted HERE because all four healthyStreak fields are
	// live when clearRespawnState runs on this path: the confirmation set
	// count/at/episodeSince (authSince is non-zero above), and the first clean reading
	// set clearedAt, which holdEpisode leaves standing when the window expires. This is
	// where the suite pins clearedAt's teardown — it cannot be pinned from
	// TestChatRotator_AuthLaneMidTurnAbortsExhaustTheRespawnCap, where clearedAt is
	// provably already zero (see the comment there). Other teardowns can also run with
	// it live (the ungated success path, the opt-out branch); this is the one pinned.
	// A leaked clearedAt is not cosmetic: it is the grace window's origin, so the next
	// episode's first clean reading would be measured against a stale one and could
	// expire the window instantly. (BOS-980)
	if h := healthyStreakOf(r); h != (healthyStreak{}) {
		t.Fatalf("healthy streak after sustained-clear recovery = %+v, want torn down", h)
	}
	// clearRespawnState really dropped the streak: a fresh edge starts at 1 again, so the
	// very next probe must not immediately hit the threshold.
	f.mu.Lock()
	f.authFailed = true
	f.mu.Unlock()
	now = now.Add(time.Minute)
	r.OnAuthFailed(testChatID)
	waitIdle(t, r)
	if len(f.switched()) != 0 {
		t.Fatalf("streak was not reset: a single probe after recovery respawned, got %+v", f.switched())
	}
}

// TestChatRotator_SuppressedAuthEdgeClearsTheGraceWindow pins the ordering that reaches the
// latch through the back door. A re-asserted marker fires OnAuthFailed, and inside the
// per-chat rate-limit window (10m by default, far longer than the grace window) that
// edge is SUPPRESSED: it never reaches rotateAuth, so it never reaches markEpisodeLive,
// yet it does re-arm the ordinary reprobeInterval timer over the pending grace timer. If
// the suppressed branch leaves clearedAt behind, the next clean reading is measured
// against a stamp minutes old, falls outside the window, and ends the episode — the exact
// STATUS_ONLY_RECOVERED regression BOS-980 exists to remove, on the exact timeline it
// targets (the marker re-asserting is what the grace window is WAITING for).
func TestChatRotator_SuppressedAuthEdgeClearsTheGraceWindow(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{authFailed: true, authResult: AuthProbeHealthy, authSince: now}
	store := &lockedAuditStore{}
	r := f.authRotator(&now, store)

	r.OnAuthFailed(testChatID)
	waitIdle(t, r)
	if n := f.sched.pendingCount(); n != 1 {
		t.Fatalf("pending re-probe timers = %d, want 1", n)
	}

	// The ordinary re-probe lands in a trough: held, re-armed at the grace window.
	now = now.Add(reprobeInterval)
	f.mu.Lock()
	f.authFailed = false
	f.mu.Unlock()
	if !f.sched.fire() {
		t.Fatal("expected an armed re-probe to fire")
	}
	waitIdle(t, r)
	if n := f.sched.pendingCount(); n != 1 {
		t.Fatalf("first clean reading must re-arm, pending timers = %d, want 1", n)
	}

	// The marker re-asserts two polls later. Still inside the rate-limit window, so this
	// edge is suppressed and only re-arms the cadence timer.
	now = now.Add(authPollInterval * authRisePolls)
	f.mu.Lock()
	f.authFailed = true
	f.mu.Unlock()
	r.OnAuthFailed(testChatID)
	waitIdle(t, r)
	if calls := f.switched(); len(calls) != 0 {
		t.Fatalf("a rate-limited edge must not rotate, got %+v", calls)
	}
	if n := f.sched.pendingCount(); n != 1 {
		t.Fatalf("suppressed edge must leave exactly one timer armed, got %d", n)
	}

	// A later re-probe lands in a second trough. Because the re-assertion cleared the
	// grace clock, this is a FIRST clean reading of the still-open episode and must be
	// held — not measured against the first trough minutes ago.
	now = now.Add(reprobeInterval)
	f.mu.Lock()
	f.authFailed = false
	f.mu.Unlock()
	if !f.sched.fire() {
		t.Fatal("expected the re-armed re-probe to fire")
	}
	waitIdle(t, r)
	for _, o := range store.outcomes() {
		if o == "ROTATION_OUTCOME_STATUS_ONLY_RECOVERED" {
			t.Fatalf("a suppressed edge left a stale grace clock and ended the episode: %v", store.outcomes())
		}
	}
	if n := f.sched.pendingCount(); n != 1 {
		t.Fatalf("the re-held episode must stay armed, pending timers = %d, want 1", n)
	}

	// And the streak really survived: the banner returns, the held timer fires, and the
	// preserved streak reaches the threshold.
	now = now.Add(authPollInterval)
	f.mu.Lock()
	f.authFailed = true
	f.mu.Unlock()
	if !f.sched.fire() {
		t.Fatal("expected the grace re-probe to fire")
	}
	waitIdle(t, r)
	if calls := f.switched(); len(calls) != 1 || !calls[0].RespawnSameAccount {
		t.Fatalf("want exactly one respawn-in-place switch, got %+v", calls)
	}
}

// TestChatRotator_RespawnDoesNotResurrectItsOwnEpisode covers the named risk: respawn-in-place
// reuses the tmux pane WITHOUT clearing the screen, so the pre-respawn login banner is still
// in statusdetect's 20-line tail afterwards and the tracker's marker — and therefore the
// AuthFailedSince anchor — never falls. The latch must not read that stale screen text as
// continuing confirmation and loop the healer until the respawn cap is exhausted: the
// post-respawn streak must restart from 1 within the same unchanged episode. (BOS-980)
func TestChatRotator_RespawnDoesNotResurrectItsOwnEpisode(t *testing.T) {
	now := time.Now()
	episode := now
	// authFailed never goes false and authSince never advances — exactly the observed
	// live behaviour of a pane whose banner survives its own respawn.
	f := &fakeDeps{authFailed: true, authResult: AuthProbeHealthy, authSince: episode}
	store := &lockedAuditStore{}
	r := f.authRotator(&now, store)

	r.OnAuthFailed(testChatID)
	waitIdle(t, r)
	if !f.sched.fire() {
		t.Fatal("expected an armed re-probe to fire")
	}
	waitIdle(t, r)
	if len(f.switched()) != 1 {
		t.Fatalf("switch calls = %d, want 1 (the first respawn)", len(f.switched()))
	}
	// The success path cleared the streak and its timer even though the pane still reads
	// auth-failed, so there is nothing left to fire.
	if n := f.sched.pendingCount(); n != 0 {
		t.Fatalf("pending timers after respawn = %d, want 0", n)
	}

	// The stale banner produces another auth-failed edge. The episode anchor is unchanged,
	// but the streak restarts, so one probe must NOT respawn again.
	now = now.Add(time.Minute)
	r.OnAuthFailed(testChatID)
	waitIdle(t, r)
	if got := f.sched.pendingCount(); got != 1 {
		t.Fatalf("fresh edge must re-arm a re-probe, pending timers = %d, want 1", got)
	}
	if len(f.switched()) != 1 {
		t.Fatalf("post-respawn stale banner respawned again immediately: switch calls = %d, want 1", len(f.switched()))
	}
	if since, ok := f.authSinceNow(testChatID); !ok || !since.Equal(episode) {
		t.Fatalf("precondition: AuthFailedSince must not advance across the respawn, got %v (ok=%v)", since, ok)
	}
}

func (f *fakeDeps) authSinceNow(_ string) (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authSince, f.authFailed
}

// TestChatRotator_RepinnedEpisodeAnchorDoesNotResetStreak guards the latch against an
// intuitive-looking "improvement": resetting the streak when status.Tracker.AuthFailedSince
// moves, on the theory that a new anchor means a new wedge. It does not. The tracker
// deletes its marker on the first clean poll, so a pane that merely flapped re-pins a
// FRESH effectiveAt when the rise-debounce next completes — which is precisely the
// population this latch exists to protect. Keying the streak on the anchor would reset it
// on every flap and silently restore the BOS-980 defect. (BOS-980)
func TestChatRotator_RepinnedEpisodeAnchorDoesNotResetStreak(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{authFailed: true, authResult: AuthProbeHealthy, authSince: now}
	r := f.authRotator(&now, nil)

	r.OnAuthFailed(testChatID)
	waitIdle(t, r)

	// The pane flapped clean for a poll and re-wedged: same wedge, brand new anchor.
	now = now.Add(authPollInterval * authRisePolls)
	f.mu.Lock()
	f.authSince = now
	f.mu.Unlock()
	if !f.sched.fire() {
		t.Fatal("expected an armed re-probe to fire")
	}
	waitIdle(t, r)

	calls := f.switched()
	if len(calls) != 1 || !calls[0].RespawnSameAccount {
		t.Fatalf("a re-pinned anchor reset the streak; want one respawn-in-place, got %+v", calls)
	}
}

// TestChatRotator_LogsStreakThresholdAndReset pins BOS-980 AC5: the daemon log must
// distinguish "streak advanced to threshold" from "streak reset". Before this change
// healthy_streak was logged only in the below-threshold branch, where — with a threshold
// of 2 — the value 1 is the only one that can ever print.
func TestChatRotator_LogsStreakThresholdAndReset(t *testing.T) {
	var buf bytes.Buffer
	now := time.Now()
	f := &fakeDeps{authFailed: true, authResult: AuthProbeHealthy, authSince: now}
	r := NewChatRotator(ChatRotatorDeps{
		Logger:     zerolog.New(&buf),
		LoadConfig: func() (config.ManagedAccountsConfig, error) { return f.cfg, nil },
		ChatContext: func(_ context.Context, _ string) (ChatContext, error) {
			return ChatContext{SessionID: "sess-1", RepoID: "repo-1", Provider: "claude", AccountID: "acct-capped"}, nil
		},
		CurrentStatus:     func(_ string) bossanovav1.ChatStatus { return f.status },
		CurrentAuthFailed: func(_ string) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.authFailed },
		AuthFailedSince:   f.authSinceNow,
		AuthProbe:         func(_ context.Context, _ string) AuthProbeResult { return AuthProbeHealthy },
		Schedule:          f.sched.schedule,
		Switch: func(_ context.Context, req SwitchRequest) (SwitchResult, error) {
			f.mu.Lock()
			f.switchCalls = append(f.switchCalls, req)
			f.mu.Unlock()
			return SwitchResult{SwitchedToLabel: "acct-capped"}, nil
		},
		Now: func() time.Time { return now },
	})

	r.OnAuthFailed(testChatID)
	waitIdle(t, r)
	if !f.sched.fire() {
		t.Fatal("expected an armed re-probe to fire")
	}
	waitIdle(t, r)

	logged := buf.String()
	if !strings.Contains(logged, "awaiting confirmation before respawn") ||
		!strings.Contains(logged, `"healthy_streak":1`) {
		t.Fatalf("missing the below-threshold streak line in:\n%s", logged)
	}
	if !strings.Contains(logged, "healthy streak reached threshold") ||
		!strings.Contains(logged, `"healthy_streak":2`) {
		t.Fatalf("missing the at-threshold streak line (AC5) in:\n%s", logged)
	}
	if !strings.Contains(logged, "healthy streak reset") ||
		!strings.Contains(logged, `"healthy_streak_reset_from":2`) {
		t.Fatalf("missing the streak-reset line (AC5) in:\n%s", logged)
	}
	if !strings.Contains(logged, `"healthy_streak_threshold":2`) {
		t.Fatalf("streak lines must carry the threshold they are measured against:\n%s", logged)
	}
}

// chatState field-count guard (see chatState.isZero).
//
// isZero is written field by field because chatState holds a func() and is
// therefore non-comparable — `*s == chatState{}` does not compile. That hand-written
// predicate is silently incomplete the moment a field is added: Go does not error on
// a struct field nothing reads, and no linter configured here checks field coverage.
// These counts are the guard that turns "the compiler will catch it" from a false
// promise into a real one.
const (
	// chatStateFields is the number of fields chatState declares. Bump it ONLY
	// together with the work TestChatStateFieldCountIsGuarded describes.
	chatStateFields = 9
	// healthyStreakFields and respawnWindowFields cover the nested lane structs
	// isZero reaches into field by field for the same reason.
	healthyStreakFields = 4
	respawnWindowFields = 2
)

// TestChatStateFieldCountIsGuarded fails when a field is added to chatState (or to
// one of the lane structs isZero destructures), because nothing else will.
//
// A field isZero does not check reads as zero forever, so gcChatLocked reclaims an
// entry that still holds LIVE state for the new lane — a premature reclaim that
// silently drops a pending re-probe, an in-flight marker or a rate-limit stamp. The
// mirror failure, a field kept out of the reset paths, leaks entries for chats no
// lane tracks. Both are invisible at compile time and at vet time.
func TestChatStateFieldCountIsGuarded(t *testing.T) {
	for _, tc := range []struct {
		typ  reflect.Type
		want int
		name string
	}{
		{reflect.TypeOf(chatState{}), chatStateFields, "chatStateFields"},
		{reflect.TypeOf(healthyStreak{}), healthyStreakFields, "healthyStreakFields"},
		{reflect.TypeOf(respawnWindow{}), respawnWindowFields, "respawnWindowFields"},
	} {
		// t.Errorf, not t.Fatalf: a change that adds a field to chatState AND to one of
		// the lane structs trips more than one row, and stopping at the first would have
		// the author fix one constant, re-run, and be surprised again.
		if got := tc.typ.NumField(); got != tc.want {
			t.Errorf(`%s has %d fields, but the guard constant %s says %d.

Extend (*chatState).isZero to cover the change — for an added field, re-read
gcChatLocked's reasoning too: a field isZero does not test makes the entry look
reclaimable while that lane still holds live state, and the lane that owns the field
must zero it and call gcChatLocked when it is done. Only then set %s = %d.`,
				tc.typ.Name(), got, tc.name, tc.want, tc.name, got)
		}
	}
}

// declaredRespawnLanes names every respawnLane the package defines, so
// TestRespawnLanePredicatesAreConsistent can hold the lane table to the rule its field
// docs argue in prose.
//
// Go exposes no reflection over a package's variables, so unlike
// TestChatStateFieldCountIsGuarded's field set this map cannot be derived at run time.
// TestRespawnLanesAreAllListed is what keeps it fail-closed anyway: it reads this
// package's own sources, so a third lane that is added and not listed here fails that
// test rather than silently going unchecked.
var declaredRespawnLanes = map[string]respawnLane{
	"authRespawnLane":       authRespawnLane,
	"proxyTokenRespawnLane": proxyTokenRespawnLane,
}

// TestRespawnLanePredicatesAreConsistent pins the one lane-predicate combination that is
// never safe: reprobe + refundAbortedCharge. That pairing IS the BOS-982 regression this
// guard exists for — a lane that re-arms an unmetered re-probe timer on every abort while
// handing its cap charge back has no termination condition left, so it polls the pane
// forever at reprobeInterval. A lane may refund an aborted charge only when something
// OTHER THAN THE CAP bounds its re-entry, and a re-arming timer is not that.
func TestRespawnLanePredicatesAreConsistent(t *testing.T) {
	// The count guard, in the same spirit as chatStateFields: the rule below covers the
	// predicates respawnLane declares TODAY, and a new one may well need a rule of its
	// own. Nothing but this would say so.
	if got := reflect.TypeOf(respawnLane{}).NumField(); got != respawnLaneFields {
		t.Errorf(`respawnLane has %d fields, but the guard constant respawnLaneFields says %d.

A new field on the lane struct may need its own consistency rule here — decide that
first, then set respawnLaneFields = %d.`, got, respawnLaneFields, got)
	}
	for name, lane := range declaredRespawnLanes {
		if lane.reprobe && lane.refundAbortedCharge {
			t.Errorf(`%s sets both reprobe and refundAbortedCharge.

A lane that re-arms its own re-probe timer on every abort and ALSO refunds the cap
charge has nothing left to stop it: the timer is unmetered (reprobeAuth charges no
reactive limiter) and the cap it would otherwise exhaust is never reached, so the lane
re-enters every reprobeInterval forever. Refund only where a rate limiter or the pane's
own next edge bounds re-entry — see respawnLane.refundAbortedCharge.`, name)
		}
		if lane.reprobe && !lane.handlesNotAttempted {
			t.Errorf(`%s re-arms its own re-probe timer but runs no not-attempted budget.

The ErrSwitchNotAttempted refund is unconditional on every lane, so on a lane that
re-arms an unmetered timer it removes the respawn cap without replacing it: a refusal
that persists — a chat row that stays unreadable, a store that stays unconfigured — is
retried every reprobeInterval forever and never parks, so the attention banner never
fires. chargeNotAttempt is that replacement bound. A lane may only decline it when
something outside the lane paces re-entry, which is the same condition
refundAbortedCharge states — see respawnLane.handlesNotAttempted.`, name)
		}
	}
}

// respawnLaneFields is the number of fields respawnLane declares. Bump it ONLY
// together with the work TestRespawnLanePredicatesAreConsistent describes.
const respawnLaneFields = 7

// TestRespawnLanesAreAllListed is what makes declaredRespawnLanes fail-closed. It reads
// this package's non-test sources and requires every respawnLane composite literal in
// them to be the initialiser of a package-level var that declaredRespawnLanes lists, so
// a third lane cannot be added and quietly skip TestRespawnLanePredicatesAreConsistent.
//
// It scans source because there is no run-time alternative: Go has no reflection over a
// package's variables, and a bare slice literal of the lanes we happen to remember is
// exactly the guard that looks stronger than it is.
//
// The staging matches TestNoRawDoChanCallInThisPackage, which scans this same package:
// rules_go compiles the library's sources without putting them in the test's runfiles,
// so this package's BUILD.bazel carries a kept `data` glob of them. The zero-file guard
// below is what stops a mis-pointed scan from passing blind.
func TestRespawnLanesAreAllListed(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed, so this guard cannot locate its own package")
	}
	dir, err := detachgate.ResolvePackageDir(thisFile)
	if err != nil {
		t.Fatalf("locate the rotation package sources: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	scanned := 0
	declared := map[string]bool{}
	var unnamed []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++
		named := map[token.Pos]bool{}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, v := range value.Values {
					if i >= len(value.Names) || !isRespawnLaneLiteral(v) {
						continue
					}
					named[v.Pos()] = true
					declared[value.Names[i].Name] = true
				}
			}
		}
		// Anything left is a lane built somewhere no name can reach — an inline literal
		// at a call site. No table can list it, so it is reported rather than ignored.
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !isRespawnLaneLiteral(lit) || named[lit.Pos()] {
				return true
			}
			unnamed = append(unnamed, fset.Position(lit.Pos()).String())
			return true
		})
	}
	if scanned == 0 {
		t.Fatal("scanned 0 non-test Go files, so a clean result proves nothing — this guard is no longer reading its own package")
	}
	t.Logf("scanned %d non-test Go files in this package", scanned)
	for name := range declared {
		if _, listed := declaredRespawnLanes[name]; !listed {
			t.Errorf("%s is a respawnLane this package declares but declaredRespawnLanes does not list; add it so TestRespawnLanePredicatesAreConsistent holds it to the refund rule", name)
		}
	}
	for name := range declaredRespawnLanes {
		if !declared[name] {
			t.Errorf("declaredRespawnLanes lists %q, which this package no longer declares as a package-level respawnLane var; drop it", name)
		}
	}
	for _, pos := range unnamed {
		t.Errorf("the respawnLane literal at %s is not a package-level var, so no lane table can reach it; declare it as one and list it in declaredRespawnLanes", pos)
	}
}

// isRespawnLaneLiteral reports whether expr is a respawnLane composite literal.
func isRespawnLaneLiteral(expr ast.Expr) bool {
	lit, ok := expr.(*ast.CompositeLit)
	if !ok {
		return false
	}
	ident, ok := lit.Type.(*ast.Ident)
	return ok && ident.Name == "respawnLane"
}

// nonZeroChatStateFields maps every LEAF field reachable from chatState — qualified
// through the nested lane structs, e.g. "healthy.clearedAt" — to a setter that makes
// THAT field non-zero and touches nothing else. TestChatStateIsZeroSeesEveryField holds
// it to the reflected leaf set, so adding a field forces an entry here, and the entry
// then forces (*chatState).isZero to actually look at the field.
//
// Leaves, not top-level fields, because isZero reads leaves: a field added INSIDE
// healthyStreak or respawnWindow drops out of the predicate exactly as silently as one
// added to chatState itself. BOS-980 adding episodeSince and clearedAt to healthyStreak
// is the realistic shape of that change, so covering only the outer struct would leave
// the likeliest case guarded by a bare count.
var nonZeroChatStateFields = map[string]func(*chatState){
	"inFlight":             func(s *chatState) { s.inFlight = true },
	"lastAttempt":          func(s *chatState) { s.lastAttempt = time.Unix(1, 0) },
	"proactiveLastAttempt": func(s *chatState) { s.proactiveLastAttempt = time.Unix(1, 0) },
	"healthy.count":        func(s *chatState) { s.healthy.count = 1 },
	"healthy.at":           func(s *chatState) { s.healthy.at = time.Unix(1, 0) },
	"healthy.episodeSince": func(s *chatState) { s.healthy.episodeSince = time.Unix(1, 0) },
	"healthy.clearedAt":    func(s *chatState) { s.healthy.clearedAt = time.Unix(1, 0) },
	"respawns.count":       func(s *chatState) { s.respawns.count = 1 },
	"respawns.windowStart": func(s *chatState) { s.respawns.windowStart = time.Unix(1, 0) },
	"notAttempts.count":    func(s *chatState) { s.notAttempts.count = 1 },
	"notAttempts.windowStart": func(s *chatState) {
		s.notAttempts.windowStart = time.Unix(1, 0)
	},
	"proxyRepairPending": func(s *chatState) { s.proxyRepairPending = true },
	"proxyRepairSettled": func(s *chatState) { s.proxyRepairSettled = true },
	"reprobeCancel":      func(s *chatState) { s.reprobeCancel = func() {} },
}

// chatStateLeafFields collects the qualified name of every leaf field reachable from
// typ, descending into nested structs and treating time.Time as a leaf (its unexported
// internals are not state any lane sets).
func chatStateLeafFields(typ reflect.Type, prefix string, out *[]string) {
	timeType := reflect.TypeOf(time.Time{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		name := prefix + f.Name
		if f.Type.Kind() == reflect.Struct && f.Type != timeType {
			chatStateLeafFields(f.Type, name+".", out)
			continue
		}
		*out = append(*out, name)
	}
}

// TestChatStateIsZeroSeesEveryField is the mechanical half of the guard that
// TestChatStateFieldCountIsGuarded only counts.
//
// The count alone catches an added field but not the wrong fix it invites: an author who
// adds a field and simply bumps chatStateFields gets a green suite with isZero still
// blind to the new field — exactly the premature reclaim gcChatLocked's reasoning warns
// about, where an entry that still holds live state for the new lane is dropped. Here a
// new field has no setter, which fails; adding the setter then fails again until isZero
// looks at the field. Extending isZero becomes the only way to go green.
func TestChatStateIsZeroSeesEveryField(t *testing.T) {
	var leaves []string
	chatStateLeafFields(reflect.TypeOf(chatState{}), "", &leaves)
	declared := make(map[string]bool, len(leaves))
	for _, name := range leaves {
		declared[name] = true
		set, ok := nonZeroChatStateFields[name]
		if !ok {
			t.Errorf("chatState.%s has no entry in nonZeroChatStateFields; add one that sets only that field, then make sure (*chatState).isZero checks it", name)
			continue
		}
		var s chatState
		set(&s)
		if s.isZero() {
			t.Errorf("isZero() = true for a chatState with only %s set — gcChatLocked would reclaim an entry that still holds live state for that lane. Extend (*chatState).isZero to cover %s.", name, name)
		}
	}
	for name := range nonZeroChatStateFields {
		if !declared[name] {
			t.Errorf("nonZeroChatStateFields has an entry for %q, which chatState no longer declares; drop it", name)
		}
	}
	// What the loop above CANNOT catch is a predicate that has stopped discriminating:
	// an isZero hard-wired to return false passes every row. Anchor the other direction
	// so that degenerate case fails. (A no-op setter is already caught by the loop
	// itself — it leaves s zero, so the real isZero returns true and the row fails.)
	var empty chatState
	if !empty.isZero() {
		t.Error("isZero() = false for a zero chatState; the predicate no longer matches an absent entry")
	}
}

// disabledTargetRefusal is the error the main.go Switch adapter produces for the
// observed BOS-981 failure: SwitchAccount refused the same-account respawn before it
// touched the pane because the bound account is disabled. Shape and wrapping match
// mapRotationSwitchError exactly (see TestMapRotationSwitchError in services/bossd/cmd).
func disabledTargetRefusal() error {
	return fmt.Errorf("%w: %w: target account is disabled: account %q is disabled",
		ErrSwitchNotAttempted, ErrSwitchAccountIneligible, "agent.yuki@kamik.ai")
}

// notAttemptCharges reports how many pre-pane-touch refusals are currently charged
// against testChatID's separate not-attempt budget — the retry bound that took over
// once refunds removed the respawn cap as the stopping condition.
func notAttemptCharges(r *ChatRotator) int {
	return notAttemptWindowOf(r).count
}

// notAttemptWindowOf is respawnWindowOf (proxy_token_repair_test.go) for the separate
// not-attempt budget: the whole window, so a test can tell "unspent" from "spent in a
// window that has since rolled over".
func notAttemptWindowOf(r *ChatRotator) respawnWindow {
	r.mu.Lock()
	defer r.mu.Unlock()
	cs := r.lookupChatLocked(testChatID)
	if cs == nil {
		return respawnWindow{}
	}
	return cs.notAttempts
}

// TestChatRotator_RespawnRefusedBeforePaneTouchDoesNotChargeCap is the BOS-981 proof.
// A respawn-in-place whose Switch is refused BEFORE the pane is touched (here: the
// bound account is disabled, the observed production failure) must not spend a life
// from the per-chat cap, and must not be recorded as ROTATION_OUTCOME_FAILED.
//
// Fails on main: there every refusal keeps its charge and writes a FAILED row, so two
// attempts exhaust respawnCap and the chat goes quiet having respawned zero times.
func TestChatRotator_RespawnRefusedBeforePaneTouchDoesNotChargeCap(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{
		authFailed:       true,
		authResult:       AuthProbeHealthy,
		fromLabel:        "agent.yuki@kamik.ai",
		respawnSwitchErr: disabledTargetRefusal(),
		decision:         Decision{Kind: DecisionNoEligibleAccount},
	}
	store := &lockedAuditStore{}
	r := f.authRotator(&now, store)

	r.OnAuthFailed(testChatID) // streak → 1
	waitIdle(t, r)
	if !f.sched.fire() { // streak → 2 → respawn attempt → refused before pane touch
		t.Fatal("expected an armed re-probe")
	}
	waitIdle(t, r)

	// (a) The cap is untouched: nothing was consumed, so nothing is charged.
	if got := respawnCharges(r); got != 0 {
		t.Fatalf("respawn charges after a pre-pane-touch refusal = %d, want 0 (cap must not burn)", got)
	}
	// (b) The refusal is not a failure.
	for _, o := range store.outcomes() {
		if o == "ROTATION_OUTCOME_FAILED" {
			t.Fatalf("pre-pane-touch refusal recorded ROTATION_OUTCOME_FAILED; outcomes = %v", store.outcomes())
		}
	}
	// Exactly one respawn attempt was made (and it was the same-account respawn).
	sw := f.switched()
	if len(sw) != 1 || !sw[0].RespawnSameAccount {
		t.Fatalf("switch calls = %+v, want exactly one RespawnSameAccount attempt", sw)
	}
}

// TestChatRotator_RespawnNotAttemptedRecordsItsOwnOutcome pins the non-ineligible
// pre-pane-touch refusal (e.g. the chat row could not be read): a dedicated
// RESPAWN_NOT_ATTEMPTED outcome naming the cause, no cap charge, no engine call — and
// a RETRY, because those causes are transient and a refunded charge must not turn a
// one-off DB blip into a permanently silent chat.
func TestChatRotator_RespawnNotAttemptedRecordsItsOwnOutcome(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{
		authFailed:       true,
		authResult:       AuthProbeHealthy,
		respawnSwitchErr: fmt.Errorf("%w: get agent chat agent-session-1: boom", ErrSwitchNotAttempted),
	}
	store := &lockedAuditStore{}
	r := f.authRotator(&now, store)

	r.OnAuthFailed(testChatID)
	waitIdle(t, r)
	if !f.sched.fire() {
		t.Fatal("expected an armed re-probe")
	}
	waitIdle(t, r)

	out := store.outcomes()
	if last := out[len(out)-1]; last != outcomeRespawnNotAttempted {
		t.Fatalf("final outcome = %q, want %q (outcomes = %v)", last, outcomeRespawnNotAttempted, out)
	}
	det := store.details()[len(store.details())-1]
	if !strings.Contains(det, "boom") {
		t.Fatalf("final detail = %q, want it to name the actual cause", det)
	}
	// The sentinel's own text only restates the sentence the Detail opens with.
	if strings.Contains(det, ErrSwitchNotAttempted.Error()) {
		t.Errorf("final detail = %q, want the redundant sentinel text stripped", det)
	}
	if got := respawnCharges(r); got != 0 {
		t.Fatalf("respawn charges = %d, want 0", got)
	}
	// A refusal that is not the account being ineligible has no rotation remedy, so it
	// must not consult the engine.
	if f.decides() != 0 {
		t.Fatalf("Decide calls = %d, want 0 (no rotation remedy for a non-account refusal)", f.decides())
	}
	// It must stay on the re-probe cadence: OnAuthFailed is edge-triggered and the pane
	// is already auth-failed, so cancelling the timer here would wedge the chat until
	// the daemon restarts.
	if n := f.sched.pendingCount(); n != 1 {
		t.Fatalf("pending re-probe timers = %d, want 1 (a transient refusal must retry)", n)
	}
}

// TestChatRotator_RespawnNotAttemptedRetriesThenParksVisibly pins the stopping
// condition for that retry. Refunding the respawn charge takes the respawn cap out of
// play, so these refusals carry their own per-chat budget; once it is spent the chat
// parks on RESPAWN_CAP_EXHAUSTED — a real proto enum value — rather than on the free
// string, which decodes to UNSPECIFIED and is explicitly ignored by the auth-failure
// corroboration that raises the attention banner.
func TestChatRotator_RespawnNotAttemptedRetriesThenParksVisibly(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{
		authFailed:       true,
		authResult:       AuthProbeHealthy,
		respawnSwitchErr: fmt.Errorf("%w: get agent chat agent-session-1: boom", ErrSwitchNotAttempted),
	}
	store := &lockedAuditStore{}
	r := f.authRotator(&now, store)

	r.OnAuthFailed(testChatID)
	waitIdle(t, r)

	// respawnCap refusals stay on the retry cadence...
	for i := 0; i < respawnCap; i++ {
		if !f.sched.fire() {
			t.Fatalf("attempt %d: expected an armed re-probe", i+1)
		}
		waitIdle(t, r)
		if last := store.outcomes()[len(store.outcomes())-1]; last != outcomeRespawnNotAttempted {
			t.Fatalf("attempt %d outcome = %q, want %q", i+1, last, outcomeRespawnNotAttempted)
		}
		if n := f.sched.pendingCount(); n != 1 {
			t.Fatalf("attempt %d: pending re-probe timers = %d, want 1", i+1, n)
		}
	}

	// ...and the next one parks.
	if !f.sched.fire() {
		t.Fatal("expected an armed re-probe for the final attempt")
	}
	waitIdle(t, r)

	out := store.outcomes()
	if last := out[len(out)-1]; last != outcomeRespawnCapExhausted {
		t.Fatalf("final outcome = %q, want %q (outcomes = %v)", last, outcomeRespawnCapExhausted, out)
	}
	if det := store.details()[len(store.details())-1]; !strings.Contains(det, "boom") {
		t.Errorf("final detail = %q, want it to name the actual cause", det)
	}
	if n := f.sched.pendingCount(); n != 0 {
		t.Fatalf("pending re-probe timers = %d, want 0 (a spent budget is a terminal park)", n)
	}
	if got := respawnCharges(r); got != 0 {
		t.Fatalf("respawn charges = %d, want 0 (no refusal ever cost a respawn life)", got)
	}
}

// TestChatRotator_IneligibleRotationTransientFailureRetriesThenParks pins the round-2
// fix: the rotate-away branch is only terminal when it reached a REAL end state. A
// transient engine/DB fault records FAILED and rotates nothing, so clearing the respawn
// state there would cancel the re-probe and — because OnAuthFailed is edge-triggered on
// an already-auth-failed pane — leave the chat permanently quiet on one blip. It must
// retry on the same bounded budget instead, then park visibly.
func TestChatRotator_IneligibleRotationTransientFailureRetriesThenParks(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{
		authFailed:       true,
		authResult:       AuthProbeHealthy,
		fromLabel:        "agent.yuki@kamik.ai",
		respawnSwitchErr: disabledTargetRefusal(),
		decideErr:        errors.New("engine unavailable"),
	}
	store := &lockedAuditStore{}
	r := f.authRotator(&now, store)

	r.OnAuthFailed(testChatID)
	waitIdle(t, r)

	// Each transient rotation failure keeps the chat on the retry cadence.
	for i := 0; i < respawnCap; i++ {
		if !f.sched.fire() {
			t.Fatalf("attempt %d: expected an armed re-probe", i+1)
		}
		waitIdle(t, r)
		if last := store.outcomes()[len(store.outcomes())-1]; last != "ROTATION_OUTCOME_FAILED" {
			t.Fatalf("attempt %d outcome = %q, want ROTATION_OUTCOME_FAILED from the failed rotation", i+1, last)
		}
		if n := f.sched.pendingCount(); n != 1 {
			t.Fatalf("attempt %d: pending re-probe timers = %d, want 1 (a blip must not park the chat)", i+1, n)
		}
	}

	// Once the budget is spent it becomes a real, visible park.
	if !f.sched.fire() {
		t.Fatal("expected an armed re-probe for the final attempt")
	}
	waitIdle(t, r)

	out := store.outcomes()
	if last := out[len(out)-1]; last != outcomeRespawnCapExhausted {
		t.Fatalf("final outcome = %q, want %q (outcomes = %v)", last, outcomeRespawnCapExhausted, out)
	}
	if det := store.details()[len(store.details())-1]; !strings.Contains(det, "agent.yuki@kamik.ai") {
		t.Errorf("final detail = %q, want it to name the ineligible bound account", det)
	}
	if n := f.sched.pendingCount(); n != 0 {
		t.Fatalf("pending re-probe timers = %d, want 0 (a spent budget is a terminal park)", n)
	}
	if got := respawnCharges(r); got != 0 {
		t.Fatalf("respawn charges = %d, want 0 (no refusal ever cost a respawn life)", got)
	}
}

// TestChatRotator_IneligibleRotationAbortedKeepsBudgetAndReprobes pins the settle-round
// fix: when the rotate-away fallback is itself refused because the chat became WORKING
// mid-turn, the pane was never touched and nothing was consumed. Charging the bounded
// not-attempt budget there would let a chat that has ALREADY RECOVERED be parked as
// RESPAWN_CAP_EXHAUSTED, and would write a FAILED audit row for a benign race. The abort
// must instead be free: no audit row, no charge, re-probe still armed.
func TestChatRotator_IneligibleRotationAbortedKeepsBudgetAndReprobes(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{
		authFailed:       true,
		authResult:       AuthProbeHealthy,
		fromLabel:        "agent.yuki@kamik.ai",
		respawnSwitchErr: disabledTargetRefusal(),
		decision:         Decision{Kind: DecisionSwitch, AccountID: "acct-b-id", Label: "acct-b"},
		switchErr:        ErrSwitchAborted,
	}
	store := &lockedAuditStore{}
	r := f.authRotator(&now, store)

	r.OnAuthFailed(testChatID)
	waitIdle(t, r)

	// Well past the budget: an abort that cost a life would have parked the chat by now.
	for i := 0; i < respawnCap+3; i++ {
		if !f.sched.fire() {
			t.Fatalf("attempt %d: expected an armed re-probe (an aborted rotation must never park the chat)", i+1)
		}
		waitIdle(t, r)
		if n := f.sched.pendingCount(); n != 1 {
			t.Fatalf("attempt %d: pending re-probe timers = %d, want 1", i+1, n)
		}
	}

	// The only row is the probe's own STATUS_ONLY_PROBE_UNCONFIRMED from the first pass;
	// every aborted rotation after it must add nothing at all — no FAILED, and above all
	// no terminal park.
	for _, got := range store.outcomes() {
		if got != "ROTATION_OUTCOME_STATUS_ONLY_PROBE_UNCONFIRMED" {
			t.Fatalf("audit outcomes = %v, want only the probe row (a mid-turn abort records nothing)", store.outcomes())
		}
	}
	if got := respawnCharges(r); got != 0 {
		t.Errorf("respawn charges = %d, want 0", got)
	}
	if got := notAttemptCharges(r); got != 0 {
		t.Errorf("not-attempt charges = %d, want 0 (an abort consumed nothing, so it must cost nothing)", got)
	}
}

// TestChatRotator_RefundRespawnKeepsEarlierChargesAndWindow covers the accounting helper
// directly: a refund must undo exactly the charge just taken, never a real respawn that
// already happened in the same window, and never restart the window (which would hand
// the chat a fresh cap).
func TestChatRotator_RefundRespawnKeepsEarlierChargesAndWindow(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{}
	r := f.authRotator(&now, &lockedAuditStore{})

	if !r.chargeRespawn(testChatID) {
		t.Fatal("first charge refused")
	}
	opened := respawnWindowOf(r).windowStart
	now = now.Add(time.Minute)
	if !r.chargeRespawn(testChatID) {
		t.Fatal("second charge refused")
	}
	r.refundRespawn(testChatID)

	if got := respawnCharges(r); got != 1 {
		t.Fatalf("charges after refund = %d, want 1 (the earlier real respawn must survive)", got)
	}
	if got := respawnWindowOf(r).windowStart; !got.Equal(opened) {
		t.Fatalf("window start = %v, want it unchanged at %v", got, opened)
	}

	// Refunding the last charge drops the window so the next respawn starts fresh.
	r.refundRespawn(testChatID)
	if got := respawnWindowOf(r); got != (respawnWindow{}) {
		t.Fatalf("window = %+v, want it dropped once no charges remain", got)
	}
}

// TestChatRotator_ChargeNotAttemptWindowRollsOver pins that the not-attempt budget is a
// rolling window like the respawn cap: a chat that exhausts it and then goes quiet for
// longer than respawnCapWindow gets a fresh budget rather than staying parked forever.
func TestChatRotator_ChargeNotAttemptWindowRollsOver(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{}
	r := f.authRotator(&now, &lockedAuditStore{})

	for i := 0; i < respawnCap; i++ {
		if !r.chargeNotAttempt(testChatID) {
			t.Fatalf("charge %d refused, want the budget to allow respawnCap attempts", i+1)
		}
		now = now.Add(time.Minute)
	}
	if r.chargeNotAttempt(testChatID) {
		t.Fatal("charge past the budget was allowed, want it refused")
	}

	now = now.Add(respawnCapWindow)
	if !r.chargeNotAttempt(testChatID) {
		t.Fatal("charge after the window rolled over was refused, want a fresh budget")
	}

	// Deregister drops the budget alongside the respawn window.
	r.Deregister(testChatID)
	if got := notAttemptWindowOf(r); got != (respawnWindow{}) {
		t.Fatalf("not-attempt budget after Deregister = %+v, want it dropped", got)
	}
}

// TestChatRotator_DisabledBoundAccountRebindsToEligibleAccount pins the BOS-981
// operator decision: a chat bound to a disabled account rebinds itself to an eligible
// enabled account and resumes, with no operator action and no cap burn — the automatic
// equivalent of the manual `boss account switch` that was observed to fix the pane.
func TestChatRotator_DisabledBoundAccountRebindsToEligibleAccount(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{
		authFailed:       true,
		authResult:       AuthProbeHealthy,
		fromLabel:        "agent.yuki@kamik.ai",
		respawnSwitchErr: disabledTargetRefusal(),
		decision:         Decision{Kind: DecisionSwitch, AccountID: "acct-b", Label: "acct-b"},
	}
	store := &lockedAuditStore{}
	r := f.authRotator(&now, store)

	r.OnAuthFailed(testChatID)
	waitIdle(t, r)
	if !f.sched.fire() {
		t.Fatal("expected an armed re-probe")
	}
	waitIdle(t, r)

	// The rotation must NOT bench the bound account's health: respawnInPlace is only
	// reachable after the authoritative probe called that same account healthy, and a
	// health = failed write would outlive the operator re-enabling it.
	if req := f.lastDecideRequest(); !req.SuppressHealthFail || req.Kind != AuthInvalidated || req.AccountID != "acct-capped" {
		t.Errorf("Decide request = %+v, want AuthInvalidated on acct-capped with SuppressHealthFail", req)
	}
	sw := f.switched()
	if len(sw) != 2 {
		t.Fatalf("switch calls = %d, want 2 (refused respawn, then a real rotation)", len(sw))
	}
	if !sw[0].RespawnSameAccount {
		t.Errorf("first switch should be the same-account respawn attempt, got %+v", sw[0])
	}
	if sw[1].RespawnSameAccount || sw[1].AccountID != "acct-b" || !sw[1].Auto {
		t.Errorf("second switch should be an automatic rotation to acct-b, got %+v", sw[1])
	}
	out := store.outcomes()
	if last := out[len(out)-1]; last != "ROTATION_OUTCOME_ROTATED" {
		t.Fatalf("final outcome = %q, want ROTATION_OUTCOME_ROTATED (outcomes = %v)", last, out)
	}
	det := store.details()
	if !strings.Contains(det[len(det)-1], "agent.yuki@kamik.ai") {
		t.Errorf("rotated detail = %q, want it to name the ineligible bound account", det[len(det)-1])
	}
	if got := respawnCharges(r); got != 0 {
		t.Fatalf("respawn charges = %d, want 0 (the refused respawn cost nothing)", got)
	}
	if n := f.sched.pendingCount(); n != 0 {
		t.Fatalf("pending re-probe timers after a successful rotation = %d, want 0", n)
	}
}

// TestChatRotator_DisabledBoundAccountNoEligibleTargetIsTerminal pins the fallback for
// the genuinely unrecoverable case: nothing to rotate to. The chat reaches a recorded
// terminal state that names the disabled account and steers the operator to the remedy,
// instead of retrying silently until the cap dies.
func TestChatRotator_DisabledBoundAccountNoEligibleTargetIsTerminal(t *testing.T) {
	now := time.Now()
	f := &fakeDeps{
		authFailed:       true,
		authResult:       AuthProbeHealthy,
		fromLabel:        "agent.yuki@kamik.ai",
		respawnSwitchErr: disabledTargetRefusal(),
		decision:         Decision{Kind: DecisionNoEligibleAccount},
	}
	store := &lockedAuditStore{}
	r := f.authRotator(&now, store)

	r.OnAuthFailed(testChatID)
	waitIdle(t, r)
	if !f.sched.fire() {
		t.Fatal("expected an armed re-probe")
	}
	waitIdle(t, r)

	out := store.outcomes()
	if last := out[len(out)-1]; last != "ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT" {
		t.Fatalf("final outcome = %q, want ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT (outcomes = %v)", last, out)
	}
	det := store.details()[len(store.details())-1]
	if !strings.Contains(det, "agent.yuki@kamik.ai") {
		t.Errorf("terminal detail = %q, want it to name the disabled bound account", det)
	}
	if !strings.Contains(det, "boss account update") {
		t.Errorf("terminal detail = %q, want it to carry the operator remedy", det)
	}
	if n := f.sched.pendingCount(); n != 0 {
		t.Fatalf("pending re-probe timers = %d, want 0 (recorded terminal state, not a silent retry)", n)
	}
	if got := respawnCharges(r); got != 0 {
		t.Fatalf("respawn charges = %d, want 0", got)
	}
}
