package session

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/tmux"
)

// Tests for Lifecycle.StartTmuxChat — the generalized form of the cron-only
// helper that previously lived in startCronTmuxChat. Cron-specific behavior
// stays in lifecycle_test.go (the cron test cluster around
// TestStartSession_CronJobID_*); this file targets the generic method
// directly so any future caller (repair, interactive UI button) gets the
// same coverage.

// startTmuxChatHarness wires up everything Lifecycle.StartTmuxChat needs:
// in-memory stores, a fake tmux client, an agent runner client returning
// realistic argv, and an agentLogsDir. Each test instantiates one of
// these, optionally tweaks the failure-injection knobs, then calls
// StartTmuxChat directly.
type startTmuxChatHarness struct {
	t          *testing.T
	sessions   *mockSessionStore
	repos      *mockRepoStore
	chats      *mockAgentChatStore
	tmuxFake   *fakeTmux
	tmuxClient *tmux.Client
	agentFake  *fakeAgentForLifecycle
	agentRun   *mockAgentRunner
	wt         *mockWorktreeManager
	logsDir    string
	lc         *Lifecycle
}

func newStartTmuxChatHarness(t *testing.T) *startTmuxChatHarness {
	t.Helper()
	h := &startTmuxChatHarness{
		t:         t,
		sessions:  newMockSessionStore(),
		repos:     newMockRepoStore(),
		chats:     &mockAgentChatStore{},
		tmuxFake:  newFakeTmux(),
		agentFake: newFakeAgent(),
		agentRun:  newMockAgentRunner(),
		wt:        &mockWorktreeManager{},
		logsDir:   t.TempDir(),
	}
	h.tmuxClient = tmux.NewClient(tmux.WithCommandFactory(h.tmuxFake.factory))
	h.repos.repos["repo-abcdef12"] = &models.Repo{
		ID:                "repo-abcdef12",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		OriginURL:         "owner/repo",
	}
	h.sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-abcdef12",
		Title:        "Some session",
		Plan:         "do the thing",
		BaseBranch:   "main",
		WorktreePath: "/tmp/worktrees/test/sess-1",
		State:        machine.ImplementingPlan,
		AgentName:    "claude",
	}
	h.lc = NewLifecycle(h.sessions, h.repos, h.chats, &stubCronJobStore{}, h.wt, h.agentRun, h.tmuxClient, newMockVCSProvider(), zerolog.Nop())
	h.lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": h.agentFake})
	h.lc.SetAgentLogsDir(h.logsDir)
	return h
}

// findCall returns the first recorded tmux call matching subcommand, or nil.
func (h *startTmuxChatHarness) findCall(subcommand string) *recordedTmuxCall {
	h.tmuxFake.mu.Lock()
	defer h.tmuxFake.mu.Unlock()
	for i := range h.tmuxFake.calls {
		if h.tmuxFake.calls[i].subcommand == subcommand {
			return &h.tmuxFake.calls[i]
		}
	}
	return nil
}

