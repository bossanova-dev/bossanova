package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/account"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/rotation"
	"github.com/recurser/bossd/internal/tmux"
	"github.com/rs/zerolog"
)

// getOnlyAccountStore is a minimal db.AccountStore that serves a single account
// from Get; the other methods are unused by accountRegistryAdapter and panic if
// called so an accidental new dependency is caught loudly.
type getOnlyAccountStore struct{ acct *models.Account }

func (s getOnlyAccountStore) Get(_ context.Context, _ string) (*models.Account, error) {
	return s.acct, nil
}
func (getOnlyAccountStore) Create(context.Context, db.CreateAccountParams) (*models.Account, error) {
	panic("unexpected Create")
}
func (getOnlyAccountStore) List(context.Context) ([]*models.Account, error) { panic("unexpected List") }
func (getOnlyAccountStore) ListByProvider(context.Context, models.AccountProvider) ([]*models.Account, error) {
	panic("unexpected ListByProvider")
}
func (getOnlyAccountStore) Update(context.Context, string, db.UpdateAccountParams) (*models.Account, error) {
	panic("unexpected Update")
}
func (getOnlyAccountStore) Delete(context.Context, string) error { panic("unexpected Delete") }
func (getOnlyAccountStore) RecordTestResult(context.Context, string, *time.Time, string) error {
	panic("unexpected RecordTestResult")
}

func (getOnlyAccountStore) RecordUsageProbe(context.Context, string, models.UsageSnapshot) error {
	panic("unexpected RecordUsageProbe")
}

func (getOnlyAccountStore) RecordInjectionFailure(context.Context, string, string) error {
	panic("unexpected RecordInjectionFailure")
}

func (getOnlyAccountStore) ClearInjectionFailure(context.Context, string) error {
	panic("unexpected ClearInjectionFailure")
}

// TestAccountRegistryAdapter_FailedHealthMapsToFailed verifies the registry
// projection maps a models.AccountHealthFailed account to AccountFailed, so the
// switch rejects it (mirroring session creation's checkAccountEligible).
// TestAccountRegistryAdapter_AuthInvalidMapsToAuthInvalid pins that durable
// verification state reaches the switch predicate. The account is ACTIVE and
// HEALTHY — recording an auth-invalid verdict deliberately leaves Health alone
// — so without this arm the switch accepts it, kills the pane, rebinds the
// chat, and only then does the respawn silently fall back to ambient
// credentials.
func TestAccountRegistryAdapter_AuthInvalidMapsToAuthInvalid(t *testing.T) {
	acct := &models.Account{
		ID:        "acct-auth-invalid",
		Label:     "benched",
		Provider:  models.AccountProviderCodex,
		Status:    models.AccountStatusActive,
		Health:    models.AccountHealthOK,
		AuthCheck: models.AuthCheck{Outcome: models.AuthCheckOutcomeAuthInvalid},
	}
	adapter := accountRegistryAdapter{store: getOnlyAccountStore{acct: acct}}
	got, err := adapter.Account(context.Background(), acct.ID)
	if err != nil {
		t.Fatalf("Account: %v", err)
	}
	if got.Status != AccountAuthInvalid {
		t.Fatalf("status = %v, want AccountAuthInvalid", got.Status)
	}
}

// TestAccountRegistryAdapter_AuthInvalidOutranksCooling pins the ordering: a
// credential confirmed rejected is not a time-bounded bench, so reporting
// "cooling" would tell an operator to wait for something that will never fix it.
func TestAccountRegistryAdapter_AuthInvalidOutranksCooling(t *testing.T) {
	future := time.Now().Add(2 * time.Hour)
	acct := &models.Account{
		ID:            "acct-both",
		Label:         "benched-and-cooling",
		Provider:      models.AccountProviderCodex,
		Status:        models.AccountStatusActive,
		Health:        models.AccountHealthOK,
		CooldownUntil: &future,
		AuthCheck:     models.AuthCheck{Outcome: models.AuthCheckOutcomeAuthInvalid},
	}
	adapter := accountRegistryAdapter{store: getOnlyAccountStore{acct: acct}}
	got, err := adapter.Account(context.Background(), acct.ID)
	if err != nil {
		t.Fatalf("Account: %v", err)
	}
	if got.Status != AccountAuthInvalid {
		t.Fatalf("status = %v, want AccountAuthInvalid (must outrank Cooling)", got.Status)
	}
}

func TestAccountRegistryAdapter_FailedHealthMapsToFailed(t *testing.T) {
	adapter := accountRegistryAdapter{store: getOnlyAccountStore{acct: &models.Account{
		ID:       "acct-9",
		Provider: models.AccountProviderClaude,
		Label:    "Sick",
		Status:   models.AccountStatusActive,
		Health:   models.AccountHealthFailed,
	}}}
	sa, err := adapter.Account(context.Background(), "acct-9")
	if err != nil {
		t.Fatalf("Account: %v", err)
	}
	if sa.Status != AccountFailed {
		t.Errorf("Status = %v, want AccountFailed", sa.Status)
	}
}

// TestAccountRegistryAdapter_OKHealthActive verifies a healthy active account
// projects to AccountActive (the switchable case).
func TestAccountRegistryAdapter_OKHealthActive(t *testing.T) {
	adapter := accountRegistryAdapter{store: getOnlyAccountStore{acct: &models.Account{
		ID:       "acct-ok",
		Provider: models.AccountProviderClaude,
		Label:    "Fine",
		Status:   models.AccountStatusActive,
		Health:   models.AccountHealthOK,
	}}}
	sa, err := adapter.Account(context.Background(), "acct-ok")
	if err != nil {
		t.Fatalf("Account: %v", err)
	}
	if sa.Status != AccountActive {
		t.Errorf("Status = %v, want AccountActive", sa.Status)
	}
}

func TestSessionAccountBinding_SystemDefaultPersistsPresentEmpty(t *testing.T) {
	sessions := newMockSessionStore()
	sessions.sessions["sess-1"] = &models.Session{ID: "sess-1"}
	binding := sessionAccountBinding{sessions: sessions}

	if err := binding.BindSessionAccount(context.Background(), "sess-1", ""); err != nil {
		t.Fatalf("BindSessionAccount system default: %v", err)
	}
	got := sessions.sessions["sess-1"].AccountID
	if got == nil || *got != "" {
		t.Fatalf("session AccountID = %v, want present-empty system default", got)
	}

	current, err := binding.SessionAccount(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("SessionAccount: %v", err)
	}
	if current != "" {
		t.Fatalf("SessionAccount = %q, want empty system default", current)
	}
}

// --- switch-account stubs ---

type stubSwitchBinding struct {
	current    string
	currentErr error
	bindErr    error
	bound      []string
}

func (s *stubSwitchBinding) SessionAccount(_ context.Context, _ string) (string, error) {
	return s.current, s.currentErr
}

func (s *stubSwitchBinding) BindSessionAccount(_ context.Context, _, accountID string) error {
	s.bound = append(s.bound, accountID)
	return s.bindErr
}

type stubSwitchRegistry struct {
	acct switchAccount
	err  error
}

func (s stubSwitchRegistry) Account(_ context.Context, _ string) (switchAccount, error) {
	return s.acct, s.err
}

