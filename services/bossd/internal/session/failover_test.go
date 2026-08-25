package session

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/models"
	libtelemetry "github.com/recurser/bossalib/telemetry"
	"github.com/recurser/bossd/internal/account"
	"github.com/recurser/bossd/internal/rotation"
)

// captureAuditStore is a thread-safe rotation.AuditStore that records every
// inserted event so tests can assert exactly-once auditing.
type captureAuditStore struct {
	mu     sync.Mutex
	events []rotation.AuditEvent
}

func (s *captureAuditStore) Insert(_ context.Context, ev rotation.AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
	return nil
}

func (s *captureAuditStore) RecentBySession(_ context.Context, _ string, _ int) ([]rotation.AuditEvent, error) {
	return nil, nil
}

func (s *captureAuditStore) all() []rotation.AuditEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]rotation.AuditEvent, len(s.events))
	copy(out, s.events)
	return out
}

// recentAuditStore is a captureAuditStore whose RecentBySession returns the
// most-recently-inserted events, so per-episode dedup (RecordExhausted /
// RecordNoEligible) can be exercised end to end.
type recentAuditStore struct{ captureAuditStore }

func (s *recentAuditStore) RecentBySession(_ context.Context, sessionID string, limit int) ([]rotation.AuditEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []rotation.AuditEvent
	for i := len(s.events) - 1; i >= 0 && len(out) < limit; i-- {
		if s.events[i].SessionID == sessionID {
			out = append(out, s.events[i])
		}
	}
	return out, nil
}

// enableFailoverProxy flips the opt-in toggle on for a fixture's lifecycle.
func enableFailoverProxy(lc *Lifecycle) {
	on := true
	lc.SetRotationConfig(config.ManagedAccountsConfig{FailoverProxy: &on})
}

func assertFailoverRotationTelemetry(t *testing.T, recorder *finalizeTelemetryRecorder, reason string) {
	t.Helper()
	if len(recorder.captures) != 1 {
		t.Fatalf("account_rotated captures = %d, want 1", len(recorder.captures))
	}
	capture := recorder.captures[0]
	if capture.event != libtelemetry.EventAccountRotated {
		t.Fatalf("event = %q, want %q", capture.event, libtelemetry.EventAccountRotated)
	}
	if capture.properties["rotation_reason"] != reason || capture.properties["provider"] != "claude" || capture.properties["status"] != "rotated" {
		t.Fatalf("account_rotated properties = %#v", capture.properties)
	}
}

// mapSwitchRegistry resolves a distinct switchAccount per id (unknown ids error),
// so tests can assert the from/to sides of an audit resolve to human labels
// rather than raw account ids.
type mapSwitchRegistry map[string]switchAccount

func (m mapSwitchRegistry) Account(_ context.Context, id string) (switchAccount, error) {
	sa, ok := m[id]
	if !ok {
		return switchAccount{}, errors.New("account not found: " + id)
	}
	return sa, nil
}

// --- RebindAndAudit ---

func TestRebindAndAudit_persistsBindingAndAuditsOnce(t *testing.T) {
	f := newRotationFixture(t)
	store := &captureAuditStore{}
	f.lc.SetRotationRecorder(rotation.NewRecorder(store, zerolog.Nop()))

	err := f.lc.RebindAndAudit(context.Background(), nil, RebindAndAuditParams{
		SessionID:     f.sessionID,
		Provider:      "claude",
		Trigger:       "ROTATION_TRIGGER_AUTH_INVALIDATED",
		FromAccount:   "acct-capped",
		BindAccountID: "acct-next",
		ToAccount:     "acct-next",
		Outcome:       "ROTATION_OUTCOME_ROTATED",
		Detail:        "unit",
	})
	if err != nil {
		t.Fatalf("RebindAndAudit: %v", err)
	}

	sess := f.sessions.sessions[f.sessionID]
	if sess.AccountID == nil || *sess.AccountID != "acct-next" {
		t.Fatalf("account_id not persisted: got %v, want acct-next", sess.AccountID)
	}
	events := store.all()
	if len(events) != 1 {
		t.Fatalf("want exactly one AuditEvent, got %d", len(events))
	}
	ev := events[0]
	if ev.Trigger != "ROTATION_TRIGGER_AUTH_INVALIDATED" || ev.FromAccount != "acct-capped" ||
		ev.ToAccount != "acct-next" || ev.Outcome != "ROTATION_OUTCOME_ROTATED" {
		t.Fatalf("unexpected audit event: %+v", ev)
	}
}

// --- PrepareFailover (table-driven) ---