// TestStartTmuxChat_HappyPath exercises the full extracted path: idempotency
// check finds nothing → BuildInteractiveCommand → tmux NewSession → row
// Create with the supplied title → UpdateTmuxSessionName → SendPlan
// (bracketed paste). Verifies the title is exactly what the caller passed
// (NOT cron's `Run "..."` template) and that the argv came from the agent
// plugin rather than a hardcoded slice.
func TestStartTmuxChat_HappyPath(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)

	const supplyTitle = "Repair: Some session"
	const supplyPrompt = "/boss-repair"

	agentSessionID, err := h.lc.StartTmuxChat(ctx, "sess-1", supplyPrompt, supplyTitle, HookOpts{})
	if err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}
	if agentSessionID == "" {
		t.Fatal("expected non-empty agentSessionID on success")
	}

	// agent_chats row was created with the supplied title (NOT cron's template).
	if len(h.chats.createCalls) != 1 {
		t.Fatalf("expected 1 agentChats.Create call, got %d", len(h.chats.createCalls))
	}
	if h.chats.createCalls[0].Title != supplyTitle {
		t.Errorf("Title = %q, want %q", h.chats.createCalls[0].Title, supplyTitle)
	}
	if h.chats.createCalls[0].SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", h.chats.createCalls[0].SessionID)
	}
	if h.chats.createCalls[0].AgentSessionID != agentSessionID {
		t.Errorf("AgentSessionID = %q, want %q", h.chats.createCalls[0].AgentSessionID, agentSessionID)
	}
	if h.chats.createCalls[0].AgentName != "claude" {
		t.Errorf("AgentName = %q, want claude", h.chats.createCalls[0].AgentName)
	}

	// Tmux NewSession was called with argv from the agent plugin's
	// BuildInteractiveCommand, not a hardcoded slice. The fake agent
	// returns argv shaped like ["sh", "-c", "claude --session-id <id> ..."].
	newSess := h.findCall("new-session")
	if newSess == nil {
		t.Fatal("expected tmux new-session call")
	}
	joined := strings.Join(newSess.args, " ")
	if !strings.Contains(joined, "sh -c") {
		t.Errorf("expected new-session argv to use sh -c shape, got: %s", joined)
	}
	if !strings.Contains(joined, "claude --session-id "+agentSessionID) {
		t.Errorf("expected new-session argv to embed claude --session-id %s, got: %s", agentSessionID, joined)
	}

	// UpdateTmuxSessionName wrote a non-empty resolved tmux name onto the row.
	if len(h.chats.tmuxNameUpdates) != 1 {
		t.Fatalf("expected 1 UpdateTmuxSessionName call, got %d", len(h.chats.tmuxNameUpdates))
	}
	if h.chats.tmuxNameUpdates[0].agentSessionID != agentSessionID {
		t.Errorf("UpdateTmuxSessionName agentSessionID = %q, want %q", h.chats.tmuxNameUpdates[0].agentSessionID, agentSessionID)
	}
	if h.chats.tmuxNameUpdates[0].name == nil || *h.chats.tmuxNameUpdates[0].name == "" {
		t.Error("expected non-nil/non-empty tmux name persisted on chat row")
	}

	// SendPlan must have run: load-buffer + paste-buffer + send-keys.
	for _, sub := range []string{"load-buffer", "paste-buffer", "send-keys"} {
		if !h.tmuxFake.hasSubcommand(sub) {
			t.Errorf("expected tmux %s call from SendPlan, none recorded", sub)
		}
	}

	// No row deletions expected on the happy path.
	if len(h.chats.deletedAgentSessionIDs) != 0 {
		t.Errorf("expected 0 deletes on happy path, got %v", h.chats.deletedAgentSessionIDs)
	}
}

// TestStartTmuxChat_TmuxUnavailable verifies fail-closed behavior when tmux
// isn't on PATH: typed FailedPrecondition error, no chat row created.
func TestStartTmuxChat_TmuxUnavailable(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.tmuxFake.available = false

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", "p", "T", HookOpts{})
	if err == nil {
		t.Fatal("expected error when tmux unavailable")
	}
	if got := grpcstatus.Code(err); got != codes.FailedPrecondition {
		t.Errorf("error code = %s, want FailedPrecondition", got)
	}
	if len(h.chats.createCalls) != 0 {
		t.Errorf("expected 0 Create calls when tmux unavailable, got %d", len(h.chats.createCalls))
	}
}

// TestStartTmuxChat_AgentRunnerNotLoaded verifies fail-closed behavior when
// the session's AgentName has no registered AgentRunnerClient.
func TestStartTmuxChat_AgentRunnerNotLoaded(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	// Replace the agent registry with an empty map so claude is unloaded.
	h.lc.SetAgents(map[string]agent.AgentRunnerClient{})

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", "p", "T", HookOpts{})
	if err == nil {
		t.Fatal("expected error when agent runner not loaded")
	}
	if got := grpcstatus.Code(err); got != codes.FailedPrecondition {
		t.Errorf("error code = %s, want FailedPrecondition", got)
	}
	if len(h.chats.createCalls) != 0 {
		t.Errorf("expected 0 Create calls when agent missing, got %d", len(h.chats.createCalls))
	}
}