// TestResolveAccountLabel covers the side-neutral account-label resolver used
// by the manual-switch, failover, and headless-rotation audits: a hit returns
// the account label, a nil registry or a lookup error degrades to the short-id
// display fallback (account.ShortID — the SAME degradation policy as
// account.Resolver.Label, which feeds the chat rotator's audit from-side), and
// an empty id resolves to the unmanaged-credentials label regardless of
// registry state — so an audited side is never blank.
func TestResolveAccountLabel(t *testing.T) {
	ctx := context.Background()

	nilReg := &Lifecycle{}
	if got := nilReg.resolveAccountLabel(ctx, "acct-x"); got != "acct-x" {
		t.Errorf("nil registry: got %q, want short-id fallback acct-x", got)
	}
	// The empty-id → unmanaged-credentials mapping is the helper's own contract:
	// it must hold even with no registry wired (nil-registry daemons/tests).
	if got := nilReg.resolveAccountLabel(ctx, ""); got != account.UnmanagedLocalCredentialsLabel {
		t.Errorf("nil registry, empty id: got %q, want %q", got, account.UnmanagedLocalCredentialsLabel)
	}

	lc := &Lifecycle{accountSwitchRegistry: mapSwitchRegistry{
		"acct-x": {ID: "acct-x", Label: "dave@kamik.ai"},
	}}
	if got := lc.resolveAccountLabel(ctx, "acct-x"); got != "dave@kamik.ai" {
		t.Errorf("registry hit: got %q, want dave@kamik.ai", got)
	}
	// Lookup failure degrades to the shared 8-char display fallback, matching
	// what the chat-rotator path (account.Resolver.Label) would store for the
	// same unresolvable id.
	if got, want := lc.resolveAccountLabel(ctx, "acct-missing-long-id"), account.ShortID("acct-missing-long-id"); got != want {
		t.Errorf("registry miss: got %q, want short-id fallback %q", got, want)
	}
	if got := lc.resolveAccountLabel(ctx, ""); got != account.UnmanagedLocalCredentialsLabel {
		t.Errorf("wired registry, empty id: got %q, want %q", got, account.UnmanagedLocalCredentialsLabel)
	}
}

type stubChatWorking struct{ working bool }

func (s stubChatWorking) IsWorking(_ string) bool { return s.working }

type stubTranscriptProbe struct{ exists bool }

func (s stubTranscriptProbe) TranscriptExists(_ context.Context, _, _, _ string) bool {
	return s.exists
}

// newSwitchHarness reuses the StartTmuxChat harness and seeds a single chat
// backed by a live tmux pane, an active target account "acct-2", and the
// account-switch seams. Individual tests tweak the seams before calling
// SwitchAccount. tmuxOpts are forwarded to the underlying tmux client, so a
// test that has to sit through the respawn's readiness wait can shorten it
// rather than spend the production budget twice.
func newSwitchHarness(t *testing.T, tmuxOpts ...tmux.Option) *startTmuxChatHarness {
	t.Helper()
	h := newStartTmuxChatHarness(t, tmuxOpts...)
	tmuxName := "bossd-agent-run-agent-1"
	h.chats.chatsBySession = map[string][]*models.AgentChat{
		"sess-1": {
			{
				SessionID:       "sess-1",
				AgentSessionID:  "agent-1",
				Title:           "My chat",
				TmuxSessionName: &tmuxName,
			},
		},
	}
	h.lc.accountSwitchRegistry = stubSwitchRegistry{acct: switchAccount{
		ID: "acct-2", Provider: "claude", Label: "Work", Status: AccountActive,
	}}
	return h
}

// TestSwitchAccount_HappyResume: an active target with a resumable chat stops
// the pane, rebinds the session, and respawns resuming the prior id WITHOUT
// resending the interrupted prompt (D12).
func TestSwitchAccount_HappyResume(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newSwitchHarness(t)
	store := &captureAuditStore{}
	h.lc.SetRotationRecorder(rotation.NewRecorder(store, zerolog.Nop()))

	res, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2",
	})
	if err != nil {
		t.Fatalf("SwitchAccount: %v", err)
	}
	if !res.Resumed {
		t.Errorf("Resumed = false, want true")
	}
	if !strings.Contains(res.NoticeText, "resumed") {
		t.Errorf("NoticeText = %q, want it to mention resumed", res.NoticeText)
	}
	// The audit Detail carries only the outcome suffix — the "switched to
	// <label> — " sentence stays in the pane notice; the rotation history
	// renders the switch line from the structured From/To fields instead.
	events := store.all()
	if len(events) != 1 {
		t.Fatalf("want exactly one AuditEvent, got %d", len(events))
	}
	if got, want := events[0].Detail, "resumed"; got != want {
		t.Errorf("audit Detail = %q, want outcome suffix %q only", got, want)
	}
	if res.TargetLabel != "Work" {
		t.Errorf("TargetLabel = %q, want Work", res.TargetLabel)
	}
	// STOP happened.
	if h.findCall("kill-session") == nil {
		t.Error("expected tmux kill-session (STOP), none recorded")
	}
	// REBIND persisted onto the CHAT (chat-scoped binding, BOS-381), not the
	// session — the session's own binding is left untouched.
	if got := h.chats.chatsBySession["sess-1"][0].AccountID; got == nil || *got != "acct-2" {
		t.Errorf("chat AccountID = %v, want acct-2", got)
	}
	if got := h.sessions.sessions["sess-1"].AccountID; got != nil {
		t.Errorf("session AccountID = %v, want nil (switch binds the chat, not the session)", got)
	}
	// RESPAWN resumed the prior id and did NOT resend the interrupted prompt.
	last := h.agentFake.LastBuildInteractiveCommand
	if last == nil {
		t.Fatal("expected a respawn BuildInteractiveCommand call")
	}
	if !last.GetResume() {
		t.Error("respawn Resume = false, want true")
	}
	if last.GetSessionId() != "agent-1" {
		t.Errorf("respawn SessionId = %q, want agent-1 (resume prior id)", last.GetSessionId())
	}
	// D12 still holds: the payload is the freshly composed account-switch
	// notice, never a replay of the interrupted prompt (the session's stored
	// plan is what a replay would reach for).
	if got, want := last.GetInitialPrompt(), res.NoticeText; got != want {
		t.Errorf("respawn InitialPrompt = %q, want the account-switch notice %q", got, want)
	}
	if last.GetInitialPrompt() == h.sessions.sessions["sess-1"].Plan {
		t.Errorf("interrupted prompt was resent: %q (D12)", last.GetInitialPrompt())
	}
	if last.GetInitialCommand() != "" {
		t.Errorf("respawn InitialCommand = %q, want empty (the notice is prompt text, not a command)",
			last.GetInitialCommand())
	}
	// BOS-1135: the notice is SUBMITTED, not left pending in the composer — that
	// is what returns the chat to a visibly running state.
	if n := h.tmuxFake.enterSendKeysCount(); n != 1 {
		t.Errorf("Enter presses = %d, want exactly 1 (the notice is submitted)", n)
	}
}

// TestSwitchAccount_StaleTranscriptStartsFresh: a missing transcript falls back
// to a fresh chat under the new account.
func TestSwitchAccount_StaleTranscriptStartsFresh(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newSwitchHarness(t)
	h.lc.accountSwitchTranscripts = stubTranscriptProbe{exists: false}
	store := &captureAuditStore{}
	h.lc.SetRotationRecorder(rotation.NewRecorder(store, zerolog.Nop()))

	res, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2",
	})
	if err != nil {
		t.Fatalf("SwitchAccount: %v", err)
	}
	if res.Resumed {
		t.Errorf("Resumed = true, want false (fresh)")
	}
	if !strings.Contains(res.NoticeText, "started fresh") {
		t.Errorf("NoticeText = %q, want it to mention started fresh", res.NoticeText)
	}
	// Fresh fallback: audit Detail is the outcome suffix only, without the
	// duplicated "switched to <label>" sentence (that stays in the notice).
	events := store.all()
	if len(events) != 1 {
		t.Fatalf("want exactly one AuditEvent, got %d", len(events))
	}
	if got, want := events[0].Detail, "started fresh (could not resume this conversation cross-account)"; got != want {
		t.Errorf("audit Detail = %q, want outcome suffix %q only", got, want)
	}
	last := h.agentFake.LastBuildInteractiveCommand
	if last == nil {
		t.Fatal("expected a respawn BuildInteractiveCommand call")
	}
	if last.GetResume() {
		t.Error("fresh respawn Resume = true, want false")
	}
	if last.GetSessionId() == "agent-1" {
		t.Error("fresh respawn reused prior id, want a freshly minted id")
	}
}

