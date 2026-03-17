package testharness_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/claude"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/testharness"
	"github.com/rs/zerolog"
)

// TestE2E_FullSessionLifecycle exercises the complete session lifecycle:
// register repo → create session → submit PR → checks pass → ready for review.
func TestE2E_FullSessionLifecycle(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	repoDir := testharness.TempRepoDir(t)

	// --- Step 1: Register a repo ---
	repoResp, err := h.Client.RegisterRepo(ctx, connect.NewRequest(&pb.RegisterRepoRequest{
		DisplayName:       "my-app",
		LocalPath:         repoDir,
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}))
	if err != nil {
		t.Fatalf("register repo: %v", err)
	}
	repoID := repoResp.Msg.Repo.Id
	if repoID == "" {
		t.Fatal("expected non-empty repo ID")
	}

	// --- Step 2: Create a session ---
	sessResp, err := h.Client.CreateSession(ctx, connect.NewRequest(&pb.CreateSessionRequest{
		RepoId: repoID,
		Title:  "Add user avatars",
		Plan:   "Add avatar upload to the user profile page",
	}))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	sess := sessResp.Msg.Session
	sessionID := sess.Id

	// Verify initial state.
	if sess.State != pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN {
		t.Fatalf("expected IMPLEMENTING_PLAN, got %v", sess.State)
	}
	if sess.ClaudeSessionId == nil || *sess.ClaudeSessionId == "" {
		t.Fatal("expected Claude session ID")
	}
	if sess.WorktreePath == "" {
		t.Fatal("expected worktree path")
	}
	if sess.BranchName == "" {
		t.Fatal("expected branch name")
	}

	// Verify mock calls.
	if len(h.Git.CreateCalls) != 1 {
		t.Fatalf("expected 1 worktree create, got %d", len(h.Git.CreateCalls))
	}
	if h.Git.CreateCalls[0].Title != "Add user avatars" {
		t.Fatalf("expected title in create opts, got %q", h.Git.CreateCalls[0].Title)
	}

	// --- Step 3: Simulate Claude output ---
	claudeID := *sess.ClaudeSessionId
	if err := h.Claude.EmitOutput(claudeID, claude.OutputLine{
		Text:      `{"type":"assistant","content":"Implementing avatar upload..."}`,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("emit output: %v", err)
	}

	// Verify output is in history.
	history := h.Claude.History(claudeID)
	if len(history) != 1 {
		t.Fatalf("expected 1 history line, got %d", len(history))
	}

	// --- Step 4: Transition to ImplementingPlan → PushingBranch → AwaitingChecks ---
	// We do this through the Lifecycle.SubmitPR which transitions through
	// PlanComplete → PushingBranch → BranchPushed → OpeningDraftPR → PROpened → AwaitingChecks.

	// First, the session must be in ImplementingPlan or GreenDraft for SubmitPR.
	// SubmitPR fires PlanComplete which requires ImplementingPlan state.
	if err := h.Lifecycle.SubmitPR(ctx, sessionID); err != nil {
		t.Fatalf("submit PR: %v", err)
	}

	// Verify push was called.
	if len(h.Git.PushCalls) != 1 {
		t.Fatalf("expected 1 push call, got %d", len(h.Git.PushCalls))
	}

	// Verify draft PR was created.
	if len(h.VCS.CreateDraftPRCalls) != 1 {
		t.Fatalf("expected 1 create draft PR call, got %d", len(h.VCS.CreateDraftPRCalls))
	}
	prOpts := h.VCS.CreateDraftPRCalls[0]
	if !prOpts.Draft {
		t.Fatal("expected draft PR")
	}
	if prOpts.Title != "Add user avatars" {
		t.Fatalf("expected PR title 'Add user avatars', got %q", prOpts.Title)
	}

	// Re-fetch session to verify state.
	getResp, err := h.Client.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: sessionID}))
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	sess = getResp.Msg.Session
	if sess.State != pb.SessionState_SESSION_STATE_AWAITING_CHECKS {
		t.Fatalf("expected AWAITING_CHECKS, got %v", sess.State)
	}
	if sess.PrNumber == nil {
		t.Fatal("expected PR number")
	}
	if sess.PrUrl == nil {
		t.Fatal("expected PR URL")
	}

	// --- Step 5: Simulate checks passing via dispatcher ---
	// Create a dispatcher to handle the ChecksPassed event.
	dispatcher := session.NewDispatcher(h.Sessions, h.Repos, h.VCS, zerolog.Nop())

	// Send ChecksPassed event through the dispatcher.
	events := make(chan session.SessionEvent, 1)
	events <- session.SessionEvent{
		SessionID: sessionID,
		Event:     vcs.ChecksPassed{PRID: int(*sess.PrNumber)},
	}
	close(events)

	dispCtx, dispCancel := context.WithTimeout(ctx, 5*time.Second)
	defer dispCancel()
	dispatcher.Run(dispCtx, events)

	// Re-fetch session — should now be ReadyForReview (GreenDraft → MarkReady → ReadyForReview).
	getResp, err = h.Client.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: sessionID}))
	if err != nil {
		t.Fatalf("get session after checks: %v", err)
	}
	sess = getResp.Msg.Session
	if sess.State != pb.SessionState_SESSION_STATE_READY_FOR_REVIEW {
		t.Fatalf("expected READY_FOR_REVIEW, got %v", sess.State)
	}

	// Verify mark-ready-for-review was called.
	if len(h.VCS.MarkReadyForReviewCalls) != 1 {
		t.Fatalf("expected 1 mark ready for review call, got %d", len(h.VCS.MarkReadyForReviewCalls))
	}

	// --- Step 6: Close the session ---
	closeResp, err := h.Client.CloseSession(ctx, connect.NewRequest(&pb.CloseSessionRequest{Id: sessionID}))
	if err != nil {
		t.Fatalf("close session: %v", err)
	}
	if closeResp.Msg.Session.State != pb.SessionState_SESSION_STATE_CLOSED {
		t.Fatalf("expected CLOSED, got %v", closeResp.Msg.Session.State)
	}
}