// TestStartTmuxChat_NewSessionFails verifies that a tmux NewSession failure
// returns an error before any agent_chats row is created.
func TestStartTmuxChat_NewSessionFails(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.tmuxFake.failSubcommand["new-session"] = true

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", "p", "T", HookOpts{})
	if err == nil {
		t.Fatal("expected error when tmux new-session fails")
	}
	if len(h.chats.createCalls) != 0 {
		t.Errorf("expected 0 agentChats.Create calls when new-session fails, got %d", len(h.chats.createCalls))
	}
	// new-session was attempted but kill-session was NOT — there's no orphan
	// to clean up because the spawn never succeeded.
	if !h.tmuxFake.hasSubcommand("new-session") {
		t.Error("expected new-session to have been attempted")
	}
	if h.tmuxFake.hasSubcommand("kill-session") {
		t.Error("did not expect kill-session when new-session itself failed")
	}
}

// TestStartTmuxChat_EmptyArgvFails verifies that an empty argv from
// BuildInteractiveCommand is treated as a hard precondition failure
// before any tmux process is spawned.
func TestStartTmuxChat_EmptyArgvFails(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	// Make BuildInteractiveCommand return empty argv.
	h.lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": &emptyArgvAgent{}})

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", "p", "T", HookOpts{})
	if err == nil {
		t.Fatal("expected error when BuildInteractiveCommand returns empty argv")
	}
	if got := grpcstatus.Code(err); got != codes.FailedPrecondition {
		t.Errorf("error code = %s, want FailedPrecondition", got)
	}
	if h.tmuxFake.hasSubcommand("new-session") {
		t.Error("did not expect tmux new-session when argv was empty")
	}
	if len(h.chats.createCalls) != 0 {
		t.Errorf("expected 0 Create calls on empty argv, got %d", len(h.chats.createCalls))
	}
}

// TestStartTmuxChat_ChatCreateFails verifies that an agentChats.Create
// failure after tmux is live tears tmux back down and leaves no row.
func TestStartTmuxChat_ChatCreateFails(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.chats.createErr = fmt.Errorf("simulated DB failure")

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", "p", "T", HookOpts{})
	if err == nil {
		t.Fatal("expected error when Create fails")
	}
	if !h.tmuxFake.hasSubcommand("new-session") {
		t.Error("expected tmux new-session before Create attempt")
	}
	if !h.tmuxFake.hasSubcommand("kill-session") {
		t.Error("expected tmux kill-session to clean up after Create failure")
	}
	if len(h.chats.deletedAgentSessionIDs) != 0 {
		t.Errorf("expected 0 deletes (Create itself failed, no row to delete), got %v", h.chats.deletedAgentSessionIDs)
	}
}

// TestStartTmuxChat_UpdateTmuxSessionNameFails verifies that an
// UpdateTmuxSessionName failure tears tmux down AND deletes the orphaned
// agent_chats row, so a retry doesn't leak.
func TestStartTmuxChat_UpdateTmuxSessionNameFails(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.chats.updateTmuxNameErr = fmt.Errorf("simulated update failure")

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", "p", "T", HookOpts{})
	if err == nil {
		t.Fatal("expected error when UpdateTmuxSessionName fails")
	}
	if !h.tmuxFake.hasSubcommand("kill-session") {
		t.Error("expected tmux kill-session after UpdateTmuxSessionName failure")
	}
	if len(h.chats.deletedAgentSessionIDs) != 1 {
		t.Errorf("expected 1 row delete after UpdateTmuxSessionName failure, got %v", h.chats.deletedAgentSessionIDs)
	}
}

