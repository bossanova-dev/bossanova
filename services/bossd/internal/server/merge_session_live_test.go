package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

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
	prStatus *vcs.PRStatus
	checks   []vcs.CheckResult
	reviews  []vcs.ReviewComment
	// prStatusErr/checksErr/reviewsErr make each of the gate's three live reads
	// individually fail, so the fail-closed contract can be asserted per read
	// rather than only in aggregate (BOS-644).
	prStatusErr error
	checksErr   error
	reviewsErr  error
	mergeErr    error
	mergeCalled bool
	// onMerge, when set, fires inside MergePR before it returns — a seam for
	// observing display-tracker state at the exact moment the blocking merge runs.
	onMerge func()
	// onPRStatus, when set, fires inside GetPRStatus before it returns. It is
	// the seam for killing the context DURING the gate reads: MergeSession
	// rejects an already-dead context up front in acquireRepoMerge, so a test
	// that merely passes a canceled context never reaches the gate at all and
	// would assert the merge-lock wait instead of the gate.
	onPRStatus func(context.Context)

	// allowed overrides the merge strategies the remote reports as enabled.
	// nil keeps the historical default of []string{"merge"}.
	allowed    []string
	allowedErr error
	// mergeStrategies records the strategy passed to every MergePR call, in
	// order, so substitution and the one-shot squash retry can be asserted
	// exactly rather than by call count alone.
	mergeStrategies []string
	// mergeOpts records the full MergePROpts of every MergePR call, in order,
	// so the head pin (BOS-1381) can be asserted on each attempt.
	mergeOpts []vcs.MergePROpts
	// prStatusCalls counts GetPRStatus reads.
	prStatusCalls int
	// checksCalls counts GetCheckResults reads, so a head-mismatch pre-check
	// can be shown to skip the remaining gate reads.
	checksCalls int
	// mergeErrByStrategy lets a single test fail one strategy and succeed on
	// another (the rebase-refused → squash-retry case). It takes precedence
	// over mergeErr when the strategy has an entry.
	mergeErrByStrategy map[string]error
	// mergeCommitSHA/mergeCommitErr drive VerifyOnBase. Both zero keeps the
	// historical default of ("", vcs.ErrPRNotMerged).
	mergeCommitSHA string
	mergeCommitErr error

	// observation/observationErr drive GetReviewObservation, the RAW
	// pre-filtering review tally. It is deliberately independent of `reviews`
	// above: `reviews` is the ALREADY-FILTERED set the gate judges, and the real
	// provider drops addressed bot reviews from it. Keeping the two separate is
	// what lets a test express the shape that a fake returning one slice for
	// both cannot — a bot that reviewed and was fully addressed, which the
	// filter removes but the observation still counts.
	observation    vcs.ReviewObservation
	observationErr error
	// onObservation, when set, fires inside GetReviewObservation. The
	// observation is a fourth context-bound read running AFTER the three
	// guarded ones, so this is the only seam that can kill the context at that
	// specific point.
	onObservation func(context.Context)
}

func (p *mergeGateProvider) GetPRStatus(ctx context.Context, _ string, _ int) (*vcs.PRStatus, error) {
	p.prStatusCalls++
	if p.onPRStatus != nil {
		p.onPRStatus(ctx)
	}
	return p.prStatus, p.prStatusErr
}
func (p *mergeGateProvider) GetCheckResults(context.Context, string, int) ([]vcs.CheckResult, error) {
	p.checksCalls++
	return p.checks, p.checksErr
}
func (p *mergeGateProvider) GetReviewComments(context.Context, string, int) ([]vcs.ReviewComment, error) {
	return p.reviews, p.reviewsErr
}

