package session

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/rotation"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// --- rotation adapter fakes ---

type fakeRotationDecider struct {
	outcome rotation.Outcome
	err     error
	calls   int
	lastSig rotation.Signal
}

func (f *fakeRotationDecider) Decide(_ context.Context, sig rotation.Signal) (rotation.Outcome, error) {
	f.calls++
	f.lastSig = sig
	return f.outcome, f.err
}

type fakeAccountMaterializer struct {
	env         map[string]string
	err         error
	calls       int
	lastAccount *models.Account
}

func (f *fakeAccountMaterializer) Materialize(_ context.Context, account *models.Account) (map[string]string, error) {
	f.calls++
	f.lastAccount = account
	return f.env, f.err
}

type fakeRotationBinding struct {
	binding RotationBinding
	bound   bool
	err     error
}

func (f *fakeRotationBinding) CurrentBinding(_ context.Context, _ *models.Session) (RotationBinding, bool, error) {
	return f.binding, f.bound, f.err
}

type fakeRateLimitProbe struct {
	snap  models.UsageSnapshot
	err   error
	calls int
}

func (f *fakeRateLimitProbe) Probe(_ context.Context, _ string) (models.UsageSnapshot, error) {
	f.calls++
	if f.err != nil {
		return models.UsageSnapshot{}, f.err
	}
	return f.snap, nil
}

type adapterShapeMaterializer struct {
	supports       bool
	supportsCalls  int
	lastProvider   string
	env            map[string]string
	materializeID  string
	materializeErr error
}

func (m *adapterShapeMaterializer) SupportsRotation(_ context.Context, provider string) (bool, error) {
	m.supportsCalls++
	m.lastProvider = provider
	return m.supports, nil
}

func (m *adapterShapeMaterializer) MaterializeAccount(_ context.Context, accountID string) (map[string]string, error) {
	m.materializeID = accountID
	return m.env, m.materializeErr
}

type adapterShapeAccountMaterializer struct{ mat *adapterShapeMaterializer }

func (m adapterShapeAccountMaterializer) Materialize(ctx context.Context, acct *models.Account) (map[string]string, error) {
	if acct == nil {
		return nil, nil
	}
	return m.mat.MaterializeAccount(ctx, acct.ID)
}

type adapterShapeRotationBinding struct{ mat *adapterShapeMaterializer }

func (b adapterShapeRotationBinding) CurrentBinding(ctx context.Context, sess *models.Session) (RotationBinding, bool, error) {
	if sess == nil || sess.AccountID == nil || *sess.AccountID == "" {
		return RotationBinding{}, false, nil
	}
	capable, err := b.mat.SupportsRotation(ctx, sess.AgentName)
	if err != nil {
		return RotationBinding{}, false, err
	}
	return RotationBinding{
		CappedAccountID: *sess.AccountID,
		Provider:        sess.AgentName,
		RotationCapable: capable,
	}, true, nil
}

// rotationFixture wires a Lifecycle in ImplementingPlan with a rotatable repo
// and the three rotation adapters, returning the pieces a test asserts on.
type rotationFixture struct {
	lc           *Lifecycle
	sessions     *mockSessionStore
	runner       *mockAgentRunner
	decider      *fakeRotationDecider
	binding      *fakeRotationBinding
	materializer *fakeAccountMaterializer
	probe        *fakeRateLimitProbe
	sessionID    string
}