// TestStartTmuxChat_SendPlanFails verifies that a SendPlan failure tears
// tmux down AND deletes the orphaned agent_chats row.
func TestStartTmuxChat_SendPlanFails(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	// Force load-buffer (the first stage of SendPlan) to fail. The
	// capture-pane ready-marker poll runs first; we leave that succeeding
	// so SendPlan reaches the real failure.
	h.tmuxFake.failSubcommand["load-buffer"] = true

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", "p", "T", HookOpts{})
	if err == nil {
		t.Fatal("expected error when SendPlan fails")
	}
	if !h.tmuxFake.hasSubcommand("kill-session") {
		t.Error("expected tmux kill-session after SendPlan failure")
	}
	if len(h.chats.deletedAgentSessionIDs) != 1 {
		t.Errorf("expected 1 row delete after SendPlan failure, got %v", h.chats.deletedAgentSessionIDs)
	}
}

// TestStartTmuxChat_AlreadyExists_LiveTmux verifies daemon-restart re-entry:
// when an existing chat row's tmux session is still alive, StartTmuxChat
// returns AlreadyExists with the original agent_session_id returned in the
// success-shaped string slot alongside the typed error. No new row, no new
// tmux session.
func TestStartTmuxChat_AlreadyExists_LiveTmux(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)

	// Pre-populate a chat row whose tmux name the fake will report alive.
	existingTmuxName := "boss-repo-abc12345"
	existingAgentSessionID := "agent-existing-12345678"
	h.chats.chatsBySession = map[string][]*models.AgentChat{
		"sess-1": {{
			ID:              "chat-existing",
			SessionID:       "sess-1",
			AgentSessionID:  existingAgentSessionID,
			TmuxSessionName: &existingTmuxName,
		}},
	}
	// fakeTmux's HasSession is implemented via the factory: tmux has-session
	// returns 0 (success) when the subcommand is allowed. Default factory
	// returns "true" so HasSession returns true.

	agentSessionID, err := h.lc.StartTmuxChat(ctx, "sess-1", "p", "T", HookOpts{})
	if err == nil {
		t.Fatal("expected AlreadyExists when a live tmux chat is present")
	}
	if got := grpcstatus.Code(err); got != codes.AlreadyExists {
		t.Errorf("error code = %s, want AlreadyExists", got)
	}
	if agentSessionID != existingAgentSessionID {
		t.Errorf("agentSessionID = %q, want existing %q", agentSessionID, existingAgentSessionID)
	}

	// No new row, no new tmux session.
	if len(h.chats.createCalls) != 0 {
		t.Errorf("expected 0 Create calls on AlreadyExists, got %d", len(h.chats.createCalls))
	}
	if h.tmuxFake.hasSubcommand("new-session") {
		t.Error("did not expect new tmux new-session when an alive row exists")
	}
	// The original row must NOT have been deleted.
	if len(h.chats.deletedAgentSessionIDs) != 0 {
		t.Errorf("expected 0 deletes on AlreadyExists, got %v", h.chats.deletedAgentSessionIDs)
	}
}