// GetReviewObservation makes the fake satisfy vcs.ReviewObserver, the optional
// capability the no-reviews-observed warning reads instead of counting bots in
// the filtered GetReviewComments result.
func (p *mergeGateProvider) GetReviewObservation(ctx context.Context, _ string, _ int) (vcs.ReviewObservation, error) {
	if p.onObservation != nil {
		p.onObservation(ctx)
	}
	return p.observation, p.observationErr
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
func (p *mergeGateProvider) MergePR(_ context.Context, _ string, _ int, opts vcs.MergePROpts) error {
	strategy := opts.Strategy
	p.mergeCalled = true
	p.mergeStrategies = append(p.mergeStrategies, strategy)
	p.mergeOpts = append(p.mergeOpts, opts)
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

// withMergeLogger swaps the server's zerolog.Nop() for one writing into buf, so
// a test can assert on a log line the gate emits. The merge gate's
// no-bot-review signal is log-only (MergeSessionResponse.detail is discarded by
// every RPC consumer), so the log is the only observable it has.
func withMergeLogger(buf *bytes.Buffer) mergeGateOpt {
	return func(s *Server, _ *models.Repo) { s.logger = zerolog.New(buf) }
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

// TestMergeSessionVerifyStatusesTreatedAsChecking pins BOS-1382's merge-gate
// treatment: a pending boss/verify status yields DisplayStatusVerifying or
// DisplayStatusNeedsHuman, which replace the Checking it used to produce and
// are treated exactly as that Checking was — they do not block an explicit
// merge — while failing CI beside a pending verify still blocks as gate=ci.
func TestMergeSessionVerifyStatusesTreatedAsChecking(t *testing.T) {
	green := vcs.CheckResult{
		Name:       "build",
		Status:     vcs.CheckStatusCompleted,
		Conclusion: checkConclusionPtr(vcs.CheckConclusionSuccess),
	}
	failed := vcs.CheckResult{
		Name:       "build",
		Status:     vcs.CheckStatusCompleted,
		Conclusion: checkConclusionPtr(vcs.CheckConclusionFailure),
	}
	verify := func(desc string) vcs.CheckResult {
		return vcs.CheckResult{Name: vcs.VerifyStatusContext, Status: vcs.CheckStatusQueued, Description: desc}
	}
	open := func(mss vcs.MergeStateStatus) *vcs.PRStatus {
		return &vcs.PRStatus{State: vcs.PRStateOpen, Mergeable: boolPtr(true), MergeStateStatus: mss}
	}

	cases := []struct {
		name        string
		prStatus    *vcs.PRStatus
		checks      []vcs.CheckResult
		reviews     []vcs.ReviewComment
		wantStatus  vcs.DisplayStatus
		wantBlocked string // "" = merge proceeds; otherwise the expected gate prefix
	}{
		{
			name:       "green CI with verifying claim is not blocked",
			prStatus:   open(vcs.MergeStateStatusClean),
			checks:     []vcs.CheckResult{green, verify("verifying… 3f2a")},
			wantStatus: vcs.DisplayStatusVerifying,
		},
		{
			name:       "green CI with needs-human park is not blocked",
			prStatus:   open(vcs.MergeStateStatusClean),
			checks:     []vcs.CheckResult{green, verify("needs human: always-human-path")},
			wantStatus: vcs.DisplayStatusNeedsHuman,
		},
		{
			name:        "failing CI with pending verify is still gate=ci",
			prStatus:    open(vcs.MergeStateStatusUnstable),
			checks:      []vcs.CheckResult{failed, verify("needs human: x")},
			wantStatus:  vcs.DisplayStatusFailing,
			wantBlocked: "merge blocked: gate=ci;",
		},
		{
			name:       "changes requested with pending verify stays unblocked like checking",
			prStatus:   open(vcs.MergeStateStatusBlocked),
			checks:     []vcs.CheckResult{green, verify("verifying… 3f2a")},
			reviews:    []vcs.ReviewComment{{Author: "reviewer", State: vcs.ReviewStateChangesRequested}},
			wantStatus: vcs.DisplayStatusVerifying,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := vcs.ComputeDisplayStatus(tc.prStatus, tc.checks, tc.reviews).Status; got != tc.wantStatus {
				t.Fatalf("precondition: display status = %d, want %d", got, tc.wantStatus)
			}
			prov := &mergeGateProvider{
				prStatus: tc.prStatus,
				checks:   tc.checks,
				reviews:  tc.reviews,
				mergeErr: errors.New("merge short-circuited in test"),
			}
			srv := mergeGateServer(t, prov, vcs.DisplayStatusPassing)

			_, err := srv.MergeSession(context.Background(), connect.NewRequest(&pb.MergeSessionRequest{Id: "s1"}))
			if tc.wantBlocked == "" {
				if connect.CodeOf(err) == connect.CodeFailedPrecondition {
					t.Fatalf("merge gate blocked a pending-verify head it must treat like checking: %v", err)
				}
				if !prov.mergeCalled {
					t.Fatal("expected execution to reach the actual merge (MergePR)")
				}
				return
			}
			if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), tc.wantBlocked) {
				t.Fatalf("err = %v, want FailedPrecondition containing %q", err, tc.wantBlocked)
			}
			if prov.mergeCalled {
				t.Fatal("a blocked PR must not reach the actual merge")
			}
		})
	}
}