func TestPrepareFailover(t *testing.T) {
	rotateOutcome := rotation.Outcome{
		Kind:        rotation.OutcomeRotate,
		NextAccount: &models.Account{ID: "acct-next", Provider: models.AccountProviderClaude, Label: "second"},
	}

	cases := []struct {
		name        string
		status      int
		enabled     bool
		outcome     rotation.Outcome
		deciderErr  bool
		materialize map[string]string
		matErr      bool
		bound       bool
		wantRotate  bool
		wantTrigger string
		wantDecide  bool // whether the decider should have been consulted
	}{
		{
			name: "429 rotates with second bearer", status: 429, enabled: true, outcome: rotateOutcome,
			materialize: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "next-token"}, bound: true,
			wantRotate: true, wantTrigger: "ROTATION_TRIGGER_USAGE_LIMITED", wantDecide: true,
		},
		{
			name: "401 rotates as auth-invalidated", status: 401, enabled: true, outcome: rotateOutcome,
			materialize: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "next-token"}, bound: true,
			wantRotate: true, wantTrigger: "ROTATION_TRIGGER_AUTH_INVALIDATED", wantDecide: true,
		},
		{
			name: "flag off passes through (no decide)", status: 429, enabled: false, outcome: rotateOutcome,
			materialize: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "next-token"}, bound: true,
			wantRotate: false, wantDecide: false,
		},
		{
			name: "non-429/401 status ignored (no decide)", status: 500, enabled: true, outcome: rotateOutcome,
			materialize: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "next-token"}, bound: true,
			wantRotate: false, wantDecide: false,
		},
		{
			name: "no rotation target passes through", status: 429, enabled: true,
			outcome:     rotation.Outcome{Kind: rotation.OutcomeAllExhausted},
			materialize: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "next-token"}, bound: true,
			wantRotate: false, wantDecide: true,
		},
		{
			name: "materialize failure passes through", status: 429, enabled: true, outcome: rotateOutcome,
			matErr: true, bound: true,
			wantRotate: false, wantDecide: true,
		},
		{
			name: "unbound session passes through", status: 429, enabled: true, outcome: rotateOutcome,
			materialize: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "next-token"}, bound: false,
			wantRotate: false, wantDecide: false,
		},
		{
			name: "empty materialized token passes through", status: 429, enabled: true, outcome: rotateOutcome,
			materialize: map[string]string{}, bound: true,
			wantRotate: false, wantDecide: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRotationFixture(t)
			if tc.enabled {
				enableFailoverProxy(f.lc)
			} else {
				f.lc.SetRotationConfig(config.ManagedAccountsConfig{FailoverProxy: boolPtr(false)})
			}
			f.decider.outcome = tc.outcome
			f.binding.bound = tc.bound
			f.materializer.env = tc.materialize
			if tc.matErr {
				f.materializer.err = context.DeadlineExceeded
			}

			res, err := f.lc.PrepareFailover(context.Background(), f.sessionID, tc.status)
			if err != nil {
				t.Fatalf("PrepareFailover returned error: %v", err)
			}
			if res.Rotate != tc.wantRotate {
				t.Fatalf("Rotate = %v, want %v", res.Rotate, tc.wantRotate)
			}
			if tc.wantRotate {
				if res.Token != "next-token" {
					t.Fatalf("Token = %q, want next-token", res.Token)
				}
				if res.NextAccountID != "acct-next" {
					t.Fatalf("NextAccountID = %q, want acct-next", res.NextAccountID)
				}
				if res.FromAccountID != "acct-capped" {
					t.Fatalf("FromAccountID = %q, want acct-capped", res.FromAccountID)
				}
				if res.Trigger != tc.wantTrigger {
					t.Fatalf("Trigger = %q, want %q", res.Trigger, tc.wantTrigger)
				}
			}
			if got := tc.wantDecide; f.decider.calls > 0 != got {
				t.Fatalf("decider consulted=%v, want %v", f.decider.calls > 0, got)
			}
		})
	}
}

// --- proxy-path non-rotate decline audits (BOS-484) ---