// TestE2E_ChecksFailedFixLoop exercises the fix loop path:
// create session → submit PR → checks fail → transition to FixingChecks → max attempts → blocked.
func TestE2E_ChecksFailedFixLoop(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	repoDir := testharness.TempRepoDir(t)

	// Register repo and create session.
	repoResp, err := h.Client.RegisterRepo(ctx, connect.NewRequest(&pb.RegisterRepoRequest{
		DisplayName:       "my-app",
		LocalPath:         repoDir,
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}))
	if err != nil {
		t.Fatalf("register repo: %v", err)
	}

	sessResp, err := h.Client.CreateSession(ctx, connect.NewRequest(&pb.CreateSessionRequest{
		RepoId: repoResp.Msg.Repo.Id,
		Title:  "Fix flaky test",
		Plan:   "Fix the flaky integration test",
	}))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	sessionID := sessResp.Msg.Session.Id

	// Submit PR to move to AwaitingChecks.
	if err := h.Lifecycle.SubmitPR(ctx, sessionID); err != nil {
		t.Fatalf("submit PR: %v", err)
	}

	// Fetch session to get PR number.
	getResp, err := h.Client.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: sessionID}))
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	prNum := int(*getResp.Msg.Session.PrNumber)

	// Send ChecksFailed — should transition to FixingChecks (attempt 1).
	dispatcher := session.NewDispatcher(h.Sessions, h.Repos, h.VCS, zerolog.Nop())

	failureConclusion := vcs.CheckConclusionFailure
	failedChecks := []vcs.CheckResult{
		{ID: "check-1", Name: "lint", Status: vcs.CheckStatusCompleted, Conclusion: &failureConclusion},
	}

	events := make(chan session.SessionEvent, 1)
	events <- session.SessionEvent{
		SessionID: sessionID,
		Event:     vcs.ChecksFailed{PRID: prNum, FailedChecks: failedChecks},
	}
	close(events)

	dispCtx, dispCancel := context.WithTimeout(ctx, 5*time.Second)
	defer dispCancel()
	dispatcher.Run(dispCtx, events)

	// Verify session is now in FixingChecks.
	getResp, err = h.Client.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: sessionID}))
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if getResp.Msg.Session.State != pb.SessionState_SESSION_STATE_FIXING_CHECKS {
		t.Fatalf("expected FIXING_CHECKS, got %v", getResp.Msg.Session.State)
	}
	if getResp.Msg.Session.AttemptCount != 1 {
		t.Fatalf("expected attempt count 1, got %d", getResp.Msg.Session.AttemptCount)
	}
}