// TestSwitchAccount_CrossAccountResumeUnsupported: even with a transcript, a
// provider that cannot resume cross-account starts fresh.
func TestSwitchAccount_CrossAccountResumeUnsupported(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	orig := resumeFeasibleCrossAccount
	resumeFeasibleCrossAccount = func(string) bool { return false }
	defer func() { resumeFeasibleCrossAccount = orig }()

	h := newSwitchHarness(t)
	h.lc.accountSwitchTranscripts = stubTranscriptProbe{exists: true}

	res, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2",
	})
	if err != nil {
		t.Fatalf("SwitchAccount: %v", err)
	}
	if res.Resumed {
		t.Errorf("Resumed = true, want false (cross-account resume unsupported)")
	}
	if !strings.Contains(res.NoticeText, "started fresh") {
		t.Errorf("NoticeText = %q, want started fresh", res.NoticeText)
	}
}

// TestSwitchAccount_CrossAgentChatSucceeds: a chat whose agent differs from its
// session's agent now switches successfully (BOS-381). The old
// ErrCrossAgentSwitchUnsupported guard existed because the switch rebound the
// SESSION; with chat-scoped binding the codex chat's own account is rebound and
// the parent claude session's binding is left untouched.
func TestSwitchAccount_CrossAgentChatSucceeds(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newSwitchHarness(t)
	// The session runs "claude"; make the selected chat a cross-agent codex chat
	// and target a codex account. The respawn must dispatch under the chat's
	// provider (BOS-381), so the codex runner must be loaded.
	h.lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": h.agentFake, "codex": h.agentFake})
	h.chats.chatsBySession["sess-1"][0].AgentName = "codex"
	h.lc.accountSwitchRegistry = stubSwitchRegistry{acct: switchAccount{
		ID: "acct-2", Provider: "codex", Label: "Codex Work", Status: AccountActive,
	}}

	res, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2",
	})
	if err != nil {
		t.Fatalf("SwitchAccount cross-agent: %v", err)
	}
	if res.TargetLabel != "Codex Work" {
		t.Errorf("TargetLabel = %q, want Codex Work", res.TargetLabel)
	}
	// The CHAT's account is rebound; the parent session's binding is untouched.
	if got := h.chats.chatsBySession["sess-1"][0].AccountID; got == nil || *got != "acct-2" {
		t.Errorf("chat AccountID = %v, want acct-2 (cross-agent chat rebound)", got)
	}
	if got := h.sessions.sessions["sess-1"].AccountID; got != nil {
		t.Errorf("session AccountID = %v, want nil (session binding untouched)", got)
	}
}

// TestSwitchAccount_NonResumableCrossAgentRespawnKeepsChatProviderAndModel covers
// the cross-product the other switch tests miss: cross-agent AND non-resumable.
// TestSwitchAccount_CrossAgentChatSucceeds resumes, so StartTmuxChat reads the
// chat row and gets the provider right for free; the non-resumable tests are
// same-agent, so the session seed happens to be correct. Only together do they
// expose the gap: with no transcript there is NO row to read, so provider and
// model fall back to the parent SESSION's unless the respawn carries the chat's
// own. A codex chat inside a claude session reaches this path routinely, because
// its provider rollout id defeats the transcript probe (see switchResumeID).
//
// The regression this guards: the fresh row came back up as claude while the
// codex account was persisted onto it, so the claude runner built argv while
// account materialization — which dispatches on the ACCOUNT's provider — handed
// that claude process codex env (BOS-1135).
func TestSwitchAccount_NonResumableCrossAgentRespawnKeepsChatProviderAndModel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newSwitchHarness(t)
	// The session runs claude; the selected chat is a cross-agent codex chat with
	// its own model, and the target is a codex account.
	h.lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": h.agentFake, "codex": h.agentFake})
	h.chats.chatsBySession["sess-1"][0].AgentName = "codex"
	h.chats.chatsBySession["sess-1"][0].Model = "gpt-5-codex"
	h.lc.accountSwitchRegistry = stubSwitchRegistry{acct: switchAccount{
		ID: "acct-2", Provider: "codex", Label: "Codex Work", Status: AccountActive,
	}}
	// Force the fresh path: no transcript means nothing to resume.
	h.lc.accountSwitchTranscripts = stubTranscriptProbe{exists: false}

	res, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2",
	})
	if err != nil {
		t.Fatalf("SwitchAccount: %v", err)
	}
	if res.Resumed {
		t.Fatal("precondition: this switch must start fresh, not resume")
	}

	h.chats.mu.Lock()
	creates := append([]db.CreateAgentChatParams(nil), h.chats.createCalls...)
	h.chats.mu.Unlock()
	if len(creates) != 1 {
		t.Fatalf("agent_chats Create calls = %d, want exactly 1 (the fresh chat)", len(creates))
	}
	fresh := creates[0]
	if fresh.AgentName != "codex" {
		t.Errorf("fresh chat AgentName = %q, want codex (the CHAT's provider), not the session seed %q",
			fresh.AgentName, h.sessions.sessions["sess-1"].AgentName)
	}
	if fresh.Model != "gpt-5-codex" {
		t.Errorf("fresh chat Model = %q, want gpt-5-codex (the CHAT's model), not the session seed %q",
			fresh.Model, h.sessions.sessions["sess-1"].Model)
	}
	// Provider and account must stay CONSISTENT: carrying one without the other is
	// the pairing this test exists to forbid.
	if fresh.AccountID == nil || *fresh.AccountID != "acct-2" {
		t.Errorf("fresh chat AccountID = %v, want acct-2 (the codex target)", fresh.AccountID)
	}
}

// TestSwitchAccount_NonResumableCrossAgentRespawnDropsSessionModelWhenChatHasNone
// proves a cross-agent chat carrying NO model of its own does not inherit the
// SESSION's model on the fresh respawn path.
//
// An empty chat model is a MEANINGFUL value, not an absent one: store.go
// documents "" as "the agent CLI default", and the runner plugin resolves it.
// Inheriting the claude session's id instead would build `codex --model
// <claude id>`, which fails — and the switch already killed the old pane by
// then, so the chat is left dead rather than merely degraded (BOS-1135).
func TestSwitchAccount_NonResumableCrossAgentRespawnDropsSessionModelWhenChatHasNone(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newSwitchHarness(t)
	h.lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": h.agentFake, "codex": h.agentFake})
	// The session seed must be NON-EMPTY or this test cannot fail: the harness
	// leaves Model empty, which would satisfy the assertion below without the
	// cross-agent reset ever running.
	h.sessions.sessions["sess-1"].Model = "claude-opus-4"
	// A cross-agent codex chat that never bound a model of its own.
	h.chats.chatsBySession["sess-1"][0].AgentName = "codex"
	h.chats.chatsBySession["sess-1"][0].Model = ""
	h.lc.accountSwitchRegistry = stubSwitchRegistry{acct: switchAccount{
		ID: "acct-2", Provider: "codex", Label: "Codex Work", Status: AccountActive,
	}}
	// Force the fresh path: no transcript means nothing to resume.
	h.lc.accountSwitchTranscripts = stubTranscriptProbe{exists: false}

	res, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2",
	})
	if err != nil {
		t.Fatalf("SwitchAccount: %v", err)
	}
	if res.Resumed {
		t.Fatal("precondition: this switch must start fresh, not resume")
	}

	h.chats.mu.Lock()
	creates := append([]db.CreateAgentChatParams(nil), h.chats.createCalls...)
	h.chats.mu.Unlock()
	if len(creates) != 1 {
		t.Fatalf("agent_chats Create calls = %d, want exactly 1 (the fresh chat)", len(creates))
	}
	fresh := creates[0]
	// The provider still comes from the chat — this test narrows the MODEL axis
	// only, and a wrong provider would make the model assertion meaningless.
	if fresh.AgentName != "codex" {
		t.Fatalf("fresh chat AgentName = %q, want codex (the CHAT's provider)", fresh.AgentName)
	}
	if fresh.Model != "" {
		t.Errorf("fresh chat Model = %q, want \"\" (the codex CLI default) — the claude session seed %q is a provider-scoped id the codex runner cannot start",
			fresh.Model, h.sessions.sessions["sess-1"].Model)
	}
}