func newRotationFixture(t *testing.T) *rotationFixture {
	t.Helper()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", CanAutoRotate: true}

	oldID := "agent-old"
	sessions.sessions["sess-1"] = &models.Session{
		ID:             "sess-1",
		RepoID:         "repo-1",
		AgentName:      "claude",
		Model:          "opus",
		Plan:           "ORIGINAL PLAN BODY",
		WorktreePath:   t.TempDir(),
		AgentSessionID: &oldID,
		State:          machine.ImplementingPlan,
	}

	runner := newMockAgentRunner()
	runner.nextID = "agent-new"

	lc := &Lifecycle{
		sessions:    sessions,
		repos:       repos,
		agentRunner: runner,
		logger:      zerolog.Nop(),
	}
	lc.SetProofEnvResolver(fakeProofEnvResolver{env: map[string]string{"PROOF_KEY": "proof-val"}})

	decider := &fakeRotationDecider{}
	binding := &fakeRotationBinding{
		binding: RotationBinding{CappedAccountID: "acct-capped", Provider: "claude", RotationCapable: true},
		bound:   true,
	}
	materializer := &fakeAccountMaterializer{env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "next-token"}}
	reset := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Millisecond)
	fetched := time.Now().UTC().Truncate(time.Millisecond)
	probe := &fakeRateLimitProbe{snap: models.UsageSnapshot{
		Util5h:    1,
		Reset5h:   &reset,
		Status:    "RATE_LIMIT_PLAN_STATUS_RATE_LIMITED",
		FetchedAt: &fetched,
	}}
	lc.SetRotationDecider(decider.Decide)
	lc.SetRotationBinding(binding)
	lc.SetAccountMaterializer(materializer)
	lc.SetRateLimitProbe(probe.Probe)

	return &rotationFixture{
		lc: lc, sessions: sessions, runner: runner,
		decider: decider, binding: binding, materializer: materializer, probe: probe,
		sessionID: "sess-1",
	}
}

// --- A. classifier table ---

func TestClassifyUsageLimit(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantOK    bool
		wantReset bool
	}{
		{"usage plain", "usage_limit_reached: try later", true, false},
		{"usage with reset", "You have reached your 5-hour limit", true, true},
		{"auth", "Please sign in again to continue", false, false},
		{"transient", "rate limit exceeded, try again", false, false},
		{"empty", "", false, false},
		{"ambiguous limit prose", "the sky is the limit for this team", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reset, ok := classifyUsageLimit(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("classifyUsageLimit(%q) ok = %v, want %v", tc.in, ok, tc.wantOK)
			}
			if tc.wantReset && reset.IsZero() {
				t.Fatalf("classifyUsageLimit(%q) reset zero, want non-zero", tc.in)
			}
			if !tc.wantReset && !reset.IsZero() {
				t.Fatalf("classifyUsageLimit(%q) reset = %v, want zero", tc.in, reset)
			}
		})
	}
}

// --- C/D. rotate happy path + steering prefix (NAMED criterion) ---

func TestAttemptUsageLimitRotation_RotateHappyPath_SteeringPrefix(t *testing.T) {
	f := newRotationFixture(t)
	f.decider.outcome = rotation.Outcome{
		Kind:            rotation.OutcomeRotate,
		NextAccount:     &models.Account{ID: "acct-next"},
		CooldownApplied: true,
	}

	handled := f.lc.attemptUsageLimitRotation(context.Background(), f.sessionID, "agent-old", "usage_limit_reached: try later")
	if !handled {
		t.Fatal("expected handled=true on rotate")
	}
	if len(f.runner.started) != 1 {
		t.Fatalf("expected exactly 1 StartByAgent, got %d", len(f.runner.started))
	}
	call := f.runner.started[0]

	// NAMED acceptance criterion: the restarted prompt starts with the exact
	// steering notice, followed by the original plan.
	wantPrefix := "You were interrupted mid-task by an account switch. Verify current workspace/git/PR state before repeating any action (commits, pushes, comments may already exist)."
	if !strings.HasPrefix(call.plan, wantPrefix) {
		t.Fatalf("restarted prompt does not start with steering notice.\n got: %q", call.plan)
	}
	if call.plan != wantPrefix+"\n\nORIGINAL PLAN BODY" {
		t.Fatalf("restarted prompt = %q, want steering + \\n\\n + plan", call.plan)
	}
	// Resume points at the old agent session id.
	if call.resume == nil || *call.resume != "agent-old" {
		t.Fatalf("resume = %v, want agent-old", call.resume)
	}
	// Account overlay present and proof env preserved.
	if call.env["CLAUDE_CODE_OAUTH_TOKEN"] != "next-token" {
		t.Errorf("account overlay missing: env=%v", call.env)
	}
	if call.env["PROOF_KEY"] != "proof-val" {
		t.Errorf("proof env not preserved: env=%v", call.env)
	}
	// Persisted state: new agent session id, incremented count, ImplementingPlan.
	s := f.sessions.sessions[f.sessionID]
	if s.AgentSessionID == nil || *s.AgentSessionID != "agent-new" {
		t.Errorf("AgentSessionID = %v, want agent-new", s.AgentSessionID)
	}
	if s.RotationAttemptCount != 1 {
		t.Errorf("RotationAttemptCount = %d, want 1", s.RotationAttemptCount)
	}
	if s.AccountID == nil || *s.AccountID != "acct-next" {
		t.Errorf("AccountID = %v, want acct-next", s.AccountID)
	}
	if s.State != machine.ImplementingPlan {
		t.Errorf("State = %v, want ImplementingPlan", s.State)
	}
	if f.materializer.lastAccount == nil || f.materializer.lastAccount.ID != "acct-next" {
		t.Errorf("materializer called with wrong account: %v", f.materializer.lastAccount)
	}
}

