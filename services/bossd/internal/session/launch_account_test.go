package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossd/internal/domain"

	"github.com/rs/zerolog"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/rotation"
)

const (
	staleAcct  = "acct-devops"
	activeAcct = "acct-hello"
)

// withAccounts wires the registry seams StartTmuxChat reads: accountGetter
// answers from rows, and the default resolver hands out next (or "" when
// every account is out of service).
func withAccounts(h *startTmuxChatHarness, rows map[string]*domain.AgentAccount, next string) *fakeAccountEnvResolver {
	withAccountRows(h.lc, rows, next)
	env := &fakeAccountEnvResolver{env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "tok"}}
	h.lc.SetAgentAccountEnvResolver(env)
	return env
}

func withAccountRows(lc *Lifecycle, rows map[string]*domain.AgentAccount, next string) {
	lc.SetAgentAccountGetter(func(_ context.Context, id string) (*domain.AgentAccount, error) {
		a, ok := rows[id]
		if !ok {
			return nil, errors.New("account not found")
		}
		return a, nil
	})
	lc.SetDefaultAgentAccountResolver(func(context.Context, string, time.Time) (string, error) {
		return next, nil
	})
}

// activeBearer is the credential a launch carries when it runs on activeAcct.
const activeBearer = "bearer-for-" + activeAcct

// withStaleAccountDisabled marks staleAcct disabled with activeAcct as the
// provider default, and materializes a bearer named for whichever account a
// launch resolves, so the launch's own env shows which account it runs on.
func withStaleAccountDisabled(lc *Lifecycle) {
	withAccountRows(lc, map[string]*domain.AgentAccount{
		staleAcct:  claudeAccount(staleAcct, domain.AgentAccountStatusDisabled),
		activeAcct: claudeAccount(activeAcct, domain.AgentAccountStatusActive),
	}, activeAcct)
	lc.SetAgentAccountEnvResolver(accountKeyedEnvResolver{})
}

