package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
)

const (
	staleAcct  = "acct-devops"
	activeAcct = "acct-hello"
)

// withAccounts wires the registry seams StartTmuxChat reads: accountGetter
// answers from rows, and the default resolver hands out next (or "" when
// every account is out of service).
func withAccounts(h *startTmuxChatHarness, rows map[string]*models.Account, next string) *fakeAccountEnvResolver {
	h.lc.SetAccountGetter(func(_ context.Context, id string) (*models.Account, error) {
		a, ok := rows[id]
		if !ok {
			return nil, errors.New("account not found")
		}
		return a, nil
	})
	h.lc.SetDefaultAccountResolver(func(context.Context, string, time.Time) (string, error) {
		return next, nil
	})
	env := &fakeAccountEnvResolver{env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "tok"}}
	h.lc.SetAccountEnvResolver(env)
	return env
}

func claudeAccount(id string, status models.AccountStatus) *models.Account {
	return &models.Account{ID: id, Label: id, Provider: models.AccountProviderClaude, Status: status, Health: models.AccountHealthOK}
}

// A session whose seed account was disabled after it was bound must not hand
// that account to its next chat. The repair plugin's fresh chats inherited the
// session seed, so every one of them launched on the disabled account.
func TestStartTmuxChat_NewChatSkipsDisabledSessionAccount(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newStartTmuxChatHarness(t)
	seed := staleAcct
	h.sessions.sessions["sess-1"].AccountID = &seed
	env := withAccounts(h, map[string]*models.Account{
		staleAcct:  claudeAccount(staleAcct, models.AccountStatusDisabled),
		activeAcct: claudeAccount(activeAcct, models.AccountStatusActive),
	}, activeAcct)

	if _, err := h.lc.StartTmuxChat(context.Background(), "sess-1", ChatInput{Prompt: "/boss-repair", Delivery: DeliverySubmit}, "title", HookOpts{}); err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}
	if len(h.chats.createCalls) != 1 {
		t.Fatalf("agent_chats Create calls = %d, want 1", len(h.chats.createCalls))
	}
	if got := derefString(h.chats.createCalls[0].AccountID); got != activeAcct {
		t.Errorf("new chat account = %q, want %q (the disabled seed must not launch)", got, activeAcct)
	}
	if env.gotSess == nil || derefString(env.gotSess.AccountID) != activeAcct {
		t.Errorf("credentials resolved for %v, want %q", env.gotSess, activeAcct)
	}
	if got := derefString(h.sessions.sessions["sess-1"].AccountID); got != activeAcct {
		t.Errorf("session account after launch = %q, want %q so the next chat starts there too", got, activeAcct)
	}
}

// A provider suspension (the org-disabled refusal, recorded as failed health
// plus the refusal text) is out of service exactly like an operator disable.
func TestStartTmuxChat_NewChatSkipsSuspendedSessionAccount(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newStartTmuxChatHarness(t)
	seed := staleAcct
	h.sessions.sessions["sess-1"].AccountID = &seed
	suspended := claudeAccount(staleAcct, models.AccountStatusActive)
	suspended.Health = models.AccountHealthFailed
	suspended.LastTestError = "account suspended: organization disabled Claude subscription access"
	withAccounts(h, map[string]*models.Account{
		staleAcct:  suspended,
		activeAcct: claudeAccount(activeAcct, models.AccountStatusActive),
	}, activeAcct)

	if _, err := h.lc.StartTmuxChat(context.Background(), "sess-1", ChatInput{Prompt: "/boss-repair", Delivery: DeliverySubmit}, "title", HookOpts{}); err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}
	if got := derefString(h.chats.createCalls[0].AccountID); got != activeAcct {
		t.Errorf("new chat account = %q, want %q (a suspended account must not launch)", got, activeAcct)
	}
}

// Disabling an account that a session is using takes effect on that session's
// next launch: nothing has to clear the binding first.
func TestStartTmuxChat_DisablingAnAccountInUseMovesTheNextLaunch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newStartTmuxChatHarness(t)
	seed := staleAcct
	h.sessions.sessions["sess-1"].AccountID = &seed
	rows := map[string]*models.Account{
		staleAcct:  claudeAccount(staleAcct, models.AccountStatusActive),
		activeAcct: claudeAccount(activeAcct, models.AccountStatusActive),
	}
	withAccounts(h, rows, activeAcct)
	ctx := context.Background()

	if _, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "/boss-repair", Delivery: DeliverySubmit}, "first", HookOpts{}); err != nil {
		t.Fatalf("first StartTmuxChat: %v", err)
	}
	if got := derefString(h.chats.createCalls[0].AccountID); got != staleAcct {
		t.Fatalf("first chat account = %q, want %q while it is still enabled", got, staleAcct)
	}

	rows[staleAcct].Status = models.AccountStatusDisabled
	if _, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "/boss-repair", Delivery: DeliverySubmit}, "second", HookOpts{AllowSiblingChat: true}); err != nil {
		t.Fatalf("second StartTmuxChat: %v", err)
	}
	if got := derefString(h.chats.createCalls[1].AccountID); got != activeAcct {
		t.Errorf("chat after disabling = %q, want %q", got, activeAcct)
	}
}