func TestAttemptUsageLimitRotation_RotatesThroughAdapterShapes(t *testing.T) {
	f := newRotationFixture(t)
	cappedID := "acct-capped"
	f.sessions.sessions[f.sessionID].AccountID = &cappedID

	mat := &adapterShapeMaterializer{
		supports: true,
		env:      map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "next-token"},
	}
	f.lc.SetRotationBinding(adapterShapeRotationBinding{mat: mat})
	f.lc.SetAccountMaterializer(adapterShapeAccountMaterializer{mat: mat})
	f.decider.outcome = rotation.Outcome{
		Kind:            rotation.OutcomeRotate,
		NextAccount:     &models.Account{ID: "acct-next"},
		CooldownApplied: true,
	}

	handled := f.lc.attemptUsageLimitRotation(context.Background(), f.sessionID, "agent-old", "usage_limit_reached: try later")
	if !handled {
		t.Fatal("expected handled=true on rotate")
	}
	if mat.supportsCalls != 1 || mat.lastProvider != "claude" {
		t.Fatalf("SupportsRotation calls/provider = %d/%q, want 1/claude", mat.supportsCalls, mat.lastProvider)
	}
	if f.decider.lastSig.Provider != "claude" ||
		f.decider.lastSig.CappedAccountID != "acct-capped" ||
		!f.decider.lastSig.RotationCapable {
		t.Fatalf("rotation signal = %+v, want provider claude capped acct-capped capable true", f.decider.lastSig)
	}
	if mat.materializeID != "acct-next" {
		t.Fatalf("MaterializeAccount called with %q, want acct-next", mat.materializeID)
	}
	if len(f.runner.started) != 1 {
		t.Fatalf("expected exactly 1 StartByAgent, got %d", len(f.runner.started))
	}
	call := f.runner.started[0]
	if !strings.HasPrefix(call.plan, steeringNotice+"\n\n") {
		t.Fatalf("restarted prompt missing steering notice prefix: %q", call.plan)
	}
	if call.env["CLAUDE_CODE_OAUTH_TOKEN"] != "next-token" {
		t.Fatalf("account env overlay missing: env=%v", call.env)
	}
	s := f.sessions.sessions[f.sessionID]
	if s.RotationAttemptCount != 1 {
		t.Fatalf("RotationAttemptCount = %d, want 1", s.RotationAttemptCount)
	}
	if s.State != machine.ImplementingPlan {
		t.Fatalf("State = %v, want ImplementingPlan", s.State)
	}
}

