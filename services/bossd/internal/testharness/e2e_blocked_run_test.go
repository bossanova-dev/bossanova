package testharness_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/testharness"
)

const blockedRunReason = "run blocked: flows CI remains red after 5 repair passes"

// blockedRun is a seeded #6655-shape run: unattended, Blocked with its repair
// budget spent, its own chat plus an operator chat, and a transcript whose
// newest message the test moves.
type blockedRun struct {
	id, runChat, otherChat string
	newest                 time.Time
}

func seedBlockedRun(t *testing.T, h *testharness.Harness, ctx context.Context, repoID string, pr int) *blockedRun {
	t.Helper()
	r := &blockedRun{id: h.SeedSession(t, repoID, pr, pb.SessionState_SESSION_STATE_BLOCKED), runChat: "agent-run", otherChat: "agent-operator"}
	reason, runChat, unattended, spent := blockedRunReason, r.runChat, true, 5
	reasonPtr, chatPtr := &reason, &runChat
	if _, err := h.Sessions.Update(ctx, r.id, db.UpdateSessionParams{
		BlockedReason: &reasonPtr, AgentSessionID: &chatPtr, IsTmuxUnattended: &unattended, LastRepairAttemptCount: &spent,
	}); err != nil {
		t.Fatal(err)
	}
	for _, chat := range []string{r.runChat, r.otherChat} {
		if _, err := h.AgentChats.Create(ctx, db.CreateAgentChatParams{SessionID: r.id, AgentSessionID: chat, AgentName: "claude", Title: chat}); err != nil {
			t.Fatal(err)
		}
	}
	row := r.row(t, h, ctx)
	r.newest = row.StateEnteredAt.Add(-time.Minute)
	h.VCS.OpenPRs = append(h.VCS.OpenPRs, vcs.PRSummary{Number: pr, HeadBranch: row.BranchName, State: vcs.PRStateOpen})
	h.Git.CommitSubjectsFunc = func(context.Context, string, string) ([]string, error) {
		return []string{"fix(flows): bump the pin"}, nil
	}
	h.Lifecycle.SetAgents(map[string]agent.AgentRunnerClient{"claude": &testharness.MockAgentClient{
		Name: "claude",
		ReadTranscriptFunc: func(req *pb.ReadTranscriptRequest) (*pb.ReadTranscriptResponse, error) {
			if req.GetAgentSessionId() != r.runChat {
				return &pb.ReadTranscriptResponse{}, nil
			}
			return &pb.ReadTranscriptResponse{Exists: true, Messages: []*pb.ChatMessage{{Role: "user", Timestamp: r.newest.Format(time.RFC3339Nano)}}}, nil
		},
	}})
	return r
}

func (r *blockedRun) row(t *testing.T, h *testharness.Harness, ctx context.Context) *models.Session {
	t.Helper()
	s, err := h.Sessions.Get(ctx, r.id)
	if err != nil || s.StateEnteredAt == nil {
		t.Fatalf("session %s = %+v, %v", r.id, s, err)
	}
	return s
}