// With every account out of service the launch is refused in plain words, and
// nothing is started on the disabled account or on the machine's own login.
func TestStartTmuxChat_AllAccountsDisabledRefusesPlainly(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newStartTmuxChatHarness(t)
	seed := staleAcct
	h.sessions.sessions["sess-1"].AccountID = &seed
	env := withAccounts(h, map[string]*models.Account{
		staleAcct: claudeAccount(staleAcct, models.AccountStatusDisabled),
	}, "")

	_, err := h.lc.StartTmuxChat(context.Background(), "sess-1", ChatInput{Prompt: "/boss-repair", Delivery: DeliverySubmit}, "title", HookOpts{})
	const want = "no claude account can be used: acct-devops is disabled and no other account is available"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("StartTmuxChat error = %v, want it to contain %q", err, want)
	}
	if call := h.findCall("new-session"); call != nil {
		t.Errorf("a tmux pane was started: %v", call.args)
	}
	if len(h.chats.createCalls) != 0 {
		t.Errorf("agent_chats Create calls = %d, want 0", len(h.chats.createCalls))
	}
	if env.calls != 0 {
		t.Errorf("credentials were resolved %d times, want 0", env.calls)
	}
}

// After a switch moves a chat off an account, the session's next chat starts on
// the account the switch moved to, not the one the session was first bound to.
func TestSwitchAccount_NextChatStartsOnSwitchedAccount(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newProxySwitchHarness(t)
	ctx := context.Background()

	if _, err := h.lc.SwitchAccount(ctx, SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2", Auto: true,
	}); err != nil {
		t.Fatalf("SwitchAccount: %v", err)
	}
	if _, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "/boss-repair", Delivery: DeliverySubmit}, "next", HookOpts{AllowSiblingChat: true}); err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}
	h.chats.mu.Lock()
	creates := append([]db.CreateAgentChatParams(nil), h.chats.createCalls...)
	h.chats.mu.Unlock()
	if len(creates) == 0 {
		t.Fatal("the next chat was never created")
	}
	if got := derefString(creates[len(creates)-1].AccountID); got != "acct-2" {
		t.Errorf("next chat account = %q, want acct-2 (the account the switch moved to)", got)
	}
	if got := derefString(h.sessions.sessions["sess-1"].AccountID); got != "acct-2" {
		t.Errorf("session account = %q, want acct-2", got)
	}
}

func derefString(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func chatAccount(t *testing.T, h *startTmuxChatHarness, agentSessionID string) string {
	t.Helper()
	chat, err := h.chats.GetByAgentSessionID(context.Background(), agentSessionID)
	if err != nil {
		t.Fatalf("get chat %s: %v", agentSessionID, err)
	}
	h.chats.mu.Lock()
	defer h.chats.mu.Unlock()
	return derefString(chat.AccountID)
}

// Switching a chat to the machine login leaves the session on its managed
// account. Panes started from the session's account read their bearer from it,
// so an empty session account would send their requests with no credentials.
func TestSwitchAccount_SwitchToMachineLoginKeepsSessionAccount(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newProxySwitchHarness(t)
	ctx := context.Background()

	if _, err := h.lc.SwitchAccount(ctx, SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "", Auto: true,
	}); err != nil {
		t.Fatalf("SwitchAccount: %v", err)
	}
	if got := chatAccount(t, h, "agent-1"); got != "" {
		t.Errorf("switched chat account = %q, want the machine login", got)
	}
	if got := derefString(h.sessions.sessions["sess-1"].AccountID); got != switchStartAccount {
		t.Fatalf("session account = %q, want %q kept", got, switchStartAccount)
	}

	if _, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "/boss-repair", Delivery: DeliverySubmit}, "next", HookOpts{AllowSiblingChat: true}); err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}
	h.chats.mu.Lock()
	creates := append([]db.CreateAgentChatParams(nil), h.chats.createCalls...)
	h.chats.mu.Unlock()
	if got := derefString(creates[len(creates)-1].AccountID); got != switchStartAccount {
		t.Errorf("next chat account = %q, want %q", got, switchStartAccount)
	}
}