func claudeAccount(id string, status domain.AgentAccountStatus) *domain.AgentAccount {
	return &domain.AgentAccount{ID: id, Label: id, Provider: domain.AgentAccountProviderClaude, Status: status, Health: domain.AgentAccountHealthOK}
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
	env := withAccounts(h, map[string]*domain.AgentAccount{
		staleAcct:  claudeAccount(staleAcct, domain.AgentAccountStatusDisabled),
		activeAcct: claudeAccount(activeAcct, domain.AgentAccountStatusActive),
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
	suspended := claudeAccount(staleAcct, domain.AgentAccountStatusActive)
	suspended.Health = domain.AgentAccountHealthFailed
	suspended.LastTestError = "account suspended: organization disabled Claude subscription access"
	withAccounts(h, map[string]*domain.AgentAccount{
		staleAcct:  suspended,
		activeAcct: claudeAccount(activeAcct, domain.AgentAccountStatusActive),
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
	rows := map[string]*domain.AgentAccount{
		staleAcct:  claudeAccount(staleAcct, domain.AgentAccountStatusActive),
		activeAcct: claudeAccount(activeAcct, domain.AgentAccountStatusActive),
	}
	withAccounts(h, rows, activeAcct)
	ctx := context.Background()

	if _, err := h.lc.StartTmuxChat(ctx, "sess-1", ChatInput{Prompt: "/boss-repair", Delivery: DeliverySubmit}, "first", HookOpts{}); err != nil {
		t.Fatalf("first StartTmuxChat: %v", err)
	}
	if got := derefString(h.chats.createCalls[0].AccountID); got != staleAcct {
		t.Fatalf("first chat account = %q, want %q while it is still enabled", got, staleAcct)
	}

	rows[staleAcct].Status = domain.AgentAccountStatusDisabled
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
	env := withAccounts(h, map[string]*domain.AgentAccount{
		staleAcct: claudeAccount(staleAcct, domain.AgentAccountStatusDisabled),
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

	if _, err := h.lc.SwitchAgentAccount(ctx, SwitchAgentAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2", Auto: true,
	}); err != nil {
		t.Fatalf("SwitchAgentAccount: %v", err)
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

	if _, err := h.lc.SwitchAgentAccount(ctx, SwitchAgentAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "", Auto: true,
	}); err != nil {
		t.Fatalf("SwitchAgentAccount: %v", err)
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
		&domain.AgentChat{SessionID: "sess-1", AgentSessionID: "agent-2", AgentName: "claude", AccountID: &follower, TmuxSessionName: &followerPane},
		&domain.AgentChat{SessionID: "sess-1", AgentSessionID: "agent-3", AgentName: "claude", AccountID: &own, TmuxSessionName: &ownPane},
	)
	h.lc.proxyRegistrar.TokenForSession("sess-1")
	h.tmuxFake.bakedBaseURL = map[string]string{
		followerPane: wantSessionProxyURL("sess-1"),
		ownPane:      wantChatProxyURL("agent-3", switchStartAccount),
	}
	h.lc.SetChatRunningProbe(func(string) bool { return true })

	if _, err := h.lc.SwitchAgentAccount(context.Background(), SwitchAgentAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2", Auto: true,
	}); err != nil {
		t.Fatalf("SwitchAgentAccount: %v", err)
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
	h.chats.chatsBySession = map[string][]*domain.AgentChat{row.ID: {
		{SessionID: row.ID, AgentSessionID: primary, AgentName: "claude", AccountID: &bound},
	}}
	withAccounts(h, map[string]*domain.AgentAccount{
		staleAcct:  claudeAccount(staleAcct, domain.AgentAccountStatusDisabled),
		activeAcct: claudeAccount(activeAcct, domain.AgentAccountStatusActive),
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
	h.chats.chatsBySession = map[string][]*domain.AgentChat{row.ID: {
		{ID: "original", SessionID: row.ID, AgentSessionID: prior, AgentName: "claude", TmuxSessionName: &pane, CreatedAt: death},
	}}
	h.tmuxFake.failSubcommand["has-session"] = true
	h.tmuxFake.failStderr["has-session"] = "can't find session"
	withAccounts(h, map[string]*domain.AgentAccount{
		staleAcct:  claudeAccount(staleAcct, domain.AgentAccountStatusDisabled),
		activeAcct: claudeAccount(activeAcct, domain.AgentAccountStatusActive),
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

// A failed session write names the account by its label and carries no ids.
func TestMoveSessionAccount_FailedWriteReadsPlainly(t *testing.T) {
	sessions := newMockSessionStore()
	sessions.sessions["sess-1"] = &models.Session{ID: "sess-1", AgentName: "claude"}
	sessions.updateHook = func(string, db.UpdateSessionParams) error { return errors.New("database is locked") }
	lc := newTestLifecycle(sessions, newMockRepoStore(), nil, nil, &mockWorktreeManager{}, newMockAgentRunner(), nil, newMockVCSProvider(), zerolog.Nop())
	lc.accountSwitchRegistry = idKeyedSwitchRegistry{}

	err := lc.MoveSessionAgentAccount(context.Background(), sessions.sessions["sess-1"], activeAcct)
	const want = "could not move this session to Label acct-hello: database is locked"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}

func newDisabledSeedStart(t *testing.T, agentName string) (*Lifecycle, *mockSessionStore, *mockAgentRunner) {
	t.Helper()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", LocalPath: "/tmp/repo", DefaultBaseBranch: "main", WorktreeBaseDir: "/tmp/worktrees"}
	seed := staleAcct
	sessions.sessions["sess-1"] = &models.Session{
		ID: "sess-1", RepoID: "repo-1", Title: "Test Session", Plan: "Do the work", BaseBranch: "main",
		AgentName: agentName, State: machine.CreatingWorktree, AccountID: &seed,
	}
	runner := newMockAgentRunner()
	lc := newTestLifecycle(sessions, repos, nil, nil, &mockWorktreeManager{}, runner, nil, newMockVCSProvider(), zerolog.Nop())
	withStaleAccountDisabled(lc)
	return lc, sessions, runner
}

// The capability preflight starts the agent once before the real launch. A
// session seeded on a disabled account preflights on the default account, and
// the session moves there.
func TestStartSession_PreflightRunsOnDefaultWhenSeedAccountDisabled(t *testing.T) {
	lc, sessions, runner := newDisabledSeedStart(t, "codex")

	if err := lc.StartSession(context.Background(), "sess-1", StartSessionOpts{
		Detach:                    true,
		HeadlessCapabilityProfile: pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
	}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if len(runner.preflights) == 0 {
		t.Fatal("the preflight never ran")
	}
	if got := runner.preflights[0].env[claudeOAuthTokenEnvKey]; got != activeBearer {
		t.Errorf("preflight credential = %q, want %q (the disabled seed must not run)", got, activeBearer)
	}
	if got := derefString(sessions.sessions["sess-1"].AccountID); got != activeAcct {
		t.Errorf("session account = %q, want %q", got, activeAcct)
	}
}

// A detached headless start on a session seeded with a disabled account runs on
// the default account instead of refusing, and the session moves there.
func TestStartSession_HeadlessRunsOnDefaultWhenSeedAccountDisabled(t *testing.T) {
	lc, sessions, runner := newDisabledSeedStart(t, "claude")

	if err := lc.StartSession(context.Background(), "sess-1", StartSessionOpts{Detach: true}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")
	if len(runner.started) != 1 {
		t.Fatalf("headless starts = %d, want 1", len(runner.started))
	}
	if got := runner.started[0].env[claudeOAuthTokenEnvKey]; got != activeBearer {
		t.Errorf("headless run credential = %q, want %q", got, activeBearer)
	}
	if got := derefString(sessions.sessions["sess-1"].AccountID); got != activeAcct {
		t.Errorf("session account = %q, want %q", got, activeAcct)
	}
}

// An orphaned run whose primary chat is bound to a disabled account resumes on
// the default account, and the chat row moves there.
func TestResumeOrphanedHeadlessRuns_DisabledChatAccountResumesOnDefault(t *testing.T) {
	f := newOrphanResumeFixture(t)
	withStaleAccountDisabled(f.lc)
	bound := staleAcct
	chats := &mockAgentChatStore{chatsBySession: map[string][]*domain.AgentChat{
		f.sessionID: {{SessionID: f.sessionID, AgentSessionID: "agent-old", AgentName: "claude", AccountID: &bound}},
	}}
	f.lc.agentChats = chats

	if n := f.lc.ResumeOrphanedHeadlessRuns(context.Background()); n != 1 {
		t.Fatalf("resumed = %d, want 1", n)
	}
	if len(f.runner.started) != 1 {
		t.Fatalf("headless starts = %d, want 1", len(f.runner.started))
	}
	if got := f.runner.started[0].env[claudeOAuthTokenEnvKey]; got != activeBearer {
		t.Errorf("resumed run credential = %q, want %q", got, activeBearer)
	}
	if got := derefString(chats.chatsBySession[f.sessionID][0].AccountID); got != activeAcct {
		t.Errorf("chat account = %q, want %q", got, activeAcct)
	}
}

// Resurrecting an archived session whose account was disabled while it was
// archived resumes it on the default account, and the session moves there.
func TestResurrectSession_DisabledSessionAccountResumesOnDefault(t *testing.T) {
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", LocalPath: "/tmp/repo", IsTrusted: true}
	archivedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	prior := "claude-old"
	seed := staleAcct
	sessions.sessions["sess-1"] = &models.Session{
		ID: "sess-1", RepoID: "repo-1", Title: "Test Session", Plan: "Do something",
		WorktreePath: "/tmp/worktrees/test-repo/test", BranchName: "test", BaseBranch: "main",
		AgentName: "claude", State: machine.ImplementingPlan, ArchivedAt: &archivedAt,
		AgentSessionID: &prior, AccountID: &seed,
	}
	runner := newMockAgentRunner()
	lc := newTestLifecycle(sessions, repos, nil, nil, &mockWorktreeManager{}, runner, nil, newMockVCSProvider(), zerolog.Nop())
	withStaleAccountDisabled(lc)

	if err := resurrectForTest(lc, context.Background(), "sess-1"); err != nil {
		t.Fatalf("ResurrectSession: %v", err)
	}
	if len(runner.started) != 1 {
		t.Fatalf("starts = %d, want 1", len(runner.started))
	}
	if got := runner.started[0].env[claudeOAuthTokenEnvKey]; got != activeBearer {
		t.Errorf("resurrected run credential = %q, want %q", got, activeBearer)
	}
	if got := derefString(sessions.sessions["sess-1"].AccountID); got != activeAcct {
		t.Errorf("session account = %q, want %q", got, activeAcct)
	}
}

// A move rewrites only the rows of panes the status tracker reports running
// that run the session's own agent. A stopped pane is never read from tmux, and
// a cross-agent chat keeps its account even when its pane carries the session's
// token.
func TestMoveSessionAccount_OnlyRunningSameAgentPanesFollow(t *testing.T) {
	h := newProxySwitchHarness(t)
	follower, stopped, codex := switchStartAccount, switchStartAccount, switchStartAccount
	followerPane, stoppedPane, codexPane := "bossd-agent-run-agent-2", "bossd-agent-run-agent-4", "bossd-agent-run-agent-5"
	h.chats.chatsBySession["sess-1"] = append(h.chats.chatsBySession["sess-1"],
		&domain.AgentChat{SessionID: "sess-1", AgentSessionID: "agent-2", AgentName: "claude", AccountID: &follower, TmuxSessionName: &followerPane},
		&domain.AgentChat{SessionID: "sess-1", AgentSessionID: "agent-4", AgentName: "claude", AccountID: &stopped, TmuxSessionName: &stoppedPane},
		&domain.AgentChat{SessionID: "sess-1", AgentSessionID: "agent-5", AgentName: "codex", AccountID: &codex, TmuxSessionName: &codexPane},
	)
	h.lc.proxyRegistrar.TokenForSession("sess-1")
	sessionURL := wantSessionProxyURL("sess-1")
	h.tmuxFake.bakedBaseURL = map[string]string{followerPane: sessionURL, stoppedPane: sessionURL, codexPane: sessionURL}
	h.lc.SetChatRunningProbe(func(agentSessionID string) bool { return agentSessionID != "agent-4" })

	if err := h.lc.MoveSessionAgentAccount(context.Background(), h.sessions.sessions["sess-1"], "acct-2"); err != nil {
		t.Fatalf("MoveSessionAgentAccount: %v", err)
	}
	for agentSessionID, want := range map[string]string{"agent-2": "acct-2", "agent-4": switchStartAccount, "agent-5": switchStartAccount} {
		if got := chatAccount(t, h, agentSessionID); got != want {
			t.Errorf("chat %s row = %q, want %q", agentSessionID, got, want)
		}
	}
	h.tmuxFake.mu.Lock()
	defer h.tmuxFake.mu.Unlock()
	for _, call := range h.tmuxFake.calls {
		if strings.Contains(strings.Join(call.args, " "), stoppedPane) {
			t.Errorf("tmux %s %v ran against the stopped pane, want no tmux call for it", call.subcommand, call.args)
		}
	}
}

// With no status tracker wired, a move still rewrites the rows of panes that
// hold the session token: every chat counts as running.
func TestMoveSessionAccount_WithoutRunningProbeRowsStillFollow(t *testing.T) {
	h := newProxySwitchHarness(t)
	follower := switchStartAccount
	followerPane := "bossd-agent-run-agent-2"
	h.chats.chatsBySession["sess-1"] = append(h.chats.chatsBySession["sess-1"],
		&domain.AgentChat{SessionID: "sess-1", AgentSessionID: "agent-2", AgentName: "claude", AccountID: &follower, TmuxSessionName: &followerPane},
	)
	h.lc.proxyRegistrar.TokenForSession("sess-1")
	h.tmuxFake.bakedBaseURL = map[string]string{followerPane: wantSessionProxyURL("sess-1")}

	if err := h.lc.MoveSessionAgentAccount(context.Background(), h.sessions.sessions["sess-1"], "acct-2"); err != nil {
		t.Fatalf("MoveSessionAgentAccount: %v", err)
	}
	if got := chatAccount(t, h, "agent-2"); got != "acct-2" {
		t.Errorf("session-token chat row = %q with no running probe, want acct-2", got)
	}
}

// A failed chat write reads as the account's label and the cause, with no ids,
// on a launch that moves off a disabled account and on a manual switch, whose
// failure is also saved on the chat row for the operator.
func TestMoveChatAccount_FailedWriteReadsPlainly(t *testing.T) {
	locked := errors.New("database is locked")

	t.Run("launch off a disabled account", func(t *testing.T) {
		h := newStartTmuxChatHarness(t)
		row := h.sessions.sessions["sess-1"]
		primary := "agent-1"
		row.AgentSessionID = &primary
		bound := staleAcct
		h.chats.chatsBySession = map[string][]*domain.AgentChat{row.ID: {
			{SessionID: row.ID, AgentSessionID: primary, AgentName: "claude", AccountID: &bound},
		}}
		withAccounts(h, map[string]*domain.AgentAccount{
			staleAcct:  claudeAccount(staleAcct, domain.AgentAccountStatusDisabled),
			activeAcct: claudeAccount(activeAcct, domain.AgentAccountStatusActive),
		}, activeAcct)
		h.lc.accountSwitchRegistry = idKeyedSwitchRegistry{}
		h.chats.updateAccountIDErr = locked

		_, _, err := h.lc.InServiceSpawnTarget(context.Background(), row)
		const want = "could not move this chat to Label acct-hello: database is locked"
		if err == nil || err.Error() != want {
			t.Fatalf("error = %v, want %q", err, want)
		}
	})

	t.Run("manual switch", func(t *testing.T) {
		h := newProxySwitchHarness(t)
		h.chats.updateAccountIDErr = locked

		_, err := h.lc.SwitchAgentAccount(context.Background(), SwitchAgentAccountParams{
			SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2",
		})
		const want = "could not move this chat to Label acct-2: database is locked"
		if err == nil || err.Error() != want {
			t.Fatalf("error = %v, want %q", err, want)
		}
		h.chats.mu.Lock()
		defer h.chats.mu.Unlock()
		if n := len(h.chats.markStartFailedCalls); n == 0 || h.chats.markStartFailedCalls[n-1].reason != want {
			t.Fatalf("saved chat error = %+v, want the last one to read %q", h.chats.markStartFailedCalls, want)
		}
	})
}

// spentDeadlineBinding writes the account only after the caller's deadline has
// passed, the way a write stuck behind a busy database finishes late.
type spentDeadlineBinding struct{}

func (spentDeadlineBinding) SessionAgentAccount(context.Context, string) (string, error) {
	return "", nil
}

func (spentDeadlineBinding) BindSessionAgentAccount(ctx context.Context, _, _ string) error {
	<-ctx.Done()
	return nil
}

// deadlineAuditStore refuses an insert on a finished context, as SQLite does.
type deadlineAuditStore struct{ captureAuditStore }

func (s *deadlineAuditStore) Create(ctx context.Context, ev rotation.AuditEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.captureAuditStore.Create(ctx, ev)
}

// A failover whose account write used up the commit's deadline still leaves
// the rotation in history.
func TestCommitFailover_AuditSurvivesASpentDeadline(t *testing.T) {
	f := newRotationFixture(t)
	store := &deadlineAuditStore{}
	f.lc.SetRotationRecorder(rotation.NewRecorder(store, zerolog.Nop()))
	f.lc.accountSwitchBinding = spentDeadlineBinding{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	if err := f.lc.CommitFailover(ctx, f.sessionID, domain.FailoverResult{
		Rotate: true, NextAccountID: "acct-next", FromAccountID: "acct-capped",
		Provider: "claude", Trigger: "ROTATION_TRIGGER_USAGE_LIMITED",
	}); err != nil {
		t.Fatalf("CommitFailover: %v", err)
	}
	if events := store.all(); len(events) != 1 || events[0].Outcome != "ROTATION_OUTCOME_ROTATED" {
		t.Fatalf("rotation history = %+v, want one ROTATED row", events)
	}
}