// TestPrepareFailover_SurfacesExhaustedDecline proves an all-exhausted proxy
// 429 (no swap target) is surfaced as a single EXHAUSTED audit row carrying the
// resolved session, from-account label, reset time, and usage-limited trigger,
// and that a repeat within the same reset minute dedups to one row.
func TestPrepareFailover_SurfacesExhaustedDecline(t *testing.T) {
	f := newRotationFixture(t)
	enableFailoverProxy(f.lc)
	store := &recentAuditStore{}
	f.lc.SetRotationRecorder(rotation.NewRecorder(store, zerolog.Nop()))
	f.lc.accountSwitchRegistry = mapSwitchRegistry{
		"acct-capped": switchAccount{Label: "capped-label"},
	}
	f.binding.bound = true
	resumeAt := time.Date(2026, 7, 23, 15, 30, 0, 0, time.UTC)
	f.decider.outcome = rotation.Outcome{Kind: rotation.OutcomeAllExhausted, ResumeAt: resumeAt}

	res, err := f.lc.PrepareFailover(context.Background(), f.sessionID, 429)
	if err != nil {
		t.Fatalf("PrepareFailover: %v", err)
	}
	if res.Rotate {
		t.Fatalf("Rotate = true, want false on exhausted pool")
	}
	events := store.all()
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.SessionID != f.sessionID || ev.ChatID != "" {
		t.Fatalf("session/chat = %q/%q, want %q/\"\"", ev.SessionID, ev.ChatID, f.sessionID)
	}
	if ev.Outcome != "ROTATION_OUTCOME_EXHAUSTED" {
		t.Fatalf("Outcome = %q, want ROTATION_OUTCOME_EXHAUSTED", ev.Outcome)
	}
	if ev.Trigger != "ROTATION_TRIGGER_USAGE_LIMITED" {
		t.Fatalf("Trigger = %q, want ROTATION_TRIGGER_USAGE_LIMITED", ev.Trigger)
	}
	if ev.Provider != "claude" || ev.FromAccount != "capped-label" {
		t.Fatalf("provider/from = %q/%q, want claude/capped-label", ev.Provider, ev.FromAccount)
	}
	if ev.ResetAt == nil || !ev.ResetAt.Equal(resumeAt) {
		t.Fatalf("ResetAt = %v, want %v", ev.ResetAt, resumeAt)
	}

	// Repeat within the same reset minute dedups to one row (per-episode).
	if _, err := f.lc.PrepareFailover(context.Background(), f.sessionID, 429); err != nil {
		t.Fatalf("PrepareFailover (repeat): %v", err)
	}
	if got := len(store.all()); got != 1 {
		t.Fatalf("audit events after repeat = %d, want 1 (dedup)", got)
	}

	// A new reset minute is a new episode and records again.
	f.decider.outcome = rotation.Outcome{
		Kind:     rotation.OutcomeAllExhausted,
		ResumeAt: resumeAt.Add(2 * time.Minute),
	}
	if _, err := f.lc.PrepareFailover(context.Background(), f.sessionID, 429); err != nil {
		t.Fatalf("PrepareFailover (new episode): %v", err)
	}
	if got := len(store.all()); got != 2 {
		t.Fatalf("audit events after new episode = %d, want 2", got)
	}
}

// TestPrepareFailover_SurfacesNoEligibleDecline proves a no-eligible-account
// proxy 429 is surfaced as a STATUS_ONLY_NO_ELIGIBLE_ACCOUNT audit row with the
// actionable detail, distinct from the exhausted outcome.
func TestPrepareFailover_SurfacesNoEligibleDecline(t *testing.T) {
	f := newRotationFixture(t)
	enableFailoverProxy(f.lc)
	store := &recentAuditStore{}
	f.lc.SetRotationRecorder(rotation.NewRecorder(store, zerolog.Nop()))
	f.binding.bound = true
	f.decider.outcome = rotation.Outcome{Kind: rotation.OutcomeNoEligibleAccount}

	if _, err := f.lc.PrepareFailover(context.Background(), f.sessionID, 429); err != nil {
		t.Fatalf("PrepareFailover: %v", err)
	}
	events := store.all()
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Outcome != "ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT" {
		t.Fatalf("Outcome = %q, want STATUS_ONLY_NO_ELIGIBLE_ACCOUNT", ev.Outcome)
	}
	if ev.Detail != rotation.NoEligibleAccountDetail {
		t.Fatalf("Detail = %q, want NoEligibleAccountDetail", ev.Detail)
	}
	if ev.ResetAt != nil {
		t.Fatalf("ResetAt = %v, want nil for no-eligible", ev.ResetAt)
	}
}

// TestPrepareFailover_StatusOnlyDeclineNotAudited proves a capability-only
// decline (OutcomeStatusOnly) is NOT surfaced as an audit row — it is not an
// exhausted-pool 429.
func TestPrepareFailover_StatusOnlyDeclineNotAudited(t *testing.T) {
	f := newRotationFixture(t)
	enableFailoverProxy(f.lc)
	store := &recentAuditStore{}
	f.lc.SetRotationRecorder(rotation.NewRecorder(store, zerolog.Nop()))
	f.binding.bound = true
	f.decider.outcome = rotation.Outcome{Kind: rotation.OutcomeStatusOnly}

	if _, err := f.lc.PrepareFailover(context.Background(), f.sessionID, 429); err != nil {
		t.Fatalf("PrepareFailover: %v", err)
	}
	if got := len(store.all()); got != 0 {
		t.Fatalf("audit events = %d, want 0 (status-only not surfaced)", got)
	}
}

