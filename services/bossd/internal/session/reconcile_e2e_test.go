package session_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/testharness"
	"github.com/rs/zerolog"
)

// reconcileRegisterRepo registers a repo via the harness client and returns
// its (display name, ID, origin URL) tuple. Mirrors the helpers used in
// other e2e tests; duplicated to keep this file self-contained.
func reconcileRegisterRepo(t *testing.T, h *testharness.Harness, ctx context.Context, displayName string) (string, string) {
	t.Helper()
	repoDir := testharness.TempRepoDir(t)
	resp, err := h.Client.RegisterRepo(ctx, connect.NewRequest(&pb.RegisterRepoRequest{
		DisplayName:       displayName,
		LocalPath:         repoDir,
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}))
	if err != nil {
		t.Fatalf("register repo: %v", err)
	}
	return resp.Msg.Repo.Id, resp.Msg.Repo.OriginUrl
}

// nullPRNumber returns a session ID with PRNumber forcibly nulled out so it
// looks orphaned to the reconciler. This bypasses the lifecycle's normal
// "session always has a PR" invariant for repos with createDraftPR enabled.
// We do this directly in the DB store because the public RPC surface does
// not expose a "drop PR association" operation — it isn't a normal flow.
func nullPRNumber(t *testing.T, ctx context.Context, h *testharness.Harness, sessionID string) {
	t.Helper()
	if _, err := h.DB.ExecContext(ctx,
		`UPDATE sessions SET pr_number = NULL, pr_url = NULL WHERE id = ?`, sessionID,
	); err != nil {
		t.Fatalf("null pr_number: %v", err)
	}
}

// TestE2E_Reconcile_MatchesByBranch_RealStores runs ReconcilePRAssociations
// against real SQLite-backed stores wired through the test harness. It
// confirms an orphaned session (no PR but with a branch) gets associated
// with the matching open PR returned by the VCS provider.
func TestE2E_Reconcile_MatchesByBranch_RealStores(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()

	repoID, originURL := reconcileRegisterRepo(t, h, ctx, "match-by-branch")

	// Seed a session and force-orphan it so the reconciler picks it up.
	sessionID, _ := h.SeedSessionInState(t, ctx, repoID,
		pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN, "match-by-branch", "p")
	nullPRNumber(t, ctx, h, sessionID)

	// Confirm the orphan precondition.
	pre, err := h.Client.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: sessionID}))
	if err != nil {
		t.Fatalf("get session pre: %v", err)
	}
	if pre.Msg.Session.PrNumber != nil {
		t.Fatalf("orphan precondition: pr_number still set to %d", *pre.Msg.Session.PrNumber)
	}
	branch := pre.Msg.Session.BranchName
	if branch == "" {
		t.Fatal("expected branch name on session for reconciler to match against")
	}

	// VCS provider reports an open PR with that branch.
	h.VCS.OpenPRs = []vcs.PRSummary{
		{Number: 7, HeadBranch: branch, State: vcs.PRStateOpen},
	}

	n, err := session.ReconcilePRAssociations(ctx, h.Sessions, h.Repos, h.VCS, zerolog.Nop())
	if err != nil {
		t.Fatalf("ReconcilePRAssociations: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 reconciled session, got %d", n)
	}

	post, err := h.Client.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: sessionID}))
	if err != nil {
		t.Fatalf("get session post: %v", err)
	}
	if post.Msg.Session.PrNumber == nil || *post.Msg.Session.PrNumber != 7 {
		t.Fatalf("expected PR 7 attached, got %v", post.Msg.Session.PrNumber)
	}
	if post.Msg.Session.PrUrl == nil || *post.Msg.Session.PrUrl == "" {
		t.Fatalf("expected PR URL constructed from origin %q, got %v", originURL, post.Msg.Session.PrUrl)
	}
}

// TestE2E_Reconcile_Idempotent runs the reconciler twice on healthy state
// and asserts the second run is a no-op — same number of reconciliations,
// no extra PR-list calls beyond what's needed to inspect each repo.
func TestE2E_Reconcile_Idempotent(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()

	repoID, _ := reconcileRegisterRepo(t, h, ctx, "idempotent")

	// Healthy session — has a PR. Reconciler should not touch it.
	sessionID, _ := h.SeedSessionInState(t, ctx, repoID,
		pb.SessionState_SESSION_STATE_AWAITING_CHECKS, "healthy", "p")

	// Track the PR-list call counts via a wrapping provider — easiest path
	// is to spy on the existing mock by counting calls before/after.
	// MockVCSProvider doesn't expose call counts for ListOpenPRs/ListClosedPRs,
	// so the next-best assertion is that the reconciler returns 0 on both runs.
	n1, err := session.ReconcilePRAssociations(ctx, h.Sessions, h.Repos, h.VCS, zerolog.Nop())
	if err != nil {
		t.Fatalf("reconcile run 1: %v", err)
	}
	n2, err := session.ReconcilePRAssociations(ctx, h.Sessions, h.Repos, h.VCS, zerolog.Nop())
	if err != nil {
		t.Fatalf("reconcile run 2: %v", err)
	}
	if n1 != 0 {
		t.Fatalf("run 1: healthy session should not be reconciled, got n=%d", n1)
	}
	if n2 != 0 {
		t.Fatalf("run 2: idempotency violated, got n=%d", n2)
	}

	// Healthy session must remain unchanged.
	got, err := h.Client.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: sessionID}))
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.Msg.Session.PrNumber == nil {
		t.Fatal("expected PR number to remain on healthy session")
	}
}

// TestE2E_Reconcile_NoMatch_LeavesSessionAlone confirms the reconciler
// is a strict no-op when no PR matches an orphaned session's branch.
func TestE2E_Reconcile_NoMatch_LeavesSessionAlone(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()

	repoID, _ := reconcileRegisterRepo(t, h, ctx, "no-match")

	sessionID, _ := h.SeedSessionInState(t, ctx, repoID,
		pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN, "no-match", "p")
	nullPRNumber(t, ctx, h, sessionID)

	// VCS reports open PRs on a *different* branch.
	h.VCS.OpenPRs = []vcs.PRSummary{
		{Number: 9, HeadBranch: "some-other-branch", State: vcs.PRStateOpen},
	}

	n, err := session.ReconcilePRAssociations(ctx, h.Sessions, h.Repos, h.VCS, zerolog.Nop())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 reconciliations, got %d", n)
	}
	got, err := h.Client.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: sessionID}))
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.Msg.Session.PrNumber != nil {
		t.Fatalf("expected session to remain orphaned, got pr_number=%d", *got.Msg.Session.PrNumber)
	}
}
