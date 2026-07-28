package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sessionreason"
	"github.com/recurser/bossalib/vcs"
	"github.com/rs/zerolog"

	gitpkg "github.com/recurser/bossd/internal/git"
	"github.com/recurser/bossd/internal/status"
)

// mergeGateProvider is a configurable vcs.Provider for MergeSession's live
// pre-merge gate. Only the reads the gate touches (GetPRStatus/GetCheckResults/
// GetReviewComments) and the merge path (GetAllowedMergeStrategies/MergePR) are
// meaningful; the rest return safe zero values. mergeCalled records whether the
// gate let execution reach the actual merge.
type mergeGateProvider struct {
	prStatus    *vcs.PRStatus
	checks      []vcs.CheckResult
	reviews     []vcs.ReviewComment
	mergeErr    error
	mergeCalled bool
	// onMerge, when set, fires inside MergePR before it returns — a seam for
	// observing display-tracker state at the exact moment the blocking merge runs.
	onMerge func()

	// allowed overrides the merge strategies the remote reports as enabled.
	// nil keeps the historical default of []string{"merge"}.
	allowed    []string
	allowedErr error
	// mergeStrategies records the strategy passed to every MergePR call, in
	// order, so substitution and the one-shot squash retry can be asserted
	// exactly rather than by call count alone.
	mergeStrategies []string
	// mergeErrByStrategy lets a single test fail one strategy and succeed on
	// another (the rebase-refused → squash-retry case). It takes precedence
	// over mergeErr when the strategy has an entry.
	mergeErrByStrategy map[string]error
	// mergeCommitSHA/mergeCommitErr drive VerifyOnBase. Both zero keeps the
	// historical default of ("", vcs.ErrPRNotMerged).
	mergeCommitSHA string
	mergeCommitErr error
}

func (p *mergeGateProvider) GetPRStatus(context.Context, string, int) (*vcs.PRStatus, error) {
	return p.prStatus, nil
}
func (p *mergeGateProvider) GetCheckResults(context.Context, string, int) ([]vcs.CheckResult, error) {
	return p.checks, nil
}
func (p *mergeGateProvider) GetReviewComments(context.Context, string, int) ([]vcs.ReviewComment, error) {
	return p.reviews, nil
}
func (p *mergeGateProvider) GetAllowedMergeStrategies(context.Context, string) ([]string, error) {
	if p.allowedErr != nil {
		return nil, p.allowedErr
	}
	if p.allowed != nil {
		return p.allowed, nil
	}
	return []string{"merge"}, nil
}
func (p *mergeGateProvider) MergePR(_ context.Context, _ string, _ int, strategy string) error {
	p.mergeCalled = true
	p.mergeStrategies = append(p.mergeStrategies, strategy)
	if p.onMerge != nil {
		p.onMerge()
	}
	if err, ok := p.mergeErrByStrategy[strategy]; ok {
		return err
	}
	if p.mergeErr != nil {
		return p.mergeErr
	}
	return nil
}
func (p *mergeGateProvider) CreateDraftPR(context.Context, vcs.CreatePROpts) (*vcs.PRInfo, error) {
	return &vcs.PRInfo{}, nil
}
func (p *mergeGateProvider) GetFailedCheckLogs(context.Context, string, string) (string, error) {
	return "", nil
}
func (p *mergeGateProvider) MarkReadyForReview(context.Context, string, int) error { return nil }
func (p *mergeGateProvider) ListOpenPRs(context.Context, string) ([]vcs.PRSummary, error) {
	return nil, nil
}
func (p *mergeGateProvider) ListClosedPRs(context.Context, string) ([]vcs.PRSummary, error) {
	return nil, nil
}
func (p *mergeGateProvider) SearchPRsByTitleTag(context.Context, string, string) ([]vcs.PRSummary, error) {
	return nil, nil
}
func (p *mergeGateProvider) UpdatePRTitle(context.Context, string, int, string) error { return nil }
func (p *mergeGateProvider) GetPRMergeCommit(context.Context, string, int) (string, error) {
	if p.mergeCommitErr != nil {
		return "", p.mergeCommitErr
	}
	if p.mergeCommitSHA != "" {
		return p.mergeCommitSHA, nil
	}
	return "", vcs.ErrPRNotMerged
}

// mergePolicyWorktrees is a minimal WorktreeManager covering everything
// MergeSession's PR path touches: the merge-commit count that drives strategy
// resolution, the two VerifyOnBase calls, and the post-merge base sync. The
// embedded nil WorktreeManager makes any other call panic, which never happens
// on this path.
type mergePolicyWorktrees struct {
	gitpkg.WorktreeManager
	mergeCommits int
	// fetchBaseErr/isAncestorErr drive the two local-git sources of
	// mergepolicy.ErrMergeVerifyInfra (the third is the provider's
	// GetPRMergeCommit query, driven by mergeGateProvider.mergeCommitErr).
	fetchBaseErr  error
	isAncestor    bool
	isAncestorErr error
	syncCalls     int
}