// TestSwitchAccount_UnattendedCodexRespawnPreservesCapabilityProfile proves an
// account switch retains the original unattended launch's capability gate. The
// respawn is tmux-hosted, but its persisted session provenance still requires
// validating the target account's CODEX_HOME before it can start.
func TestSwitchAccount_UnattendedCodexRespawnPreservesCapabilityProfile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newSwitchHarness(t)
	h.sessions.sessions["sess-1"].IsTmuxUnattended = true
	h.chats.chatsBySession["sess-1"][0].AgentName = "codex"
	h.lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": h.agentFake, "codex": h.agentFake})
	h.lc.accountSwitchRegistry = stubSwitchRegistry{acct: switchAccount{
		ID: "acct-2", Provider: "codex", Label: "Codex Work", Status: AccountActive,
	}}

	if _, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2",
	}); err != nil {
		t.Fatalf("SwitchAccount: %v", err)
	}
	if h.agentRun.preflightCalls != 1 || len(h.agentRun.preflights) != 1 {
		t.Fatalf("preflight calls = %d, records = %d; want 1 each", h.agentRun.preflightCalls, len(h.agentRun.preflights))
	}
	if got := h.agentRun.preflights[0]; got.agentName != "codex" || got.profile != bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1 {
		t.Fatalf("preflight = %+v, want codex with tracker-plan-attachment-v1", got)
	}
}

// TestSwitchAccount_RepairCodexRespawnPreservesCapabilityProfile proves a
// repair chat's autonomous capability requirement survives account rotation
// even when its parent session is otherwise interactive.
func TestSwitchAccount_RepairCodexRespawnPreservesCapabilityProfile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newSwitchHarness(t)
	h.chats.chatsBySession["sess-1"][0].Title = "Repair: interactive session"
	h.chats.chatsBySession["sess-1"][0].AgentName = "codex"
	h.lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": h.agentFake, "codex": h.agentFake})
	h.lc.accountSwitchRegistry = stubSwitchRegistry{acct: switchAccount{
		ID: "acct-2", Provider: "codex", Label: "Codex Work", Status: AccountActive,
	}}

	if _, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2",
	}); err != nil {
		t.Fatalf("SwitchAccount: %v", err)
	}
	if h.agentRun.preflightCalls != 1 || len(h.agentRun.preflights) != 1 {
		t.Fatalf("preflight calls = %d, records = %d; want 1 each", h.agentRun.preflightCalls, len(h.agentRun.preflights))
	}
	if got := h.agentRun.preflights[0]; got.agentName != "codex" || got.profile != bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1 {
		t.Fatalf("preflight = %+v, want codex with tracker-plan-attachment-v1", got)
	}
}

// TestSwitchAccount_RespawnsTargetBesideLiveSibling: a session with another live
// chat still respawns the switched chat. StartTmuxChat's session-wide live-chat
// idempotency check would otherwise find the sibling's pane and return
// AlreadyExists — leaving the just-killed target stopped — so the switch bypasses
// that guard (HookOpts.AllowSiblingChat) for the targeted chat.
func TestSwitchAccount_RespawnsTargetBesideLiveSibling(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newSwitchHarness(t)
	siblingTmux := "bossd-agent-run-agent-sibling"
	h.chats.chatsBySession["sess-1"] = append(h.chats.chatsBySession["sess-1"], &models.AgentChat{
		SessionID:       "sess-1",
		AgentSessionID:  "agent-sibling",
		Title:           "Sibling",
		TmuxSessionName: &siblingTmux,
	})

	res, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2",
	})
	if err != nil {
		t.Fatalf("SwitchAccount with a live sibling: %v", err)
	}
	if !res.Resumed {
		t.Errorf("Resumed = false, want true (target respawned beside the sibling)")
	}
	if h.findCall("kill-session") == nil {
		t.Error("expected the target pane to be stopped")
	}
	last := h.agentFake.LastBuildInteractiveCommand
	if last == nil {
		t.Fatal("expected the target chat to be respawned")
	}
	if last.GetSessionId() != "agent-1" {
		t.Errorf("respawn SessionId = %q, want agent-1 (the switched chat, not the sibling)", last.GetSessionId())
	}
}

// TestSwitchResumeID_KeysOnLogicalAgentSessionID: the respawn resumes under the
// chat's logical agent_session_id, never the provider session id. StartTmuxChat
// reuses the resume id as the boss correlation key (row/tmux name/log path), so a
// Codex provider id (which differs from the agent_session_id) would re-key the
// respawn onto the provider id and orphan the selected chat.
func TestSwitchResumeID_KeysOnLogicalAgentSessionID(t *testing.T) {
	provider := "codex-rollout-xyz"
	got, ok := switchResumeID(&models.AgentChat{
		AgentSessionID:    "agent-1",
		ProviderSessionID: &provider,
	})
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if got != "agent-1" {
		t.Fatalf("resume id = %q, want agent-1 (logical id, not the provider rollout id)", got)
	}

	// No agent_session_id ⇒ nothing to resume.
	if _, ok := switchResumeID(&models.AgentChat{}); ok {
		t.Error("empty chat ok = true, want false")
	}
	if _, ok := switchResumeID(nil); ok {
		t.Error("nil chat ok = true, want false")
	}
}

// TestSwitchAccount_MidTurnRefusedWithoutForce: a WORKING chat is not
// interrupted unless forced; no pane is killed.
func TestSwitchAccount_MidTurnRefusedWithoutForce(t *testing.T) {
	h := newSwitchHarness(t)
	h.lc.accountSwitchWorking = stubChatWorking{working: true}

	_, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2",
	})
	if !errors.Is(err, ErrChatMidTurn) {
		t.Fatalf("err = %v, want ErrChatMidTurn", err)
	}
	if h.findCall("kill-session") != nil {
		t.Error("mid-turn refusal must not kill the pane")
	}
	if got := h.chats.chatsBySession["sess-1"][0].AccountID; got != nil {
		t.Errorf("chat AccountID = %v, want nil (no rebind on refusal)", got)
	}
}

// TestSwitchAccount_MidTurnForceProceeds: Force=true interrupts a WORKING chat.
func TestSwitchAccount_MidTurnForceProceeds(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newSwitchHarness(t)
	h.lc.accountSwitchWorking = stubChatWorking{working: true}

	res, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2", Force: true,
	})
	if err != nil {
		t.Fatalf("SwitchAccount(force): %v", err)
	}
	if !res.Resumed {
		t.Errorf("Resumed = false, want true")
	}
	if h.findCall("kill-session") == nil {
		t.Error("forced switch must kill the pane")
	}
}

// TestSwitchAccount_AutoRefusesRecoveredWorkingChat: the automatic rotation path
// (Auto) still honors the mid-turn guard. If the chat recovered to WORKING in the
// window between the rotator's LIMITED re-check and the switch, Auto — which can
// never set Force — must abort with ErrChatMidTurn rather than kill the live turn.
func TestSwitchAccount_AutoRefusesRecoveredWorkingChat(t *testing.T) {
	h := newSwitchHarness(t)
	h.lc.accountSwitchWorking = stubChatWorking{working: true}

	_, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2", Auto: true,
	})
	if !errors.Is(err, ErrChatMidTurn) {
		t.Fatalf("err = %v, want ErrChatMidTurn (Auto must not interrupt a recovered WORKING chat)", err)
	}
	if h.findCall("kill-session") != nil {
		t.Error("Auto refusal on a recovered WORKING chat must not kill the pane")
	}
	if got := h.chats.chatsBySession["sess-1"][0].AccountID; got != nil {
		t.Errorf("chat AccountID = %v, want nil (no rebind on refusal)", got)
	}
}

