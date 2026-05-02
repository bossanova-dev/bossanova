package testharness_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/claude"
	gitpkg "github.com/recurser/bossd/internal/git"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/taskorchestrator"
	"github.com/recurser/bossd/internal/testharness"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// createSessionFromStream is a test helper that opens a CreateSession stream,
// drains it, and returns the final Session.
func createSessionFromStream(t *testing.T, client bossanovav1connect.DaemonServiceClient, ctx context.Context, req *pb.CreateSessionRequest) *pb.Session {
	t.Helper()
	stream, err := client.CreateSession(ctx, connect.NewRequest(req))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	defer stream.Close() //nolint:errcheck // test cleanup

	var sess *pb.Session
	for stream.Receive() {
		msg := stream.Msg()
		if sc := msg.GetSessionCreated(); sc != nil {
			sess = sc.GetSession()
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("create session stream error: %v", err)
	}
	if sess == nil {
		t.Fatal("expected SessionCreated in stream")
	}
	return sess
}

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

	// Enable auto-merge so the dispatcher will mark PRs ready for review.
	autoMerge := true
	if _, err := h.Client.UpdateRepo(ctx, connect.NewRequest(&pb.UpdateRepoRequest{
		Id:           repoID,
		CanAutoMerge: &autoMerge,
	})); err != nil {
		t.Fatalf("update repo: %v", err)
	}

	// --- Step 2: Create a session ---
	sess := createSessionFromStream(t, h.Client, ctx, &pb.CreateSessionRequest{
		RepoId: repoID,
		Title:  "Add user avatars",
		Plan:   "Add avatar upload to the user profile page",
	})
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

	// Verify push was called twice: once by createDraftPR during
	// StartSession (the placeholder empty commit so the PR can be opened)
	// and once by SubmitPR (to push any implementation commits that
	// landed on top of the placeholder).
	if len(h.Git.PushCalls) != 2 {
		t.Fatalf("expected 2 push calls, got %d", len(h.Git.PushCalls))
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
	dispatcher := session.NewDispatcher(h.Sessions, h.Repos, h.VCS, nil, zerolog.Nop())

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

	sess := createSessionFromStream(t, h.Client, ctx, &pb.CreateSessionRequest{
		RepoId: repoResp.Msg.Repo.Id,
		Title:  "Fix flaky test",
		Plan:   "Fix the flaky integration test",
	})
	sessionID := sess.Id

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
	dispatcher := session.NewDispatcher(h.Sessions, h.Repos, h.VCS, nil, zerolog.Nop())

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

	sess := createSessionFromStream(t, h.Client, ctx, &pb.CreateSessionRequest{
		RepoId: repoResp.Msg.Repo.Id,
		Title:  "Refactor auth",
		Plan:   "Refactor the auth module",
	})
	sessionID := sess.Id

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
	createSessionFromStream(t, h.Client, ctx, &pb.CreateSessionRequest{
		RepoId: repoID, Title: "Session A", Plan: "Plan A",
	})

	sessB := createSessionFromStream(t, h.Client, ctx, &pb.CreateSessionRequest{
		RepoId: repoID, Title: "Session B", Plan: "Plan B",
	})

	// Close session B.
	_, err = h.Client.CloseSession(ctx, connect.NewRequest(&pb.CloseSessionRequest{Id: sessB.Id}))
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

	// Enable auto-merge so the dispatcher will mark PRs ready for review.
	autoMerge := true
	if _, err := h.Client.UpdateRepo(ctx, connect.NewRequest(&pb.UpdateRepoRequest{
		Id:           repoResp.Msg.Repo.Id,
		CanAutoMerge: &autoMerge,
	})); err != nil {
		t.Fatalf("update repo: %v", err)
	}

	sess := createSessionFromStream(t, h.Client, ctx, &pb.CreateSessionRequest{
		RepoId: repoResp.Msg.Repo.Id,
		Title:  "Add feature X",
		Plan:   "Implement feature X",
	})
	sessionID := sess.Id

	if err := h.Lifecycle.SubmitPR(ctx, sessionID); err != nil {
		t.Fatalf("submit PR: %v", err)
	}

	// Transition through checks passed first (AwaitingChecks → GreenDraft → ReadyForReview).
	getResp, err := h.Client.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: sessionID}))
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	prNum := int(*getResp.Msg.Session.PrNumber)

	dispatcher := session.NewDispatcher(h.Sessions, h.Repos, h.VCS, nil, zerolog.Nop())

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

// TestE2E_ConflictDetectedTransition verifies that a ConflictDetected event
// fired against a session in AwaitingChecks transitions it into FixingChecks
// (under max attempts), increments the attempt counter, and surfaces the
// MERGE_CONFLICT_UNRESOLVABLE attention reason when the repo is not configured
// to auto-resolve conflicts.
func TestE2E_ConflictDetectedTransition(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	repoDir := testharness.TempRepoDir(t)

	repoResp, err := h.Client.RegisterRepo(ctx, connect.NewRequest(&pb.RegisterRepoRequest{
		DisplayName:       "my-app",
		LocalPath:         repoDir,
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}))
	if err != nil {
		t.Fatalf("register repo: %v", err)
	}

	// can_auto_resolve_conflicts defaults to true at the DB level. Disable it
	// so the FixingChecks state surfaces the MERGE_CONFLICT_UNRESOLVABLE
	// attention reason (see vcs.ComputeAttentionStatus).
	autoResolve := false
	if _, err := h.Client.UpdateRepo(ctx, connect.NewRequest(&pb.UpdateRepoRequest{
		Id:                      repoResp.Msg.Repo.Id,
		CanAutoResolveConflicts: &autoResolve,
	})); err != nil {
		t.Fatalf("update repo: %v", err)
	}

	sess := createSessionFromStream(t, h.Client, ctx, &pb.CreateSessionRequest{
		RepoId: repoResp.Msg.Repo.Id,
		Title:  "Add feature with conflict",
		Plan:   "Add a feature that will conflict",
	})
	sessionID := sess.Id

	if err := h.Lifecycle.SubmitPR(ctx, sessionID); err != nil {
		t.Fatalf("submit PR: %v", err)
	}

	getResp, err := h.Client.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: sessionID}))
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if getResp.Msg.Session.State != pb.SessionState_SESSION_STATE_AWAITING_CHECKS {
		t.Fatalf("expected AWAITING_CHECKS before event, got %v", getResp.Msg.Session.State)
	}
	prNum := int(*getResp.Msg.Session.PrNumber)

	dispatcher := session.NewDispatcher(h.Sessions, h.Repos, h.VCS, nil, zerolog.Nop())
	events := make(chan session.SessionEvent, 1)
	events <- session.SessionEvent{SessionID: sessionID, Event: vcs.ConflictDetected{PRID: prNum}}
	close(events)

	dispCtx, dispCancel := context.WithTimeout(ctx, 5*time.Second)
	defer dispCancel()
	dispatcher.Run(dispCtx, events)

	getResp, err = h.Client.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: sessionID}))
	if err != nil {
		t.Fatalf("get session after conflict: %v", err)
	}
	if getResp.Msg.Session.State != pb.SessionState_SESSION_STATE_FIXING_CHECKS {
		t.Fatalf("expected FIXING_CHECKS, got %v", getResp.Msg.Session.State)
	}
	if getResp.Msg.Session.AttemptCount != 1 {
		t.Fatalf("expected attempt count 1, got %d", getResp.Msg.Session.AttemptCount)
	}

	// With CanAutoResolveConflicts disabled above, FixingChecks surfaces the
	// MERGE_CONFLICT_UNRESOLVABLE attention reason.
	att := getResp.Msg.Session.AttentionStatus
	if att == nil || !att.NeedsAttention {
		t.Fatalf("expected NeedsAttention=true, got %+v", att)
	}
	if att.Reason != pb.AttentionReason_ATTENTION_REASON_MERGE_CONFLICT_UNRESOLVABLE {
		t.Fatalf("expected reason MERGE_CONFLICT_UNRESOLVABLE, got %v", att.Reason)
	}
}