// TestStartTmuxChat_StaleTmux_PreservesRowAndStartsFresh verifies that an
// existing chat row whose tmux session has already exited is preserved as
// a historical record (its tmux_session_name is cleared so it no longer
// counts toward idempotency), while a fresh launch proceeds in parallel.
//
// Regression test for the repair-chat-visibility bug: previously the
// idempotency check deleted stale rows, which silently wiped historical
// repair chats every time the repair sweeper revisited a session. Now a
// stale row stays in the chat list (visible as a "stopped" historical
// chat) and only its tmux pointer is unlinked.
func TestStartTmuxChat_StaleTmux_PreservesRowAndStartsFresh(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)

	staleTmuxName := "boss-repo-stale123"
	staleAgentSessionID := "agent-stale-87654321"
	h.chats.chatsBySession = map[string][]*models.AgentChat{
		"sess-1": {{
			ID:              "chat-stale",
			SessionID:       "sess-1",
			AgentSessionID:  staleAgentSessionID,
			TmuxSessionName: &staleTmuxName,
		}},
	}
	// Make tmux report has-session=false for the stale name so the row is
	// classified as a completed historical run.
	h.tmuxFake.failSubcommand["has-session"] = true

	agentSessionID, err := h.lc.StartTmuxChat(ctx, "sess-1", "p", "T", HookOpts{})
	if err != nil {
		t.Fatalf("StartTmuxChat after stale row: %v", err)
	}
	if agentSessionID == "" {
		t.Fatal("expected fresh agentSessionID alongside the preserved stale row")
	}
	if agentSessionID == staleAgentSessionID {
		t.Errorf("expected fresh agentSessionID, got the stale one (%q)", staleAgentSessionID)
	}

	// The stale row must NOT be deleted — it stays as a historical record
	// in the chat list. Deleting was the original bug.
	if slices.Contains(h.chats.deletedAgentSessionIDs, staleAgentSessionID) {
		t.Errorf("expected stale row %q to be preserved, but it was deleted; deletes=%v",
			staleAgentSessionID, h.chats.deletedAgentSessionIDs)
	}

	// Instead, the stale row's tmux_session_name must have been cleared
	// (set to nil) so it no longer interferes with future idempotency
	// checks for this session.
	var clearedStale bool
	for _, upd := range h.chats.tmuxNameUpdates {
		if upd.agentSessionID == staleAgentSessionID && upd.name == nil {
			clearedStale = true
			break
		}
	}
	if !clearedStale {
		t.Errorf("expected UpdateTmuxSessionName(%q, nil) to clear the stale row, got updates=%+v",
			staleAgentSessionID, h.chats.tmuxNameUpdates)
	}

	// Fresh launch still produces a new row + tmux session in parallel.
	if len(h.chats.createCalls) != 1 {
		t.Errorf("expected 1 Create call alongside the preserved stale row, got %d", len(h.chats.createCalls))
	}
	if !h.tmuxFake.hasSubcommand("new-session") {
		t.Error("expected fresh tmux new-session alongside the preserved stale row")
	}
}

// TestStartTmuxChat_MissingAgentLogsDir verifies the fail-closed setter:
// an unconfigured agentLogsDir returns FailedPrecondition before any
// side effects.
func TestStartTmuxChat_MissingAgentLogsDir(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.lc.SetAgentLogsDir("") // explicitly clear

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", "p", "T", HookOpts{})
	if err == nil {
		t.Fatal("expected error when agentLogsDir is unset")
	}
	if got := grpcstatus.Code(err); got != codes.FailedPrecondition {
		t.Errorf("error code = %s, want FailedPrecondition", got)
	}
	if h.tmuxFake.hasSubcommand("new-session") {
		t.Error("did not expect tmux new-session when agentLogsDir unset")
	}
}

// TestStartTmuxChat_NoWorktreePath verifies that a session with an empty
// worktree path can't host a tmux chat — fail-closed FailedPrecondition.
func TestStartTmuxChat_NoWorktreePath(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.sessions.sessions["sess-1"].WorktreePath = ""

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", "p", "T", HookOpts{})
	if err == nil {
		t.Fatal("expected error when session has no worktree path")
	}
	if got := grpcstatus.Code(err); got != codes.FailedPrecondition {
		t.Errorf("error code = %s, want FailedPrecondition", got)
	}
}