// TestSwitchAccount_AutoNoticeWording: on the Auto path the outcome notice uses
// the D4 AutoRotateNotice wording carrying the previous account's reset time,
// distinct from the manual "switched to <label> — resumed" text.
func TestSwitchAccount_AutoNoticeWording(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newSwitchHarness(t)
	reset := time.Date(2026, 7, 3, 15, 0, 0, 0, time.Local)

	res, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2",
		Auto: true, PreviousResetAt: reset,
	})
	if err != nil {
		t.Fatalf("SwitchAccount(auto): %v", err)
	}
	if !res.Resumed {
		t.Errorf("Resumed = false, want true")
	}
	if !strings.Contains(res.NoticeText, "switched to Work") {
		t.Errorf("NoticeText = %q, want it to mention the target label", res.NoticeText)
	}
	if !strings.Contains(res.NoticeText, "resets ~15:00") {
		t.Errorf("NoticeText = %q, want the D4 reset clause", res.NoticeText)
	}
	// The Auto notice is NOT the manual "— resumed" wording.
	if strings.Contains(res.NoticeText, "— resumed") {
		t.Errorf("NoticeText = %q, want the auto wording, not the manual resumed text", res.NoticeText)
	}
}

// TestSwitchAccount_CoolingTargetRefused: a cooling target is refused before any
// stop/rebind.
func TestSwitchAccount_CoolingTargetRefused(t *testing.T) {
	h := newSwitchHarness(t)
	until := time.Now().Add(time.Hour)
	h.lc.accountSwitchRegistry = stubSwitchRegistry{acct: switchAccount{
		ID: "acct-2", Provider: "claude", Label: "Cooler", Status: AccountCooling, CooldownUntil: &until,
	}}

	_, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2",
	})
	if !errors.Is(err, ErrAccountCooling) {
		t.Fatalf("err = %v, want ErrAccountCooling", err)
	}
	if h.findCall("kill-session") != nil {
		t.Error("cooling refusal must not kill the pane")
	}
	if got := h.chats.chatsBySession["sess-1"][0].AccountID; got != nil {
		t.Errorf("chat AccountID = %v, want nil (no rebind)", got)
	}
}

// TestSwitchAccount_DisabledTargetRefused: a disabled target is refused before
// any stop/rebind.
func TestSwitchAccount_DisabledTargetRefused(t *testing.T) {
	h := newSwitchHarness(t)
	h.lc.accountSwitchRegistry = stubSwitchRegistry{acct: switchAccount{
		ID: "acct-2", Provider: "claude", Label: "Off", Status: AccountDisabled,
	}}

	_, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2",
	})
	if !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("err = %v, want ErrAccountDisabled", err)
	}
	if h.findCall("kill-session") != nil {
		t.Error("disabled refusal must not kill the pane")
	}
	// BOS-981: the refusal is additionally tagged as never having reached the pane, so
	// the automatic respawn path can refund its cap charge...
	if !errors.Is(err, ErrSwitchNotAttempted) {
		t.Errorf("err = %v, want it to also be ErrSwitchNotAttempted", err)
	}
	// ...without changing what an operator sees: the tag must not alter the message.
	if want := "target account is disabled: account \"Off\" is disabled"; err.Error() != want {
		t.Errorf("err.Error() = %q, want %q (the not-attempted tag must be invisible)", err.Error(), want)
	}
}

// TestSwitchAccount_RespawnSameAccountStillRefusesDisabledAccount pins the BOS-981
// security-adjacent boundary: RespawnSameAccount bypasses the idempotent no-op
// short-circuit, and BOS-981 teaches the rotator to treat its refusal as free — but it
// must NOT become a way to run work on a disabled account. A same-account respawn onto
// a disabled account is still refused, before the pane is touched.
func TestSwitchAccount_RespawnSameAccountStillRefusesDisabledAccount(t *testing.T) {
	h := newSwitchHarness(t)
	bound := "acct-2"
	h.chats.chatsBySession["sess-1"][0].AccountID = &bound
	h.lc.accountSwitchRegistry = stubSwitchRegistry{acct: switchAccount{
		ID: "acct-2", Provider: "claude", Label: "Off", Status: AccountDisabled,
	}}

	_, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2",
		Auto: true, RespawnSameAccount: true,
	})
	if !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("err = %v, want ErrAccountDisabled (RespawnSameAccount must not bypass the eligibility gate)", err)
	}
	if h.findCall("kill-session") != nil {
		t.Error("a refused respawn must not kill the pane")
	}
	if !errors.Is(err, ErrSwitchNotAttempted) {
		t.Errorf("err = %v, want it to also be ErrSwitchNotAttempted", err)
	}
}

// TestSwitchAccount_ForceDoesNotBypassDisabledAccount pins the other half of the same
// boundary: --force only overrides the mid-turn guard, never account eligibility.
func TestSwitchAccount_ForceDoesNotBypassDisabledAccount(t *testing.T) {
	h := newSwitchHarness(t)
	h.lc.accountSwitchRegistry = stubSwitchRegistry{acct: switchAccount{
		ID: "acct-2", Provider: "claude", Label: "Off", Status: AccountDisabled,
	}}

	_, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2",
		Force: true, RespawnSameAccount: true,
	})
	if !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("err = %v, want ErrAccountDisabled (force must not bypass the eligibility gate)", err)
	}
	if h.findCall("kill-session") != nil {
		t.Error("a refused switch must not kill the pane")
	}
}

// TestSwitchAccount_FailedHealthTargetRefused: a target whose last health check
// failed is refused server-side before any stop/rebind, mirroring session
// creation's checkAccountEligible gate so an MCP/API caller (or a health change
// after the picker listed accounts) cannot bind a sidelined account.
func TestSwitchAccount_FailedHealthTargetRefused(t *testing.T) {
	h := newSwitchHarness(t)
	h.lc.accountSwitchRegistry = stubSwitchRegistry{acct: switchAccount{
		ID: "acct-2", Provider: "claude", Label: "Sick", Status: AccountFailed,
	}}

	_, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2",
	})
	if !errors.Is(err, ErrAccountFailed) {
		t.Fatalf("err = %v, want ErrAccountFailed", err)
	}
	if h.findCall("kill-session") != nil {
		t.Error("failed-health refusal must not kill the pane")
	}
	if got := h.chats.chatsBySession["sess-1"][0].AccountID; got != nil {
		t.Errorf("chat AccountID = %v, want nil (no rebind)", got)
	}
}

// TestSwitchAccount_AuthInvalidTargetRefusedBeforePaneTouch is the ordering
// guarantee that makes this finding matter: the refusal must land ABOVE the
// pane-touch boundary. A spawn-time refusal arrives only after the chat has
// been killed and rebound, at which point the respawn silently falls back to
// ambient credentials and the disruption is already done.
func TestSwitchAccount_AuthInvalidTargetRefusedBeforePaneTouch(t *testing.T) {
	h := newSwitchHarness(t)
	h.lc.accountSwitchRegistry = stubSwitchRegistry{acct: switchAccount{
		ID: "acct-2", Provider: "claude", Label: "Benched", Status: AccountAuthInvalid,
	}}

	_, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2",
	})
	if !errors.Is(err, ErrAccountAuthInvalid) {
		t.Fatalf("err = %v, want ErrAccountAuthInvalid", err)
	}
	if h.findCall("kill-session") != nil {
		t.Error("auth-invalid refusal must not kill the pane")
	}
	if got := h.chats.chatsBySession["sess-1"][0].AccountID; got != nil {
		t.Errorf("chat AccountID = %v, want nil (no rebind)", got)
	}
}