// TestE2E_ReviewSubmittedTransition verifies that a ReviewSubmitted event
// fired against a session in ReadyForReview transitions it into FixingChecks
// (under max attempts) and increments the attempt counter.
func TestE2E_ReviewSubmittedTransition(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	repoDir := testharness.TempRepoDir(t)

	repoResp, err := h.Client.RegisterRepo(ctx, connect.NewRequest(&pb.RegisterRepoRequest{
		DisplayName:       "my-app",
		LocalPath:         repoDir,
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}))
	if err != nil {
		t.Fatalf("register repo: %v", err)
	}

	// Enable auto-merge so ChecksPassed transitions through GreenDraft to
	// ReadyForReview, where ReviewSubmitted is permitted.
	autoMerge := true
	if _, err := h.Client.UpdateRepo(ctx, connect.NewRequest(&pb.UpdateRepoRequest{
		Id:           repoResp.Msg.Repo.Id,
		CanAutoMerge: &autoMerge,
	})); err != nil {
		t.Fatalf("update repo: %v", err)
	}

	sess := createSessionFromStream(t, h.Client, ctx, &pb.CreateSessionRequest{
		RepoId: repoResp.Msg.Repo.Id,
		Title:  "Add feature for review",
		Plan:   "Implement and request review",
	})
	sessionID := sess.Id

	if err := h.Lifecycle.SubmitPR(ctx, sessionID); err != nil {
		t.Fatalf("submit PR: %v", err)
	}

	getResp, err := h.Client.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: sessionID}))
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	prNum := int(*getResp.Msg.Session.PrNumber)

	dispatcher := session.NewDispatcher(h.Sessions, h.Repos, h.VCS, nil, zerolog.Nop())

	// Drive ChecksPassed first so the session lands in ReadyForReview.
	checksEvents := make(chan session.SessionEvent, 1)
	checksEvents <- session.SessionEvent{SessionID: sessionID, Event: vcs.ChecksPassed{PRID: prNum}}
	close(checksEvents)
	dispCtx1, dispCancel1 := context.WithTimeout(ctx, 5*time.Second)
	dispatcher.Run(dispCtx1, checksEvents)
	dispCancel1()

	getResp, err = h.Client.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: sessionID}))
	if err != nil {
		t.Fatalf("get session after checks passed: %v", err)
	}
	if getResp.Msg.Session.State != pb.SessionState_SESSION_STATE_READY_FOR_REVIEW {
		t.Fatalf("expected READY_FOR_REVIEW before review event, got %v", getResp.Msg.Session.State)
	}

	// Now fire ReviewSubmitted with one comment.
	reviewEvents := make(chan session.SessionEvent, 1)
	reviewEvents <- session.SessionEvent{
		SessionID: sessionID,
		Event: vcs.ReviewSubmitted{
			PRID: prNum,
			Comments: []vcs.ReviewComment{
				{Author: "reviewer", Body: "please rename this", State: vcs.ReviewStateChangesRequested},
			},
		},
	}
	close(reviewEvents)
	dispCtx2, dispCancel2 := context.WithTimeout(ctx, 5*time.Second)
	dispatcher.Run(dispCtx2, reviewEvents)
	dispCancel2()

	getResp, err = h.Client.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: sessionID}))
	if err != nil {
		t.Fatalf("get session after review: %v", err)
	}
	if getResp.Msg.Session.State != pb.SessionState_SESSION_STATE_FIXING_CHECKS {
		t.Fatalf("expected FIXING_CHECKS after review, got %v", getResp.Msg.Session.State)
	}
	if getResp.Msg.Session.AttemptCount != 1 {
		t.Fatalf("expected attempt count 1 after review, got %d", getResp.Msg.Session.AttemptCount)
	}
}

// TestE2E_PRClosedTransition verifies that a PRClosed event transitions the
// session into the terminal Closed state, mirroring TestE2E_PRMergedTransition.
func TestE2E_PRClosedTransition(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	repoDir := testharness.TempRepoDir(t)

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

	sess := createSessionFromStream(t, h.Client, ctx, &pb.CreateSessionRequest{
		RepoId: repoID,
		Title:  "Abandoned feature",
		Plan:   "Implement, then close PR without merging",
	})
	sessionID := sess.Id

	if err := h.Lifecycle.SubmitPR(ctx, sessionID); err != nil {
		t.Fatalf("submit PR: %v", err)
	}

	getResp, err := h.Client.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: sessionID}))
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	prNum := int(*getResp.Msg.Session.PrNumber)

	dispatcher := session.NewDispatcher(h.Sessions, h.Repos, h.VCS, nil, zerolog.Nop())
	events := make(chan session.SessionEvent, 1)
	events <- session.SessionEvent{SessionID: sessionID, Event: vcs.PRClosed{PRID: prNum}}
	close(events)
	dispCtx, dispCancel := context.WithTimeout(ctx, 5*time.Second)
	defer dispCancel()
	dispatcher.Run(dispCtx, events)

	getResp, err = h.Client.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: sessionID}))
	if err != nil {
		t.Fatalf("get session after pr closed: %v", err)
	}
	if getResp.Msg.Session.State != pb.SessionState_SESSION_STATE_CLOSED {
		t.Fatalf("expected CLOSED, got %v", getResp.Msg.Session.State)
	}

	// ListSessions filtered to CLOSED should include the session.
	closedResp, err := h.Client.ListSessions(ctx, connect.NewRequest(&pb.ListSessionsRequest{
		RepoId: &repoID,
		States: []pb.SessionState{pb.SessionState_SESSION_STATE_CLOSED},
	}))
	if err != nil {
		t.Fatalf("list closed sessions: %v", err)
	}
	if len(closedResp.Msg.Sessions) != 1 || closedResp.Msg.Sessions[0].Id != sessionID {
		t.Fatalf("expected closed session in CLOSED-filtered list, got %v", closedResp.Msg.Sessions)
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

	sess := createSessionFromStream(t, h.Client, ctx, &pb.CreateSessionRequest{
		RepoId: repoResp.Msg.Repo.Id,
		Title:  "Chat tracking test",
		Plan:   "Test chat tracking",
	})
	sessionID := sess.Id

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

// getSessionState fetches the current state of a session. Used by
// TestE2E_SessionControlRPCs subtests for before/after assertions.
func getSessionState(t *testing.T, h *testharness.Harness, ctx context.Context, sessionID string) pb.SessionState {
	t.Helper()
	resp, err := h.Client.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: sessionID}))
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	return resp.Msg.Session.State
}

