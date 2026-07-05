package server

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sessionreason"
	"github.com/recurser/bossalib/vcs"
	"github.com/rs/zerolog"

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
	return []string{"merge"}, nil
}
func (p *mergeGateProvider) MergePR(context.Context, string, int, string) error {
	p.mergeCalled = true
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
func (p *mergeGateProvider) UpdatePRTitle(context.Context, string, int, string) error { return nil }
func (p *mergeGateProvider) GetPRMergeCommit(context.Context, string, int) (string, error) {
	return "", vcs.ErrPRNotMerged
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

func mergeGateServer(t *testing.T, prov *mergeGateProvider, staleStatus vcs.DisplayStatus) *Server {
	t.Helper()
	tracker := status.NewDisplayTracker()
	// Seed a STALE, non-Passing tracker entry — the old gate would veto the
	// merge on this alone. The live gate must ignore it.
	tracker.Set("s1", vcs.DisplayInfo{Status: staleStatus})
	return &Server{
		sessions: &lifecycleSessionStoreFake{session: blockedFixLoopSession()},
		repos: &archiveRepoStoreFake{repo: &models.Repo{
			ID:                "r1",
			OriginURL:         "https://github.com/acme/repo",
			DefaultBaseBranch: "main",
			LocalPath:         "/x",
		}},
		provider:       prov,
		displayTracker: tracker,
		logger:         zerolog.Nop(),
	}
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
