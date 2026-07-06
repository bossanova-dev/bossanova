package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/models"
)

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
	if res.TargetLabel != "Work" {
		t.Errorf("TargetLabel = %q, want Work", res.TargetLabel)
	}
	// STOP happened.
	if h.findCall("kill-session") == nil {
		t.Error("expected tmux kill-session (STOP), none recorded")
	}
	// REBIND persisted onto the session (default binding over the session store).
	if got := h.sessions.sessions["sess-1"].AccountID; got == nil || *got != "acct-2" {
		t.Errorf("session AccountID = %v, want acct-2", got)
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

// TestSwitchAccount_CrossAgentChatRefused: a chat whose agent differs from its
// session's agent is refused before any stop/rebind. The session-scoped respawn
// (StartTmuxChat under sess.AgentName) and rebind (BindSessionAccount) would
// otherwise relaunch it as the wrong agent and bind the session to another
// provider's account.
func TestSwitchAccount_CrossAgentChatRefused(t *testing.T) {
	h := newSwitchHarness(t)
	// The session runs "claude"; make the selected chat a cross-agent codex chat.
	h.chats.chatsBySession["sess-1"][0].AgentName = "codex"

	_, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2",
	})
	if !errors.Is(err, ErrCrossAgentSwitchUnsupported) {
		t.Fatalf("err = %v, want ErrCrossAgentSwitchUnsupported", err)
	}
	if h.findCall("kill-session") != nil {
		t.Error("cross-agent refusal must not kill the pane")
	}
	if got := h.sessions.sessions["sess-1"].AccountID; got != nil {
		t.Errorf("session AccountID = %v, want nil (no rebind on refusal)", got)
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
	if got := h.sessions.sessions["sess-1"].AccountID; got != nil {
		t.Errorf("session AccountID = %v, want nil (no rebind on refusal)", got)
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
	if got := h.sessions.sessions["sess-1"].AccountID; got != nil {
		t.Errorf("session AccountID = %v, want nil (no rebind on refusal)", got)
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
	if got := h.sessions.sessions["sess-1"].AccountID; got != nil {
		t.Errorf("session AccountID = %v, want nil (no rebind)", got)
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

// TestSwitchAccount_IdempotentNoop: switching to the already-bound account is a
// success no-op — no stop, no rebind, no respawn.
func TestSwitchAccount_IdempotentNoop(t *testing.T) {
	h := newSwitchHarness(t)
	bound := "acct-2"
	h.sessions.sessions["sess-1"].AccountID = &bound

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