// registerOpt is a functional option for registerTestRepo.
type registerOpt func(*registerOpts)

type registerOpts struct {
	worktreeBaseDir string
}

// withWorktreeBaseDir overrides the WorktreeBaseDir used by registerTestRepo.
// When not set, the default "/tmp/worktrees" is used.
func withWorktreeBaseDir(dir string) registerOpt {
	return func(o *registerOpts) {
		o.worktreeBaseDir = dir
	}
}

// registerTestRepo is a helper that registers a repo with default test values
// and returns the repo ID. Used by TestE2E_SessionControlRPCs subtests and
// cron E2E tests. Pass withWorktreeBaseDir to override the worktree base dir.
func registerTestRepo(t *testing.T, h *testharness.Harness, ctx context.Context, opts ...registerOpt) string {
	t.Helper()
	o := &registerOpts{worktreeBaseDir: "/tmp/worktrees"}
	for _, opt := range opts {
		opt(o)
	}
	repoDir := testharness.TempRepoDir(t)
	resp, err := h.Client.RegisterRepo(ctx, connect.NewRequest(&pb.RegisterRepoRequest{
		DisplayName:       "my-app",
		LocalPath:         repoDir,
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   o.worktreeBaseDir,
	}))
	if err != nil {
		t.Fatalf("register repo: %v", err)
	}
	return resp.Msg.Repo.Id
}

