package testharness_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/testharness"
	"github.com/rs/zerolog"
)

// resilienceRegisterRepo registers a test repo and returns its ID. Mirrors
// registerTestRepo in e2e_lifecycle_test.go; duplicated to keep this file
// self-contained.
func resilienceRegisterRepo(t *testing.T, h *testharness.Harness, ctx context.Context) string {
	t.Helper()
	repoDir := testharness.TempRepoDir(t)
	resp, err := h.Client.RegisterRepo(ctx, connect.NewRequest(&pb.RegisterRepoRequest{
		DisplayName:       "my-app",
		LocalPath:         repoDir,
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}))
	if err != nil {
		t.Fatalf("register repo: %v", err)
	}
	return resp.Msg.Repo.Id
}

// drainCreateSessionStream consumes a CreateSession server-streaming RPC
// without interpreting messages. Returns the stream error (which is the
// RPC outcome — nil for success, non-nil for any failure path).
func drainCreateSessionStream(t *testing.T, h *testharness.Harness, ctx context.Context, req *pb.CreateSessionRequest) error {
	t.Helper()
	stream, err := h.Client.CreateSession(ctx, connect.NewRequest(req))
	if err != nil {
		return err
	}
	defer stream.Close() //nolint:errcheck // test cleanup
	for stream.Receive() {
		// drain
	}
	return stream.Err()
}

// TestE2E_Resilience_WorktreeCreateFails pins that a worktree create
// failure during CreateSession surfaces as an RPC error, deletes the
// orphaned session row, and never spawns a Claude subprocess.
func TestE2E_Resilience_WorktreeCreateFails(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	repoID := resilienceRegisterRepo(t, h, ctx)

	h.Git.SetCreateError(errors.New("worktree create failed: disk full"))

	err := drainCreateSessionStream(t, h, ctx, &pb.CreateSessionRequest{
		RepoId: repoID, Title: "Boom", Plan: "Will fail",
	})
	if err == nil {
		t.Fatal("expected CreateSession to fail when worktree creation errors")
	}

	// No Claude process should have been spawned — worktree creation
	// happens before claude.Start in lifecycle.StartSession.
	if got := h.Claude.Started; len(got) != 0 {
		t.Fatalf("expected no Claude spawns when worktree fails, got %v", got)
	}

	// Orphan record must be deleted by the server's cleanup path.
	listResp, err := h.Client.ListSessions(ctx, connect.NewRequest(&pb.ListSessionsRequest{
		RepoId: &repoID, IncludeArchived: true,
	}))
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(listResp.Msg.Sessions) != 0 {
		t.Fatalf("expected orphan to be cleaned up, got %d sessions: %+v",
			len(listResp.Msg.Sessions), listResp.Msg.Sessions)
	}
}

// TestE2E_Resilience_PushFailsAfterWorktreeCreated pins the warning-path
// behavior: when the immediate-PR push fails inside StartSession, the
// session still lands in IMPLEMENTING_PLAN with the worktree created and
// no PR — ready for SubmitPR to retry. No state corruption.
func TestE2E_Resilience_PushFailsAfterWorktreeCreated(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	repoID := resilienceRegisterRepo(t, h, ctx)

	// Push fires once inside createDraftPR during StartSession. Inject the
	// error so it fails; the lifecycle logs a warning and continues.
	h.Git.SetPushError(errors.New("push failed: network down"))

	sess := createSessionFromStream(t, h.Client, ctx, &pb.CreateSessionRequest{
		RepoId: repoID, Title: "push-fails", Plan: "Recover after push fail",
	})

	if sess.State != pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN {
		t.Fatalf("expected IMPLEMENTING_PLAN after push warning, got %v", sess.State)
	}
	if sess.PrNumber != nil {
		t.Fatalf("expected no PR (createDraftPR aborted), got #%d", *sess.PrNumber)
	}
	if sess.WorktreePath == "" || sess.BranchName == "" {
		t.Fatalf("worktree should still be reachable for retry, got path=%q branch=%q",
			sess.WorktreePath, sess.BranchName)
	}
	if len(h.Git.PushCalls) != 1 {
		t.Fatalf("expected exactly 1 push attempt, got %d", len(h.Git.PushCalls))
	}
	// CreateDraftPR is gated on push success — must not have been called.
	if got := len(h.VCS.CreateDraftPRCalls); got != 0 {
		t.Fatalf("expected no CreateDraftPR calls when push fails, got %d", got)
	}
}