// TestPrepareFailover_ChatTargetSurfacesDeclineWithResolvedIDs proves the chat
// proxy path records the decline against the RESOLVED session id plus the chat
// id (the ids hydration reads), not the raw proxy target.
func TestPrepareFailover_ChatTargetSurfacesDeclineWithResolvedIDs(t *testing.T) {
	f := newRotationFixture(t)
	enableFailoverProxy(f.lc)
	store := &recentAuditStore{}
	f.lc.SetRotationRecorder(rotation.NewRecorder(store, zerolog.Nop()))
	f.lc.agentChats = &mockAgentChatStore{chatsBySession: map[string][]*models.AgentChat{
		f.sessionID: {{
			SessionID:      f.sessionID,
			AgentSessionID: "chat-1",
			AgentName:      "claude",
			AccountID:      stringPtr("acct-chat"),
		}},
	}}
	f.binding.binding.CappedAccountID = "acct-chat"
	resumeAt := time.Date(2026, 7, 23, 16, 0, 0, 0, time.UTC)
	f.decider.outcome = rotation.Outcome{Kind: rotation.OutcomeAllExhausted, ResumeAt: resumeAt}

	res, err := f.lc.PrepareFailover(context.Background(), ProxyTargetForChat("chat-1", "acct-default"), 429)
	if err != nil {
		t.Fatalf("PrepareFailover: %v", err)
	}
	if res.Rotate {
		t.Fatalf("Rotate = true, want false on exhausted pool")
	}
	events := store.all()
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.SessionID != f.sessionID {
		t.Fatalf("SessionID = %q, want resolved %q", ev.SessionID, f.sessionID)
	}
	if ev.ChatID != "chat-1" {
		t.Fatalf("ChatID = %q, want chat-1", ev.ChatID)
	}
	if ev.Outcome != "ROTATION_OUTCOME_EXHAUSTED" {
		t.Fatalf("Outcome = %q, want ROTATION_OUTCOME_EXHAUSTED", ev.Outcome)
	}
}

// --- CurrentBearer (first-leg proxy translation, BOS-326) ---