// TestE2E_SessionControlRPCs exercises the session-control RPCs (Stop, Pause,
// Resume, Retry, Close, Merge) and asserts their current behavior — including
// the parts that remain intentionally partial pending full state-machine
// integration. Each subtest uses a fresh Harness so side-effects (stopped
// Claude sessions, merge calls) don't bleed between cases.
func TestE2E_SessionControlRPCs(t *testing.T) {
	t.Run("StopSession stops Claude and sets state to CLOSED", func(t *testing.T) {
		h := testharness.New(t)
		ctx := context.Background()
		repoID := registerTestRepo(t, h, ctx)

		sessionID, _ := h.SeedSessionInState(t, ctx, repoID,
			pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN, "Stop me", "stop plan")

		getResp, err := h.Client.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: sessionID}))
		if err != nil {
			t.Fatalf("get session: %v", err)
		}
		claudeID := *getResp.Msg.Session.ClaudeSessionId

		if _, err := h.Client.StopSession(ctx, connect.NewRequest(&pb.StopSessionRequest{Id: sessionID})); err != nil {
			t.Fatalf("stop session: %v", err)
		}

		// Claude runner must have received a Stop call for this session's
		// Claude ID.
		found := false
		for _, id := range h.Claude.Stopped {
			if id == claudeID {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected Claude session %q in Stopped slice, got %v", claudeID, h.Claude.Stopped)
		}

		// Session should now be CLOSED (lifecycle.StopSession transitions it).
		getResp, err = h.Client.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: sessionID}))
		if err != nil {
			t.Fatalf("get session after stop: %v", err)
		}
		if got := getResp.Msg.Session.State; got != pb.SessionState_SESSION_STATE_CLOSED {
			t.Fatalf("expected CLOSED after stop, got %v", got)
		}
	})

	t.Run("RetrySession clears blocked_reason and re-enables automation", func(t *testing.T) {
		h := testharness.New(t)
		ctx := context.Background()
		repoID := registerTestRepo(t, h, ctx)

		sessionID, _ := h.SeedSessionInState(t, ctx, repoID,
			pb.SessionState_SESSION_STATE_BLOCKED, "Retry me", "retry plan")

		// Baseline: after SeedSessionInState(BLOCKED), state machine set
		// blocked_reason and (per Blocked.OnEntry actions) may have
		// disabled automation — verify we're actually blocked first.
		getResp, err := h.Client.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: sessionID}))
		if err != nil {
			t.Fatalf("get session: %v", err)
		}
		if getResp.Msg.Session.State != pb.SessionState_SESSION_STATE_BLOCKED {
			t.Fatalf("expected BLOCKED before retry, got %v", getResp.Msg.Session.State)
		}

		// Retry.
		retryResp, err := h.Client.RetrySession(ctx, connect.NewRequest(&pb.RetrySessionRequest{Id: sessionID}))
		if err != nil {
			t.Fatalf("retry session: %v", err)
		}
		if retryResp.Msg.Session.BlockedReason != nil && *retryResp.Msg.Session.BlockedReason != "" {
			t.Fatalf("expected BlockedReason cleared after retry, got %q", *retryResp.Msg.Session.BlockedReason)
		}
		if !retryResp.Msg.Session.AutomationEnabled {
			t.Fatal("expected AutomationEnabled=true after retry")
		}

		// Neither Claude nor VCS should have been touched.
		if len(h.Claude.Stopped) != 0 {
			t.Fatalf("retry should not stop Claude, got %v", h.Claude.Stopped)
		}
		if len(h.VCS.MergePRCalls) != 0 {
			t.Fatalf("retry should not merge, got %v", h.VCS.MergePRCalls)
		}
	})

	t.Run("CloseSession sets state to CLOSED and does not merge", func(t *testing.T) {
		h := testharness.New(t)
		ctx := context.Background()
		repoID := registerTestRepo(t, h, ctx)

		// Close from ImplementingPlan — a session with no PR. This also
		// pins that CloseSession never dispatches MergePR.
		sessionID, _ := h.SeedSessionInState(t, ctx, repoID,
			pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN, "Close me", "close plan")

		closeResp, err := h.Client.CloseSession(ctx, connect.NewRequest(&pb.CloseSessionRequest{Id: sessionID}))
		if err != nil {
			t.Fatalf("close session: %v", err)
		}
		if closeResp.Msg.Session.State != pb.SessionState_SESSION_STATE_CLOSED {
			t.Fatalf("expected CLOSED, got %v", closeResp.Msg.Session.State)
		}
		if len(h.VCS.MergePRCalls) != 0 {
			t.Fatalf("close must not call MergePR, got %v", h.VCS.MergePRCalls)
		}
	})

	// MergeSession happy path — parameterized across merge strategies so the
	// test covers that the server forwards repo.MergeStrategy verbatim to
	// provider.MergePR.
	mergeStrategies := []models.MergeStrategy{
		models.MergeStrategyMerge,
		models.MergeStrategyRebase,
		models.MergeStrategySquash,
	}
	for _, strategy := range mergeStrategies {
		strategy := strategy
		t.Run("MergeSession happy path strategy="+string(strategy), func(t *testing.T) {
			h := testharness.New(t)
			ctx := context.Background()
			repoID := registerTestRepo(t, h, ctx)

			// Land the session in ReadyForReview so the PR has been
			// opened and checks have passed.
			autoMerge := true
			if _, err := h.Client.UpdateRepo(ctx, connect.NewRequest(&pb.UpdateRepoRequest{
				Id:           repoID,
				CanAutoMerge: &autoMerge,
			})); err != nil {
				t.Fatalf("update repo (auto-merge): %v", err)
			}
			strategyStr := string(strategy)
			if _, err := h.Client.UpdateRepo(ctx, connect.NewRequest(&pb.UpdateRepoRequest{
				Id:            repoID,
				MergeStrategy: &strategyStr,
			})); err != nil {
				t.Fatalf("update repo (merge strategy): %v", err)
			}

			sessionID, prNum := h.SeedSessionInState(t, ctx, repoID,
				pb.SessionState_SESSION_STATE_READY_FOR_REVIEW,
				"Merge via "+strategyStr, "merge plan")

			// Mark the PR as passing in the display tracker so the
			// MergeSession "PR not passing" guard lets the call through.
			h.DisplayTracker.Set(sessionID, vcs.DisplayInfo{Status: vcs.DisplayStatusPassing})

			if _, err := h.Client.MergeSession(ctx, connect.NewRequest(&pb.MergeSessionRequest{Id: sessionID})); err != nil {
				t.Fatalf("merge session: %v", err)
			}

			if got := len(h.VCS.MergePRCalls); got != 1 {
				t.Fatalf("expected 1 MergePR call, got %d", got)
			}
			call := h.VCS.MergePRCalls[0]
			if call.PRID != prNum {
				t.Fatalf("expected PRID=%d, got %d", prNum, call.PRID)
			}
			if call.Strategy != strategyStr {
				t.Fatalf("expected strategy=%q, got %q", strategyStr, call.Strategy)
			}
		})
	}

	t.Run("MergeSession rejects when PR is not passing", func(t *testing.T) {
		h := testharness.New(t)
		ctx := context.Background()
		repoID := registerTestRepo(t, h, ctx)

		autoMerge := true
		if _, err := h.Client.UpdateRepo(ctx, connect.NewRequest(&pb.UpdateRepoRequest{
			Id:           repoID,
			CanAutoMerge: &autoMerge,
		})); err != nil {
			t.Fatalf("update repo: %v", err)
		}

		sessionID, _ := h.SeedSessionInState(t, ctx, repoID,
			pb.SessionState_SESSION_STATE_READY_FOR_REVIEW, "Merge rejected", "merge plan")

		// Signal the display tracker that the PR is failing, which
		// trips the guard in MergeSession.
		h.DisplayTracker.Set(sessionID, vcs.DisplayInfo{Status: vcs.DisplayStatusFailing, HasFailures: true})

		stateBefore := getSessionState(t, h, ctx, sessionID)

		_, err := h.Client.MergeSession(ctx, connect.NewRequest(&pb.MergeSessionRequest{Id: sessionID}))
		if err == nil {
			t.Fatal("expected MergeSession to fail when PR is not passing")
		}
		if code := connect.CodeOf(err); code != connect.CodeFailedPrecondition {
			t.Fatalf("expected FailedPrecondition, got %v (%v)", code, err)
		}

		if len(h.VCS.MergePRCalls) != 0 {
			t.Fatalf("expected no MergePR calls on failure, got %v", h.VCS.MergePRCalls)
		}
		if got := getSessionState(t, h, ctx, sessionID); got != stateBefore {
			t.Fatalf("merge failure changed state: %v -> %v", stateBefore, got)
		}
	})

	// MergeSession local-only path: session has no PR number because the
	// draft-PR creation failed (e.g. local-only repo with no GitHub remote).
	// StartSession logs a warning and proceeds, so the session exists with
	// a branch but PRNumber == nil. The RPC must route to MergeLocalBranch
	// and NOT call gh pr merge. Pins the new branch added to MergeSession.
	t.Run("MergeSession local-only path when session has no PR", func(t *testing.T) {
		h := testharness.New(t)
		ctx := context.Background()
		repoID := registerTestRepo(t, h, ctx)

		// Make draft-PR creation fail so StartSession leaves PRNumber=nil.
		// StartSession's PR creation is already best-effort (warning only),
		// so this matches the real local-only-repo failure mode.
		h.VCS.SetCreatePRError(fmt.Errorf("no origin configured"))

		sessionID, _ := h.SeedSessionInState(t, ctx, repoID,
			pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
			"Local-only session", "local plan")

		// Sanity: session really has no PR, but does have a branch.
		sessResp, err := h.Client.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: sessionID}))
		if err != nil {
			t.Fatalf("get session: %v", err)
		}
		if sessResp.Msg.Session.PrNumber != nil {
			t.Fatalf("precondition: expected no PR number, got %d", *sessResp.Msg.Session.PrNumber)
		}
		if sessResp.Msg.Session.BranchName == "" {
			t.Fatalf("precondition: expected a branch name, got empty")
		}

		// Baseline the call count so we only count merge-time calls, not
		// the draft-PR attempt that fired during StartSession.
		prCallsBefore := len(h.VCS.MergePRCalls)

		if _, err := h.Client.MergeSession(ctx, connect.NewRequest(&pb.MergeSessionRequest{Id: sessionID})); err != nil {
			t.Fatalf("local merge: %v", err)
		}

		if got := len(h.Git.MergeLocalBranchCalls); got != 1 {
			t.Fatalf("expected 1 MergeLocalBranch call, got %d", got)
		}
		call := h.Git.MergeLocalBranchCalls[0]
		if call.Head != sessResp.Msg.Session.BranchName {
			t.Errorf("MergeLocalBranch head=%q, want %q", call.Head, sessResp.Msg.Session.BranchName)
		}
		if got := len(h.VCS.MergePRCalls); got != prCallsBefore {
			t.Errorf("MergePR must not run in local-only path, got %d new calls", got-prCallsBefore)
		}
	})

	// MergeSession verification failure — the madverts-core regression
	// case. gh reports the PR as merged with a specific commit, but our
	// post-merge `merge-base --is-ancestor` check says that commit isn't
	// on origin/<base>. The RPC must surface the failure rather than
	// silently mark the merge successful.
	t.Run("MergeSession detects merge commit missing from origin/<base>", func(t *testing.T) {
		h := testharness.New(t)
		ctx := context.Background()
		repoID := registerTestRepo(t, h, ctx)

		autoMerge := true
		if _, err := h.Client.UpdateRepo(ctx, connect.NewRequest(&pb.UpdateRepoRequest{
			Id:           repoID,
			CanAutoMerge: &autoMerge,
		})); err != nil {
			t.Fatalf("update repo: %v", err)
		}

		sessionID, prNum := h.SeedSessionInState(t, ctx, repoID,
			pb.SessionState_SESSION_STATE_READY_FOR_REVIEW,
			"Merge-commit-orphan regression", "plan")

		h.DisplayTracker.Set(sessionID, vcs.DisplayInfo{Status: vcs.DisplayStatusPassing})
		// Simulate the madverts scenario: gh says merged with a commit
		// that isn't on origin/main — e.g. because something force-pushed
		// origin/main to remove the merge commit.
		h.VCS.SetPRMergeCommit(prNum, "76b35392orphaned")
		h.Git.IsAncestorFn = func(_ context.Context, _, _, _ string) (bool, error) { return false, nil }

		_, err := h.Client.MergeSession(ctx, connect.NewRequest(&pb.MergeSessionRequest{Id: sessionID}))
		if err == nil {
			t.Fatal("expected MergeSession to error when merge commit is not on origin/<base>")
		}
		if code := connect.CodeOf(err); code != connect.CodeInternal {
			t.Errorf("want CodeInternal, got %v (%v)", code, err)
		}
		if !strings.Contains(err.Error(), "verification failed") {
			t.Errorf("err message should mention verification; got %v", err)
		}
		// The verification failure must NOT trigger a local base-branch
		// sync — there is nothing good to fast-forward to.
		// (gh already called MergePR once, which is expected.)
		if got := len(h.VCS.MergePRCalls); got != 1 {
			t.Errorf("expected exactly 1 MergePR call, got %d", got)
		}
	})

	t.Run("PauseSession disables automation and ResumeSession re-enables it", func(t *testing.T) {
		h := testharness.New(t)
		ctx := context.Background()
		repoID := registerTestRepo(t, h, ctx)

		sessionID, _ := h.SeedSessionInState(t, ctx, repoID,
			pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN, "Pause me", "pause plan")

		// Baseline: automation starts enabled.
		getResp, err := h.Client.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: sessionID}))
		if err != nil {
			t.Fatalf("get session: %v", err)
		}
		if !getResp.Msg.Session.AutomationEnabled {
			t.Fatal("expected AutomationEnabled=true before pause")
		}
		startState := getResp.Msg.Session.State

		// Pause.
		pauseResp, err := h.Client.PauseSession(ctx, connect.NewRequest(&pb.PauseSessionRequest{Id: sessionID}))
		if err != nil {
			t.Fatalf("pause session: %v", err)
		}
		if pauseResp.Msg.Session.AutomationEnabled {
			t.Fatal("expected AutomationEnabled=false after pause")
		}
		if pauseResp.Msg.Session.State != startState {
			t.Fatalf("pause changed state: %v -> %v", startState, pauseResp.Msg.Session.State)
		}

		// Resume.
		resumeResp, err := h.Client.ResumeSession(ctx, connect.NewRequest(&pb.ResumeSessionRequest{Id: sessionID}))
		if err != nil {
			t.Fatalf("resume session: %v", err)
		}
		if !resumeResp.Msg.Session.AutomationEnabled {
			t.Fatal("expected AutomationEnabled=true after resume")
		}
		if resumeResp.Msg.Session.State != startState {
			t.Fatalf("resume changed state: %v -> %v", startState, resumeResp.Msg.Session.State)
		}

		// Pause/Resume must not touch the Claude runner.
		if len(h.Claude.Stopped) != 0 {
			t.Fatalf("pause/resume should not stop Claude, got %v", h.Claude.Stopped)
		}
	})
}