// TestStartCronTmuxChat_WrapperPropagatesPlanAndCronTitle pins the wrapper
// contract: the cron entry point (startCronTmuxChat) must continue to call
// StartTmuxChat with prompt=session.Plan and title=`Run "<cron name>"`,
// regardless of how the underlying method evolves.
func TestStartCronTmuxChat_WrapperPropagatesPlanAndCronTitle(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)

	h.sessions.sessions["sess-1"].Plan = "Run the audit"
	h.sessions.sessions["sess-1"].Title = "Nightly audit"

	_, err := h.lc.startCronTmuxChat(ctx, "sess-1", StartSessionOpts{}, h.sessions.sessions["sess-1"], nil)
	if err != nil {
		t.Fatalf("startCronTmuxChat: %v", err)
	}

	if len(h.chats.createCalls) != 1 {
		t.Fatalf("expected 1 Create call, got %d", len(h.chats.createCalls))
	}
	if got, want := h.chats.createCalls[0].Title, `Run "Nightly audit"`; got != want {
		t.Errorf("Title = %q, want %q", got, want)
	}

	// load-buffer carries the plan content into tmux. Verify by reading
	// its stdin (the fake records args, but plan goes via stdin); we settle
	// for confirming load-buffer + paste-buffer + send-keys all ran.
	for _, sub := range []string{"load-buffer", "paste-buffer", "send-keys"} {
		if !h.tmuxFake.hasSubcommand(sub) {
			t.Errorf("expected tmux %s call (SendPlan), none recorded", sub)
		}
	}
}

// TestStartTmuxChat_HookOptsToken_ConfiguresRunKeyedHook verifies that a
// non-empty HookOpts.Token causes StartTmuxChat to call
// ConfigureFinalizeHook with the agent_session_id, the supplied token, and
// the lifecycle's recorded hook port. This is the run-keyed hook the
// repair plugin's StartChatRun relies on for its WaitChatRun signal.
func TestStartTmuxChat_HookOptsToken_ConfiguresRunKeyedHook(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.lc.SetHookPort(54321)

	const tok = "tok-run-12345"
	agentSessionID, err := h.lc.StartTmuxChat(ctx, "sess-1", "p", "T", HookOpts{Token: tok})
	if err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}

	// ConfigureFinalizeHook was called with run-keyed args.
	got := h.agentFake.LastConfigureHookReq
	if got == nil {
		t.Fatal("expected ConfigureFinalizeHook to be called when HookOpts.Token is non-empty")
	}
	if got.GetAgentSessionId() != agentSessionID {
		t.Errorf("AgentSessionId = %q, want %q", got.GetAgentSessionId(), agentSessionID)
	}
	if got.GetHookToken() != tok {
		t.Errorf("HookToken = %q, want %q", got.GetHookToken(), tok)
	}
	if got.GetHookPort() != 54321 {
		t.Errorf("HookPort = %d, want 54321", got.GetHookPort())
	}
	if got.GetSessionId() != "sess-1" {
		t.Errorf("SessionId = %q, want sess-1", got.GetSessionId())
	}
}

// TestStartTmuxChat_HookOptsEmpty_DoesNotConfigureHook verifies the cron
// path's invariant: when HookOpts is zero, ConfigureFinalizeHook is NOT
// called from StartTmuxChat (cron wires its session-keyed hook earlier).
func TestStartTmuxChat_HookOptsEmpty_DoesNotConfigureHook(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.lc.SetHookPort(12345)

	if _, err := h.lc.StartTmuxChat(ctx, "sess-1", "p", "T", HookOpts{}); err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}
	if h.agentFake.LastConfigureHookReq != nil {
		t.Errorf("ConfigureFinalizeHook should not be called when HookOpts is empty; got %+v", h.agentFake.LastConfigureHookReq)
	}
}

// TestStartTmuxChat_HookOptsTokenWithoutHookPort_FailsClosed verifies that
// a token without a configured hook port is rejected with FailedPrecondition
// and tears the live tmux session + chat row down so a retry can mint
// fresh state.
func TestStartTmuxChat_HookOptsTokenWithoutHookPort_FailsClosed(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	// Deliberately don't call SetHookPort.

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", "p", "T", HookOpts{Token: "tok"})
	if err == nil {
		t.Fatal("expected error when hook port unset and HookOpts.Token non-empty")
	}
	if got := grpcstatus.Code(err); got != codes.FailedPrecondition {
		t.Errorf("error code = %s, want FailedPrecondition", got)
	}
	if !h.tmuxFake.hasSubcommand("kill-session") {
		t.Error("expected tmux kill-session after hook port precondition failure")
	}
	if len(h.chats.deletedAgentSessionIDs) != 1 {
		t.Errorf("expected 1 row delete after hook port precondition failure, got %v", h.chats.deletedAgentSessionIDs)
	}
}

