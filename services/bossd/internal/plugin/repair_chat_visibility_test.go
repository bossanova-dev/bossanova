package plugin

import (
	"context"
	"testing"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/status"
)

// Task 6 — regression coverage for the user-facing fix.
//
// Before this work, when the repair plugin kicked off `/boss-repair` against
// a session, no row was inserted into agent_chats and the in-flight repair
// was invisible: ListChats returned the original chat only, and the
// operator had no way to see, attach to, or audit the repair from the
// chat list. The new StartChatRun → Lifecycle.StartTmuxChat path inserts
// the row + populates tmux_session_name as a side effect of starting the
// tmux-hosted run.
//
// This file exercises that contract end-to-end at the daemon-internal
// layer: a real in-memory agent_chats store, a Lifecycle stand-in that
// mirrors the row + tmux-name writes a real Lifecycle does, the real
// HostServiceServer wiring StartChatRun → Lifecycle, and finally a
// ListBySession query against the same store the daemon's ListChats RPC
// reads from. If any link in that chain breaks, ListChats stops surfacing
// the repair chat — which is exactly the regression we are guarding
// against. Run-state map population (activeRuns / runCompletion /
// runHookTokens / agentSessionByID / runSessionByID) is covered by
// host_service_test.go's TestStartChatRun_HappyPath; this file only
// asserts the row + tmux-name reaches ListBySession.

// dbWritingChatLifecycle is a ChatLifecycle implementation that performs
// the same row + tmux-name persistence the real *session.Lifecycle does
// inside StartTmuxChat, but without spawning tmux or talking to an
// AgentRunner subprocess. It exists so the visibility test can assert on
// the row written through the agent_chats store without depending on a
// tmux binary on PATH.
//
// Real Lifecycle.StartTmuxChat (services/bossd/internal/session/tmux_chat.go)
// is responsible for:
//
//  1. minting an agent_session_id and tmux name,
//  2. spawning tmux,
//  3. agentChats.Create(...) with the supplied title,
//  4. agentChats.UpdateTmuxSessionName(...) so the chat list surfaces the
//     live tmux name,
//  5. configuring the run-keyed Stop hook,
//  6. SendPlan-ing the prompt as a bracketed paste.
//
// Steps 3 and 4 are the row+name writes the visibility regression cares
// about; this fake mirrors those exactly. Steps 1, 5, 6 don't change the
// observable post-StartChatRun state for ListChats. Step 2 only matters
// for actual chat attachment, which is out of scope for this test.
type dbWritingChatLifecycle struct {
	chats          db.AgentChatStore
	agentSessionID string
	tmuxName       string

	// recorded fields for assertions
	gotSessionID string
	gotPrompt    string
	gotTitle     string
	gotHookOpts  session.HookOpts
}

func (lc *dbWritingChatLifecycle) StartTmuxChat(ctx context.Context, sessionID, prompt, title string, hookOpts session.HookOpts) (string, error) {
	lc.gotSessionID = sessionID
	lc.gotPrompt = prompt
	lc.gotTitle = title
	lc.gotHookOpts = hookOpts

	if _, err := lc.chats.Create(ctx, db.CreateAgentChatParams{
		SessionID:      sessionID,
		AgentSessionID: lc.agentSessionID,
		AgentName:      "claude",
		Title:          title,
	}); err != nil {
		return "", err
	}
	tmux := lc.tmuxName
	if err := lc.chats.UpdateTmuxSessionName(ctx, lc.agentSessionID, &tmux); err != nil {
		return "", err
	}
	return lc.agentSessionID, nil
}