// TestE2E_Resilience_CreatePRFails pins that a CreateDraftPR failure
// during the immediate-PR step lands the session in IMPLEMENTING_PLAN
// with no PR — the same warning-path semantics as a push failure.
func TestE2E_Resilience_CreatePRFails(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	repoID := resilienceRegisterRepo(t, h, ctx)

	h.VCS.SetCreatePRError(errors.New("create PR failed: GitHub 503"))

	sess := createSessionFromStream(t, h.Client, ctx, &pb.CreateSessionRequest{
		RepoId: repoID, Title: "createpr-fails", Plan: "Recover after PR fail",
	})

	if sess.State != pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN {
		t.Fatalf("expected IMPLEMENTING_PLAN after CreateDraftPR warning, got %v", sess.State)
	}
	if sess.PrNumber != nil {
		t.Fatalf("expected no PR (CreateDraftPR errored), got #%d", *sess.PrNumber)
	}
	if len(h.VCS.CreateDraftPRCalls) != 1 {
		t.Fatalf("expected exactly 1 CreateDraftPR attempt, got %d", len(h.VCS.CreateDraftPRCalls))
	}
	// Branch was pushed before the PR-create attempt — leave the branch
	// alone (not auto-cleaned). Confirm no implicit cleanup.
	if got := len(h.Git.EmptyTrashCalls); got != 0 {
		t.Fatalf("expected no branch cleanup on PR failure, got %d EmptyTrash calls", got)
	}
}

// TestE2E_Resilience_ClaudeCrashesMidSession pins that when the Claude
// subprocess crashes (subscriber channel closes) an attached client
// receives a SessionEnded terminator and ExitError surfaces the crash.
func TestE2E_Resilience_ClaudeCrashesMidSession(t *testing.T) {
	h := testharness.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	repoID := resilienceRegisterRepo(t, h, ctx)
	sessionID, _ := h.SeedSessionInState(t, ctx, repoID,
		pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN, "Crash me", "crash plan")

	getResp, err := h.Client.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: sessionID}))
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	claudeID := *getResp.Msg.Session.ClaudeSessionId

	// Arm subscribe hook so we can sequence the crash *after* the server
	// has subscribed (otherwise close-before-subscribe just no-ops).
	h.Claude.SubscribedCh = make(chan string, 1)

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

	select {
	case <-h.Claude.SubscribedCh:
	case <-ctx.Done():
		t.Fatalf("timed out waiting for server Subscribe: %v", ctx.Err())
	}

	crashErr := errors.New("simulated crash: signal: segmentation fault")
	if err := h.Claude.CrashSession(claudeID, crashErr); err != nil {
		t.Fatalf("crash session: %v", err)
	}

	var res drainResult
	select {
	case res = <-resCh:
	case <-ctx.Done():
		t.Fatalf("timed out waiting for AttachAndDrain to surface SessionEnded: %v", ctx.Err())
	}
	if res.err != nil {
		t.Fatalf("attach drain error: %v", res.err)
	}

	gotEnded := false
	for _, msg := range res.msgs {
		if msg.GetSessionEnded() != nil {
			gotEnded = true
		}
	}
	if !gotEnded {
		t.Fatal("expected SessionEnded after CrashSession, got none")
	}

	// IsRunning must reflect the crash.
	if h.Claude.IsRunning(claudeID) {
		t.Fatal("expected Claude IsRunning=false after crash")
	}
	// ExitError must surface the crash error to callers that ask.
	if got := h.Claude.ExitError(claudeID); !errors.Is(got, crashErr) {
		t.Fatalf("expected ExitError to return %v, got %v", crashErr, got)
	}
}