// TestE2E_AttachSession_StreamsEvents verifies that the server-streaming
// AttachSession RPC delivers the initial StateChange, replays the Claude
// output ring-buffer to a freshly-attached client, and terminates with a
// SessionEnded when the Claude subscription channel closes.
//
// Synchronization: the mock's SubscribedCh hook lets the test wait until the
// server has finished sending history and has called Subscribe. Only then do
// we Stop Claude, which closes the subscribe channel and drives the handler
// to SessionEnded. No fixed sleeps — this is the predicate-based drain the
// flight plan calls for.
func TestE2E_AttachSession_StreamsEvents(t *testing.T) {
	h := testharness.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	repoID := registerTestRepo(t, h, ctx)
	sessionID, _ := h.SeedSessionInState(t, ctx, repoID,
		pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN, "Stream me", "stream plan")

	getResp, err := h.Client.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: sessionID}))
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	claudeID := *getResp.Msg.Session.ClaudeSessionId

	// Arm the subscribe hook before attaching so the first (only) call
	// from the AttachSession handler is captured.
	h.Claude.SubscribedCh = make(chan string, 1)

	// Pre-seed the mock's ring buffer. The server's AttachSession handler
	// flushes history before subscribing, so these three lines are
	// guaranteed to appear in the initial burst.
	for i := 0; i < 3; i++ {
		if err := h.Claude.EmitOutputLine(claudeID, fmt.Sprintf("line %d", i+1)); err != nil {
			t.Fatalf("emit line %d: %v", i+1, err)
		}
	}

	type drainResult struct {
		msgs []*pb.AttachSessionResponse
		err  error
	}
	resCh := make(chan drainResult, 1)
	go func() {
		msgs, err := h.AttachAndDrain(ctx, sessionID, func(r *pb.AttachSessionResponse) bool {
			return r.GetSessionEnded() != nil
		})
		resCh <- drainResult{msgs: msgs, err: err}
	}()

	// Wait for the server to subscribe — history has been streamed to
	// the client by this point because Subscribe is called after the
	// history-burst loop in server.AttachSession.
	select {
	case <-h.Claude.SubscribedCh:
	case <-ctx.Done():
		t.Fatalf("timed out waiting for server Subscribe: %v", ctx.Err())
	}

	// Closing the subscribe channel drives the handler's for-range to
	// exit; the handler then sends SessionEnded and returns.
	if err := h.Claude.Stop(claudeID); err != nil {
		t.Fatalf("stop claude: %v", err)
	}

	var res drainResult
	select {
	case res = <-resCh:
	case <-ctx.Done():
		t.Fatalf("timed out waiting for drain: %v", ctx.Err())
	}
	if res.err != nil {
		t.Fatalf("drain error: %v", res.err)
	}

	var stateChanges, outputLines int
	var gotEnded bool
	for i, msg := range res.msgs {
		switch {
		case msg.GetStateChange() != nil:
			stateChanges++
		case msg.GetOutputLine() != nil:
			outputLines++
		case msg.GetSessionEnded() != nil:
			gotEnded = true
			if i != len(res.msgs)-1 {
				t.Errorf("SessionEnded at index %d of %d; expected it to be last", i, len(res.msgs))
			}
		}
	}
	if stateChanges < 1 {
		t.Errorf("expected ≥1 StateChange, got %d (msgs=%d)", stateChanges, len(res.msgs))
	}
	if outputLines < 3 {
		t.Errorf("expected ≥3 OutputLine, got %d (msgs=%d)", outputLines, len(res.msgs))
	}
	if !gotEnded {
		t.Error("expected SessionEnded terminator")
	}
}