// TestSwitchAccount_IdempotentNoop: switching to the already-bound account is a
// success no-op — no stop, no rebind, no respawn.
func TestSwitchAccount_IdempotentNoop(t *testing.T) {
	h := newSwitchHarness(t)
	bound := "acct-2"
	// The chat (not the session) is the authority: seed the CHAT's account so the
	// switch detects it is already on the target.
	h.chats.chatsBySession["sess-1"][0].AccountID = &bound

	res, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2",
	})
	if err != nil {
		t.Fatalf("SwitchAccount: %v", err)
	}
	if res.Resumed {
		t.Errorf("Resumed = true, want false on no-op")
	}
	if !strings.Contains(res.NoticeText, "already on Work") {
		t.Errorf("NoticeText = %q, want it to say already on Work", res.NoticeText)
	}
	if h.findCall("kill-session") != nil {
		t.Error("no-op must not kill the pane")
	}
	if h.agentFake.LastBuildInteractiveCommand != nil {
		t.Error("no-op must not respawn a chat")
	}
}

// TestSwitchAccount_RespawnSameAccountBypassesNoop pins the BOS-482 respawn-in-place
// path: with RespawnSameAccount set, a switch whose target IS the currently-bound
// account does NOT take the idempotent no-op — it stops and respawns the pane on the
// same account (resuming the prior id) to refresh stale injected auth wiring, and the
// notice describes a refresh rather than a switch.
func TestSwitchAccount_RespawnSameAccountBypassesNoop(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newSwitchHarness(t)
	bound := "acct-2"
	h.chats.chatsBySession["sess-1"][0].AccountID = &bound

	res, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2",
		Auto: true, RespawnSameAccount: true,
	})
	if err != nil {
		t.Fatalf("SwitchAccount(respawn): %v", err)
	}
	// It must NOT short-circuit as a no-op.
	if strings.Contains(res.NoticeText, "already on") {
		t.Fatalf("respawn-in-place took the no-op path: NoticeText = %q", res.NoticeText)
	}
	if !res.Resumed {
		t.Errorf("Resumed = false, want true (same-account respawn resumes when a transcript exists)")
	}
	if !strings.Contains(res.NoticeText, "refreshed auth") || !strings.Contains(res.NoticeText, "respawned in place") {
		t.Errorf("NoticeText = %q, want respawn-in-place wording", res.NoticeText)
	}
	// STOP + RESPAWN actually happened.
	if h.findCall("kill-session") == nil {
		t.Error("respawn-in-place must stop the pane")
	}
	last := h.agentFake.LastBuildInteractiveCommand
	if last == nil {
		t.Fatal("respawn-in-place must respawn the chat")
	}
	if !last.GetResume() || last.GetSessionId() != "agent-1" {
		t.Errorf("respawn should resume prior id agent-1, got resume=%v id=%q", last.GetResume(), last.GetSessionId())
	}
}

// TestSwitchAccount_RespawnSameAccountResumesWithoutCrossAccountGate pins that the
// same-account respawn resume rule skips the cross-account feasibility gate: even when
// resumeFeasibleCrossAccount reports false, a same-account respawn still resumes
// (there is no cross-account move to be infeasible).
func TestSwitchAccount_RespawnSameAccountResumesWithoutCrossAccountGate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newSwitchHarness(t)
	bound := "acct-2"
	h.chats.chatsBySession["sess-1"][0].AccountID = &bound

	// Force the cross-account gate closed; a genuine cross-account switch would start
	// fresh, but a same-account respawn must ignore it.
	orig := resumeFeasibleCrossAccount
	resumeFeasibleCrossAccount = func(string) bool { return false }
	t.Cleanup(func() { resumeFeasibleCrossAccount = orig })

	res, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2",
		Auto: true, RespawnSameAccount: true,
	})
	if err != nil {
		t.Fatalf("SwitchAccount(respawn): %v", err)
	}
	if !res.Resumed {
		t.Errorf("Resumed = false, want true (same-account respawn ignores the cross-account gate)")
	}
}

// TestSwitchAccount_FailureAfterStopStampsStartError: a rebind failure after
// STOP stamps the chat row start-error and returns the wrapped error (the chat
// is not left silently vanished). Uses a headless chat (no tmux name) so STOP is
// a no-op and the test needs no tmux.
func TestSwitchAccount_FailureAfterStopStampsStartError(t *testing.T) {
	h := newStartTmuxChatHarness(t)
	h.chats.chatsBySession = map[string][]*models.AgentChat{
		"sess-1": {
			{SessionID: "sess-1", AgentSessionID: "agent-1", Title: "My chat"},
		},
	}
	h.lc.accountSwitchRegistry = stubSwitchRegistry{acct: switchAccount{
		ID: "acct-2", Provider: "claude", Label: "Work", Status: AccountActive,
	}}
	h.lc.accountSwitchBinding = &stubSwitchBinding{current: "", bindErr: errors.New("db down")}

	_, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2",
	})
	if err == nil {
		t.Fatal("expected an error when rebind fails after STOP")
	}
	if !strings.Contains(err.Error(), "db down") {
		t.Errorf("err = %v, want it to wrap the rebind failure", err)
	}
	var stamped bool
	for _, c := range h.chats.markStartFailedCalls {
		if c.agentSessionID == "agent-1" {
			stamped = true
		}
	}
	if !stamped {
		t.Error("expected the chat row to be stamped start-failed after the post-STOP failure")
	}
}

// TestSwitchAccount_DeadlineEndedRespawnCleansUpOnLiveContext is the assertion
// BOS-897's budget makes necessary. Once executeAccountSwitch runs the switch
// under its derived respawn budget, the context that reaches the respawn can
// expire mid-readiness-wait — and every best-effort cleanup on that path was
// issuing its write on that same context, which turns each one into an instant
// no-op precisely when it is needed: the pane leaks and the chat row keeps
// reading live.
//
// The two seams this leans on (recordedTmuxCall.ctxErr and
// markStartFailedCall.ctxErr) exist because the obvious assertions cannot tell
// detached from undetached on their own. fakeTmux records a subcommand BEFORE
// running it, and the mock store recorded the call while discarding the
// context, so "kill-session was issued" and "MarkStartFailed was called" are
// both true whether or not the write could actually land.
//
// A shortened readiness budget with a single attempt is what lets the caller's
// context be the thing that ends the wait, in well under a second.
func TestSwitchAccount_DeadlineEndedRespawnCleansUpOnLiveContext(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timing-sensitive tmux test in -short; run make test-bossd for coverage")
	}
	h := newSwitchHarness(t,
		tmux.WithSessionStartReadyDeadline(3*time.Second),
		tmux.WithSessionStartReadyAttempts(1),
	)
	// The respawned pane never draws a composer, so the readiness wait runs
	// until something stops it — here, the caller's own deadline.
	h.tmuxFake.capturePaneOutput = "Welcome to Claude — still booting\n"

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_, err := h.lc.SwitchAccount(ctx, SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2",
	})
	if err == nil {
		t.Fatal("expected the switch to fail once its context expired during the respawn")
	}

	// Two kills: the STOP that precedes the respawn (issued while the context
	// was still live, so it proves nothing) and the failure cleanup after the
	// wait burned the budget. Only the second is under test.
	if got := h.tmuxFake.subcommandCount("kill-session"); got < 2 {
		t.Fatalf("kill-session calls = %d, want at least 2 (STOP plus the failure cleanup); "+
			"without the cleanup kill the assertion below would read the STOP and pass vacuously", got)
	}
	if kill := h.tmuxFake.lastCall("kill-session"); kill == nil {
		t.Fatal("no kill-session recorded at all")
	} else if kill.ctxErr != nil {
		t.Errorf("the failure-cleanup kill-session was issued on a dead context (%v), so tmux never ran it "+
			"and the respawned pane leaks; killTmuxChatBestEffort must detach from the caller's context",
			kill.ctxErr)
	}

	// Both start-error stamps must survive the same expiry: failStartBestEffort's
	// (inside StartTmuxChat) and the switch's own (stampSwitchStartError). A row
	// that keeps its live tmux pointer after a failed switch is exactly the
	// "silently vanished chat" both stamps exist to prevent.
	var sawRespawnStamp, sawStartStamp bool
	h.chats.mu.Lock()
	stamps := append([]markStartFailedCall(nil), h.chats.markStartFailedCalls...)
	h.chats.mu.Unlock()
	for _, c := range stamps {
		if c.agentSessionID != "agent-1" {
			continue
		}
		switch {
		case strings.Contains(c.reason, "respawn after switch failed"):
			sawRespawnStamp = true
			if c.ctxErr != nil {
				t.Errorf("stampSwitchStartError wrote on a dead context (%v); the row keeps reading live", c.ctxErr)
			}
		case strings.Contains(c.reason, "send plan failed"):
			sawStartStamp = true
			if c.ctxErr != nil {
				t.Errorf("failStartBestEffort's stamp wrote on a dead context (%v); the row keeps reading live", c.ctxErr)
			}
		}
	}
	if !sawRespawnStamp {
		t.Errorf("no switch start-error stamp recorded for agent-1; calls = %+v", stamps)
	}
	if !sawStartStamp {
		t.Errorf("no StartTmuxChat start-error stamp recorded for agent-1; calls = %+v", stamps)
	}
}