// TestE2E_ArchiveAndResurrect exercises the archive/resurrect cycle.
func TestE2E_ArchiveAndResurrect(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	repoDir := testharness.TempRepoDir(t)

	// Register repo and create session.
	repoResp, err := h.Client.RegisterRepo(ctx, connect.NewRequest(&pb.RegisterRepoRequest{
		DisplayName:       "my-app",
		LocalPath:         repoDir,
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}))
	if err != nil {
		t.Fatalf("register repo: %v", err)
	}

	sessResp, err := h.Client.CreateSession(ctx, connect.NewRequest(&pb.CreateSessionRequest{
		RepoId: repoResp.Msg.Repo.Id,
		Title:  "Refactor auth",
		Plan:   "Refactor the auth module",
	}))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	sessionID := sessResp.Msg.Session.Id

	// Archive the session.
	archiveResp, err := h.Client.ArchiveSession(ctx, connect.NewRequest(&pb.ArchiveSessionRequest{Id: sessionID}))
	if err != nil {
		t.Fatalf("archive session: %v", err)
	}
	if archiveResp.Msg.Session.ArchivedAt == nil {
		t.Fatal("expected ArchivedAt to be set")
	}

	// Verify worktree was archived.
	if len(h.Git.ArchiveCalls) != 1 {
		t.Fatalf("expected 1 archive call, got %d", len(h.Git.ArchiveCalls))
	}

	// Verify session no longer appears in active list.
	listResp, err := h.Client.ListSessions(ctx, connect.NewRequest(&pb.ListSessionsRequest{}))
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(listResp.Msg.Sessions) != 0 {
		t.Fatalf("expected 0 active sessions, got %d", len(listResp.Msg.Sessions))
	}

	// Resurrect the session.
	resResp, err := h.Client.ResurrectSession(ctx, connect.NewRequest(&pb.ResurrectSessionRequest{Id: sessionID}))
	if err != nil {
		t.Fatalf("resurrect session: %v", err)
	}
	if resResp.Msg.Session.ArchivedAt != nil {
		t.Fatal("expected ArchivedAt to be nil after resurrection")
	}
	if resResp.Msg.Session.State != pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN {
		t.Fatalf("expected IMPLEMENTING_PLAN after resurrect, got %v", resResp.Msg.Session.State)
	}

	// Verify worktree was resurrected.
	if len(h.Git.ResurrectCalls) != 1 {
		t.Fatalf("expected 1 resurrect call, got %d", len(h.Git.ResurrectCalls))
	}

	// Verify a new Claude process was started (2 total: original + resurrect).
	if resResp.Msg.Session.ClaudeSessionId == nil {
		t.Fatal("expected new Claude session ID")
	}
}

// TestE2E_ListSessionsWithStateFilter tests listing sessions filtered by state.
func TestE2E_ListSessionsWithStateFilter(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	repoDir := testharness.TempRepoDir(t)

	// Register repo.
	repoResp, err := h.Client.RegisterRepo(ctx, connect.NewRequest(&pb.RegisterRepoRequest{
		DisplayName:       "my-app",
		LocalPath:         repoDir,
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}))
	if err != nil {
		t.Fatalf("register repo: %v", err)
	}
	repoID := repoResp.Msg.Repo.Id

	// Create two sessions.
	_, err = h.Client.CreateSession(ctx, connect.NewRequest(&pb.CreateSessionRequest{
		RepoId: repoID, Title: "Session A", Plan: "Plan A",
	}))
	if err != nil {
		t.Fatalf("create session A: %v", err)
	}

	sessB, err := h.Client.CreateSession(ctx, connect.NewRequest(&pb.CreateSessionRequest{
		RepoId: repoID, Title: "Session B", Plan: "Plan B",
	}))
	if err != nil {
		t.Fatalf("create session B: %v", err)
	}

	// Close session B.
	_, err = h.Client.CloseSession(ctx, connect.NewRequest(&pb.CloseSessionRequest{Id: sessB.Msg.Session.Id}))
	if err != nil {
		t.Fatalf("close session B: %v", err)
	}

	// List only ImplementingPlan sessions.
	listResp, err := h.Client.ListSessions(ctx, connect.NewRequest(&pb.ListSessionsRequest{
		RepoId: &repoID,
		States: []pb.SessionState{pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN},
	}))
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(listResp.Msg.Sessions) != 1 {
		t.Fatalf("expected 1 implementing session, got %d", len(listResp.Msg.Sessions))
	}
	if listResp.Msg.Sessions[0].Title != "Session A" {
		t.Fatalf("expected Session A, got %q", listResp.Msg.Sessions[0].Title)
	}

	// List including archived shows both.
	allResp, err := h.Client.ListSessions(ctx, connect.NewRequest(&pb.ListSessionsRequest{
		RepoId:          &repoID,
		IncludeArchived: true,
	}))
	if err != nil {
		t.Fatalf("list all sessions: %v", err)
	}
	if len(allResp.Msg.Sessions) != 2 {
		t.Fatalf("expected 2 total sessions, got %d", len(allResp.Msg.Sessions))
	}
}