func TestCurrentBearer(t *testing.T) {
	acct := &models.Account{ID: "acct-capped", Provider: models.AccountProviderClaude, Label: "bound"}

	cases := []struct {
		name                  string
		enabled               bool
		bound                 bool
		wireGetter            bool
		getterAcct            *models.Account
		getterErr             bool
		materialize           map[string]string
		matErr                error
		failUntil             int
		wantToken             string
		wantErr               bool
		wantBearerUnavailable bool
		wantMaterializeCalls  int
	}{
		{
			name: "bound account → its materialized bearer", enabled: true, bound: true,
			wireGetter: true, getterAcct: acct,
			materialize: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "bound-token"},
			wantToken:   "bound-token",
		},
		{
			name: "proxy disabled after spawn still translates", enabled: false, bound: true,
			wireGetter: true, getterAcct: acct,
			materialize: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "bound-token"},
			wantToken:   "bound-token",
		},
		{
			name: "unbound session → empty", enabled: true, bound: false,
			wireGetter: true, getterAcct: acct,
			materialize: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "bound-token"},
			wantToken:   "",
		},
		{
			name: "no account getter wired → empty", enabled: true, bound: true,
			wireGetter:  false,
			materialize: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "bound-token"},
			wantToken:   "",
		},
		{
			name: "getter error → error", enabled: true, bound: true,
			wireGetter: true, getterErr: true,
			wantErr: true,
		},
		{
			name: "materialize fails every retry → ErrBearerUnavailable", enabled: true, bound: true,
			wireGetter: true, getterAcct: acct, matErr: context.DeadlineExceeded,
			wantErr: true, wantBearerUnavailable: true,
		},
		{
			name: "materialize fails then recovers on retry → bearer", enabled: true, bound: true,
			wireGetter: true, getterAcct: acct, matErr: context.DeadlineExceeded, failUntil: 2,
			materialize: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "bound-token"},
			wantToken:   "bound-token",
		},
		{
			name: "non-transient materialize failure → ordinary error", enabled: true, bound: true,
			wireGetter: true, getterAcct: acct,
			matErr:               grpcstatus.Error(codes.FailedPrecondition, "credential missing for account"),
			wantErr:              true,
			wantMaterializeCalls: 1,
		},
		{
			name: "empty materialized token → empty", enabled: true, bound: true,
			wireGetter: true, getterAcct: acct,
			materialize: map[string]string{},
			wantToken:   "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRotationFixture(t)
			// Instant, deterministic retry schedule (3 attempts, no backoff) so the
			// transient-Materialize path exercises retry + exhaustion without sleeps.
			f.lc.SetBearerRetryForTest(3, 0)
			if tc.enabled {
				enableFailoverProxy(f.lc)
			} else {
				f.lc.SetRotationConfig(config.ManagedAccountsConfig{FailoverProxy: boolPtr(false)})
			}
			f.binding.bound = tc.bound
			f.materializer.env = tc.materialize
			f.materializer.failUntil = tc.failUntil
			f.materializer.err = tc.matErr
			if tc.wireGetter {
				f.lc.SetAccountGetter(func(_ context.Context, id string) (*models.Account, error) {
					if tc.getterErr {
						return nil, context.DeadlineExceeded
					}
					if id != "acct-capped" {
						t.Fatalf("getter called with id=%q, want acct-capped", id)
					}
					return tc.getterAcct, nil
				})
			}

			got, err := f.lc.CurrentBearer(context.Background(), f.sessionID)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("CurrentBearer: want error, got nil (token=%q)", got)
				}
				if tc.wantBearerUnavailable && !errors.Is(err, ErrBearerUnavailable) {
					t.Fatalf("CurrentBearer error = %v, want wrapped ErrBearerUnavailable", err)
				}
				if !tc.wantBearerUnavailable && errors.Is(err, ErrBearerUnavailable) {
					t.Fatalf("CurrentBearer error = %v, must not wrap ErrBearerUnavailable", err)
				}
				if tc.wantMaterializeCalls != 0 && f.materializer.calls != tc.wantMaterializeCalls {
					t.Fatalf("Materialize calls=%d, want %d", f.materializer.calls, tc.wantMaterializeCalls)
				}
				return
			}
			if err != nil {
				t.Fatalf("CurrentBearer: unexpected error: %v", err)
			}
			if got != tc.wantToken {
				t.Fatalf("CurrentBearer = %q, want %q", got, tc.wantToken)
			}
			if tc.wantMaterializeCalls != 0 && f.materializer.calls != tc.wantMaterializeCalls {
				t.Fatalf("Materialize calls=%d, want %d", f.materializer.calls, tc.wantMaterializeCalls)
			}
		})
	}
}

func TestCurrentBearer_ChatTargetUsesChatAccount(t *testing.T) {
	cases := []struct {
		name              string
		chatAccountID     *string
		fallbackAccountID string
		wantGetterID      string
		wantToken         string
	}{
		{
			name:              "persisted chat account wins",
			chatAccountID:     stringPtr("acct-chat"),
			fallbackAccountID: "acct-default",
			wantGetterID:      "acct-chat",
			wantToken:         "chat-token",
		},
		{
			name:              "pre-persist chat falls back to resolved account",
			chatAccountID:     nil,
			fallbackAccountID: "acct-default",
			wantGetterID:      "acct-default",
			wantToken:         "default-token",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRotationFixture(t)
			f.lc.agentChats = &mockAgentChatStore{chatsBySession: map[string][]*models.AgentChat{
				f.sessionID: {{
					SessionID:      f.sessionID,
					AgentSessionID: "chat-1",
					AgentName:      "claude",
					AccountID:      tc.chatAccountID,
				}},
			}}
			f.materializer.env = map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": tc.wantToken}
			f.lc.SetAccountGetter(func(_ context.Context, id string) (*models.Account, error) {
				if id != tc.wantGetterID {
					t.Fatalf("getter called with id=%q, want %q", id, tc.wantGetterID)
				}
				return &models.Account{ID: id, Provider: models.AccountProviderClaude, Label: "chat"}, nil
			})

			got, err := f.lc.CurrentBearer(context.Background(), ProxyTargetForChat("chat-1", tc.fallbackAccountID))
			if err != nil {
				t.Fatalf("CurrentBearer: %v", err)
			}
			if got != tc.wantToken {
				t.Fatalf("CurrentBearer = %q, want %q", got, tc.wantToken)
			}
		})
	}
}