// TestMergeSessionRejectsUnverifiableLiveRead pins the fail-CLOSED contract
// (BOS-644). The gate judges a PR from three live provider reads; before this
// change every one of them discarded its error and fell through to `return nil`
// — "no block". A GitHub outage, an expired token or a malformed response
// therefore did not hold the merge, it *silently removed the gate*, which is the
// one moment the gate matters most. Each read failing on its own must now block
// with gate=pending and never reach MergePR. The nil-status-nil-error case is
// listed separately because it is not an error at all: the provider reports
// success with nothing to judge, which is equally unverifiable.
func TestMergeSessionRejectsUnverifiableLiveRead(t *testing.T) {
	cases := []struct {
		name string
		prov *mergeGateProvider
		// wantRead is the read named in the block detail, so a future
		// refactor that swaps the reads' order can't silently mislabel them.
		wantRead string
	}{
		{
			name:     "PR status read fails",
			prov:     &mergeGateProvider{prStatusErr: errors.New("gh: HTTP 502")},
			wantRead: "could not read the PR status",
		},
		{
			name: "PR status read returns nothing",
			prov: &mergeGateProvider{
				prStatus: nil,
				checks:   livePassingChecks(),
			},
			wantRead: "could not read the PR status",
		},
		{
			name: "check results read fails",
			prov: &mergeGateProvider{
				prStatus:  openCleanPRStatus(),
				checksErr: errors.New("gh: HTTP 502"),
			},
			wantRead: "could not read the check results",
		},
		{
			name: "review comments read fails",
			prov: &mergeGateProvider{
				prStatus:   openCleanPRStatus(),
				checks:     livePassingChecks(),
				reviewsErr: vcs.ErrReviewThreadsUnverified,
			},
			wantRead: "could not read the review comments",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A stale *Passing* tracker entry is the trap: if the gate fell
			// back to the tracker instead of blocking, this would merge.
			srv := mergeGateServer(t, tc.prov, vcs.DisplayStatusPassing)

			_, err := srv.MergeSession(context.Background(), connect.NewRequest(&pb.MergeSessionRequest{Id: "s1"}))
			if connect.CodeOf(err) != connect.CodeFailedPrecondition {
				t.Fatalf("code = %v, want FailedPrecondition (err=%v)", connect.CodeOf(err), err)
			}
			if err == nil || !strings.Contains(err.Error(), "merge blocked: gate=pending;") {
				t.Fatalf("error = %v, want it to contain 'merge blocked: gate=pending;'", err)
			}
			if !strings.Contains(err.Error(), tc.wantRead) {
				t.Errorf("error = %v, want it to name the failed read (%q)", err, tc.wantRead)
			}
			if tc.prov.mergeCalled {
				t.Fatal("an unverifiable PR state must not reach the actual merge")
			}
		})
	}
}

// TestLiveMergeBlockSkipsWithoutProviderOrOrigin pins the two inputs that are
// legitimately unreadable and must stay non-blocking: a server with no provider
// configured, and a repo with no origin URL. Neither is a failed read — there is
// no remote to consult at all — so the fail-closed change above must not turn
// them into a permanent block on every such repo. Called directly because
// MergeSession's own callers never reach the gate without a provider.
func TestLiveMergeBlockSkipsWithoutProviderOrOrigin(t *testing.T) {
	t.Run("no provider", func(t *testing.T) {
		srv := &Server{logger: zerolog.Nop()}
		block, err := srv.liveMergeBlock(context.Background(), "https://github.com/acme/repo", 42, "")
		if err != nil {
			t.Fatalf("liveMergeBlock err = %v, want nil when no provider is configured", err)
		}
		if block != nil {
			t.Fatalf("liveMergeBlock = %+v, want nil when no provider is configured", block)
		}
	})
	t.Run("no origin URL", func(t *testing.T) {
		prov := &mergeGateProvider{prStatusErr: errors.New("must never be called")}
		srv := &Server{provider: prov, logger: zerolog.Nop()}
		block, err := srv.liveMergeBlock(context.Background(), "", 42, "")
		if err != nil {
			t.Fatalf("liveMergeBlock err = %v, want nil when the repo has no origin URL", err)
		}
		if block != nil {
			t.Fatalf("liveMergeBlock = %+v, want nil when the repo has no origin URL", block)
		}
	})
}