// TestE2E_PRMergedTransition verifies that PRMerged event transitions to Merged state.
func TestE2E_PRMergedTransition(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	repoDir := testharness.TempRepoDir(t)

	// Set up repo + session + submit PR.
	repoResp, err := h.Client.RegisterRepo(ctx, connect.NewRequest(&pb.RegisterRepoRequest{
		DisplayName:       "my-app",
		LocalPath:         repoDir,
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}))
	if err != nil {
		t.Fatalf("register repo: %v", err)
	}

	sessResp, err := h.Client.CreateSession(ctx, connect.NewRequest(&pb.CreateSessionRequest{
		RepoId: repoResp.Msg.Repo.Id,
		Title:  "Add feature X",
		Plan:   "Implement feature X",
	}))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	sessionID := sessResp.Msg.Session.Id

	if err := h.Lifecycle.SubmitPR(ctx, sessionID); err != nil {
		t.Fatalf("submit PR: %v", err)
	}

	// Transition through checks passed first (AwaitingChecks → GreenDraft → ReadyForReview).
	getResp, err := h.Client.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: sessionID}))
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	prNum := int(*getResp.Msg.Session.PrNumber)

	dispatcher := session.NewDispatcher(h.Sessions, h.Repos, h.VCS, zerolog.Nop())

	// Checks passed.
	events := make(chan session.SessionEvent, 1)
	events <- session.SessionEvent{SessionID: sessionID, Event: vcs.ChecksPassed{PRID: prNum}}
	close(events)
	dispCtx, dispCancel := context.WithTimeout(ctx, 5*time.Second)
	dispatcher.Run(dispCtx, events)
	dispCancel()

	// Now simulate PR merged.
	events2 := make(chan session.SessionEvent, 1)
	events2 <- session.SessionEvent{SessionID: sessionID, Event: vcs.PRMerged{PRID: prNum}}
	close(events2)
	dispCtx2, dispCancel2 := context.WithTimeout(ctx, 5*time.Second)
	dispatcher.Run(dispCtx2, events2)
	dispCancel2()

	// Verify merged state.
	getResp, err = h.Client.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: sessionID}))
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if getResp.Msg.Session.State != pb.SessionState_SESSION_STATE_MERGED {
		t.Fatalf("expected MERGED, got %v", getResp.Msg.Session.State)
	}
}