func TestCurrentBearer_BlockingMaterializeHonorsRetryBudget(t *testing.T) {
	f := newRotationFixture(t)
	f.lc.SetBearerRetryForTest(1, 0, 10*time.Millisecond)
	enableFailoverProxy(f.lc)
	f.binding.bound = true
	f.materializer.blockUntilContext = true
	f.lc.SetAccountGetter(func(_ context.Context, id string) (*models.Account, error) {
		if id != "acct-capped" {
			t.Fatalf("getter called with id=%q, want acct-capped", id)
		}
		return &models.Account{ID: "acct-capped", Provider: models.AccountProviderClaude, Label: "bound"}, nil
	})

	start := time.Now()
	got, err := f.lc.CurrentBearer(context.Background(), f.sessionID)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("CurrentBearer: want error, got nil (token=%q)", got)
	}
	if !errors.Is(err, ErrBearerUnavailable) {
		t.Fatalf("CurrentBearer error = %v, want wrapped ErrBearerUnavailable", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("CurrentBearer elapsed %s, want retry budget to cap blocking materialize promptly", elapsed)
	}
	if f.materializer.calls != 1 {
		t.Fatalf("Materialize calls=%d, want 1", f.materializer.calls)
	}
}

func stringPtr(s string) *string { return &s }

func TestPrepareAndCommitFailover_ChatTargetBindsChatAccount(t *testing.T) {
	enableFinalizeTelemetry(t)
	f := newRotationFixture(t)
	recorder := &finalizeTelemetryRecorder{}
	f.lc.SetTelemetry(recorder)
	enableFailoverProxy(f.lc)
	chatStore := &mockAgentChatStore{chatsBySession: map[string][]*models.AgentChat{
		f.sessionID: {{
			SessionID:      f.sessionID,
			AgentSessionID: "chat-1",
			AgentName:      "claude",
			AccountID:      stringPtr("acct-chat"),
		}},
	}}
	f.lc.agentChats = chatStore
	f.binding.binding.CappedAccountID = "acct-chat"
	f.decider.outcome = rotation.Outcome{
		Kind:        rotation.OutcomeRotate,
		NextAccount: &models.Account{ID: "acct-next", Provider: models.AccountProviderClaude},
	}
	f.materializer.env = map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "next-token"}

	res, err := f.lc.PrepareFailover(context.Background(), ProxyTargetForChat("chat-1", "acct-default"), 429)
	if err != nil {
		t.Fatalf("PrepareFailover: %v", err)
	}
	if !res.Rotate || res.Token != "next-token" || res.FromAccountID != "acct-chat" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if got := f.decider.lastSig.CappedAccountID; got != "acct-chat" {
		t.Fatalf("decider capped account = %q, want acct-chat", got)
	}
	if f.binding.lastSession == nil || f.binding.lastSession.AccountID == nil || *f.binding.lastSession.AccountID != "acct-chat" {
		t.Fatalf("binding session account = %#v, want acct-chat", f.binding.lastSession)
	}
	if err := f.lc.CommitFailover(context.Background(), ProxyTargetForChat("chat-1", "acct-default"), res); err != nil {
		t.Fatalf("CommitFailover: %v", err)
	}
	if len(chatStore.accountIDUpdates) != 1 {
		t.Fatalf("chat account updates = %d, want 1", len(chatStore.accountIDUpdates))
	}
	if got := chatStore.accountIDUpdates[0].accountID; got == nil || *got != "acct-next" {
		t.Fatalf("chat account update = %v, want acct-next", got)
	}
	if sessAcct := f.sessions.sessions[f.sessionID].AccountID; sessAcct != nil {
		t.Fatalf("session account = %v, want unchanged nil", sessAcct)
	}
	assertFailoverRotationTelemetry(t, recorder, "usage_limit")
}

func TestCommitFailover_ChatTargetBindsSessionForSameAgentDefault(t *testing.T) {
	enableFinalizeTelemetry(t)
	f := newRotationFixture(t)
	recorder := &finalizeTelemetryRecorder{}
	f.lc.SetTelemetry(recorder)
	f.lc.agentChats = &mockAgentChatStore{chatsBySession: map[string][]*models.AgentChat{
		f.sessionID: {{
			SessionID:      f.sessionID,
			AgentSessionID: "chat-1",
			AgentName:      "claude",
		}},
	}}
	res := FailoverResult{
		Rotate:        true,
		NextAccountID: "acct-next",
		FromAccountID: "acct-default",
		Provider:      "claude",
		Trigger:       "ROTATION_TRIGGER_USAGE_LIMITED",
	}
	if err := f.lc.CommitFailover(context.Background(), ProxyTargetForChat("chat-1", "acct-default"), res); err != nil {
		t.Fatalf("CommitFailover: %v", err)
	}
	if got := f.sessions.sessions[f.sessionID].AccountID; got == nil || *got != "acct-next" {
		t.Fatalf("session account = %v, want acct-next", got)
	}
	assertFailoverRotationTelemetry(t, recorder, "usage_limit")
}