// TestMergeSessionWarnsWhenNoBotReviewObserved pins the observability half of
// BOS-644. The review gate can only block on evidence an external bot produced,
// so a bot that was uninstalled, rate-limited or silently crashed leaves the
// gate with nothing to block on — and the resulting merge is indistinguishable
// in the logs from one that passed a real review. The WARN is what makes that
// difference visible. It keys on *bot* reviews, so a human approval alongside
// zero bot reviews still warns: that is precisely the silent-loss case.
func TestMergeSessionWarnsWhenNoBotReviewObserved(t *testing.T) {
	cases := []struct {
		name string
		// reviews is the FILTERED set the gate judges; observation is the RAW
		// pre-filtering tally. They differ exactly where the real provider drops
		// a review, which is the point of several cases below.
		reviews     []vcs.ReviewComment
		observation vcs.ReviewObservation
		wantWarn    bool
		wantReviews string
	}{
		{
			name:        "no reviews at all",
			observation: vcs.ReviewObservation{},
			wantWarn:    true,
			wantReviews: `"reviews_total":0`,
		},
		{
			name:        "human approval but no bot review",
			reviews:     []vcs.ReviewComment{{Author: "alice", State: vcs.ReviewStateApproved}},
			observation: vcs.ReviewObservation{Total: 1},
			wantWarn:    true,
			wantReviews: `"reviews_total":1`,
		},
		{
			name: "bot review observed",
			reviews: []vcs.ReviewComment{
				{Author: "alice", State: vcs.ReviewStateApproved},
				{Author: "cursor[bot]", State: vcs.ReviewStateCommented},
			},
			observation: vcs.ReviewObservation{Total: 2, Bot: 1},
			wantWarn:    false,
		},
		{
			// The regression this warning shipped with: a bot reviewed and every
			// one of its threads was then resolved, so GetReviewComments DROPS
			// the review and the filtered set has zero bot entries — while the
			// bot demonstrably ran. Counting bots in `reviews` warns here, which
			// means warning on the healthy, fully-addressed, ready-to-merge PR
			// and never distinguishing the absent-reviewer case at all.
			name:        "bot reviewed but its addressed review was filtered out",
			reviews:     []vcs.ReviewComment{{Author: "alice", State: vcs.ReviewStateApproved}},
			observation: vcs.ReviewObservation{Total: 2, Bot: 1},
			wantWarn:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := &mergeGateProvider{
				prStatus:    openCleanPRStatus(),
				checks:      livePassingChecks(),
				reviews:     tc.reviews,
				observation: tc.observation,
				mergeErr:    errors.New("merge short-circuited in test"),
			}
			var logs bytes.Buffer
			srv := mergeGateServer(t, prov, vcs.DisplayStatusPassing, withMergeLogger(&logs))

			_, err := srv.MergeSession(context.Background(), connect.NewRequest(&pb.MergeSessionRequest{Id: "s1"}))
			if connect.CodeOf(err) == connect.CodeFailedPrecondition {
				t.Fatalf("green PR was rejected by the merge gate: %v", err)
			}
			if !prov.mergeCalled {
				t.Fatal("expected execution to reach the actual merge (MergePR), but the gate blocked it")
			}

			gotWarn := strings.Contains(logs.String(), `"review_gate":"no_reviews_observed"`)
			if gotWarn != tc.wantWarn {
				t.Fatalf("review_gate=no_reviews_observed logged = %t, want %t (logs: %s)", gotWarn, tc.wantWarn, logs.String())
			}
			if !tc.wantWarn {
				return
			}
			for _, want := range []string{tc.wantReviews, `"bot_reviews_total":0`, `"level":"warn"`} {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("logs = %s, want them to contain %q", logs.String(), want)
				}
			}
		})
	}
}

