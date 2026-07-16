package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/account"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/rotation"
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

// TestAccountRegistryAdapter_FailedHealthMapsToFailed verifies the registry
// projection maps a models.AccountHealthFailed account to AccountFailed, so the
// switch rejects it (mirroring session creation's checkAccountEligible).
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
// SwitchAccount.
func newSwitchHarness(t *testing.T) *startTmuxChatHarness {
	t.Helper()
	h := newStartTmuxChatHarness(t)
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
	if last.GetInitialPrompt() != "" || last.GetInitialCommand() != "" {
		t.Errorf("interrupted prompt was resent: prompt=%q command=%q, want both empty",
			last.GetInitialPrompt(), last.GetInitialCommand())
	}
	if n := h.tmuxFake.enterSendKeysCount(); n != 0 {
		t.Errorf("no submit expected (D12), got %d Enter presses", n)
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