func TestAttemptUsageLimitRotation_ProbeGate(t *testing.T) {
	limitedReset := time.Date(2026, 7, 7, 17, 0, 0, 0, time.UTC)
	fetched := time.Date(2026, 7, 7, 15, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		snap        models.UsageSnapshot
		probeErr    error
		wantHandled bool
		wantDecide  bool
		wantReset   *time.Time
	}{
		{
			name: "limited uses probe reset",
			snap: models.UsageSnapshot{
				Util5h:    1,
				Reset5h:   &limitedReset,
				Status:    "RATE_LIMIT_PLAN_STATUS_RATE_LIMITED",
				FetchedAt: &fetched,
			},
			wantHandled: true,
			wantDecide:  true,
			wantReset:   &limitedReset,
		},
		{
			name: "healthy falls through",
			snap: models.UsageSnapshot{
				Util5h:    0.08,
				Status:    "RATE_LIMIT_PLAN_STATUS_ACTIVE",
				FetchedAt: &fetched,
			},
		},
		{name: "probe error falls through", probeErr: context.Canceled},
		{name: "auth failure falls through", probeErr: status.Error(codes.Unauthenticated, "auth invalidated")},
		{
			name: "unsupported falls through",
			snap: models.UsageSnapshot{
				Status:    "RATE_LIMIT_PLAN_STATUS_UNSUPPORTED",
				FetchedAt: &fetched,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRotationFixture(t)
			f.probe.snap = tt.snap
			f.probe.err = tt.probeErr
			f.decider.outcome = rotation.Outcome{
				Kind:            rotation.OutcomeRotate,
				NextAccount:     &models.Account{ID: "acct-next"},
				CooldownApplied: true,
			}

			handled := f.lc.attemptUsageLimitRotation(context.Background(), f.sessionID, "agent-old", "You have reached your 5-hour limit")
			if handled != tt.wantHandled {
				t.Fatalf("handled = %v, want %v", handled, tt.wantHandled)
			}
			if (f.decider.calls > 0) != tt.wantDecide {
				t.Fatalf("decider calls = %d, want called=%v", f.decider.calls, tt.wantDecide)
			}
			if tt.wantReset != nil {
				if f.decider.lastSig.ResetAt == nil || !f.decider.lastSig.ResetAt.Equal(*tt.wantReset) {
					t.Fatalf("ResetAt = %v, want %v", f.decider.lastSig.ResetAt, *tt.wantReset)
				}
			}
			if !tt.wantDecide && len(f.runner.started) != 0 {
				t.Fatalf("starts = %d, want 0", len(f.runner.started))
			}
		})
	}
}

// Re-arm: rotated headless run arms the poll fallback with the new id.
func TestAttemptUsageLimitRotation_RearmsPollFallback(t *testing.T) {
	f := newRotationFixture(t)
	f.decider.outcome = rotation.Outcome{Kind: rotation.OutcomeRotate, NextAccount: &models.Account{ID: "acct-next"}, CooldownApplied: true}

	armer := &fakePollArmer{}
	f.lc.pollArmer = armer
	f.lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": newFakeAgent()})

	if !f.lc.attemptUsageLimitRotation(context.Background(), f.sessionID, "agent-old", "usage_limit_reached") {
		t.Fatal("expected handled=true")
	}
	if !armer.armCalled || armer.armedID != "agent-new" {
		t.Fatalf("poll fallback not re-armed for rotated run: armed=%v id=%q", armer.armCalled, armer.armedID)
	}
}

// --- Bounded-attempts exhaustion park ---

func TestAttemptUsageLimitRotation_BoundedExhaustionPark(t *testing.T) {
	f := newRotationFixture(t)
	f.sessions.sessions[f.sessionID].RotationAttemptCount = 3 // == default MaxRotations

	var captured db.UpdateSessionParams
	f.sessions.updateHook = func(_ string, p db.UpdateSessionParams) error {
		if p.BlockedReason != nil {
			captured = p
		}
		return nil
	}

	handled := f.lc.attemptUsageLimitRotation(context.Background(), f.sessionID, "agent-old", "usage_limit_reached")
	if !handled {
		t.Fatal("expected handled=true on bounded-exhaustion park")
	}
	if f.decider.calls != 0 {
		t.Errorf("decider should NOT be called on bounded exhaustion, got %d calls", f.decider.calls)
	}
	if len(f.runner.started) != 0 {
		t.Errorf("no restart expected, got %d", len(f.runner.started))
	}
	if f.sessions.sessions[f.sessionID].State != machine.Blocked {
		t.Errorf("session not parked to Blocked")
	}
	if captured.BlockedReason == nil || *captured.BlockedReason == nil || !strings.Contains(**captured.BlockedReason, "max rotations") {
		t.Errorf("reason should mention max rotations, got %v", captured.BlockedReason)
	}
	if captured.RotationResumeAt != nil {
		t.Errorf("resume_at should be NULL on bounded exhaustion, got %v", captured.RotationResumeAt)
	}
}