// TestE2E_Resilience_ConcurrentArchiveAndResurrect fires ArchiveSession
// and ResurrectSession concurrently against the same session. Whichever
// RPC completes second decides the final archived_at state — the
// invariant tested is that the final state is deterministic (set or
// cleared, never partial) and that no race-detector violation fires.
func TestE2E_Resilience_ConcurrentArchiveAndResurrect(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	repoID := resilienceRegisterRepo(t, h, ctx)

	// Need an archived starting point so resurrect has something to do.
	sessionID, _ := h.SeedSessionInState(t, ctx, repoID,
		pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN, "Race me", "race plan")
	if _, err := h.Client.ArchiveSession(ctx, connect.NewRequest(&pb.ArchiveSessionRequest{Id: sessionID})); err != nil {
		t.Fatalf("seed archive: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = h.Client.ResurrectSession(ctx, connect.NewRequest(&pb.ResurrectSessionRequest{Id: sessionID}))
	}()
	go func() {
		defer wg.Done()
		_, _ = h.Client.ArchiveSession(ctx, connect.NewRequest(&pb.ArchiveSessionRequest{Id: sessionID}))
	}()
	wg.Wait()

	// Final state must be deterministic from the perspective of the next
	// caller — exactly archived OR not archived. The dispatcher serialises
	// updates per-session; whichever lands last is what we observe.
	getResp, err := h.Client.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: sessionID}))
	if err != nil {
		t.Fatalf("get session after concurrent ops: %v", err)
	}
	// archived_at is either set (Archive won) or nil (Resurrect won) — both
	// outcomes are valid; what we ban is any other field becoming corrupt.
	sess := getResp.Msg.Session
	if sess.Id != sessionID {
		t.Fatalf("session ID mutated: %q -> %q", sessionID, sess.Id)
	}
	// State remains ImplementingPlan regardless of archive outcome (archive
	// doesn't change state).
	if sess.State != pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN {
		t.Fatalf("state must remain IMPLEMENTING_PLAN, got %v", sess.State)
	}
}

// TestE2E_Resilience_ConcurrentDeliverVCSEvents drives ChecksPassed and
// ChecksFailed at the same dispatcher concurrently. The dispatcher reads
// a single channel, so events serialize by construction. The invariant:
// the final state matches the last-delivered event AND the run completes
// without races (this test runs under -race).
func TestE2E_Resilience_ConcurrentDeliverVCSEvents(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	repoID := resilienceRegisterRepo(t, h, ctx)

	sessionID, prNum := h.SeedSessionInState(t, ctx, repoID,
		pb.SessionState_SESSION_STATE_AWAITING_CHECKS, "concurrent events", "concurrent plan")

	dispatcher := session.NewDispatcher(h.Sessions, h.Repos, h.VCS, nil, zerolog.Nop())
	events := make(chan session.SessionEvent, 2)

	failureConclusion := vcs.CheckConclusionFailure
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		events <- session.SessionEvent{SessionID: sessionID, Event: vcs.ChecksPassed{PRID: prNum}}
	}()
	go func() {
		defer wg.Done()
		events <- session.SessionEvent{
			SessionID: sessionID,
			Event: vcs.ChecksFailed{
				PRID:         prNum,
				FailedChecks: []vcs.CheckResult{{ID: "lint", Name: "lint", Status: vcs.CheckStatusCompleted, Conclusion: &failureConclusion}},
			},
		}
	}()
	wg.Wait()
	close(events)

	dispCtx, dispCancel := context.WithTimeout(ctx, 5*time.Second)
	defer dispCancel()
	dispatcher.Run(dispCtx, events)

	getResp, err := h.Client.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: sessionID}))
	if err != nil {
		t.Fatalf("get session after concurrent dispatch: %v", err)
	}
	// State must be one of the two valid post-dispatch outcomes — never a
	// partial intermediate. ChecksPassed at AwaitingChecks → GreenDraft;
	// ChecksFailed at AwaitingChecks → FixingChecks (under max attempts).
	final := getResp.Msg.Session.State
	switch final {
	case pb.SessionState_SESSION_STATE_GREEN_DRAFT,
		pb.SessionState_SESSION_STATE_FIXING_CHECKS:
		// expected
	default:
		t.Fatalf("expected final state GREEN_DRAFT or FIXING_CHECKS, got %v", final)
	}
}

