package testharness_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/testharness"
)

// keepCurrentPair seeds two sibling sessions in the same repo, both with an
// open passing PR, and marks the second one as behind the base so the
// keep-current sweep has exactly one eligible candidate. It returns the session
// that will be merged and the sibling's branch name.
func keepCurrentPair(t *testing.T, h *testharness.Harness, ctx context.Context, repoID string) (mergeSessionID, siblingBranch string) {
	t.Helper()

	mergeSessionID, _ = h.SeedSessionInState(t, ctx, repoID,
		pb.SessionState_SESSION_STATE_READY_FOR_REVIEW, "Lands first", "plan")
	siblingID, _ := h.SeedSessionInState(t, ctx, repoID,
		pb.SessionState_SESSION_STATE_READY_FOR_REVIEW, "Still in flight", "plan")

	// The merge guard reads live display status; both PRs are green.
	h.DisplayTracker.Set(mergeSessionID, vcs.DisplayInfo{Status: vcs.DisplayStatusPassing})
	h.DisplayTracker.Set(siblingID, vcs.DisplayInfo{Status: vcs.DisplayStatusPassing})

	resp, err := h.Client.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: siblingID}))
	if err != nil {
		t.Fatalf("get sibling session: %v", err)
	}
	siblingBranch = resp.Msg.Session.BranchName
	if siblingBranch == "" {
		t.Fatal("sibling session has no branch name")
	}
	h.Git.SetBehindBase(siblingBranch, 2)
	return mergeSessionID, siblingBranch
}

// setShouldKeepBranchesCurrent flips the per-repo opt-in over the wire, exactly as an
// operator would.
func setShouldKeepBranchesCurrent(t *testing.T, h *testharness.Harness, ctx context.Context, repoID string, on bool) {
	t.Helper()
	if _, err := h.Client.UpdateRepo(ctx, connect.NewRequest(&pb.UpdateRepoRequest{
		Id:                        repoID,
		ShouldKeepBranchesCurrent: &on,
	})); err != nil {
		t.Fatalf("update repo (should_keep_branches_current=%v): %v", on, err)
	}
}

// TestE2E_ShouldKeepBranchesCurrent_SweepsSiblingsWhenOptedIn is the end-to-end
// acceptance test for BOS-521: merging one session's PR in an opted-in repo
// rebases every other in-flight branch onto the advanced base and force-pushes
// it, and does so by rebasing — never by merging the base back in.
func TestE2E_ShouldKeepBranchesCurrent_SweepsSiblingsWhenOptedIn(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	repoID := registerTestRepo(t, h, ctx)
	setShouldKeepBranchesCurrent(t, h, ctx, repoID, true)

	swept := make(chan (<-chan struct{}), 4)
	h.Server.SetKeepCurrentSweepHookForTest(func(done <-chan struct{}) { swept <- done })

	mergeSessionID, siblingBranch := keepCurrentPair(t, h, ctx, repoID)

	if _, err := h.Client.MergeSession(ctx, connect.NewRequest(&pb.MergeSessionRequest{Id: mergeSessionID})); err != nil {
		t.Fatalf("merge session: %v", err)
	}

	select {
	case done := <-swept:
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("keep-current sweep did not finish")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("merge did not enqueue a keep-current sweep")
	}

	rebased := h.Git.RebasedBranches()
	if len(rebased) != 1 || rebased[0] != siblingBranch {
		t.Fatalf("rebased branches = %v, want [%s] (the in-flight sibling only)", rebased, siblingBranch)
	}
	// Rebase, never merge: the sweep must not introduce a merge commit by
	// pulling the base into the branch. (The real-git proof that a rebase adds
	// no merge commits lives in internal/git's RebaseOntoBaseAndPush tests.)
	if got := len(h.Git.MergeLocalBranchCalls); got != 0 {
		t.Fatalf("keep-current performed %d local merges, want 0", got)
	}
}

// TestE2E_ShouldKeepBranchesCurrent_DefaultOffLeavesBranchesAlone verifies the
// feature is genuinely opt-in: a repo that never enabled it sees no rebases and
// no sweep at all, so the merge path costs exactly what it did before.
func TestE2E_ShouldKeepBranchesCurrent_DefaultOffLeavesBranchesAlone(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	repoID := registerTestRepo(t, h, ctx)

	swept := make(chan (<-chan struct{}), 4)
	h.Server.SetKeepCurrentSweepHookForTest(func(done <-chan struct{}) { swept <- done })

	mergeSessionID, _ := keepCurrentPair(t, h, ctx, repoID)

	if _, err := h.Client.MergeSession(ctx, connect.NewRequest(&pb.MergeSessionRequest{Id: mergeSessionID})); err != nil {
		t.Fatalf("merge session: %v", err)
	}

	select {
	case <-swept:
		t.Fatal("keep-current sweep ran for a repo that never opted in")
	case <-time.After(500 * time.Millisecond):
	}

	if got := h.Git.RebasedBranches(); len(got) != 0 {
		t.Fatalf("rebased %v with should_keep_branches_current off, want no branches touched", got)
	}
	// The opt-out must be free, not merely harmless: the drift metric calls
	// CountBehindBase, which fetches from the network while holding the
	// per-repo merge gate. An opted-out repo must not pay for it.
	if got := h.Git.BehindBaseCallCount(); got != 0 {
		t.Fatalf("CountBehindBase called %d times with should_keep_branches_current off, want 0", got)
	}
}