// TestMergeSessionPreservesCanceledContextAtGate pins that a context that dies
// mid-gate is reported as the cancellation it is. The three gate reads are
// context-bound, so a caller that hangs up or hits its deadline makes them
// error — and treating that as an unverifiable provider read would answer a
// canceled request with FailedPrecondition plus advice to retry once the
// provider is reachable, discarding the Canceled / DeadlineExceeded semantics
// MergeSession preserves while queued behind the repo merge lock.
//
// The context MUST still be alive when MergeSession is entered and die inside
// the gate: acquireRepoMerge rejects an already-dead context before contending
// for the merge lock, so passing a pre-canceled context asserts that guard
// rather than this one and passes even with the fix reverted.
func TestMergeSessionPreservesCanceledContextAtGate(t *testing.T) {
	cases := []struct {
		name     string
		ctx      func(t *testing.T) (context.Context, func(context.Context))
		wantCode connect.Code
	}{
		{
			name: "canceled",
			ctx: func(t *testing.T) (context.Context, func(context.Context)) {
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				// Cancel from inside the gate's first read, the way a caller
				// hanging up kills an in-flight context-bound `gh` call.
				return ctx, func(context.Context) { cancel() }
			},
			wantCode: connect.CodeCanceled,
		},
		{
			name: "deadline exceeded",
			ctx: func(t *testing.T) (context.Context, func(context.Context)) {
				ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
				t.Cleanup(cancel)
				// Block inside the read until the deadline actually expires.
				return ctx, func(c context.Context) { <-c.Done() }
			},
			wantCode: connect.CodeDeadlineExceeded,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, kill := tc.ctx(t)
			// The read then fails the way a killed context-bound `gh` call
			// fails: a non-nil error, with ctx.Err() already set.
			prov := &mergeGateProvider{
				onPRStatus:  kill,
				prStatusErr: errors.New("signal: killed"),
			}
			srv := mergeGateServer(t, prov, vcs.DisplayStatusPassing)

			_, err := srv.MergeSession(ctx, connect.NewRequest(&pb.MergeSessionRequest{Id: "s1"}))
			if got := connect.CodeOf(err); got != tc.wantCode {
				t.Fatalf("MergeSession code = %v, want %v (err: %v)", got, tc.wantCode, err)
			}
			// The failure must not be reported as an unverifiable provider read
			// telling a caller who hung up to retry when the provider recovers.
			if strings.Contains(fmt.Sprint(err), "gate=pending") {
				t.Errorf("canceled gate read reported as gate=pending: %v", err)
			}
			if prov.mergeCalled {
				t.Fatal("a canceled gate read must never fall through to MergePR")
			}
		})
	}
}

// TestMergeSessionObservationErrorDoesNotBlockMerge pins that the raw review
// observation stays OBSERVABILITY, never a fourth gate read. It is not part of
// the fail-closed decision: an unreadable tally means "we cannot tell whether a
// reviewer ran", which is not evidence that the PR is unmergeable, so a green
// PR whose observation read fails must still merge — and must not claim a
// reviewer was absent when it simply could not look.
func TestMergeSessionObservationErrorDoesNotBlockMerge(t *testing.T) {
	prov := &mergeGateProvider{
		prStatus:       openCleanPRStatus(),
		checks:         livePassingChecks(),
		observationErr: errors.New("gh: rate limited"),
		mergeErr:       errors.New("merge short-circuited in test"),
	}
	var logs bytes.Buffer
	srv := mergeGateServer(t, prov, vcs.DisplayStatusPassing, withMergeLogger(&logs))

	_, err := srv.MergeSession(context.Background(), connect.NewRequest(&pb.MergeSessionRequest{Id: "s1"}))
	if connect.CodeOf(err) == connect.CodeFailedPrecondition {
		t.Fatalf("an unreadable review observation blocked a green PR: %v", err)
	}
	if !prov.mergeCalled {
		t.Fatal("expected execution to reach MergePR despite the observation read failing")
	}
	if strings.Contains(logs.String(), `"review_gate":"no_reviews_observed"`) {
		t.Errorf("an unreadable observation must not be reported as an absent reviewer (logs: %s)", logs.String())
	}
}