// stuckComposerPane models a pane whose composer holds whatever was last pasted
// into it and empties only on C-u: Enter is SWALLOWED. That is exactly the
// condition tmux.OutcomeNotSubmitted names — a live composer still holding the
// payload after the verifier's Enter retry — and it is the one shape a static
// capture-pane fixture cannot express, because the verifier's own clear/redeliver
// dance has to move the pane between states for the run to reach that verdict.
//
// The marker row renders the notice's first word rather than the whole notice:
// composerHoldsPayload matches a prefix in either direction, so this holds the
// payload for the verifier without the fixture having to predict the label and
// outcome suffix SwitchAccount composes at run time.
func stuckComposerPane(calls []recordedTmuxCall) string {
	held := false
	for _, c := range calls {
		if c.subcommand == "paste-buffer" {
			held = true
			continue
		}
		if c.subcommand != "send-keys" || len(c.args) == 0 {
			continue
		}
		switch c.args[len(c.args)-1] {
		case "C-u":
			held = false
		case "Enter":
			// Swallowed by the TUI — the whole point of the fixture.
		default:
			// A literal `send-keys -l -- <text>`, which is how sendPlan delivers
			// a single-line payload like this notice.
			held = true
		}
	}
	if held {
		return "Welcome to Claude\n❯ switched\n"
	}
	return "Welcome to Claude\n❯\n"
}

// composerHoldsNotice replays stuckComposerPane's rule over everything the fake
// recorded, answering the question the ticket actually cares about: when the
// switch returned, was the notice still sitting at the prompt?
func composerHoldsNotice(f *fakeTmux) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Contains(stuckComposerPane(f.calls), "switched")
}

// TestSwitchAccount_UnsubmittedNoticeIsClearedFromTheComposer is the other half
// of "the notice is submitted, not left pending". Treating OutcomeNotSubmitted
// as a merely-undelivered optional payload keeps the launch — correctly, the
// account is already rebound and the pane is up — but it also LEAVES the notice
// in the composer, which is the exact reported symptom: a chat that comes back
// parked behind a pending housekeeping line the operator's next Enter would
// submit. The switch must still succeed AND the composer must come back empty.
func TestSwitchAccount_UnsubmittedNoticeIsClearedFromTheComposer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newSwitchHarness(t)
	h.tmuxFake.capturePaneFunc = stuckComposerPane

	res, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2",
	})
	// The non-fatal contract is a named acceptance criterion of this ticket, and
	// the clear must not quietly convert the outcome back into a failure.
	if err != nil {
		t.Fatalf("SwitchAccount: %v; an unsubmitted cosmetic notice must not fail a completed switch", err)
	}
	if res.TargetLabel != "Work" {
		t.Errorf("TargetLabel = %q, want Work", res.TargetLabel)
	}
	if got := h.chats.chatsBySession["sess-1"][0].AccountID; got == nil || *got != "acct-2" {
		t.Errorf("chat AccountID = %v, want acct-2 (the rebind must stand)", got)
	}
	// The pane must NOT be reaped: the outcome is classified, so the launch
	// stands and only the payload did not land.
	if h.tmuxFake.subcommandCount("kill-session") != 1 {
		t.Errorf("kill-session calls = %d, want exactly 1 (the STOP before the respawn); "+
			"a second kill means the classified submit failure went down the fatal path",
			h.tmuxFake.subcommandCount("kill-session"))
	}
	if composerHoldsNotice(h.tmuxFake) {
		t.Error("the account-switch notice was left pending in the composer after the switch; " +
			"the next Enter would submit a stale housekeeping message — the very shape this ticket removes")
	}
}

// TestSwitchAccount_UnclearableNoticeStillLeavesTheSwitchStanding pins the
// decision taken where the reviewer offered a fork. A composer that C-u cannot
// empty is one the verifier's own bounded clear dance already failed to empty,
// so escalating to fatal here would run failStartBestEffort — which KILLS a pane
// that is up, correctly wired, and bound to the account the switch just moved it
// to. A stray line in a working composer is strictly less harm than destroying a
// completed switch, so the clear stays best-effort and the launch stands.
func TestSwitchAccount_UnclearableNoticeStillLeavesTheSwitchStanding(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newSwitchHarness(t)
	// A composer that renders the payload forever: every C-u is a no-op, so the
	// clear exhausts its presses and fails.
	h.tmuxFake.capturePaneOutput = "Welcome to Claude\n❯ switched\n"

	if _, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2",
	}); err != nil {
		t.Fatalf("SwitchAccount: %v; a composer that cannot be cleared must not fail a completed switch", err)
	}
	if h.tmuxFake.subcommandCount("kill-session") != 1 {
		t.Errorf("kill-session calls = %d, want exactly 1 (the STOP before the respawn); "+
			"the pane must survive an uncleanable cosmetic notice",
			h.tmuxFake.subcommandCount("kill-session"))
	}
	if got := h.chats.chatsBySession["sess-1"][0].AccountID; got == nil || *got != "acct-2" {
		t.Errorf("chat AccountID = %v, want acct-2 (the rebind must stand)", got)
	}
}