// A switch that moves the session's account moves every live pane started from
// it, because those panes read their bearer from the session. Their rows are
// rewritten so they show the account they now bill. A pane holding its own chat
// token reads its own row and keeps its account.
func TestSwitchAccount_SessionTokenPanesMoveAndTheirRowsSaySo(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newProxySwitchHarness(t)
	follower, own := switchStartAccount, switchStartAccount
	followerPane, ownPane := "bossd-agent-run-agent-2", "bossd-agent-run-agent-3"
	h.chats.chatsBySession["sess-1"] = append(h.chats.chatsBySession["sess-1"],
		&models.AgentChat{SessionID: "sess-1", AgentSessionID: "agent-2", AgentName: "claude", AccountID: &follower, TmuxSessionName: &followerPane},
		&models.AgentChat{SessionID: "sess-1", AgentSessionID: "agent-3", AgentName: "claude", AccountID: &own, TmuxSessionName: &ownPane},
	)
	h.tmuxFake.bakedBaseURL = map[string]string{
		followerPane: wantSessionProxyURL("sess-1"),
		ownPane:      wantChatProxyURL("agent-3", switchStartAccount),
	}

	if _, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2", Auto: true,
	}); err != nil {
		t.Fatalf("SwitchAccount: %v", err)
	}
	if got := derefString(h.sessions.sessions["sess-1"].AccountID); got != "acct-2" {
		t.Fatalf("session account = %q, want acct-2", got)
	}
	if got := chatAccount(t, h, "agent-2"); got != "acct-2" {
		t.Errorf("session-token chat row = %q, want acct-2, the account its pane now bills", got)
	}
	if got := chatAccount(t, h, "agent-3"); got != switchStartAccount {
		t.Errorf("chat-token chat row = %q, want %q kept", got, switchStartAccount)
	}
}

// Orphan resume, resurrect, session start and the host repair run read their
// account through InServiceSpawnTarget. A primary chat bound to a disabled
// account moves to the default, and the move lands on the chat row, not on the
// session.
func TestInServiceSpawnTarget_DisabledChatAccountMovesTheChat(t *testing.T) {
	h := newStartTmuxChatHarness(t)
	ctx := context.Background()
	row := h.sessions.sessions["sess-1"]
	primary := "agent-1"
	row.AgentSessionID = &primary
	bound := staleAcct
	h.chats.chatsBySession = map[string][]*models.AgentChat{row.ID: {
		{SessionID: row.ID, AgentSessionID: primary, AgentName: "claude", AccountID: &bound},
	}}
	withAccounts(h, map[string]*models.Account{
		staleAcct:  claudeAccount(staleAcct, models.AccountStatusDisabled),
		activeAcct: claudeAccount(activeAcct, models.AccountStatusActive),
	}, activeAcct)

	spawn, chat, err := h.lc.InServiceSpawnTarget(ctx, row)
	if err != nil {
		t.Fatalf("InServiceSpawnTarget: %v", err)
	}
	if got := derefString(spawn.AccountID); got != activeAcct {
		t.Errorf("spawn account = %q, want %q", got, activeAcct)
	}
	if got := derefString(chat.AccountID); got != activeAcct {
		t.Errorf("spawn chat account = %q, want %q", got, activeAcct)
	}
	if got := chatAccount(t, h, primary); got != activeAcct {
		t.Errorf("chat row account = %q, want %q", got, activeAcct)
	}
	if row.AccountID != nil {
		t.Errorf("session account = %q, want it untouched", *row.AccountID)
	}
}

// A dropped run on a session whose account was disabled resumes on the
// default account, and the session moves there so its next chat does too.
func TestRunResume_DroppedRunOnDisabledAccountMovesTheSession(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newStartTmuxChatHarness(t)
	ctx := context.Background()
	row := h.sessions.sessions["sess-1"]
	row.IsTmuxUnattended = true
	prior := "old-run"
	pane := "old-pane"
	row.AgentSessionID = &prior
	seed := staleAcct
	row.AccountID = &seed
	death := time.Date(2026, 10, 15, 14, 0, 0, 0, time.UTC)
	row.UpdatedAt = death
	now := death.Add(time.Minute)
	h.lc.SetClockForTest(func() time.Time { return now })
	h.chats.chatsBySession = map[string][]*models.AgentChat{row.ID: {
		{ID: "original", SessionID: row.ID, AgentSessionID: prior, AgentName: "claude", TmuxSessionName: &pane, CreatedAt: death},
	}}
	h.tmuxFake.failSubcommand["has-session"] = true
	h.tmuxFake.failStderr["has-session"] = "can't find session"
	withAccounts(h, map[string]*models.Account{
		staleAcct:  claudeAccount(staleAcct, models.AccountStatusDisabled),
		activeAcct: claudeAccount(activeAcct, models.AccountStatusActive),
	}, activeAcct)
	h.lc.SetRunResumeStore(&runResumeLedger{})

	h.lc.ResumeDroppedRuns(ctx)
	h.chats.mu.Lock()
	creates := append([]db.CreateAgentChatParams(nil), h.chats.createCalls...)
	h.chats.mu.Unlock()
	if len(creates) != 1 {
		t.Fatalf("agent_chats Create calls = %d, want 1 replacement run", len(creates))
	}
	if got := derefString(creates[0].AccountID); got != activeAcct {
		t.Errorf("replacement run account = %q, want %q", got, activeAcct)
	}
	if got := derefString(row.AccountID); got != activeAcct {
		t.Errorf("session account = %q, want %q so its next chat starts there too", got, activeAcct)
	}
}