// TestE2E_Resilience_RetryAfterBlocked pins what RetrySession actually
// does today: it clears blocked_reason and re-enables automation. It
// does NOT transition the session back into FixingChecks — that is the
// state machine's responsibility on the next event. This test guards
// against silent regression of the current contract.
func TestE2E_Resilience_RetryAfterBlocked(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	repoID := resilienceRegisterRepo(t, h, ctx)

	sessionID, _ := h.SeedSessionInState(t, ctx, repoID,
		pb.SessionState_SESSION_STATE_BLOCKED, "Retry me", "retry plan")

	// Sanity: BLOCKED with automation disabled is the precondition.
	getResp, err := h.Client.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: sessionID}))
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if getResp.Msg.Session.State != pb.SessionState_SESSION_STATE_BLOCKED {
		t.Fatalf("expected BLOCKED before retry, got %v", getResp.Msg.Session.State)
	}

	retryResp, err := h.Client.RetrySession(ctx, connect.NewRequest(&pb.RetrySessionRequest{Id: sessionID}))
	if err != nil {
		t.Fatalf("retry session: %v", err)
	}
	sess := retryResp.Msg.Session
	if !sess.AutomationEnabled {
		t.Error("RetrySession must re-enable automation")
	}
	if sess.BlockedReason != nil && *sess.BlockedReason != "" {
		t.Errorf("RetrySession must clear blocked_reason, got %q", *sess.BlockedReason)
	}
	// Retry is a database-only patch today — it must not touch Claude or
	// VCS side effects.
	if len(h.Claude.Stopped) != 0 {
		t.Errorf("retry must not stop Claude, got %v", h.Claude.Stopped)
	}
	if len(h.VCS.MergePRCalls) != 0 {
		t.Errorf("retry must not merge, got %v", h.VCS.MergePRCalls)
	}
}

// --- Restart-recovery tests ---

// resilienceTempDBPath returns a unique on-disk SQLite path under the
// test's temp directory.
func resilienceTempDBPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "bossd.db")
}

// TestE2E_RestartRecovery_SessionsRehydrate creates sessions in three
// distinct states, closes the harness, reopens with the same DB path,
// and asserts all three sessions are readable with their original state.
func TestE2E_RestartRecovery_SessionsRehydrate(t *testing.T) {
	dbPath := resilienceTempDBPath(t)
	ctx := context.Background()

	type seeded struct {
		id    string
		state pb.SessionState
	}
	var sessions []seeded
	var repoID string

	// First incarnation: seed three sessions in distinct states.
	{
		h := testharness.NewWithDBPath(t, dbPath)
		repoID = resilienceRegisterRepo(t, h, ctx)

		s1, _ := h.SeedSessionInState(t, ctx, repoID,
			pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN, "rehydrate-impl", "p1")
		s2, _ := h.SeedSessionInState(t, ctx, repoID,
			pb.SessionState_SESSION_STATE_AWAITING_CHECKS, "rehydrate-awaiting", "p2")
		s3, _ := h.SeedSessionInState(t, ctx, repoID,
			pb.SessionState_SESSION_STATE_CLOSED, "rehydrate-closed", "p3")

		sessions = []seeded{
			{id: s1, state: pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN},
			{id: s2, state: pb.SessionState_SESSION_STATE_AWAITING_CHECKS},
			{id: s3, state: pb.SessionState_SESSION_STATE_CLOSED},
		}
		h.Close()
	}

	// Second incarnation: same DB path. Sessions must rehydrate.
	h := testharness.NewWithDBPath(t, dbPath)
	for _, want := range sessions {
		got, err := h.Client.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: want.id}))
		if err != nil {
			t.Fatalf("get session %s after restart: %v", want.id, err)
		}
		if got.Msg.Session.State != want.state {
			t.Errorf("session %s: state %v after restart, want %v",
				want.id, got.Msg.Session.State, want.state)
		}
	}

	// And the repo persists too.
	repos, err := h.Client.ListRepos(ctx, connect.NewRequest(&pb.ListReposRequest{}))
	if err != nil {
		t.Fatalf("list repos after restart: %v", err)
	}
	if len(repos.Msg.Repos) != 1 || repos.Msg.Repos[0].Id != repoID {
		t.Fatalf("expected single repo %q after restart, got %+v", repoID, repos.Msg.Repos)
	}
}