// TestMergeSessionPreservesCancellationAfterObservationRead covers the gap the
// observation read itself opened. It is a FOURTH context-bound provider read,
// running after the three guarded ones, and it deliberately swallows its own
// errors so an unreadable tally cannot block an approved merge. A dead context
// is not an ordinary failure though: swallowing it let the merge run on with an
// expired context until MergePR failed and surfaced as CodeInternal — the exact
// misclassification the gate's cancellation handling exists to prevent.
//
// All three gate reads succeed here, so only the observation can be responsible
// for the cancellation.
func TestMergeSessionPreservesCancellationAfterObservationRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	prov := &mergeGateProvider{
		prStatus: openCleanPRStatus(),
		checks:   livePassingChecks(),
		// The observation dies with the caller, and reports an ordinary error
		// alongside it — exactly what a killed context-bound `gh` call returns.
		onObservation:  func(context.Context) { cancel() },
		observationErr: errors.New("signal: killed"),
		mergeErr:       errors.New("MergePR must never be reached"),
	}
	srv := mergeGateServer(t, prov, vcs.DisplayStatusPassing)

	_, err := srv.MergeSession(ctx, connect.NewRequest(&pb.MergeSessionRequest{Id: "s1"}))
	if got := connect.CodeOf(err); got != connect.CodeCanceled {
		t.Fatalf("MergeSession code = %v, want %v (err: %v)", got, connect.CodeCanceled, err)
	}
	if prov.mergeCalled {
		t.Fatal("a merge must not proceed on an expired context after the observation read")
	}
}

// TestMergeSessionOrdinaryObservationFailureStillMerges is the other half of the
// pair above: with the context ALIVE, an observation error stays swallowed and
// the merge proceeds. Without this, "preserve cancellation" could be satisfied
// by blocking on every observation failure, which would turn a log-only signal
// into a fourth gate read.
func TestMergeSessionOrdinaryObservationFailureStillMerges(t *testing.T) {
	prov := &mergeGateProvider{
		prStatus:       openCleanPRStatus(),
		checks:         livePassingChecks(),
		observationErr: errors.New("gh: rate limited"),
		mergeErr:       errors.New("merge short-circuited in test"),
	}
	srv := mergeGateServer(t, prov, vcs.DisplayStatusPassing)

	_, err := srv.MergeSession(context.Background(), connect.NewRequest(&pb.MergeSessionRequest{Id: "s1"}))
	if code := connect.CodeOf(err); code == connect.CodeCanceled || code == connect.CodeDeadlineExceeded {
		t.Fatalf("a live-context observation failure was reported as cancellation: %v", err)
	}
	if !prov.mergeCalled {
		t.Fatal("an ordinary observation failure must not block the merge")
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

// headPin is a valid 40-hex head SHA for the BOS-1381 head-pinned merge tests.
var headPin = strings.Repeat("0a1b", 10)

func pinnedMergeRequest(pin string) *connect.Request[pb.MergeSessionRequest] {
	return connect.NewRequest(&pb.MergeSessionRequest{Id: "s1", ExpectedHeadSha: pin})
}

// TestMergeSessionHeadPinPreCheckRefusesMovedHead: when the live PR head
// differs from the pin, the RPC refuses with a HEAD_MISMATCH FailedPrecondition
// before any merge, and skips the gate's remaining reads.
func TestMergeSessionHeadPinPreCheckRefusesMovedHead(t *testing.T) {
	live := strings.Repeat("ffee", 10)
	st := openCleanPRStatus()
	st.HeadSHA = live
	prov := &mergeGateProvider{prStatus: st, checks: livePassingChecks()}
	srv := mergeGateServer(t, prov, vcs.DisplayStatusPassing)

	_, err := srv.MergeSession(context.Background(), pinnedMergeRequest(headPin))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition (err=%v)", connect.CodeOf(err), err)
	}
	for _, want := range []string{"HEAD_MISMATCH", headPin, live} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
	if prov.mergeCalled {
		t.Fatal("MergePR was called despite a known head mismatch")
	}
	// The pre-gate display refresh also reads checks, so compare against an
	// unpinned run of the same setup: the pinned run must make one fewer
	// check read (the gate's own) because the mismatch short-circuits it.
	unpinned := &mergeGateProvider{prStatus: st, checks: livePassingChecks(), mergeErr: errors.New("merge short-circuited in test")}
	_, _ = mergeGateServer(t, unpinned, vcs.DisplayStatusPassing).MergeSession(context.Background(), pinnedMergeRequest(""))
	if prov.checksCalls != unpinned.checksCalls-1 {
		t.Errorf("GetCheckResults: pinned %d reads, unpinned %d; a head mismatch must skip the gate's remaining reads", prov.checksCalls, unpinned.checksCalls)
	}
}

// TestMergeSessionHeadPinRefusesAlreadyMergedAtOtherHead: the already-merged
// short-circuit must honour the pin. A PR merged upstream at a head other than
// the pin is a HEAD_MISMATCH refusal, not an idempotent success; a PR merged at
// the pinned head stays a success. Neither calls MergePR.
func TestMergeSessionHeadPinRefusesAlreadyMergedAtOtherHead(t *testing.T) {
	merged := strings.Repeat("ffee", 10)
	prov := &mergeGateProvider{
		prStatus: &vcs.PRStatus{State: vcs.PRStateMerged, HeadSHA: merged},
		checks:   livePassingChecks(),
	}
	srv := mergeGateServer(t, prov, vcs.DisplayStatusPassing)

	_, err := srv.MergeSession(context.Background(), pinnedMergeRequest(headPin))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition (err=%v)", connect.CodeOf(err), err)
	}
	for _, want := range []string{"HEAD_MISMATCH", headPin, merged} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
	if prov.mergeCalled {
		t.Fatal("MergePR was called for an already-merged PR")
	}

	same := &mergeGateProvider{
		prStatus: &vcs.PRStatus{State: vcs.PRStateMerged, HeadSHA: strings.ToUpper(headPin)},
		checks:   livePassingChecks(),
	}
	if _, err := mergeGateServer(t, same, vcs.DisplayStatusPassing).MergeSession(context.Background(), pinnedMergeRequest(headPin)); err != nil {
		t.Fatalf("PR already merged at the pinned head must succeed, got %v", err)
	}
	if same.mergeCalled {
		t.Fatal("MergePR was called for an already-merged PR")
	}
}