// --- CommitFailover: account_id persisted to the SECOND account, one audit ---

func TestCommitFailover_persistsSecondAccountAndAudits(t *testing.T) {
	enableFinalizeTelemetry(t)
	f := newRotationFixture(t)
	recorder := &finalizeTelemetryRecorder{}
	f.lc.SetTelemetry(recorder)
	enableFailoverProxy(f.lc)
	store := &captureAuditStore{}
	f.lc.SetRotationRecorder(rotation.NewRecorder(store, zerolog.Nop()))

	res := FailoverResult{
		Rotate:        true,
		Token:         "next-token",
		NextAccountID: "acct-next",
		FromAccountID: "acct-capped",
		Provider:      "claude",
		Trigger:       "ROTATION_TRIGGER_USAGE_LIMITED",
	}
	if err := f.lc.CommitFailover(context.Background(), f.sessionID, res); err != nil {
		t.Fatalf("CommitFailover: %v", err)
	}

	sess := f.sessions.sessions[f.sessionID]
	if sess.AccountID == nil || *sess.AccountID != "acct-next" {
		t.Fatalf("persisted account is not the second account: got %v, want acct-next", sess.AccountID)
	}
	events := store.all()
	if len(events) != 1 {
		t.Fatalf("want exactly one AuditEvent, got %d", len(events))
	}
	// No registry is wired in this fixture, so the audit sides degrade to the
	// shared short-id display fallback (account.ShortID) — the same policy the
	// chat-rotator path (account.Resolver.Label) uses for unresolvable ids.
	if events[0].ToAccount != account.ShortID("acct-next") || events[0].Outcome != "ROTATION_OUTCOME_ROTATED" {
		t.Fatalf("unexpected audit event: %+v", events[0])
	}
	if !strings.Contains(events[0].Detail, "no pane respawn") {
		t.Fatalf("audit detail should note no pane respawn, got %q", events[0].Detail)
	}
	assertFailoverRotationTelemetry(t, recorder, "usage_limit")
}

// TestCommitFailover_auditsResolvedLabels pins that both audit sides are
// resolved to human labels (via the account registry) instead of the raw
// account ids the FailoverResult carries, so the TUI renders emails.
func TestCommitFailover_auditsResolvedLabels(t *testing.T) {
	f := newRotationFixture(t)
	enableFailoverProxy(f.lc)
	store := &captureAuditStore{}
	f.lc.SetRotationRecorder(rotation.NewRecorder(store, zerolog.Nop()))
	f.lc.accountSwitchRegistry = mapSwitchRegistry{
		"acct-capped": {ID: "acct-capped", Label: "yuki@kamik.ai", Status: AccountActive},
		"acct-next":   {ID: "acct-next", Label: "dave@kamik.ai", Status: AccountActive},
	}

	res := FailoverResult{
		Rotate:        true,
		NextAccountID: "acct-next",
		FromAccountID: "acct-capped",
		Provider:      "claude",
		Trigger:       "ROTATION_TRIGGER_USAGE_LIMITED",
	}
	if err := f.lc.CommitFailover(context.Background(), f.sessionID, res); err != nil {
		t.Fatalf("CommitFailover: %v", err)
	}
	events := store.all()
	if len(events) != 1 {
		t.Fatalf("want exactly one AuditEvent, got %d", len(events))
	}
	if events[0].FromAccount != "yuki@kamik.ai" || events[0].ToAccount != "dave@kamik.ai" {
		t.Fatalf("audit did not resolve labels: from=%q to=%q, want yuki@kamik.ai / dave@kamik.ai",
			events[0].FromAccount, events[0].ToAccount)
	}
}

func TestCommitFailover_noRotateIsNoOp(t *testing.T) {
	f := newRotationFixture(t)
	store := &captureAuditStore{}
	f.lc.SetRotationRecorder(rotation.NewRecorder(store, zerolog.Nop()))

	if err := f.lc.CommitFailover(context.Background(), f.sessionID, FailoverResult{Rotate: false}); err != nil {
		t.Fatalf("CommitFailover: %v", err)
	}
	if len(store.all()) != 0 {
		t.Fatalf("no-op commit should record no audit, got %d", len(store.all()))
	}
}

// --- Log hygiene: the bearer never appears in any log line ---