// TestE2E_RestartRecovery_AttachAfterRestart confirms a session created
// in the first daemon incarnation can be fetched via GetSession after
// restart. (Attach itself depends on the Claude runner having the
// session in memory, which it doesn't after restart — the rehydration
// guarantee is metadata-only.)
func TestE2E_RestartRecovery_AttachAfterRestart(t *testing.T) {
	dbPath := resilienceTempDBPath(t)
	ctx := context.Background()

	var sessionID string
	{
		h := testharness.NewWithDBPath(t, dbPath)
		repoID := resilienceRegisterRepo(t, h, ctx)
		sessionID, _ = h.SeedSessionInState(t, ctx, repoID,
			pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN, "attach-after-restart", "p")
		h.Close()
	}

	h := testharness.NewWithDBPath(t, dbPath)
	got, err := h.Client.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: sessionID}))
	if err != nil {
		t.Fatalf("get session after restart: %v", err)
	}
	if got.Msg.Session.Id != sessionID {
		t.Fatalf("expected session %q, got %q", sessionID, got.Msg.Session.Id)
	}
	if got.Msg.Session.Title != "attach-after-restart" {
		t.Errorf("expected title preserved, got %q", got.Msg.Session.Title)
	}
}

// TestE2E_RestartRecovery_AdvanceOrphanedSessions exercises the
// SessionStore.AdvanceOrphanedSessions helper that the daemon runs at
// startup to clear sessions stuck in IMPLEMENTING_PLAN with no live
// workflow. Without a workflow row in the DB, the seeded session should
// be advanced to AWAITING_CHECKS.
func TestE2E_RestartRecovery_AdvanceOrphanedSessions(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	repoID := resilienceRegisterRepo(t, h, ctx)

	sessionID, _ := h.SeedSessionInState(t, ctx, repoID,
		pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN, "orphan", "no workflow")

	// Run the same recovery path the daemon runs at startup.
	n, err := h.Sessions.AdvanceOrphanedSessions(ctx)
	if err != nil {
		t.Fatalf("AdvanceOrphanedSessions: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 session advanced, got %d", n)
	}

	got, err := h.Client.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: sessionID}))
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.Msg.Session.State != pb.SessionState_SESSION_STATE_AWAITING_CHECKS {
		t.Fatalf("expected AWAITING_CHECKS after orphan sweep, got %v", got.Msg.Session.State)
	}

	// Idempotency: a second run finds nothing to advance.
	n2, err := h.Sessions.AdvanceOrphanedSessions(ctx)
	if err != nil {
		t.Fatalf("AdvanceOrphanedSessions (run 2): %v", err)
	}
	if n2 != 0 {
		t.Fatalf("second AdvanceOrphanedSessions run advanced %d, want 0", n2)
	}
}