func TestAttemptUsageLimitRotation_BoundedExhaustionRequiresConfirmedProbe(t *testing.T) {
	f := newRotationFixture(t)
	f.sessions.sessions[f.sessionID].RotationAttemptCount = 3 // == default MaxRotations
	fetched := time.Date(2026, 7, 7, 15, 0, 0, 0, time.UTC)
	f.probe.snap = models.UsageSnapshot{
		Util5h:    0.08,
		Status:    "RATE_LIMIT_PLAN_STATUS_ACTIVE",
		FetchedAt: &fetched,
	}

	handled := f.lc.attemptUsageLimitRotation(context.Background(), f.sessionID, "agent-old", "usage_limit_reached")
	if handled {
		t.Fatal("healthy probe must fall through even when max rotations is reached")
	}
	if f.decider.calls != 0 {
		t.Fatalf("decider calls = %d, want 0", f.decider.calls)
	}
	if f.sessions.sessions[f.sessionID].State == machine.Blocked {
		t.Fatal("healthy probe must not park the session as bounded exhaustion")
	}
}

func TestAttemptUsageLimitRotation_UsesLiveMaxRotations(t *testing.T) {
	f := newRotationFixture(t)
	enabled := true
	f.lc.SetRotationConfig(config.RotationConfig{MaxRotationsPerRun: 1})
	f.lc.SetRotationConfigLoader(func() (config.RotationConfig, error) {
		return config.RotationConfig{
			Enabled:            &enabled,
			MaxRotationsPerRun: 5,
		}, nil
	})
	f.sessions.sessions[f.sessionID].RotationAttemptCount = 1
	f.decider.outcome = rotation.Outcome{
		Kind:            rotation.OutcomeRotate,
		NextAccount:     &models.Account{ID: "acct-next"},
		CooldownApplied: true,
	}

	handled := f.lc.attemptUsageLimitRotation(context.Background(), f.sessionID, "agent-old", "usage_limit_reached")
	if !handled {
		t.Fatal("expected handled=true")
	}
	if f.decider.calls != 1 {
		t.Fatalf("decider calls = %d, want 1 (live max should allow rotation)", f.decider.calls)
	}
	if len(f.runner.started) != 1 {
		t.Fatalf("started runs = %d, want 1", len(f.runner.started))
	}
}

// --- All-cooling park ---

func TestAttemptUsageLimitRotation_AllCoolingPark(t *testing.T) {
	f := newRotationFixture(t)
	resumeAt := time.Date(2026, 7, 5, 15, 4, 0, 0, time.UTC)
	f.decider.outcome = rotation.Outcome{Kind: rotation.OutcomeAllExhausted, ResumeAt: resumeAt}

	var captured db.UpdateSessionParams
	f.sessions.updateHook = func(_ string, p db.UpdateSessionParams) error {
		if p.BlockedReason != nil {
			captured = p
		}
		return nil
	}

	handled := f.lc.attemptUsageLimitRotation(context.Background(), f.sessionID, "agent-old", "usage_limit_reached")
	if !handled {
		t.Fatal("expected handled=true on all-cooling park")
	}
	if len(f.runner.started) != 0 {
		t.Errorf("no restart expected on all-cooling, got %d", len(f.runner.started))
	}
	if f.sessions.sessions[f.sessionID].State != machine.Blocked {
		t.Errorf("session not parked to Blocked")
	}
	if captured.RotationResumeAt == nil || *captured.RotationResumeAt == nil {
		t.Fatalf("resume_at must be set on all-cooling park")
	}
	if **captured.RotationResumeAt != resumeAt.Format(time.RFC3339) {
		t.Errorf("resume_at = %q, want %q", **captured.RotationResumeAt, resumeAt.Format(time.RFC3339))
	}
	if captured.BlockedReason == nil || *captured.BlockedReason == nil || !strings.Contains(**captured.BlockedReason, "15:04") {
		t.Errorf("reason should mention cooling time 15:04, got %v", captured.BlockedReason)
	}
}