func (m *mergePolicyWorktrees) CountMergeCommits(context.Context, string, string, string) (int, error) {
	return m.mergeCommits, nil
}

func (m *mergePolicyWorktrees) FetchBase(context.Context, string, string) error {
	return m.fetchBaseErr
}

func (m *mergePolicyWorktrees) IsAncestor(context.Context, string, string, string) (bool, error) {
	return m.isAncestor, m.isAncestorErr
}

func (m *mergePolicyWorktrees) SyncBaseBranch(context.Context, string, string) error {
	m.syncCalls++
	return nil
}

func boolPtr(b bool) *bool { return &b }

func checkConclusionPtr(c vcs.CheckConclusion) *vcs.CheckConclusion { return &c }

// blockedFixLoopSession returns a session row that is Blocked with the stale
// FixLoopExhausted reason and an associated PR — the exact live-observed wedge.
func blockedFixLoopSession() *models.Session {
	pr := 42
	reason := sessionreason.FixLoopExhausted()
	return &models.Session{
		ID:            "s1",
		RepoID:        "r1",
		PRNumber:      &pr,
		State:         machine.Blocked,
		BlockedReason: &reason,
		BaseBranch:    "main",
		BranchName:    "feature",
	}
}

// mergeGateOpt tweaks the server (or the repo row it serves) built by
// mergeGateServer. Variadic so the original three-argument call sites keep
// working untouched.
type mergeGateOpt func(*Server, *models.Repo)

// withRebaseStrategy configures the repo row for rebase merges — the only
// boss-configured strategy the BOS-513 cases need, and the one whose
// merge-commit incompatibility drives the whole squash-fallback path.
func withRebaseStrategy() mergeGateOpt {
	return func(_ *Server, r *models.Repo) { r.MergeStrategy = models.MergeStrategyRebase }
}

// withMergeWorktrees wires a WorktreeManager into the server. mergeGateServer
// deliberately leaves it nil by default (the historical behaviour the existing
// gate tests rely on).
func withMergeWorktrees(wt gitpkg.WorktreeManager) mergeGateOpt {
	return func(s *Server, _ *models.Repo) { s.worktrees = wt }
}

func mergeGateServer(t *testing.T, prov *mergeGateProvider, staleStatus vcs.DisplayStatus, opts ...mergeGateOpt) *Server {
	t.Helper()
	tracker := status.NewDisplayTracker()
	// Seed a STALE, non-Passing tracker entry — the old gate would veto the
	// merge on this alone. The live gate must ignore it.
	tracker.Set("s1", vcs.DisplayInfo{Status: staleStatus})
	repo := &models.Repo{
		ID:                "r1",
		OriginURL:         "https://github.com/acme/repo",
		DefaultBaseBranch: "main",
		LocalPath:         "/x",
	}
	srv := &Server{
		sessions:       &lifecycleSessionStoreFake{session: blockedFixLoopSession()},
		repos:          &archiveRepoStoreFake{repo: repo},
		provider:       prov,
		displayTracker: tracker,
		logger:         zerolog.Nop(),
	}
	for _, opt := range opts {
		opt(srv, repo)
	}
	return srv
}

// TestMergeSessionAllowsLiveGreenDespiteStaleBlocked is the BOS-235 Bug 2
// headline: a session persisted as Blocked+FixLoopExhausted with a stale
// non-Passing tracker entry must still merge when the LIVE PR is green +
// mergeable. The gate must not return failed_precondition; execution must
// reach the actual merge.
func TestMergeSessionAllowsLiveGreenDespiteStaleBlocked(t *testing.T) {
	// Live PR: open, mergeable, one passed check, approved review.
	green := &mergeGateProvider{
		prStatus: &vcs.PRStatus{
			State:            vcs.PRStateOpen,
			Mergeable:        boolPtr(true),
			MergeStateStatus: vcs.MergeStateStatusClean,
		},
		checks: []vcs.CheckResult{{
			Status:     vcs.CheckStatusCompleted,
			Conclusion: checkConclusionPtr(vcs.CheckConclusionSuccess),
		}},
		reviews:  []vcs.ReviewComment{{Author: "reviewer", State: vcs.ReviewStateApproved}},
		mergeErr: errors.New("merge short-circuited in test"),
	}
	srv := mergeGateServer(t, green, vcs.DisplayStatusRejected)

	_, err := srv.MergeSession(context.Background(), connect.NewRequest(&pb.MergeSessionRequest{Id: "s1"}))
	if connect.CodeOf(err) == connect.CodeFailedPrecondition {
		t.Fatalf("live-green PR was rejected by the merge gate: %v", err)
	}
	if !green.mergeCalled {
		t.Fatal("expected execution to reach the actual merge (MergePR), but the gate blocked it")
	}
}