func TestFailover_neverLogsToken(t *testing.T) {
	var buf bytes.Buffer
	f := newRotationFixture(t)
	f.lc.logger = zerolog.New(&buf)
	enableFailoverProxy(f.lc)
	store := &captureAuditStore{}
	f.lc.SetRotationRecorder(rotation.NewRecorder(store, zerolog.New(&buf)))

	const secret = "next-token"
	f.decider.outcome = rotation.Outcome{
		Kind:        rotation.OutcomeRotate,
		NextAccount: &models.Account{ID: "acct-next", Provider: models.AccountProviderClaude},
	}
	f.materializer.env = map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": secret}

	res, err := f.lc.PrepareFailover(context.Background(), f.sessionID, 429)
	if err != nil {
		t.Fatalf("PrepareFailover: %v", err)
	}
	if !res.Rotate || res.Token != secret {
		t.Fatalf("expected rotate with token, got %+v", res)
	}
	if err := f.lc.CommitFailover(context.Background(), f.sessionID, res); err != nil {
		t.Fatalf("CommitFailover: %v", err)
	}

	// Also exercise the materialize-failure log path (which logs err + ids).
	f.materializer.err = context.DeadlineExceeded
	if _, err := f.lc.PrepareFailover(context.Background(), f.sessionID, 401); err != nil {
		t.Fatalf("PrepareFailover(matErr): %v", err)
	}

	if strings.Contains(buf.String(), secret) {
		t.Fatalf("token leaked into logs:\n%s", buf.String())
	}
}

// --- proxyBaseURL gating ---

func TestProxyBaseURL_gating(t *testing.T) {
	f := newRotationFixture(t)
	enableFailoverProxy(f.lc)
	f.lc.SetProxyPort(4321)
	f.lc.SetProxyRegistrar(stubRegistrar{token: "tok"})

	acctID := "acct-1"
	claude := &models.Session{ID: "s1", AgentName: "claude", AccountID: &acctID}
	if got := f.lc.proxyBaseURL(claude); got != "http://127.0.0.1:4321/s/tok" {
		t.Fatalf("claude session base URL = %q, want loopback proxy URL", got)
	}

	// Codex session: never injected (only Claude honors ANTHROPIC_BASE_URL).
	codex := &models.Session{ID: "s2", AgentName: "codex"}
	if got := f.lc.proxyBaseURL(codex); got != "" {
		t.Fatalf("codex session must not get a proxy URL, got %q", got)
	}

	// Proxy not live (port 0): no injection.
	f.lc.SetProxyPort(0)
	if got := f.lc.proxyBaseURL(claude); got != "" {
		t.Fatalf("unbound proxy must not inject, got %q", got)
	}

	// Flag off: no injection even when live.
	f.lc.SetProxyPort(4321)
	f.lc.SetRotationConfig(config.ManagedAccountsConfig{FailoverProxy: boolPtr(false)})
	if got := f.lc.proxyBaseURL(claude); got != "" {
		t.Fatalf("flag off must not inject, got %q", got)
	}
}

// TestProxyBaseURL_fixedPort confirms the injection gate is unchanged by the
// BOS-409 fixed-port change: a fixed proxyPort (44127) composes the expected
// loopback URL, and proxyPort == 0 still suppresses injection entirely.
func TestProxyBaseURL_fixedPort(t *testing.T) {
	f := newRotationFixture(t)
	enableFailoverProxy(f.lc)
	f.lc.SetProxyRegistrar(stubRegistrar{token: "tok"})

	acctID := "acct-1"
	claude := &models.Session{ID: "s1", AgentName: "claude", AccountID: &acctID}

	f.lc.SetProxyPort(44127)
	if got := f.lc.proxyBaseURL(claude); got != "http://127.0.0.1:44127/s/tok" {
		t.Fatalf("fixed-port base URL = %q, want http://127.0.0.1:44127/s/tok", got)
	}

	f.lc.SetProxyPort(0)
	if got := f.lc.proxyBaseURL(claude); got != "" {
		t.Fatalf("proxyPort==0 must still suppress injection, got %q", got)
	}
}

type stubRegistrar struct{ token string }

func (s stubRegistrar) TokenForSession(string) string { return s.token }
func (s stubRegistrar) ForgetBearer(string)           {}
func (s stubRegistrar) ForgetAllBearers()             {}
func (s stubRegistrar) TokenForChat(string, string, string) string {
	return s.token
}
func (s stubRegistrar) AdoptToken(string, string)                        {}
func (s stubRegistrar) AdoptTokenForChat(string, string, string, string) {}
func (s stubRegistrar) RebuildTokenRegistry(context.Context) error       { return nil }