// TestE2E_CreateSession_QuickChat verifies that QuickChat=true skips all
// side-effects that a normal session triggers: no worktree is created, no
// branch is pushed, no draft PR is opened. The session lands in
// ImplementingPlan pointing at the repo's base directory with an empty
// branch name.
func TestE2E_CreateSession_QuickChat(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	repoDir := testharness.TempRepoDir(t)

	repoResp, err := h.Client.RegisterRepo(ctx, connect.NewRequest(&pb.RegisterRepoRequest{
		DisplayName:       "quick-chat-app",
		LocalPath:         repoDir,
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}))
	if err != nil {
		t.Fatalf("register repo: %v", err)
	}
	repoID := repoResp.Msg.Repo.Id

	sess := createSessionFromStream(t, h.Client, ctx, &pb.CreateSessionRequest{
		RepoId:    repoID,
		Title:     "Ask a quick question",
		Plan:      "",
		QuickChat: true,
	})

	if len(h.Git.CreateCalls) != 0 {
		t.Fatalf("quick chat must not create a worktree, got %d calls: %+v",
			len(h.Git.CreateCalls), h.Git.CreateCalls)
	}
	if len(h.Git.PushCalls) != 0 {
		t.Fatalf("quick chat must not push a branch, got %d calls: %+v",
			len(h.Git.PushCalls), h.Git.PushCalls)
	}
	if len(h.VCS.CreateDraftPRCalls) != 0 {
		t.Fatalf("quick chat must not open a draft PR, got %d calls: %+v",
			len(h.VCS.CreateDraftPRCalls), h.VCS.CreateDraftPRCalls)
	}

	if sess.BranchName != "" {
		t.Errorf("quick chat session has BranchName=%q; expected empty", sess.BranchName)
	}
	if sess.WorktreePath != repoDir {
		t.Errorf("quick chat WorktreePath=%q; expected repo base %q", sess.WorktreePath, repoDir)
	}
	// Quick chat starts Claude on-demand at attach time, not at create, so
	// ClaudeSessionId is unset on the created session.
	if sess.ClaudeSessionId != nil && *sess.ClaudeSessionId != "" {
		t.Errorf("quick chat should defer Claude start; got ClaudeSessionId=%q", *sess.ClaudeSessionId)
	}
	if sess.State != pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN {
		t.Errorf("expected IMPLEMENTING_PLAN, got %v", sess.State)
	}
}