// TestMergeSessionAllowsLiveApproved covers the Approved (10) green value: a
// fully-green AND approved PR computes DisplayStatusApproved, which the old
// gate (== Passing only) refused.
func TestMergeSessionAllowsLiveApproved(t *testing.T) {
	approved := &mergeGateProvider{
		prStatus: &vcs.PRStatus{
			State:             vcs.PRStateOpen,
			Mergeable:         boolPtr(true),
			MergeStateStatus:  vcs.MergeStateStatusClean,
			LatestReviewState: vcs.ReviewStateApproved,
		},
		checks: []vcs.CheckResult{{
			Status:     vcs.CheckStatusCompleted,
			Conclusion: checkConclusionPtr(vcs.CheckConclusionSuccess),
		}},
		mergeErr: errors.New("merge short-circuited in test"),
	}
	// Stale tracker also says Approved — irrelevant; the point is the live read.
	srv := mergeGateServer(t, approved, vcs.DisplayStatusApproved)

	_, err := srv.MergeSession(context.Background(), connect.NewRequest(&pb.MergeSessionRequest{Id: "s1"}))
	if connect.CodeOf(err) == connect.CodeFailedPrecondition {
		t.Fatalf("live-approved PR was rejected by the merge gate: %v", err)
	}
	if !approved.mergeCalled {
		t.Fatal("expected execution to reach the actual merge (MergePR), but the gate blocked it")
	}
}

// TestMergeSessionAllowsEmptyCodexCommentedReview is the BOS-254 merge-gate
// headline: a green, mergeable PR whose sole outstanding review is an empty
// chatgpt-codex-connector[bot] COMMENTED review (the state the fixed provider
// now returns for boilerplate-only bot reviews) must NOT be blocked — the gate
// returns nil and execution reaches the actual merge.
func TestMergeSessionAllowsEmptyCodexCommentedReview(t *testing.T) {
	prov := &mergeGateProvider{
		prStatus: &vcs.PRStatus{
			State:            vcs.PRStateOpen,
			Mergeable:        boolPtr(true),
			MergeStateStatus: vcs.MergeStateStatusClean,
		},
		checks: []vcs.CheckResult{{
			Status:     vcs.CheckStatusCompleted,
			Conclusion: checkConclusionPtr(vcs.CheckConclusionSuccess),
		}},
		reviews: []vcs.ReviewComment{{
			Author: "chatgpt-codex-connector[bot]",
			Body:   "### 💡 Codex Review\n\nHere are some automated review suggestions for this pull request.",
			State:  vcs.ReviewStateCommented,
		}},
		mergeErr: errors.New("merge short-circuited in test"),
	}
	srv := mergeGateServer(t, prov, vcs.DisplayStatusRejected)

	_, err := srv.MergeSession(context.Background(), connect.NewRequest(&pb.MergeSessionRequest{Id: "s1"}))
	if connect.CodeOf(err) == connect.CodeFailedPrecondition {
		t.Fatalf("empty-codex-COMMENTED PR was rejected by the merge gate: %v", err)
	}
	if !prov.mergeCalled {
		t.Fatal("expected execution to reach the actual merge (MergePR), but the gate blocked it")
	}
}