// TestStartChatRun_RepairChatVisibleInListChats is the regression test
// for the original bug. It pins the user-facing contract: after
// StartChatRun returns successfully, the repair chat must be present in
// agentChats.ListBySession (the table backing DaemonService.ListChats),
// with the supplied "Repair: <title>" title and a non-empty
// tmux_session_name. Before this work the row didn't exist at all and
// the chat picker showed only the original (idle) chat.
func TestStartChatRun_RepairChatVisibleInListChats(t *testing.T) {
	ctx := t.Context()
	sqlDB := openTestDB(t)
	chats := db.NewAgentChatStore(sqlDB)
	sessions := db.NewSessionStore(sqlDB)
	repos := db.NewRepoStore(sqlDB)

	repo, err := repos.Create(ctx, db.CreateRepoParams{
		DisplayName:       "test-repo",
		LocalPath:         "/tmp/repo",
		OriginURL:         "https://github.com/test/repair-chat-visibility.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	sess, err := sessions.Create(ctx, db.CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "broken session",
		Plan:         "docs/plans/test-plan.md",
		WorktreePath: "/tmp/worktrees/broken-session",
		BranchName:   "broken-session",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	const (
		agentSessionID = "agent-repair-1234"
		tmuxName       = "boss-repair-tmux-1234"
		title          = "Repair: broken session"
	)
	lc := &dbWritingChatLifecycle{
		chats:          chats,
		agentSessionID: agentSessionID,
		tmuxName:       tmuxName,
	}

	srv := NewHostServiceServer(&mockVCSProvider{})
	srv.SetSessionDeps(repos, sessions, chats, status.NewDisplayTracker(), status.NewTracker())
	srv.SetAgentClients(map[string]agent.AgentRunnerClient{"claude": newFakeAgentClient()})
	srv.SetAgentLogsDir(t.TempDir())
	srv.SetLifecycle(lc)

	resp, err := srv.StartChatRun(ctx, &bossanovav1.StartChatRunHostRequest{
		SessionId: sess.ID,
		Prompt:    "/boss-repair",
		Title:     title,
	})
	if err != nil {
		t.Fatalf("StartChatRun: %v", err)
	}
	if got := resp.GetAgentSessionId(); got != agentSessionID {
		t.Fatalf("AgentSessionId = %q, want %q", got, agentSessionID)
	}

	// The lifecycle must have been called with the exact title the spec
	// pins ("Repair: <session title>"), preserved through the daemon path.
	// Repair-plugin unit tests cover the title-format-on-the-plugin-side,
	// but this test pins the daemon-side preservation: a future caller
	// rewriting the title between StartChatRun and StartTmuxChat would
	// be caught here.
	if lc.gotTitle != title {
		t.Errorf("lifecycle saw title = %q, want %q", lc.gotTitle, title)
	}
	if lc.gotPrompt != "/boss-repair" {
		t.Errorf("lifecycle saw prompt = %q, want /boss-repair", lc.gotPrompt)
	}
	if lc.gotHookOpts.Token == "" {
		t.Error("lifecycle should receive a non-empty hook token from StartChatRun")
	}

	// THE regression assertion: the chat row exists in the agent_chats
	// table, with the supplied title and the tmux session name populated.
	// This is what backs DaemonService.ListChats — if it's not here, the
	// chat picker has nothing to render, and the repair becomes invisible
	// (the original bug). Anything that breaks the StartChatRun →
	// Lifecycle → row insert chain trips this test.
	listed, err := chats.ListBySession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("ListBySession: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("ListBySession returned %d chats, want 1", len(listed))
	}
	row := listed[0]
	if row.Title != title {
		t.Errorf("row.Title = %q, want %q", row.Title, title)
	}
	if row.AgentSessionID != agentSessionID {
		t.Errorf("row.AgentSessionID = %q, want %q", row.AgentSessionID, agentSessionID)
	}
	if row.SessionID != sess.ID {
		t.Errorf("row.SessionID = %q, want %q", row.SessionID, sess.ID)
	}
	if row.TmuxSessionName == nil || *row.TmuxSessionName != tmuxName {
		got := "<nil>"
		if row.TmuxSessionName != nil {
			got = *row.TmuxSessionName
		}
		t.Errorf("row.TmuxSessionName = %q, want %q (non-empty so the picker can attach)", got, tmuxName)
	}
}

// TestRepairChatVisibility_EndToEnd_HookCompletesRun exercises the second
// half of the data-flow diagram in the spec: after the chat row is in
// place, the claude Stop hook POSTs /hooks/agent-run-complete/{id}, the
// daemon's CompleteAgentRun signals the run-completion channel, and a
// concurrent WaitChatRun unblocks with the propagated exit_error.
//
// The hook server is in-process; we drive the same code path it would by
// calling CompleteAgentRun directly. (The HTTP handler in
// services/bossd/internal/server/hook_server.go is a thin wrapper around
// this — its own unit tests cover the wire-level concerns.)
func TestRepairChatVisibility_EndToEnd_HookCompletesRun(t *testing.T) {
	ctx := t.Context()
	sqlDB := openTestDB(t)
	chats := db.NewAgentChatStore(sqlDB)
	sessions := db.NewSessionStore(sqlDB)
	repos := db.NewRepoStore(sqlDB)

	repo, err := repos.Create(ctx, db.CreateRepoParams{
		DisplayName:       "test-repo-e2e",
		LocalPath:         "/tmp/repo",
		OriginURL:         "https://github.com/test/repair-e2e.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	sess, err := sessions.Create(ctx, db.CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "broken session",
		Plan:         "docs/plans/test-plan.md",
		WorktreePath: "/tmp/worktrees/broken-session-e2e",
		BranchName:   "broken-session-e2e",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	lc := &dbWritingChatLifecycle{
		chats:          chats,
		agentSessionID: "agent-repair-e2e",
		tmuxName:       "boss-tmux-e2e",
	}

	displayTracker := status.NewDisplayTracker()
	srv := NewHostServiceServer(&mockVCSProvider{})
	srv.SetSessionDeps(repos, sessions, chats, displayTracker, status.NewTracker())
	srv.SetAgentClients(map[string]agent.AgentRunnerClient{"claude": newFakeAgentClient()})
	srv.SetAgentLogsDir(t.TempDir())
	srv.SetLifecycle(lc)

	// The repair plugin would call SetRepairing(true) before StartChatRun;
	// stage that here so we can verify CompleteAgentRun clears it. The
	// alternative (asserting it stays unset) would let a regression where
	// the flag never clears slip through silently.
	displayTracker.SetRepairing(sess.ID, true)

	startResp, err := srv.StartChatRun(ctx, &bossanovav1.StartChatRunHostRequest{
		SessionId: sess.ID,
		Prompt:    "/boss-repair",
		Title:     "Repair: broken session",
	})
	if err != nil {
		t.Fatalf("StartChatRun: %v", err)
	}
	agentSessionID := startResp.GetAgentSessionId()

	// Pull the per-run hook token out of the daemon state — in production
	// the claude plugin's hook script reads it from a settings file the
	// claude plugin writes during ConfigureFinalizeHook. Here we shortcut
	// straight to what the script would echo back as the Authorization
	// header.
	srv.runMu.Lock()
	token := srv.runHookTokens[agentSessionID]
	srv.runMu.Unlock()
	if token == "" {
		t.Fatal("runHookTokens missing for agent_session_id; StartChatRun should have registered it")
	}

	// Fire CompleteAgentRun from a goroutine so the wait actually sleeps
	// before the signal arrives (mirrors the production race window in
	// host_service_test.go's TestWaitChatRun_HookSignalsCleanExit). The
	// inverse arrangement (WaitChatRun in the goroutine, signal first)
	// races CompleteAgentRun's runCompletion delete against WaitChatRun's
	// runCompletion lookup — when the signal lands first, the lookup
	// 404s with FailedPrecondition. This isn't a daemon bug: production
	// always has the WaitChatRun caller blocking before claude can
	// possibly Stop, and we mirror that ordering here.
	const exitMsg = "exit status 1: build failed"
	go func() {
		// 20ms matches host_service_test.go's existing pattern; long
		// enough that WaitChatRun's runMu lookup completes first under
		// realistic scheduler pressure, short enough that the test
		// stays under 100ms wall time.
		time.Sleep(20 * time.Millisecond)
		_, _ = srv.CompleteAgentRun(context.Background(), agentSessionID, token, exitMsg)
	}()

	resp, err := srv.WaitChatRun(ctx, &bossanovav1.WaitChatRunHostRequest{
		AgentSessionId: agentSessionID,
	})
	if err != nil {
		t.Fatalf("WaitChatRun: %v", err)
	}
	if resp.GetExitError() != exitMsg {
		t.Errorf("WaitChatRun exit_error = %q, want %q (must round-trip the hook payload)",
			resp.GetExitError(), exitMsg)
	}

	// IsRepairing must be cleared post-completion. A regression that
	// leaves it set wedges the badge on "repairing" forever.
	if entry := displayTracker.Get(sess.ID); entry != nil && entry.IsRepairing {
		t.Error("IsRepairing should be cleared by CompleteAgentRun")
	}

	// The chat row stays in agent_chats post-completion — it's an audit
	// trail, not transient. The chat picker continues to surface it as a
	// historical chat the operator can attach to.
	listed, err := chats.ListBySession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("ListBySession (post-complete): %v", err)
	}
	if len(listed) != 1 {
		t.Errorf("ListBySession returned %d chats post-complete, want 1 (row is audit trail)", len(listed))
	}
}

// TestStartChatRun_IdleGate_HasActiveChatVisibleViaHostListSessions ties
// the user-facing contract to the repair plugin's idle gate. The plugin
// reads HasActiveChat + LastChatActivityAt off ListSessions
// (host_service.go ListSessions) and defers a second sweep when the
// in-flight repair chat is still talking. The integration angle: after
// StartChatRun + a chatTracker heartbeat, the same ListSessions response
// the repair plugin queries reflects HasActiveChat=true with a recent
// LastChatActivityAt. That's the wire-level guarantee that makes the
// plugin's already-unit-tested gate kick in for the in-flight repair.
//
// Without this test, a regression that breaks the agentChats →
// chatTracker join in ListSessions (eg. someone changing how
// HasActiveChat is computed) would silently re-enable duplicate repair
// sweeps.
func TestStartChatRun_IdleGate_HasActiveChatVisibleViaHostListSessions(t *testing.T) {
	ctx := t.Context()
	sqlDB := openTestDB(t)
	chats := db.NewAgentChatStore(sqlDB)
	sessions := db.NewSessionStore(sqlDB)
	repos := db.NewRepoStore(sqlDB)

	repo, err := repos.Create(ctx, db.CreateRepoParams{
		DisplayName:       "test-repo-gate",
		LocalPath:         "/tmp/repo",
		OriginURL:         "https://github.com/test/repair-gate.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	sess, err := sessions.Create(ctx, db.CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "gate session",
		Plan:         "docs/plans/test-plan.md",
		WorktreePath: "/tmp/worktrees/gate",
		BranchName:   "gate",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	const agentSessionID = "agent-gate-1"
	lc := &dbWritingChatLifecycle{
		chats:          chats,
		agentSessionID: agentSessionID,
		tmuxName:       "boss-tmux-gate",
	}

	displayTracker := status.NewDisplayTracker()
	chatTracker := status.NewTracker()
	srv := NewHostServiceServer(&mockVCSProvider{})
	srv.SetSessionDeps(repos, sessions, chats, displayTracker, chatTracker)
	srv.SetAgentClients(map[string]agent.AgentRunnerClient{"claude": newFakeAgentClient()})
	srv.SetAgentLogsDir(t.TempDir())
	srv.SetLifecycle(lc)

	if _, err := srv.StartChatRun(ctx, &bossanovav1.StartChatRunHostRequest{
		SessionId: sess.ID,
		Prompt:    "/boss-repair",
		Title:     "Repair: gate session",
	}); err != nil {
		t.Fatalf("StartChatRun: %v", err)
	}

	// Stamp a fresh heartbeat on the chat tracker so ListSessions joins
	// the new row with a live entry, mirroring what the claude plugin's
	// stream-json frame handler does in production.
	heartbeat := time.Now()
	chatTracker.Update(agentSessionID, bossanovav1.ChatStatus_CHAT_STATUS_WORKING, heartbeat)

	listResp, err := srv.ListSessions(ctx, &bossanovav1.HostServiceListSessionsRequest{})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	var found *bossanovav1.Session
	for _, s := range listResp.GetSessions() {
		if s.GetId() == sess.ID {
			found = s
			break
		}
	}
	if found == nil {
		t.Fatalf("ListSessions did not return seeded session %s", sess.ID)
	}
	if !found.GetHasActiveChat() {
		t.Error("HasActiveChat = false after StartChatRun + heartbeat; the repair plugin's idle gate would treat the session as repairable and fire a duplicate sweep")
	}
	if got := found.GetLastChatActivityAt(); got == nil || got.AsTime().Before(heartbeat.Add(-time.Second)) {
		t.Errorf("LastChatActivityAt = %v, want at or after heartbeat (%v); repair plugin's silent_for math depends on this", got, heartbeat)
	}
}

// Compile-time verification that the *models.AgentChat fields the
// visibility tests read still exist. A schema bump that drops Title or
// TmuxSessionName fails the build here rather than at the assertion site.
var _ = models.AgentChat{}.Title
var _ = models.AgentChat{}.TmuxSessionName