// TestSwitchAccount_UnconfirmedDeliveryOnAVanishedPaneFailsTheSwitch pins the
// one place the "a classified outcome means the launch stands" rule has to be
// crossed. OutcomeUnconfirmed is the verdict a FAILED capture-pane produces, and
// a pane whose process exited after NewSession produces that identical failed
// read — this spawn path arms no RemainOnExit, so the exit collapses the session
// outright rather than leaving a readable zombie. Classification therefore
// cannot tell "up but unreadable" from "gone", and softening it unconditionally
// returned SUCCESS from a switch whose row kept a tmux_session_name pointing at
// nothing: a chat the operator can see, click, and never open.
//
// So the outcome alone does not decide it — tmux is asked, and only a DEFINITE
// "can't find session" turns the degrade back into a failed launch. The two
// siblings above hold the other side of that line: an indeterminate probe, or
// any other classified outcome, still leaves the completed switch standing.
func TestSwitchAccount_UnconfirmedDeliveryOnAVanishedPaneFailsTheSwitch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newSwitchHarness(t)
	fake := h.tmuxFake
	// The respawned pane comes up and draws a composer — readiness MUST pass, or
	// the readiness gate's own paneVanishedErr (OutcomeUnclassified) takes the
	// fatal branch above this one and the test proves nothing. It then dies the
	// instant readiness is satisfied: every later capture-pane fails, which is
	// what makes the submit verifier answer OutcomeUnconfirmed, and has-session
	// answers the definite miss that licenses the reap.
	//
	// capturePaneFunc runs with the fake's lock held, so the flip writes the maps
	// directly rather than going back through the fake.
	fake.capturePaneFunc = func(calls []recordedTmuxCall) string {
		respawned, capturesAfterRespawn := false, 0
		for _, c := range calls {
			switch c.subcommand {
			case "new-session":
				respawned, capturesAfterRespawn = true, 0
			case "capture-pane":
				if respawned {
					capturesAfterRespawn++
				}
			}
		}
		if respawned && capturesAfterRespawn >= 1 {
			fake.failSubcommand["capture-pane"] = true
			fake.failStderr["capture-pane"] = "can't find pane: bossd-agent-run-agent-1"
			// An empty stderr would map to (false, error) — an INDETERMINATE
			// probe, which confirmPaneVanished deliberately refuses to act on.
			// Only tmux's own words make the miss definite.
			fake.failSubcommand["has-session"] = true
			fake.failStderr["has-session"] = "can't find session: bossd-agent-run-agent-1"
		}
		return "Welcome to Claude\n❯\n"
	}

	_, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2",
	})
	if err == nil {
		t.Fatal("SwitchAccount returned success for a switch whose respawned pane is gone; " +
			"the chat row would keep a tmux_session_name pointing at nothing")
	}

	// Two kills: the STOP that precedes the respawn (which the siblings assert
	// alone, because their launch stands) and failStartBestEffort's reap of the
	// pane this path just declared dead.
	if got := fake.subcommandCount("kill-session"); got != 2 {
		t.Errorf("kill-session calls = %d, want exactly 2 (the STOP before the respawn plus the failure reap); "+
			"only 1 means the unconfirmed delivery was softened into success on a vanished pane", got)
	}

	// The row must not be left reading live: failStartBestEffort stamps it so the
	// chat list shows "(failed to start)" instead of a pane that is not there.
	var stamped bool
	h.chats.mu.Lock()
	stamps := append([]markStartFailedCall(nil), h.chats.markStartFailedCalls...)
	h.chats.mu.Unlock()
	for _, c := range stamps {
		if c.agentSessionID == "agent-1" && strings.Contains(c.reason, "send plan failed") {
			stamped = true
		}
	}
	if !stamped {
		t.Errorf("no start-error stamp recorded for agent-1; calls = %+v", stamps)
	}
}

// TestSwitchAccount_UnsubmittedDeliveryOnAVanishedPaneFailsTheSwitch holds the
// OTHER half of the same line, and it is the half that is easy to miss.
// OutcomeNotSubmitted reads like a verdict about a pane that was READ — "a
// composer is drawn and it still holds the payload" — and for the sibling above
// this test it is exactly that. But the Enter retry is entered only AFTER a
// confirmed-pending verdict, so every failure inside its clear/redeliver/Enter
// dance is classified not-submitted too, deliberately: C-u cannot submit, so
// telling the operator "this may already have run" would invert the advice. A
// pane that DIES during that dance therefore mints not-submitted from a tmux
// call that failed rather than a composer that was read — the same evidence
// gap OutcomeUnconfirmed has — and a guard that trusted the verdict's name let
// it through: the switch returned SUCCESS, the best-effort clear below fired a
// C-u at a session that no longer exists and only logged its failure, and the
// row kept a tmux_session_name pointing at nothing.
//
// So the probe governs both verdicts, and both siblings above still stand: the
// notice-cleared case is a live composer this test never reaches, and neither
// gets reaped without tmux's own definite "can't find session".
func TestSwitchAccount_UnsubmittedDeliveryOnAVanishedPaneFailsTheSwitch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newSwitchHarness(t)
	fake := h.tmuxFake
	// The pane must live long enough to be classified NOT-SUBMITTED rather than
	// unconfirmed, and that verdict cannot be faked by a static fixture: it is
	// reached only through a confirmed-pending verdict, which needs a composer
	// that actually holds the payload while Enter is swallowed. So the pane runs
	// as stuckComposerPane — readiness passes, the notice lands, the first
	// waitForSubmission times out with the payload still at the prompt — and it
	// dies at the verifier's own first C-u, which is a key that cannot be sent
	// before that pending verdict. The next post-clear capture fails, and the
	// verifier answers not-submitted because the only key it has pressed since
	// is a clear.
	//
	// capturePaneFunc runs with the fake's lock held, so the flip writes the maps
	// directly rather than going back through the fake.
	fake.capturePaneFunc = func(calls []recordedTmuxCall) string {
		cleared := false
		for _, c := range calls {
			if c.subcommand == "send-keys" && len(c.args) > 0 && c.args[len(c.args)-1] == "C-u" {
				cleared = true
			}
		}
		if !cleared {
			return stuckComposerPane(calls)
		}
		fake.failSubcommand["capture-pane"] = true
		fake.failStderr["capture-pane"] = "can't find pane: bossd-agent-run-agent-1"
		// An empty stderr would map to (false, error) — an INDETERMINATE probe,
		// which confirmPaneVanished deliberately refuses to act on. Only tmux's
		// own words make the miss definite.
		fake.failSubcommand["has-session"] = true
		fake.failStderr["has-session"] = "can't find session: bossd-agent-run-agent-1"
		// This one capture still answers (the fail flag applies from the NEXT call
		// on), and it must answer "the box still holds the payload". Reporting an
		// empty composer would license the redelivery and a second Enter, and the
		// dead pane would then be read after that Enter — which is the UNCONFIRMED
		// verdict this test exists to stay out of.
		return "Welcome to Claude\n❯ switched\n"
	}

	_, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2",
	})
	if err == nil {
		t.Fatal("SwitchAccount returned success for a switch whose respawned pane died during the submit retry; " +
			"the chat row would keep a tmux_session_name pointing at nothing")
	}
	// Pin WHICH verdict got here. Every assertion below passes just as well for
	// an OutcomeUnconfirmed run, so without this the fixture could drift into the
	// sibling's branch and prove nothing about this one.
	if got := tmux.OutcomeOf(err); got != tmux.OutcomeNotSubmitted {
		t.Fatalf("delivery outcome = %v, want %v; this test is only about the "+
			"not-submitted half of the guard, and %v is the sibling's case",
			got, tmux.OutcomeNotSubmitted, tmux.OutcomeUnconfirmed)
	}

	// Two kills, exactly as in the unconfirmed sibling: the STOP that precedes
	// the respawn, plus failStartBestEffort's reap of the pane this path just
	// proved dead. One kill means the not-submitted verdict was softened into
	// success and the only other thing sent at the dead session was the
	// best-effort C-u, whose failure is merely logged.
	if got := fake.subcommandCount("kill-session"); got != 2 {
		t.Errorf("kill-session calls = %d, want exactly 2 (the STOP before the respawn plus the failure reap); "+
			"only 1 means the unsubmitted delivery was softened into success on a vanished pane", got)
	}

	// The row must not be left reading live: failStartBestEffort stamps it so the
	// chat list shows "(failed to start)" instead of a pane that is not there.
	var stamped bool
	h.chats.mu.Lock()
	stamps := append([]markStartFailedCall(nil), h.chats.markStartFailedCalls...)
	h.chats.mu.Unlock()
	for _, c := range stamps {
		if c.agentSessionID == "agent-1" && strings.Contains(c.reason, "send plan failed") {
			stamped = true
		}
	}
	if !stamped {
		t.Errorf("no start-error stamp recorded for agent-1; calls = %+v", stamps)
	}
}