// TestMergeSessionRejectsActionableCodexReview pins the other half of BOS-254:
// a live actionable changes-requested review (what the fixed provider returns
// for a codex review carrying real inline suggestions) still blocks with
// gate=review, exactly as the get/list surface reports it.
func TestMergeSessionRejectsActionableCodexReview(t *testing.T) {
	prov := &mergeGateProvider{
		prStatus: &vcs.PRStatus{
			State:            vcs.PRStateOpen,
			Mergeable:        boolPtr(true),
			MergeStateStatus: vcs.MergeStateStatusBlocked,
		},
		checks: []vcs.CheckResult{{
			Status:     vcs.CheckStatusCompleted,
			Conclusion: checkConclusionPtr(vcs.CheckConclusionSuccess),
		}},
		reviews: []vcs.ReviewComment{{
			Author: "chatgpt-codex-connector[bot]",
			Body:   "handle the nil case",
			State:  vcs.ReviewStateChangesRequested,
		}},
	}
	srv := mergeGateServer(t, prov, vcs.DisplayStatusPassing)

	_, err := srv.MergeSession(context.Background(), connect.NewRequest(&pb.MergeSessionRequest{Id: "s1"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition (err=%v)", connect.CodeOf(err), err)
	}
	if err == nil || !strings.Contains(err.Error(), "merge blocked: gate=review;") {
		t.Fatalf("error = %v, want it to contain 'merge blocked: gate=review;'", err)
	}
	if prov.mergeCalled {
		t.Fatal("an actionable changes-requested PR must not reach the actual merge")
	}
}

// TestMergeSessionRejectsLiveNotGreen pins that the gate still preserves its
// original intent: a truly red/conflicted/rejected LIVE PR is refused with
// failed_precondition and never reaches the merge.
func TestMergeSessionRejectsLiveNotGreen(t *testing.T) {
	cases := []struct {
		name     string
		prStatus *vcs.PRStatus
		checks   []vcs.CheckResult
		reviews  []vcs.ReviewComment
	}{
		{
			name: "failing checks",
			prStatus: &vcs.PRStatus{
				State:            vcs.PRStateOpen,
				Mergeable:        boolPtr(true),
				MergeStateStatus: vcs.MergeStateStatusUnstable,
			},
			checks: []vcs.CheckResult{{
				Status:     vcs.CheckStatusCompleted,
				Conclusion: checkConclusionPtr(vcs.CheckConclusionFailure),
			}},
		},
		{
			name: "unresolved conflict",
			prStatus: &vcs.PRStatus{
				State:            vcs.PRStateOpen,
				Mergeable:        boolPtr(false),
				MergeStateStatus: vcs.MergeStateStatusDirty,
			},
			checks: []vcs.CheckResult{{
				Status:     vcs.CheckStatusCompleted,
				Conclusion: checkConclusionPtr(vcs.CheckConclusionSuccess),
			}},
		},
		{
			name: "changes requested",
			prStatus: &vcs.PRStatus{
				State:             vcs.PRStateOpen,
				Mergeable:         boolPtr(true),
				MergeStateStatus:  vcs.MergeStateStatusBlocked,
				LatestReviewState: vcs.ReviewStateChangesRequested,
			},
			checks: []vcs.CheckResult{{
				Status:     vcs.CheckStatusCompleted,
				Conclusion: checkConclusionPtr(vcs.CheckConclusionSuccess),
			}},
			reviews: []vcs.ReviewComment{{Author: "reviewer", State: vcs.ReviewStateChangesRequested}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := &mergeGateProvider{prStatus: tc.prStatus, checks: tc.checks, reviews: tc.reviews}
			// A stale Passing tracker entry must NOT let a live-bad PR merge.
			srv := mergeGateServer(t, prov, vcs.DisplayStatusPassing)

			_, err := srv.MergeSession(context.Background(), connect.NewRequest(&pb.MergeSessionRequest{Id: "s1"}))
			if connect.CodeOf(err) != connect.CodeFailedPrecondition {
				t.Fatalf("code = %v, want FailedPrecondition (err=%v)", connect.CodeOf(err), err)
			}
			if prov.mergeCalled {
				t.Fatal("a live-bad PR must not reach the actual merge")
			}
		})
	}
}

// livePassingChecks is the one-success-check slice every BOS-513 case needs to
// get past the live pre-merge gate and reach the strategy/merge/verify path.
func livePassingChecks() []vcs.CheckResult {
	return []vcs.CheckResult{{
		Status:     vcs.CheckStatusCompleted,
		Conclusion: checkConclusionPtr(vcs.CheckConclusionSuccess),
	}}
}

// openCleanPRStatus is a live PR read that passes the gate: open, mergeable,
// clean. Returned by value-pointer so a test can mutate State mid-merge.
func openCleanPRStatus() *vcs.PRStatus {
	return &vcs.PRStatus{
		State:            vcs.PRStateOpen,
		Mergeable:        boolPtr(true),
		MergeStateStatus: vcs.MergeStateStatusClean,
	}
}

// TestMergeSessionShortCircuitsWhenPRAlreadyMerged pins BOS-513's idempotency
// leg: a merge retried against a PR the provider already reports as MERGED must
// return success without calling MergePR again. This is the stranded-merge
// recovery path — a merge that landed remotely but whose RPC failed afterwards
// (e.g. verification infra error) would otherwise be permanently unretryable.
func TestMergeSessionShortCircuitsWhenPRAlreadyMerged(t *testing.T) {
	prov := &mergeGateProvider{
		prStatus: &vcs.PRStatus{State: vcs.PRStateMerged},
		checks:   livePassingChecks(),
	}
	// worktrees stays nil: the short-circuit's best-effort base sync must not
	// panic on a server without a worktree manager.
	srv := mergeGateServer(t, prov, vcs.DisplayStatusPassing)

	resp, err := srv.MergeSession(context.Background(), connect.NewRequest(&pb.MergeSessionRequest{Id: "s1"}))
	if err != nil {
		t.Fatalf("already-merged PR must return success, got %v", err)
	}
	if resp == nil || resp.Msg.GetSession() == nil {
		t.Fatal("expected a session on the successful response")
	}
	if prov.mergeCalled {
		t.Fatalf("MergePR must not be called for an already-merged PR (strategies=%v)", prov.mergeStrategies)
	}
}

// TestMergeSessionShortCircuitSyncsBaseWhenPRAlreadyMerged is the companion to
// the nil-worktrees case above: the stranded attempt may have died BEFORE its
// local base sync, so the idempotent retry must still run that sync even though
// it skips the merge entirely.
func TestMergeSessionShortCircuitSyncsBaseWhenPRAlreadyMerged(t *testing.T) {
	prov := &mergeGateProvider{
		prStatus: &vcs.PRStatus{State: vcs.PRStateMerged},
		checks:   livePassingChecks(),
	}
	wt := &mergePolicyWorktrees{isAncestor: true}
	srv := mergeGateServer(t, prov, vcs.DisplayStatusPassing, withMergeWorktrees(wt))

	if _, err := srv.MergeSession(context.Background(), connect.NewRequest(&pb.MergeSessionRequest{Id: "s1"})); err != nil {
		t.Fatalf("already-merged PR must return success, got %v", err)
	}
	if wt.syncCalls != 1 {
		t.Fatalf("SyncBaseBranch calls = %d, want 1 (the short-circuit must still sync the local base)", wt.syncCalls)
	}
	if prov.mergeCalled {
		t.Fatalf("MergePR must not be called for an already-merged PR (strategies=%v)", prov.mergeStrategies)
	}
}

// TestMergeSessionShortCircuitHardFailsWhenMergeNotOnBase pins the #2222 guard
// on the IDEMPOTENT leg. Without it, the guard survives only the first call:
// merge lands → base is rewritten → call 1 hard-fails CodeInternal → drivers and
// the repair loop treat CodeInternal as retryable → call 2 short-circuits on
// "PR already merged" and reports SUCCESS, silently burying the incident. The
// short-circuit must run the same base-ancestry verification and hard-fail.
func TestMergeSessionShortCircuitHardFailsWhenMergeNotOnBase(t *testing.T) {
	prov := &mergeGateProvider{
		prStatus:       &vcs.PRStatus{State: vcs.PRStateMerged},
		checks:         livePassingChecks(),
		mergeCommitSHA: "deadbeef",
	}
	// Completed check, negative answer: the merge commit is NOT on origin/main.
	wt := &mergePolicyWorktrees{isAncestor: false}
	srv := mergeGateServer(t, prov, vcs.DisplayStatusPassing, withMergeWorktrees(wt))

	_, err := srv.MergeSession(context.Background(), connect.NewRequest(&pb.MergeSessionRequest{Id: "s1"}))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("code = %v, want Internal (err=%v)", connect.CodeOf(err), err)
	}
	if err == nil || !strings.Contains(err.Error(), "merge verification failed") {
		t.Fatalf("error = %v, want it to contain 'merge verification failed'", err)
	}
	if prov.mergeCalled {
		t.Fatalf("MergePR must not be called for an already-merged PR (strategies=%v)", prov.mergeStrategies)
	}
}

// TestMergeSessionShortCircuitAcceptsInfraVerificationFailure is the companion
// asymmetry: on the idempotent leg the provider API has ALREADY confirmed the PR
// is merged, so a verification that could not COMPLETE (here the local fetch)
// adds nothing and must not strand the retry. Only the semantic "merged, but the
// commit is not on base" answer is actionable.
func TestMergeSessionShortCircuitAcceptsInfraVerificationFailure(t *testing.T) {
	prov := &mergeGateProvider{
		prStatus:       &vcs.PRStatus{State: vcs.PRStateMerged},
		checks:         livePassingChecks(),
		mergeCommitSHA: "abc123",
	}
	wt := &mergePolicyWorktrees{fetchBaseErr: errors.New("git fetch: could not resolve host")}
	srv := mergeGateServer(t, prov, vcs.DisplayStatusPassing, withMergeWorktrees(wt))

	if _, err := srv.MergeSession(context.Background(), connect.NewRequest(&pb.MergeSessionRequest{Id: "s1"})); err != nil {
		t.Fatalf("an infra verification failure on an API-confirmed merge must succeed, got %v", err)
	}
	if prov.mergeCalled {
		t.Fatalf("MergePR must not be called for an already-merged PR (strategies=%v)", prov.mergeStrategies)
	}
}

// TestMergeSessionRejectsIncompatibleRebaseStrategy pins the terminal-refusal
// mapping: rebase configured, no squash enabled upstream, and merge commits on
// the branch is a combination that can never succeed. It must surface as
// FailedPrecondition carrying the MERGE_STRATEGY_INCOMPATIBLE token — never
// CodeInternal, which drivers/repair would retry forever.
func TestMergeSessionRejectsIncompatibleRebaseStrategy(t *testing.T) {
	prov := &mergeGateProvider{
		prStatus: openCleanPRStatus(),
		checks:   livePassingChecks(),
		allowed:  []string{"rebase"},
	}
	wt := &mergePolicyWorktrees{mergeCommits: 2, isAncestor: true}
	srv := mergeGateServer(t, prov, vcs.DisplayStatusPassing,
		withRebaseStrategy(), withMergeWorktrees(wt))

	_, err := srv.MergeSession(context.Background(), connect.NewRequest(&pb.MergeSessionRequest{Id: "s1"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition (err=%v)", connect.CodeOf(err), err)
	}
	if err == nil || !strings.Contains(err.Error(), "MERGE_STRATEGY_INCOMPATIBLE") {
		t.Fatalf("error = %v, want it to contain MERGE_STRATEGY_INCOMPATIBLE", err)
	}
	if prov.mergeCalled {
		t.Fatal("an incompatible strategy must be refused before MergePR")
	}
}

// TestMergeSessionSubstitutesSquashForRebase pins the pre-check substitution:
// rebase configured, merge commits present, squash enabled upstream => one
// MergePR call with "squash" and a non-empty response Detail explaining it.
func TestMergeSessionSubstitutesSquashForRebase(t *testing.T) {
	prov := &mergeGateProvider{
		prStatus:       openCleanPRStatus(),
		checks:         livePassingChecks(),
		allowed:        []string{"rebase", "squash"},
		mergeCommitSHA: "abc123",
	}
	wt := &mergePolicyWorktrees{mergeCommits: 1, isAncestor: true}
	srv := mergeGateServer(t, prov, vcs.DisplayStatusPassing,
		withRebaseStrategy(), withMergeWorktrees(wt))

	resp, err := srv.MergeSession(context.Background(), connect.NewRequest(&pb.MergeSessionRequest{Id: "s1"}))
	if err != nil {
		t.Fatalf("expected a successful substituted merge, got %v", err)
	}
	if got := prov.mergeStrategies; len(got) != 1 || got[0] != "squash" {
		t.Fatalf("MergePR strategies = %v, want exactly [squash]", got)
	}
	if resp.Msg.GetDetail() == "" {
		t.Fatal("expected a non-empty Detail describing the strategy substitution")
	}
}

// TestMergeSessionRetriesSquashAfterRebaseRefusal pins the reactive backstop:
// the pre-check saw no merge commits (count 0) so rebase survived, but GitHub
// refuses the rebase anyway. Squash is enabled, so MergeSession retries exactly
// once with squash and succeeds.
func TestMergeSessionRetriesSquashAfterRebaseRefusal(t *testing.T) {
	prov := &mergeGateProvider{
		prStatus: openCleanPRStatus(),
		checks:   livePassingChecks(),
		allowed:  []string{"rebase", "squash"},
		mergeErrByStrategy: map[string]error{
			"rebase": errors.New("GraphQL: This branch can't be rebased"),
		},
		mergeCommitSHA: "abc123",
	}
	wt := &mergePolicyWorktrees{mergeCommits: 0, isAncestor: true}
	srv := mergeGateServer(t, prov, vcs.DisplayStatusPassing,
		withRebaseStrategy(), withMergeWorktrees(wt))

	resp, err := srv.MergeSession(context.Background(), connect.NewRequest(&pb.MergeSessionRequest{Id: "s1"}))
	if err != nil {
		t.Fatalf("expected the squash retry to succeed, got %v", err)
	}
	want := []string{"rebase", "squash"}
	if got := prov.mergeStrategies; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("MergePR strategies = %v, want %v (retry exactly once)", got, want)
	}
	if resp.Msg.GetDetail() == "" {
		t.Fatal("expected a non-empty Detail describing the squash retry")
	}
}

// TestMergeSessionSurfacesBothErrorsWhenSquashRetryFails pins the diagnostic
// contract of the reactive leg's unhappy path. When the squash retry ALSO
// fails, the retry error alone is unreadable in a log: it gives no hint that a
// first merge was attempted with a different strategy, or why. Both failures
// must appear, and the retry error must stay in the errors.Is chain so callers
// can still classify it.
func TestMergeSessionSurfacesBothErrorsWhenSquashRetryFails(t *testing.T) {
	rebaseErr := errors.New("GraphQL: This branch can't be rebased")
	retryErr := errors.New("squash merge blocked by branch protection")
	prov := &mergeGateProvider{
		prStatus: openCleanPRStatus(),
		checks:   livePassingChecks(),
		allowed:  []string{"rebase", "squash"},
		mergeErrByStrategy: map[string]error{
			"rebase": rebaseErr,
			"squash": retryErr,
		},
	}
	// Zero merge commits, so the pre-check leaves rebase in place and the
	// refusal is classified reactively.
	wt := &mergePolicyWorktrees{mergeCommits: 0, isAncestor: true}
	srv := mergeGateServer(t, prov, vcs.DisplayStatusPassing,
		withRebaseStrategy(), withMergeWorktrees(wt))

	_, err := srv.MergeSession(context.Background(), connect.NewRequest(&pb.MergeSessionRequest{Id: "s1"}))
	if err == nil {
		t.Fatal("expected the failed squash retry to surface an error")
	}
	// A failed merge is retryable infrastructure, not a terminal precondition.
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("code = %v, want Internal (err=%v)", connect.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), rebaseErr.Error()) {
		t.Errorf("error = %v, want it to report the original rebase refusal", err)
	}
	if !strings.Contains(err.Error(), retryErr.Error()) {
		t.Errorf("error = %v, want it to report the squash retry failure", err)
	}
	if !errors.Is(err, retryErr) {
		t.Errorf("error = %v, want the retry error to stay in the errors.Is chain", err)
	}
	want := []string{"rebase", "squash"}
	if got := prov.mergeStrategies; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("MergePR strategies = %v, want %v (retry exactly once, no loop)", got, want)
	}
}