// --- Kill switch + per-repo opt-out ---

func TestAttemptUsageLimitRotation_KillSwitch(t *testing.T) {
	f := newRotationFixture(t)
	disabled := false
	f.lc.SetRotationConfig(config.RotationConfig{Enabled: &disabled})
	f.decider.outcome = rotation.Outcome{Kind: rotation.OutcomeRotate, NextAccount: &models.Account{ID: "x"}, CooldownApplied: true}

	if f.lc.attemptUsageLimitRotation(context.Background(), f.sessionID, "agent-old", "usage_limit_reached") {
		t.Fatal("kill switch must return handled=false")
	}
	if f.decider.calls != 0 || len(f.runner.started) != 0 {
		t.Error("no rotation work should happen with kill switch")
	}
}

func TestAttemptUsageLimitRotation_RepoOptOut(t *testing.T) {
	f := newRotationFixture(t)
	f.lc.repos.(*mockRepoStore).repos["repo-1"].CanAutoRotate = false
	f.decider.outcome = rotation.Outcome{Kind: rotation.OutcomeRotate, NextAccount: &models.Account{ID: "x"}, CooldownApplied: true}

	if f.lc.attemptUsageLimitRotation(context.Background(), f.sessionID, "agent-old", "usage_limit_reached") {
		t.Fatal("repo opt-out must return handled=false")
	}
	if len(f.runner.started) != 0 {
		t.Error("no restart with repo opt-out")
	}
}

// --- Regression: generic crash is not intercepted ---

func TestAttemptUsageLimitRotation_GenericCrashNotIntercepted(t *testing.T) {
	f := newRotationFixture(t)
	if f.lc.attemptUsageLimitRotation(context.Background(), f.sessionID, "agent-old", "panic: nil pointer dereference") {
		t.Fatal("generic crash must not be handled by rotation")
	}
	if f.decider.calls != 0 || len(f.runner.started) != 0 {
		t.Error("generic crash must not trigger rotation work")
	}
}

// --- Duplicate signals -> single rotation ---

func TestAttemptUsageLimitRotation_DuplicateSignalSwallowed(t *testing.T) {
	f := newRotationFixture(t)
	f.decider.outcome = rotation.Outcome{
		Kind:            rotation.OutcomeRotate,
		NextAccount:     &models.Account{ID: "acct-next"},
		CooldownApplied: false, // engine already applied cooldown for the original signal
	}

	handled := f.lc.attemptUsageLimitRotation(context.Background(), f.sessionID, "agent-old", "usage_limit_reached")
	if !handled {
		t.Fatal("duplicate cap should be swallowed as handled=true")
	}
	if len(f.runner.started) != 0 {
		t.Fatalf("duplicate cap must NOT restart, got %d restarts", len(f.runner.started))
	}
}

// --- Status-only -> falls through to Block ---

func TestAttemptUsageLimitRotation_StatusOnlyFallsThrough(t *testing.T) {
	f := newRotationFixture(t)
	f.decider.outcome = rotation.Outcome{Kind: rotation.OutcomeStatusOnly}
	if f.lc.attemptUsageLimitRotation(context.Background(), f.sessionID, "agent-old", "usage_limit_reached") {
		t.Fatal("status-only must return handled=false (today's Block)")
	}
}

// --- Unbound session -> falls through ---

func TestAttemptUsageLimitRotation_UnboundFallsThrough(t *testing.T) {
	f := newRotationFixture(t)
	f.binding.bound = false
	if f.lc.attemptUsageLimitRotation(context.Background(), f.sessionID, "agent-old", "usage_limit_reached") {
		t.Fatal("unbound session must return handled=false")
	}
	if f.decider.calls != 0 {
		t.Error("decider must not be called when unbound")
	}
}

// --- Wiring: SignalSessionRunComplete top-of-function intercept ---