// TestE2E_CreateSession_ForceBranch verifies that the ForceBranch flag on
// CreateSession propagates all the way through lifecycle.StartSession to
// worktrees.Create as CreateOpts.Force. The server exposes no direct
// mechanism to simulate a pre-existing branch in the mock, but the flag
// propagation path is what the state-machine depends on: it's exactly
// that flag the real worktree manager uses to decide whether to delete
// the existing branch before creating the worktree.
//
// The negative subtest pins the server's AlreadyExists mapping: when the
// worktree manager reports ErrBranchExists and ForceBranch is not set,
// the server must surface connect.CodeAlreadyExists and delete the
// orphaned session record.
func TestE2E_CreateSession_ForceBranch(t *testing.T) {
	t.Run("ForceBranch=true propagates Force to CreateOpts", func(t *testing.T) {
		h := testharness.New(t)
		ctx := context.Background()
		repoDir := testharness.TempRepoDir(t)

		repoResp, err := h.Client.RegisterRepo(ctx, connect.NewRequest(&pb.RegisterRepoRequest{
			DisplayName:       "force-branch-app",
			LocalPath:         repoDir,
			DefaultBaseBranch: "main",
			WorktreeBaseDir:   "/tmp/worktrees",
		}))
		if err != nil {
			t.Fatalf("register repo: %v", err)
		}

		sess := createSessionFromStream(t, h.Client, ctx, &pb.CreateSessionRequest{
			RepoId:      repoResp.Msg.Repo.Id,
			Title:       "Retry with force",
			Plan:        "Retry plan",
			ForceBranch: true,
		})
		if sess.State != pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN {
			t.Fatalf("expected IMPLEMENTING_PLAN, got %v", sess.State)
		}

		if len(h.Git.CreateCalls) != 1 {
			t.Fatalf("expected 1 Create call, got %d: %+v",
				len(h.Git.CreateCalls), h.Git.CreateCalls)
		}
		if !h.Git.CreateCalls[0].Force {
			t.Errorf("expected CreateOpts.Force=true when ForceBranch=true, got false")
		}
	})

	t.Run("ForceBranch=false + branch collision returns AlreadyExists", func(t *testing.T) {
		h := testharness.New(t)
		ctx := context.Background()
		repoDir := testharness.TempRepoDir(t)

		repoResp, err := h.Client.RegisterRepo(ctx, connect.NewRequest(&pb.RegisterRepoRequest{
			DisplayName:       "collision-app",
			LocalPath:         repoDir,
			DefaultBaseBranch: "main",
			WorktreeBaseDir:   "/tmp/worktrees",
		}))
		if err != nil {
			t.Fatalf("register repo: %v", err)
		}
		repoID := repoResp.Msg.Repo.Id

		// Stage the first Create to fail as though the branch already
		// exists remotely; subsequent calls succeed. This mirrors the
		// real git manager's behavior without actually touching the
		// filesystem.
		h.Git.CreateFunc = func(_ context.Context, opts gitpkg.CreateOpts) (*gitpkg.CreateResult, error) {
			return nil, gitpkg.ErrBranchExists
		}

		// CreateSession is server-streaming — the initial call only
		// returns an error if the RPC itself fails to open. Errors from
		// the lifecycle surface through stream.Err() after draining.
		stream, err := h.Client.CreateSession(ctx, connect.NewRequest(&pb.CreateSessionRequest{
			RepoId:      repoID,
			Title:       "Colliding branch",
			Plan:        "Should fail",
			ForceBranch: false,
		}))
		if err != nil {
			t.Fatalf("unexpected stream-open error: %v", err)
		}
		defer stream.Close() //nolint:errcheck // test cleanup
		for stream.Receive() {
			// drain; no SessionCreated should arrive on the failure path
		}
		streamErr := stream.Err()
		if streamErr == nil {
			t.Fatal("expected stream error when branch collides and ForceBranch=false")
		}
		if code := connect.CodeOf(streamErr); code != connect.CodeAlreadyExists {
			t.Fatalf("expected CodeAlreadyExists, got %v (%v)", code, streamErr)
		}

		// The orphaned session record should have been deleted (see
		// server.CreateSession cleanup path). ListSessions should
		// return zero rows for this repo.
		listResp, err := h.Client.ListSessions(ctx, connect.NewRequest(&pb.ListSessionsRequest{
			RepoId: &repoID,
		}))
		if err != nil {
			t.Fatalf("list sessions: %v", err)
		}
		if len(listResp.Msg.Sessions) != 0 {
			t.Errorf("expected orphaned session to be deleted, got %d sessions", len(listResp.Msg.Sessions))
		}
	})
}

// TestE2E_CreateSession_AttachExistingPR verifies that when CreateSession is
// called with PrNumber set, the server checks out the existing PR's head
// branch via CreateFromExistingBranch (not Create), does NOT push a new
// branch, and does NOT open a new draft PR.
func TestE2E_CreateSession_AttachExistingPR(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	repoDir := testharness.TempRepoDir(t)

	repoResp, err := h.Client.RegisterRepo(ctx, connect.NewRequest(&pb.RegisterRepoRequest{
		DisplayName:       "attach-pr-app",
		LocalPath:         repoDir,
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}))
	if err != nil {
		t.Fatalf("register repo: %v", err)
	}
	repoID := repoResp.Msg.Repo.Id

	// Seed MockVCS so it can report the PR's head branch when the server
	// asks for PR 42's status, and so the listing surface (ListOpenPRs)
	// also shows the PR — mirroring a realistic GitHub API response.
	h.VCS.OpenPRs = []vcs.PRSummary{{
		Number:     42,
		Title:      "Existing PR",
		HeadBranch: "feature/x",
		State:      vcs.PRStateOpen,
		Author:     "collaborator",
	}}
	mergeable := true
	h.VCS.SetPRStatus(42, &vcs.PRStatus{
		State:      vcs.PRStateOpen,
		Mergeable:  &mergeable,
		HeadBranch: "feature/x",
		BaseBranch: "main",
	})

	prNumber := int32(42)
	sess := createSessionFromStream(t, h.Client, ctx, &pb.CreateSessionRequest{
		RepoId:   repoID,
		Title:    "Continue work on existing PR",
		PrNumber: &prNumber,
	})

	// Existing-PR path: worktree is created from the PR head branch, not
	// a fresh one. Assert exactly one CreateFromExistingBranch call with
	// BranchName="feature/x"; zero Create calls.
	if len(h.Git.CreateFromExistingBranchCalls) != 1 {
		t.Fatalf("expected 1 CreateFromExistingBranch call, got %d: %+v",
			len(h.Git.CreateFromExistingBranchCalls), h.Git.CreateFromExistingBranchCalls)
	}
	if got := h.Git.CreateFromExistingBranchCalls[0].BranchName; got != "feature/x" {
		t.Errorf("expected CreateFromExistingBranch BranchName='feature/x', got %q", got)
	}
	if len(h.Git.CreateCalls) != 0 {
		t.Errorf("expected no Create calls when attaching to existing PR, got %d: %+v",
			len(h.Git.CreateCalls), h.Git.CreateCalls)
	}

	// No fresh branch push: the PR already exists on the remote and
	// StartSession only pushes when creating a new draft PR on empty PRs.
	if len(h.Git.PushCalls) != 0 {
		t.Errorf("expected no branch pushes when attaching to existing PR, got %d: %+v",
			len(h.Git.PushCalls), h.Git.PushCalls)
	}

	// No new draft PR: the lifecycle's createDraftPR is only invoked when
	// session.PRNumber is nil.
	if len(h.VCS.CreateDraftPRCalls) != 0 {
		t.Errorf("expected no CreateDraftPR calls, got %d: %+v",
			len(h.VCS.CreateDraftPRCalls), h.VCS.CreateDraftPRCalls)
	}

	// Session should reflect the PR: branch copied from CreateResult
	// (which echoed the opts), PrNumber carried through.
	if sess.BranchName != "feature/x" {
		t.Errorf("expected BranchName='feature/x', got %q", sess.BranchName)
	}
	if sess.PrNumber == nil || *sess.PrNumber != 42 {
		t.Errorf("expected PrNumber=42, got %v", sess.PrNumber)
	}
	if sess.State != pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN {
		t.Errorf("expected IMPLEMENTING_PLAN, got %v", sess.State)
	}
}