// TestE2E_ChatTrackingLifecycle exercises RecordChat, ListChats, and UpdateChatTitle.
func TestE2E_ChatTrackingLifecycle(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	repoDir := testharness.TempRepoDir(t)

	// Register repo and create session.
	repoResp, err := h.Client.RegisterRepo(ctx, connect.NewRequest(&pb.RegisterRepoRequest{
		DisplayName:       "chat-app",
		LocalPath:         repoDir,
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}))
	if err != nil {
		t.Fatalf("register repo: %v", err)
	}

	sessResp, err := h.Client.CreateSession(ctx, connect.NewRequest(&pb.CreateSessionRequest{
		RepoId: repoResp.Msg.Repo.Id,
		Title:  "Chat tracking test",
		Plan:   "Test chat tracking",
	}))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	sessionID := sessResp.Msg.Session.Id

	// Record a chat.
	chatResp, err := h.Client.RecordChat(ctx, connect.NewRequest(&pb.RecordChatRequest{
		SessionId: sessionID,
		ClaudeId:  "claude-chat-001",
		Title:     "First chat",
	}))
	if err != nil {
		t.Fatalf("record chat: %v", err)
	}
	if chatResp.Msg.Chat.ClaudeId != "claude-chat-001" {
		t.Fatalf("expected claude_id = %q, got %q", "claude-chat-001", chatResp.Msg.Chat.ClaudeId)
	}
	if chatResp.Msg.Chat.Title != "First chat" {
		t.Fatalf("expected title = %q, got %q", "First chat", chatResp.Msg.Chat.Title)
	}

	// Record a second chat.
	_, err = h.Client.RecordChat(ctx, connect.NewRequest(&pb.RecordChatRequest{
		SessionId: sessionID,
		ClaudeId:  "claude-chat-002",
		Title:     "Second chat",
	}))
	if err != nil {
		t.Fatalf("record second chat: %v", err)
	}

	// List chats.
	listResp, err := h.Client.ListChats(ctx, connect.NewRequest(&pb.ListChatsRequest{
		SessionId: sessionID,
	}))
	if err != nil {
		t.Fatalf("list chats: %v", err)
	}
	if len(listResp.Msg.Chats) != 2 {
		t.Fatalf("expected 2 chats, got %d", len(listResp.Msg.Chats))
	}

	// Update chat title by claude_id.
	_, err = h.Client.UpdateChatTitle(ctx, connect.NewRequest(&pb.UpdateChatTitleRequest{
		ClaudeId: "claude-chat-001",
		Title:    "Updated first chat",
	}))
	if err != nil {
		t.Fatalf("update chat title: %v", err)
	}

	// Verify updated title.
	listResp, err = h.Client.ListChats(ctx, connect.NewRequest(&pb.ListChatsRequest{
		SessionId: sessionID,
	}))
	if err != nil {
		t.Fatalf("list chats after update: %v", err)
	}
	found := false
	for _, c := range listResp.Msg.Chats {
		if c.ClaudeId == "claude-chat-001" && c.Title == "Updated first chat" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected to find chat with updated title")
	}
}

// TestE2E_RecordChat_ValidationErrors tests validation for RecordChat.
func TestE2E_RecordChat_ValidationErrors(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()

	t.Run("missing session_id", func(t *testing.T) {
		_, err := h.Client.RecordChat(ctx, connect.NewRequest(&pb.RecordChatRequest{
			ClaudeId: "claude-001",
		}))
		if err == nil {
			t.Fatal("expected error for missing session_id")
		}
	})

	t.Run("missing claude_id", func(t *testing.T) {
		_, err := h.Client.RecordChat(ctx, connect.NewRequest(&pb.RecordChatRequest{
			SessionId: "sess-1",
		}))
		if err == nil {
			t.Fatal("expected error for missing claude_id")
		}
	})
}

// TestE2E_ValidateRepoPath tests repo path validation.
func TestE2E_ValidateRepoPath(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()

	t.Run("empty path", func(t *testing.T) {
		resp, err := h.Client.ValidateRepoPath(ctx, connect.NewRequest(&pb.ValidateRepoPathRequest{}))
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		if resp.Msg.IsValid {
			t.Error("expected IsValid=false for empty path")
		}
		if resp.Msg.ErrorMessage == "" {
			t.Error("expected error message")
		}
	})

	t.Run("nonexistent path", func(t *testing.T) {
		resp, err := h.Client.ValidateRepoPath(ctx, connect.NewRequest(&pb.ValidateRepoPathRequest{
			LocalPath: "/nonexistent/path/that/does/not/exist",
		}))
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		if resp.Msg.IsValid {
			t.Error("expected IsValid=false for nonexistent path")
		}
	})

	t.Run("valid path", func(t *testing.T) {
		repoDir := testharness.TempRepoDir(t)
		resp, err := h.Client.ValidateRepoPath(ctx, connect.NewRequest(&pb.ValidateRepoPathRequest{
			LocalPath: repoDir,
		}))
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		// The mock always returns IsGitRepo=true for any dir.
		if !resp.Msg.IsValid {
			t.Errorf("expected IsValid=true, error=%q", resp.Msg.ErrorMessage)
		}
	})
}