// TestMergeSessionHeadPinBeatsGateBlock: the gate's verdict is about a
// different head, so a known mismatch takes precedence over a red gate.
func TestMergeSessionHeadPinBeatsGateBlock(t *testing.T) {
	st := openCleanPRStatus()
	st.HeadSHA = strings.Repeat("ffee", 10)
	prov := &mergeGateProvider{prStatus: st, checks: []vcs.CheckResult{{
		Status:     vcs.CheckStatusCompleted,
		Conclusion: checkConclusionPtr(vcs.CheckConclusionFailure),
	}}}
	srv := mergeGateServer(t, prov, vcs.DisplayStatusPassing)

	_, err := srv.MergeSession(context.Background(), pinnedMergeRequest(headPin))
	if err == nil || !strings.Contains(err.Error(), "HEAD_MISMATCH") {
		t.Fatalf("err = %v, want a HEAD_MISMATCH refusal", err)
	}
	if strings.Contains(err.Error(), "merge blocked: gate=") {
		t.Errorf("err = %v, want the head mismatch, not the gate block", err)
	}
}

// TestMergeSessionHeadPinThreadsToMergePR: a matching pin (in any case) and an
// unknown live head both proceed, and MergePR receives the normalized pin.
func TestMergeSessionHeadPinThreadsToMergePR(t *testing.T) {
	for name, liveHead := range map[string]string{"matching live head": headPin, "unknown live head": ""} {
		t.Run(name, func(t *testing.T) {
			st := openCleanPRStatus()
			st.HeadSHA = liveHead
			prov := &mergeGateProvider{prStatus: st, checks: livePassingChecks(), mergeCommitSHA: "abc123"}
			srv := mergeGateServer(t, prov, vcs.DisplayStatusPassing,
				withMergeWorktrees(&mergePolicyWorktrees{isAncestor: true}))

			if _, err := srv.MergeSession(context.Background(), pinnedMergeRequest(strings.ToUpper(headPin))); err != nil {
				t.Fatalf("MergeSession: %v", err)
			}
			if len(prov.mergeOpts) != 1 || prov.mergeOpts[0].ExpectedHeadSHA != headPin {
				t.Fatalf("MergePR opts = %+v, want one call with ExpectedHeadSHA %s", prov.mergeOpts, headPin)
			}
		})
	}
}