// TestE2E_EmptyTrash_WithAgeFilter pins the older_than filter on EmptyTrash.
// Three sessions are archived and backdated to 30d / 10d / 1d ago; EmptyTrash
// with a 7-day threshold should delete the 30d + 10d records (and their
// branches) while leaving the 1d session intact. Straddling the boundary
// (10d > 7d, 1d < 7d) confirms the filter is actually applied — without it,
// all three would be swept.
func TestE2E_EmptyTrash_WithAgeFilter(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	repoDir := testharness.TempRepoDir(t)

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

	// Seed three sessions. Titles are already hyphen/lowercase so the mock's
	// sanitize() yields branch names identical to the titles — easy to assert
	// against in EmptyTrashCalls.
	type seeded struct {
		id     string
		branch string
	}
	mkSession := func(title string) seeded {
		sess := createSessionFromStream(t, h.Client, ctx, &pb.CreateSessionRequest{
			RepoId: repoID,
			Title:  title,
			Plan:   "Plan for " + title,
		})
		if sess.BranchName == "" {
			t.Fatalf("session %q: expected branch name, got empty", title)
		}
		return seeded{id: sess.Id, branch: sess.BranchName}
	}
	old := mkSession("old-30d")
	mid := mkSession("mid-10d")
	recent := mkSession("recent-1d")

	// Archive all three, then backdate via the test-only SQL helper.
	now := time.Now().UTC()
	for _, s := range []seeded{old, mid, recent} {
		if _, err := h.Client.ArchiveSession(ctx, connect.NewRequest(&pb.ArchiveSessionRequest{Id: s.id})); err != nil {
			t.Fatalf("archive %s: %v", s.id, err)
		}
	}
	h.SetArchivedAt(t, old.id, now.Add(-30*24*time.Hour))
	h.SetArchivedAt(t, mid.id, now.Add(-10*24*time.Hour))
	h.SetArchivedAt(t, recent.id, now.Add(-1*24*time.Hour))

	// Clear the EmptyTrashCalls slice so the post-EmptyTrash assertion only
	// sees calls from the RPC under test (ArchiveSession does not call
	// EmptyTrash today, but guard against future coupling).
	h.Git.EmptyTrashCalls = nil

	// Threshold: 7 days ago. After-semantics means anything archived at or
	// before this timestamp is deleted; anything newer is kept.
	threshold := now.Add(-7 * 24 * time.Hour)
	resp, err := h.Client.EmptyTrash(ctx, connect.NewRequest(&pb.EmptyTrashRequest{
		OlderThan: timestamppb.New(threshold),
	}))
	if err != nil {
		t.Fatalf("empty trash: %v", err)
	}
	if resp.Msg.DeletedCount != 2 {
		t.Fatalf("expected DeletedCount=2 (old+mid), got %d", resp.Msg.DeletedCount)
	}

	// Only the 1d session should remain when listing archived.
	listResp, err := h.Client.ListSessions(ctx, connect.NewRequest(&pb.ListSessionsRequest{
		RepoId:          &repoID,
		IncludeArchived: true,
	}))
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(listResp.Msg.Sessions) != 1 {
		t.Fatalf("expected 1 archived session remaining, got %d: %+v",
			len(listResp.Msg.Sessions), listResp.Msg.Sessions)
	}
	if listResp.Msg.Sessions[0].Id != recent.id {
		t.Fatalf("expected recent session %q to remain, got %q",
			recent.id, listResp.Msg.Sessions[0].Id)
	}

	// Git cleanup: server groups branches per repo, so we expect one
	// EmptyTrash call containing old + mid (order matches ListArchived's
	// created_at DESC, but we don't want to couple to that — check as a set).
	if len(h.Git.EmptyTrashCalls) != 1 {
		t.Fatalf("expected 1 git EmptyTrash call, got %d: %+v",
			len(h.Git.EmptyTrashCalls), h.Git.EmptyTrashCalls)
	}
	call := h.Git.EmptyTrashCalls[0]
	if len(call.Branches) != 2 {
		t.Fatalf("expected 2 branches in EmptyTrash call, got %d: %+v",
			len(call.Branches), call.Branches)
	}
	branches := map[string]bool{call.Branches[0]: true, call.Branches[1]: true}
	if !branches[old.branch] || !branches[mid.branch] {
		t.Fatalf("expected branches to include %q and %q, got %+v",
			old.branch, mid.branch, call.Branches)
	}
	if branches[recent.branch] {
		t.Fatalf("recent session's branch %q must NOT appear in EmptyTrash call", recent.branch)
	}
}

// TestE2E_PluginSession_DeferPRDefaultsFalse is an FL3-5 regression gate for
// outside-voice concern #7: plugin-initiated sessions must *not* accidentally
// defer PR creation. Dependabot, Linear, and repair plugins all flow through
// taskorchestrator.SessionCreator — the same path we're exercising here —
// and rely on the zero-valued DeferPR to keep existing behavior.
//
// The test drives the plugin path explicitly by constructing a SessionCreator
// against the harness's lifecycle + mock VCS, invoking it with the subset of
// CreateSessionOpts a plugin would produce (no DeferPR, no CronJobID), and
// asserting a draft PR was created up front.
func TestE2E_PluginSession_DeferPRDefaultsFalse(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	repoDir := testharness.TempRepoDir(t)

	repoResp, err := h.Client.RegisterRepo(ctx, connect.NewRequest(&pb.RegisterRepoRequest{
		DisplayName:       "dependabot-repo",
		LocalPath:         repoDir,
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}))
	if err != nil {
		t.Fatalf("register repo: %v", err)
	}
	repoID := repoResp.Msg.Repo.Id

	creator := taskorchestrator.NewSessionCreator(h.Sessions, h.Lifecycle, zerolog.Nop())

	// Simulate exactly what orchestrator.handleCreateSession builds for a
	// plugin-discovered task: no DeferPR, no CronJobID, no HookToken.
	sess, err := creator.CreateSession(ctx, taskorchestrator.CreateSessionOpts{
		RepoID:     repoID,
		Title:      "Bump lodash",
		Plan:       "Fix the CI failure after upgrading lodash",
		BaseBranch: "main",
		HeadBranch: "dependabot/npm/lodash-4.17.21",
	})
	if err != nil {
		t.Fatalf("plugin-path create session: %v", err)
	}

	// Up-front draft PR must have fired — if it didn't, DeferPR leaked from
	// the cron path into the plugin path.
	if len(h.VCS.CreateDraftPRCalls) != 1 {
		t.Fatalf("expected 1 CreateDraftPR call via plugin path, got %d", len(h.VCS.CreateDraftPRCalls))
	}
	if !h.VCS.CreateDraftPRCalls[0].Draft {
		t.Error("expected draft=true for plugin-initiated PR")
	}

	// CronJobID must remain nil — plugin-initiated sessions must never
	// acquire a cron linkage by accident.
	if sess.CronJobID != nil {
		t.Errorf("expected CronJobID to be nil for plugin session, got %q", *sess.CronJobID)
	}
	if sess.PRNumber == nil {
		t.Error("expected PRNumber to be populated after plugin-path session creation")
	}
}