func (r *blockedRun) served(t *testing.T, h *testharness.Harness, ctx context.Context) *pb.Session {
	t.Helper()
	resp, err := h.Client.ListSessions(ctx, connect.NewRequest(&pb.ListSessionsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range resp.Msg.GetSessions() {
		if s.GetId() == r.id {
			return s
		}
	}
	t.Fatalf("session %s not served", r.id)
	return nil
}

// assertStillBlocked fails unless the run is Blocked with reason, its budget
// spent and no PR readied.
func (r *blockedRun) assertStillBlocked(t *testing.T, h *testharness.Harness, ctx context.Context, reason string) {
	t.Helper()
	got := r.row(t, h, ctx)
	gotReason := ""
	if got.BlockedReason != nil {
		gotReason = *got.BlockedReason
	}
	if got.State != machine.Blocked || gotReason != reason || got.LastRepairAttemptCount != 5 {
		t.Fatalf("state=%v reason=%q repair attempts=%d, want Blocked with %q and the budget spent", got.State, gotReason, got.LastRepairAttemptCount, reason)
	}
	if n := len(h.VCS.MarkReadyForReviewCalls); n != 0 {
		t.Fatalf("%d MarkReadyForReview calls, want the PR left draft", n)
	}
}

func (r *blockedRun) finish(h *testharness.Harness, ctx context.Context, requester string, blockedReason string) error {
	_, err := h.Client.FinishUnattendedRun(ctx, connect.NewRequest(&pb.FinishUnattendedRunRequest{
		SessionId: r.id, RequesterAgentSessionId: requester, IsBlocked: blockedReason != "", BlockedReason: blockedReason,
	}))
	return err
}

// TestE2E_BlockedRunReadsInProgressAndMovesOnlyOnItsOwnFinish drives the
// #6655 shape end to end. A redraw keeps the Fix. A turn of the run's own chat
// reads in progress and Fix again when it stops, with the run still Blocked
// after the stranded sweep. Another chat cannot finish it. The run's own
// finish --blocked re-Blocks it with the new reason and a new block time, and
// its own plain finish readies the PR with a fresh repair budget.
func TestE2E_BlockedRunReadsInProgressAndMovesOnlyOnItsOwnFinish(t *testing.T) {
	h := testharness.New(t)
	defer h.Close()
	ctx := h.Ctx()
	r := seedBlockedRun(t, h, ctx, h.SeedRepo(t, "https://github.com/test/repo.git"), 6655)

	if got := r.served(t, h, ctx).GetAttention().GetKind(); got != pb.AttentionKind_ATTENTION_KIND_FIX {
		t.Fatalf("blocked run attention = %v, want FIX", got)
	}
	h.SeedChatStatus(r.runChat, pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())
	h.Lifecycle.MarkWorkingBlockedRuns(ctx)
	if got := r.served(t, h, ctx).GetAttention().GetKind(); got != pb.AttentionKind_ATTENTION_KIND_FIX {
		t.Fatalf("redraw-only attention = %v, want FIX kept", got)
	}

	r.newest = time.Now()
	h.Lifecycle.MarkWorkingBlockedRuns(ctx)
	if got := r.served(t, h, ctx); got.GetAttention().GetKind() != pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS || got.GetAttentionStatus() != nil {
		t.Fatalf("attention=%v attention_status=%v, want in progress with no hint while the agent works", got.GetAttention(), got.GetAttentionStatus())
	}
	r.assertStillBlocked(t, h, ctx, blockedRunReason)

	h.SeedChatStatus(r.runChat, pb.ChatStatus_CHAT_STATUS_IDLE, time.Now())
	h.Lifecycle.MarkWorkingBlockedRuns(ctx)
	if _, err := h.Lifecycle.RecoverStrandedCronSessionsPeriodic(ctx); err != nil {
		t.Fatal(err)
	}
	if got := r.served(t, h, ctx); got.GetAttention().GetKind() != pb.AttentionKind_ATTENTION_KIND_FIX || got.GetAttentionStatus() == nil {
		t.Fatalf("attention=%v attention_status=%v, want FIX back once the agent stops", got.GetAttention(), got.GetAttentionStatus())
	}
	r.assertStillBlocked(t, h, ctx, blockedRunReason)

	for _, blocked := range []string{"", "operator says so"} {
		if err := r.finish(h, ctx, r.otherChat, blocked); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("finish (blocked=%q) from another chat = %v, want PermissionDenied", blocked, err)
		}
	}
	firstBlock := *r.row(t, h, ctx).StateEnteredAt

	// A new chat cancels a pending finish --blocked before it fires. Its
	// reason never reached the row, so nothing re-Blocks on it later.
	if err := r.finish(h, ctx, r.runChat, "a report the new chat drops"); err != nil {
		t.Fatalf("finish --blocked from the run's chat: %v", err)
	}
	if _, err := h.Client.RecordChat(ctx, connect.NewRequest(&pb.RecordChatRequest{SessionId: r.id, AgentSessionId: "0199a3c1-7b2e-7d40-9c55-1a2b3c4d5e63"})); err != nil {
		t.Fatalf("RecordChat(new): %v", err)
	}
	r.assertStillBlocked(t, h, ctx, blockedRunReason)
	if got := r.row(t, h, ctx); got.RunBlockedReason != nil || !got.StateEnteredAt.Equal(firstBlock) {
		t.Fatalf("run_blocked_reason=%v state_entered_at=%v, want no report and the first block (%v)", got.RunBlockedReason, got.StateEnteredAt, firstBlock)
	}

	// The deferrer fires the finish once the session settles; the test fires
	// it directly through the same lifecycle entry.
	if err := r.finish(h, ctx, r.runChat, "flows still red after bumping the pin"); err != nil {
		t.Fatalf("finish --blocked from the run's chat: %v", err)
	}
	if _, err := h.Lifecycle.FinishUnattendedRun(ctx, r.id); err != nil {
		t.Fatal(err)
	}
	r.assertStillBlocked(t, h, ctx, "run blocked: flows still red after bumping the pin")
	if got := r.row(t, h, ctx); !got.StateEnteredAt.After(firstBlock) || got.RunBlockedReason != nil {
		t.Fatalf("state_entered_at=%v (first block %v) run_blocked_reason=%v, want a new block with the report consumed", got.StateEnteredAt, firstBlock, got.RunBlockedReason)
	}

	if err := r.finish(h, ctx, r.runChat, ""); err != nil {
		t.Fatalf("finish from the run's chat: %v", err)
	}
	if res, err := h.Lifecycle.FinishUnattendedRun(ctx, r.id); err != nil || res.Outcome != models.CronJobOutcomePRCreated {
		t.Fatalf("plain finish = %+v, %v; want pr_created", res, err)
	}
	got := r.row(t, h, ctx)
	if got.State != machine.ReadyForReview || got.BlockedReason != nil || got.LastRepairAttemptCount != 0 || len(h.VCS.MarkReadyForReviewCalls) != 1 {
		t.Fatalf("state=%v reason=%v repair attempts=%d ready calls=%d, want ReadyForReview, no block, a fresh budget and the PR readied",
			got.State, got.BlockedReason, got.LastRepairAttemptCount, len(h.VCS.MarkReadyForReviewCalls))
	}
}

// TestE2E_KeepCurrentRebaseLeavesABlockedRunBlocked is the second verdict's
// F2: the keep-current sweep rebases and force-pushes a blocked branch whose
// agent already answered a question after the block. The run stays Blocked
// with its draft and spent budget.
func TestE2E_KeepCurrentRebaseLeavesABlockedRunBlocked(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	repoID := registerTestRepo(t, h, ctx)
	setShouldKeepBranchesCurrent(t, h, ctx, repoID, true)
	swept := make(chan (<-chan struct{}), 4)
	h.Server.SetKeepCurrentSweepHookForTest(func(done <-chan struct{}) { swept <- done })

	mergeID, _ := h.SeedSessionInState(t, ctx, repoID, pb.SessionState_SESSION_STATE_READY_FOR_REVIEW, "Lands first", "plan")
	h.DisplayTracker.Set(mergeID, vcs.DisplayInfo{Status: vcs.DisplayStatusPassing})
	r := seedBlockedRun(t, h, ctx, repoID, 6646)
	r.newest = time.Now()
	blockedBranch := r.row(t, h, ctx).BranchName
	h.Git.SetBehindBase(blockedBranch, 2)

	if _, err := h.Client.MergeSession(ctx, connect.NewRequest(&pb.MergeSessionRequest{Id: mergeID})); err != nil {
		t.Fatalf("merge session: %v", err)
	}
	select {
	case done := <-swept:
		<-done
	case <-time.After(30 * time.Second):
		t.Fatal("merge did not run a keep-current sweep")
	}
	if rebased := h.Git.RebasedBranches(); len(rebased) != 1 || rebased[0] != blockedBranch {
		t.Fatalf("rebased branches = %v, want the blocked branch %s", rebased, blockedBranch)
	}

	h.Lifecycle.MarkWorkingBlockedRuns(ctx)
	if _, err := h.Lifecycle.RecoverStrandedCronSessionsPeriodic(ctx); err != nil {
		t.Fatal(err)
	}
	r.assertStillBlocked(t, h, ctx, blockedRunReason)
}