// TestMergeSessionHeadPinCarriedIntoSquashRetry: the rebase-refused squash
// retry carries the same pin as the first attempt.
func TestMergeSessionHeadPinCarriedIntoSquashRetry(t *testing.T) {
	st := openCleanPRStatus()
	st.HeadSHA = headPin
	prov := &mergeGateProvider{
		prStatus: st,
		checks:   livePassingChecks(),
		allowed:  []string{"rebase", "squash"},
		mergeErrByStrategy: map[string]error{
			"rebase": errors.New("GraphQL: This branch can't be rebased"),
		},
		mergeCommitSHA: "abc123",
	}
	srv := mergeGateServer(t, prov, vcs.DisplayStatusPassing,
		withRebaseStrategy(), withMergeWorktrees(&mergePolicyWorktrees{isAncestor: true}))

	if _, err := srv.MergeSession(context.Background(), pinnedMergeRequest(headPin)); err != nil {
		t.Fatalf("expected the squash retry to succeed, got %v", err)
	}
	if len(prov.mergeOpts) != 2 {
		t.Fatalf("MergePR calls = %+v, want 2 (rebase then squash)", prov.mergeOpts)
	}
	for i, o := range prov.mergeOpts {
		if o.ExpectedHeadSHA != headPin {
			t.Errorf("call %d (%s) ExpectedHeadSHA = %q, want %q", i, o.Strategy, o.ExpectedHeadSHA, headPin)
		}
	}
}

// TestMergeSessionRemoteHeadMismatchIsFailedPrecondition: a MergePR failure
// wrapping vcs.ErrHeadMismatch maps to FailedPrecondition with the token and
// the re-read live head, never CodeInternal, and is never retried with squash.
func TestMergeSessionRemoteHeadMismatchIsFailedPrecondition(t *testing.T) {
	st := openCleanPRStatus()
	st.HeadSHA = headPin
	prov := &mergeGateProvider{
		prStatus: st,
		checks:   livePassingChecks(),
		allowed:  []string{"rebase", "squash"},
		mergeErrByStrategy: map[string]error{
			"rebase": fmt.Errorf("merge PR: %w: GraphQL: Head branch was modified. Review and try the merge again. (mergePullRequest)", vcs.ErrHeadMismatch),
		},
	}
	srv := mergeGateServer(t, prov, vcs.DisplayStatusPassing,
		withRebaseStrategy(), withMergeWorktrees(&mergePolicyWorktrees{isAncestor: true}))

	_, err := srv.MergeSession(context.Background(), pinnedMergeRequest(headPin))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition (err=%v)", connect.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "HEAD_MISMATCH") || !strings.Contains(err.Error(), "live head "+headPin) {
		t.Errorf("err = %v, want HEAD_MISMATCH with the re-read live head", err)
	}
	if !errors.Is(err, vcs.ErrHeadMismatch) {
		t.Errorf("err = %v, want vcs.ErrHeadMismatch in the chain", err)
	}
	if got := prov.mergeStrategies; len(got) != 1 {
		t.Fatalf("MergePR strategies = %v, want exactly one attempt (no squash retry)", got)
	}
}

// TestMergeSessionRejectsInvalidHeadPin: a malformed pin is InvalidArgument
// before any read.
func TestMergeSessionRejectsInvalidHeadPin(t *testing.T) {
	for _, pin := range []string{"abc", headPin[:39], headPin + "0", strings.Repeat("x", 40)} {
		prov := &mergeGateProvider{prStatus: openCleanPRStatus()}
		srv := mergeGateServer(t, prov, vcs.DisplayStatusPassing)
		_, err := srv.MergeSession(context.Background(), pinnedMergeRequest(pin))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("pin %q: code = %v, want InvalidArgument (err=%v)", pin, connect.CodeOf(err), err)
		}
		if prov.prStatusCalls != 0 || prov.mergeCalled {
			t.Errorf("pin %q: provider was read (%d status reads, merge=%v)", pin, prov.prStatusCalls, prov.mergeCalled)
		}
	}
}

// TestMergeSessionHeadPinRefusedForLocalOnlyMerge: a session without a PR has
// no remote head to pin, so a pinned request fails closed without merging.
func TestMergeSessionHeadPinRefusedForLocalOnlyMerge(t *testing.T) {
	sess := blockedFixLoopSession()
	sess.PRNumber = nil
	merged := false
	srv := mergeGateServer(t, &mergeGateProvider{}, vcs.DisplayStatusPassing,
		withMergeWorktrees(&mergeLocalWorktrees{onMerge: func() { merged = true }}))
	srv.sessions = &lifecycleSessionStoreFake{session: sess}

	_, err := srv.MergeSession(context.Background(), pinnedMergeRequest(headPin))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition (err=%v)", connect.CodeOf(err), err)
	}
	if strings.Contains(err.Error(), "HEAD_MISMATCH") {
		t.Errorf("err = %v; nothing moved, so it must not carry HEAD_MISMATCH", err)
	}
	if merged {
		t.Fatal("MergeLocalBranch was called for a pinned local-only merge")
	}
}