// A handled usage-limit rotation returns before the finalize/block fan-out; a
// generic crash flows through to the normal fan-out (regression).
func TestSignalSessionRunComplete_RotationInterceptWiring(t *testing.T) {
	t.Run("usage-limit rotates and skips fan-out", func(t *testing.T) {
		f := newRotationFixture(t)
		f.decider.outcome = rotation.Outcome{Kind: rotation.OutcomeRotate, NextAccount: &models.Account{ID: "acct-next"}, CooldownApplied: true}
		completer := &recordingPollCompleter{}
		f.lc.SetPollCompleter(completer)

		f.lc.SignalSessionRunComplete(f.sessionID, "agent-old", "usage_limit_reached")

		if len(f.runner.started) != 1 {
			t.Fatalf("expected rotation restart, got %d starts", len(f.runner.started))
		}
		if completer.count() != 0 {
			t.Errorf("handled rotation must skip the finalize fan-out, got %d completer calls", completer.count())
		}
	})

	t.Run("generic crash flows to fan-out and blocks", func(t *testing.T) {
		f := newRotationFixture(t)
		completer := &recordingPollCompleter{}
		f.lc.SetPollCompleter(completer)

		f.lc.SignalSessionRunComplete(f.sessionID, "agent-old", "panic: nil pointer dereference")

		if len(f.runner.started) != 0 {
			t.Errorf("generic crash must not restart, got %d starts", len(f.runner.started))
		}
		if completer.count() != 1 {
			t.Errorf("generic crash must run the fan-out, got %d completer calls", completer.count())
		}
		// finalizeHeadlessRunIfApplicable blocks the non-unattended headless run.
		if f.sessions.sessions[f.sessionID].State != machine.Blocked {
			t.Errorf("generic crash should Block the session, state=%v", f.sessions.sessions[f.sessionID].State)
		}
	})
}

// --- Not ImplementingPlan -> falls through ---

func TestAttemptUsageLimitRotation_WrongStateFallsThrough(t *testing.T) {
	f := newRotationFixture(t)
	f.sessions.sessions[f.sessionID].State = machine.Blocked
	if f.lc.attemptUsageLimitRotation(context.Background(), f.sessionID, "agent-old", "usage_limit_reached") {
		t.Fatal("non-ImplementingPlan session must return handled=false")
	}
}

// --- Nil adapters -> falls through ---

func TestAttemptUsageLimitRotation_NilAdaptersFallThrough(t *testing.T) {
	f := newRotationFixture(t)
	f.lc.rotationBinding = nil
	if f.lc.attemptUsageLimitRotation(context.Background(), f.sessionID, "agent-old", "usage_limit_reached") {
		t.Fatal("nil binding must return handled=false")
	}

	f2 := newRotationFixture(t)
	f2.lc.rotationDecider = nil
	if f2.lc.attemptUsageLimitRotation(context.Background(), f2.sessionID, "agent-old", "usage_limit_reached") {
		t.Fatal("nil decider must return handled=false")
	}

	f3 := newRotationFixture(t)
	f3.lc.rateLimitProbe = nil
	if f3.lc.attemptUsageLimitRotation(context.Background(), f3.sessionID, "agent-old", "usage_limit_reached") {
		t.Fatal("nil rate-limit probe must return handled=false")
	}
}

func TestHasLiveRotationSeams(t *testing.T) {
	f := newRotationFixture(t)
	if !f.lc.HasLiveRotationSeams() {
		t.Fatal("HasLiveRotationSeams = false, want true when binding and materializer are wired")
	}

	f.lc.rotationBinding = nil
	if f.lc.HasLiveRotationSeams() {
		t.Fatal("HasLiveRotationSeams = true with nil binding, want false")
	}

	f = newRotationFixture(t)
	f.lc.accountMaterializer = nil
	if f.lc.HasLiveRotationSeams() {
		t.Fatal("HasLiveRotationSeams = true with nil materializer, want false")
	}

	f = newRotationFixture(t)
	f.lc.rateLimitProbe = nil
	if f.lc.HasLiveRotationSeams() {
		t.Fatal("HasLiveRotationSeams = true with nil probe, want false")
	}
}