// TestStartTmuxChat_HookConfigureFails_TearsDown verifies that a
// ConfigureFinalizeHook RPC failure tears tmux down AND deletes the
// orphaned agent_chats row, mirroring the SendPlan cleanup path.
func TestStartTmuxChat_HookConfigureFails_TearsDown(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.lc.SetHookPort(12345)
	h.agentFake.ConfigureHookErr = fmt.Errorf("simulated hook config failure")

	_, err := h.lc.StartTmuxChat(ctx, "sess-1", "p", "T", HookOpts{Token: "tok"})
	if err == nil {
		t.Fatal("expected error when ConfigureFinalizeHook fails")
	}
	if !h.tmuxFake.hasSubcommand("kill-session") {
		t.Error("expected tmux kill-session after ConfigureFinalizeHook failure")
	}
	if len(h.chats.deletedAgentSessionIDs) != 1 {
		t.Errorf("expected 1 row delete after ConfigureFinalizeHook failure, got %v", h.chats.deletedAgentSessionIDs)
	}
	// SendPlan should NOT have run — failure happens before step 9.
	if h.tmuxFake.hasSubcommand("load-buffer") {
		t.Error("did not expect SendPlan (load-buffer) after hook configure failure")
	}
}

// TestStartTmuxChatArmsPollWhenHookUnsupported verifies the run-keyed
// path arms the poll fallback when the agent reports IsSupported=false
// (e.g. codex). Plugins that own a finalize hook (claude) skip it.
func TestStartTmuxChatArmsPollWhenHookUnsupported(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.lc.SetHookPort(54321)
	h.agentFake.IsSupported = false // hookless agent

	armer := &fakePollArmer{}
	h.lc.SetPollArmer(armer)
	h.lc.SetDaemonCtx(ctx)

	id, err := h.lc.StartTmuxChat(ctx, "sess-1", "the prompt", "the title", HookOpts{Token: "tok-2"})
	if err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}
	if !armer.armCalled {
		t.Error("poll fallback should be armed when ConfigureFinalizeHook reports IsSupported=false")
	}
	if armer.armedID != id {
		t.Errorf("armed agent_session_id = %q, want %q", armer.armedID, id)
	}
}

// TestStartTmuxChatDoesNotArmPollWhenHookSupported verifies the existing
// claude path does NOT trigger the poll fallback.
func TestStartTmuxChatDoesNotArmPollWhenHookSupported(t *testing.T) {
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.lc.SetHookPort(54321)
	// fakeAgent.IsSupported defaults to true.

	armer := &fakePollArmer{}
	h.lc.SetPollArmer(armer)
	h.lc.SetDaemonCtx(ctx)

	if _, err := h.lc.StartTmuxChat(ctx, "sess-1", "p", "T", HookOpts{Token: "tok-2"}); err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}
	if armer.armCalled {
		t.Error("poll fallback should NOT be armed when hook is supported")
	}
}

// emptyArgvAgent is an AgentRunnerClient whose BuildInteractiveCommand
// returns no argv at all — used to drive the empty-argv fail-closed path.
type emptyArgvAgent struct {
	fakeAgentForLifecycle
}

func (a *emptyArgvAgent) BuildInteractiveCommand(_ context.Context, _ *bossanovav1.BuildInteractiveCommandRequest) (*bossanovav1.BuildInteractiveCommandResponse, error) {
	return &bossanovav1.BuildInteractiveCommandResponse{}, nil
}

// Compile-time assertion that emptyArgvAgent still satisfies the interface
// — guards against the embedded fakeAgentForLifecycle's signature drifting.
var _ agent.AgentRunnerClient = (*emptyArgvAgent)(nil)