// TestMergeSessionRejectsRebaseRefusalWhenSquashDisabled pins the never-
// CodeInternal invariant on the REACTIVE leg. This branch is reachable in
// production whenever the pre-check fails open (CountMergeCommits errored, or a
// merge commit landed between the count and the merge): GitHub refuses the
// rebase and no squash fallback exists upstream, so the combination is terminal
// and must surface as FailedPrecondition carrying MERGE_STRATEGY_INCOMPATIBLE.
// CodeInternal here would make drivers and the repair loop retry forever.
func TestMergeSessionRejectsRebaseRefusalWhenSquashDisabled(t *testing.T) {
	prov := &mergeGateProvider{
		prStatus: openCleanPRStatus(),
		checks:   livePassingChecks(),
		allowed:  []string{"rebase", "merge"},
		mergeErrByStrategy: map[string]error{
			"rebase": errors.New("GraphQL: This branch can't be rebased"),
		},
	}
	// Zero merge commits, so the pre-check leaves rebase in place and the
	// refusal has to be classified reactively.
	wt := &mergePolicyWorktrees{mergeCommits: 0, isAncestor: true}
	srv := mergeGateServer(t, prov, vcs.DisplayStatusPassing,
		withRebaseStrategy(), withMergeWorktrees(wt))

	_, err := srv.MergeSession(context.Background(), connect.NewRequest(&pb.MergeSessionRequest{Id: "s1"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition (err=%v)", connect.CodeOf(err), err)
	}
	if err == nil || !strings.Contains(err.Error(), "MERGE_STRATEGY_INCOMPATIBLE") {
		t.Fatalf("error = %v, want it to contain MERGE_STRATEGY_INCOMPATIBLE", err)
	}
	if got := prov.mergeStrategies; len(got) != 1 || got[0] != "rebase" {
		t.Fatalf("MergePR strategies = %v, want exactly [rebase] (no retry without squash)", got)
	}
}

// TestMergeSessionSurfacesRebaseRefusalWhenStrategiesUnreadable pins the
// conservative branch: the refusal is classifiable, but the follow-up read of
// the enabled strategies fails, so we cannot confirm squash exists. The ORIGINAL
// merge error is surfaced as CodeInternal (today's behaviour, and retryable —
// the read may succeed next time) and no retry is attempted.
func TestMergeSessionSurfacesRebaseRefusalWhenStrategiesUnreadable(t *testing.T) {
	mergeErr := errors.New("GraphQL: This branch can't be rebased")
	prov := &mergeGateProvider{
		prStatus: openCleanPRStatus(),
		checks:   livePassingChecks(),
		// ResolveStrategy falls back to the configured strategy when this
		// read fails, so the merge still runs as rebase.
		allowedErr:         errors.New("gh: 502 Bad Gateway"),
		mergeErrByStrategy: map[string]error{"rebase": mergeErr},
	}
	wt := &mergePolicyWorktrees{mergeCommits: 0, isAncestor: true}
	srv := mergeGateServer(t, prov, vcs.DisplayStatusPassing,
		withRebaseStrategy(), withMergeWorktrees(wt))

	_, err := srv.MergeSession(context.Background(), connect.NewRequest(&pb.MergeSessionRequest{Id: "s1"}))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("code = %v, want Internal (err=%v)", connect.CodeOf(err), err)
	}
	if err == nil || !strings.Contains(err.Error(), mergeErr.Error()) {
		t.Fatalf("error = %v, want it to carry the original merge failure %q", err, mergeErr)
	}
	if got := prov.mergeStrategies; len(got) != 1 || got[0] != "rebase" {
		t.Fatalf("MergePR strategies = %v, want exactly [rebase] (no retry on an unreadable strategy list)", got)
	}
}

// TestMergeSessionAcceptsAPIMergedWhenVerificationInfraFails pins the 3.5
// fallback: verification that could not COMPLETE (an infra failure, here the PR
// merge-commit query) must not strand a merge the provider confirms landed.
func TestMergeSessionAcceptsAPIMergedWhenVerificationInfraFails(t *testing.T) {
	prov := &mergeGateProvider{
		prStatus:       openCleanPRStatus(),
		checks:         livePassingChecks(),
		mergeCommitErr: errors.New("gh: connection reset by peer"),
	}
	// The merge lands remotely: the next PR read reports MERGED.
	prov.onMerge = func() { prov.prStatus.State = vcs.PRStateMerged }
	wt := &mergePolicyWorktrees{isAncestor: true}
	srv := mergeGateServer(t, prov, vcs.DisplayStatusPassing, withMergeWorktrees(wt))

	_, err := srv.MergeSession(context.Background(), connect.NewRequest(&pb.MergeSessionRequest{Id: "s1"}))
	if err != nil {
		t.Fatalf("API-confirmed merge with an infra verification failure must succeed, got %v", err)
	}
	if len(prov.mergeStrategies) != 1 {
		t.Fatalf("MergePR strategies = %v, want exactly one call", prov.mergeStrategies)
	}
	if wt.syncCalls != 1 {
		t.Fatalf("SyncBaseBranch calls = %d, want 1 (the fallback must continue to the base sync)", wt.syncCalls)
	}
}

// TestMergeSessionAcceptsAPIMergedWhenLocalVerificationInfraFails covers the
// other two sources of mergepolicy.ErrMergeVerifyInfra — the local fetch and the
// ancestor check — so the fallback is pinned to the sentinel rather than to the
// one provider-side failure the case above happens to use.
func TestMergeSessionAcceptsAPIMergedWhenLocalVerificationInfraFails(t *testing.T) {
	cases := []struct {
		name string
		wt   *mergePolicyWorktrees
	}{
		{
			name: "fetch base fails",
			wt:   &mergePolicyWorktrees{fetchBaseErr: errors.New("git fetch: could not resolve host")},
		},
		{
			name: "ancestor check fails",
			wt:   &mergePolicyWorktrees{isAncestorErr: errors.New("git merge-base: bad object")},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := &mergeGateProvider{
				prStatus:       openCleanPRStatus(),
				checks:         livePassingChecks(),
				mergeCommitSHA: "abc123",
			}
			prov.onMerge = func() { prov.prStatus.State = vcs.PRStateMerged }
			srv := mergeGateServer(t, prov, vcs.DisplayStatusPassing, withMergeWorktrees(tc.wt))

			if _, err := srv.MergeSession(context.Background(), connect.NewRequest(&pb.MergeSessionRequest{Id: "s1"})); err != nil {
				t.Fatalf("API-confirmed merge with an infra verification failure must succeed, got %v", err)
			}
			if tc.wt.syncCalls != 1 {
				t.Fatalf("SyncBaseBranch calls = %d, want 1 (the fallback must continue to the base sync)", tc.wt.syncCalls)
			}
		})
	}
}

// TestMergeSessionHardFailsWhenMergeNotOnBase pins the guard the fallback must
// NOT weaken: a COMPLETED verification with a negative answer (merge commit is
// not an ancestor of origin/<base>) is the madverts-core PR #2222 incident
// class. It stays a hard CodeInternal failure even though the provider reports
// the PR as merged.
func TestMergeSessionHardFailsWhenMergeNotOnBase(t *testing.T) {
	prov := &mergeGateProvider{
		prStatus:       openCleanPRStatus(),
		checks:         livePassingChecks(),
		mergeCommitSHA: "deadbeef",
	}
	prov.onMerge = func() { prov.prStatus.State = vcs.PRStateMerged }
	wt := &mergePolicyWorktrees{isAncestor: false}
	srv := mergeGateServer(t, prov, vcs.DisplayStatusPassing, withMergeWorktrees(wt))

	_, err := srv.MergeSession(context.Background(), connect.NewRequest(&pb.MergeSessionRequest{Id: "s1"}))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("code = %v, want Internal (err=%v)", connect.CodeOf(err), err)
	}
	if err == nil || !strings.Contains(err.Error(), "merge verification failed") {
		t.Fatalf("error = %v, want it to contain 'merge verification failed'", err)
	}
}
