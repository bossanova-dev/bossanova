package server

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"

	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/prstate"
)

type prStateRepos struct {
	repoStore
	repos []*domain.Repo
}

func (r prStateRepos) List(context.Context) ([]*domain.Repo, error) { return r.repos, nil }

type prStateCall struct {
	repo string
	pr   int
	opts prstate.Options
}

type fakePRStateReader struct {
	snap  prstate.Snapshot
	err   error
	calls []prStateCall
}

func (f *fakePRStateReader) Read(_ context.Context, repo string, pr int, opts prstate.Options) (prstate.Snapshot, error) {
	f.calls = append(f.calls, prStateCall{repo, pr, opts})
	return f.snap, f.err
}

func prStateServer(reader PRStateReader) *Server {
	return &Server{prState: reader, repos: prStateRepos{repos: []*domain.Repo{
		{ID: "r1", OriginURL: "git@github.com:Acme/Widgets.git"},
		{ID: "r2", OriginURL: "https://github.com/acme/gadgets"},
	}}}
}

func getPRState(srv *Server, req *pb.GetPRStateRequest) (*pb.GetPRStateResponse, error) {
	resp, err := srv.GetPRState(context.Background(), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func TestGetPRStateResolvesRegisteredRepoForms(t *testing.T) {
	for _, repo := range []string{"acme/widgets", "ACME/Widgets", "https://github.com/acme/widgets.git", "git@github.com:acme/widgets.git"} {
		t.Run(repo, func(t *testing.T) {
			reader := &fakePRStateReader{}
			resp, err := getPRState(prStateServer(reader), &pb.GetPRStateRequest{Repo: repo, PrNumber: 7, ShouldBypassCache: true, ShouldIncludeReviewComments: true, ShouldIncludeWorkflowRuns: true})
			if err != nil {
				t.Fatalf("GetPRState: %v", err)
			}
			want := prStateCall{"acme/widgets", 7, prstate.Options{Fresh: true, IncludeReviewComments: true, IncludeWorkflowRuns: true}}
			if len(reader.calls) != 1 || reader.calls[0] != want {
				t.Fatalf("reads = %+v, want %+v", reader.calls, want)
			}
			if resp.GetRepo() != "acme/widgets" || resp.GetPrNumber() != 7 {
				t.Fatalf("repo/pr = %q/%d", resp.GetRepo(), resp.GetPrNumber())
			}
		})
	}
}

func TestGetPRStateRefusals_Callbacks_AC230302(t *testing.T) {
	tests := []struct {
		name   string
		srv    func(*fakePRStateReader) *Server
		req    *pb.GetPRStateRequest
		reader *fakePRStateReader
		want   connect.Code
	}{
		{name: "unregistered repo", req: &pb.GetPRStateRequest{Repo: "acme/other", PrNumber: 7}, want: connect.CodeNotFound},
		{name: "empty repo", req: &pb.GetPRStateRequest{Repo: " ", PrNumber: 7}, want: connect.CodeInvalidArgument},
		{name: "zero pr", req: &pb.GetPRStateRequest{Repo: "acme/widgets"}, want: connect.CodeInvalidArgument},
		{name: "negative pr", req: &pb.GetPRStateRequest{Repo: "acme/widgets", PrNumber: -3}, want: connect.CodeInvalidArgument},
		{name: "no reader", req: &pb.GetPRStateRequest{Repo: "acme/widgets", PrNumber: 7}, want: connect.CodeFailedPrecondition,
			srv: func(*fakePRStateReader) *Server { return &Server{} }},
		{name: "rate limited", req: &pb.GetPRStateRequest{Repo: "acme/widgets", PrNumber: 7}, want: connect.CodeResourceExhausted,
			reader: &fakePRStateReader{err: fmt.Errorf("get PR status: %w", vcs.ErrRateLimited)}},
		{name: "read failure", req: &pb.GetPRStateRequest{Repo: "acme/widgets", PrNumber: 7}, want: connect.CodeUnavailable,
			reader: &fakePRStateReader{err: errors.New("gh: connection reset")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reader := tc.reader
			if reader == nil {
				reader = &fakePRStateReader{}
			}
			srv := prStateServer(reader)
			if tc.srv != nil {
				srv = tc.srv(reader)
			}
			_, err := getPRState(srv, tc.req)
			if got := connect.CodeOf(err); got != tc.want {
				t.Fatalf("code = %v (%v), want %v", got, err, tc.want)
			}
			if tc.want != connect.CodeResourceExhausted && tc.want != connect.CodeUnavailable && len(reader.calls) != 0 {
				t.Fatalf("a refused request read GitHub: %+v", reader.calls)
			}
		})
	}
}

func TestGetPRStateMapsSnapshot_Callbacks_AC230301(t *testing.T) {
	mergeable := true
	settled := false
	success := vcs.CheckConclusionSuccess
	path, line := "main.go", 12
	observed := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	reader := &fakePRStateReader{snap: prstate.Snapshot{
		Status: vcs.PRStatus{
			State: vcs.PRStateOpen, Draft: true, HeadSHA: "abc", HeadBranch: "feat", BaseBranch: "main",
			Mergeable: &mergeable, MergeStateStatus: vcs.MergeStateStatusHasHooks,
			ReviewDecisionState: vcs.ReviewStateRequired, LatestReviewState: vcs.ReviewStateCommented,
		},
		Checks: []vcs.CheckResult{
			{Name: "ci", Status: vcs.CheckStatusCompleted, Conclusion: &success, Workflow: "build"},
			{Name: vcs.VerifyStatusContext, Status: vcs.CheckStatusInProgress, Description: "waiting: merge-state-unknown"},
		},
		HasBuildReceipt: true,
		Verdict:         vcs.CheckVerdict{State: vcs.CheckVerdictGreen, Reason: "ok", Total: 1, Passed: 1},
		VerifyPhase:     vcs.VerifyPhase{Kind: vcs.VerifyPhaseWaiting, Reason: "merge-state-unknown"},
		RunsSettled:     &settled,
		HeldRuns:        []prstate.HeldWorkflowRun{{Name: "deploy", Status: "queued", Event: "push", Class: prstate.HeldClassInFlight}},
		ReviewComments:  []vcs.ReviewComment{{Author: "rev", Body: "nit", State: vcs.ReviewStateChangesRequested, Path: &path, Line: &line}},
		ObservedAt:      observed,
	}}
	resp, err := getPRState(prStateServer(reader), &pb.GetPRStateRequest{Repo: "acme/widgets", PrNumber: 7})
	if err != nil {
		t.Fatalf("GetPRState: %v", err)
	}
	if resp.GetState() != pb.PRState_PR_STATE_OPEN || !resp.GetIsDraft() || resp.GetHeadSha() != "abc" || resp.GetHeadBranch() != "feat" || resp.GetBaseBranch() != "main" {
		t.Fatalf("pr fields = %v", resp)
	}
	if resp.IsMergeable == nil || !resp.GetIsMergeable() || resp.GetMergeStateStatus() != "has_hooks" {
		t.Fatalf("mergeability = %v %q", resp.IsMergeable, resp.GetMergeStateStatus())
	}
	if resp.GetReviewDecision() != pb.ReviewState_REVIEW_STATE_REQUIRED || resp.GetLatestReviewState() != pb.ReviewState_REVIEW_STATE_COMMENTED {
		t.Fatalf("review = %v / %v", resp.GetReviewDecision(), resp.GetLatestReviewState())
	}
	checks := resp.GetChecks()
	if len(checks) != 2 || checks[0].GetConclusion() != pb.CheckConclusion_CHECK_CONCLUSION_SUCCESS || checks[1].Conclusion != nil ||
		checks[0].GetWorkflowName() != "build" || checks[1].GetWorkflowName() != "" ||
		checks[1].GetStatus() != pb.CheckStatus_CHECK_STATUS_IN_PROGRESS || checks[1].GetDescription() != "waiting: merge-state-unknown" {
		t.Fatalf("checks = %v", checks)
	}
	v := resp.GetCheckVerdict()
	if v.GetState() != pb.PRCheckVerdictState_PR_CHECK_VERDICT_STATE_GREEN || v.GetReason() != "ok" || v.GetTotalCount() != 1 || v.GetPassedCount() != 1 {
		t.Fatalf("verdict = %v", v)
	}
	if p := resp.GetVerifyPhase(); p.GetKind() != pb.PRVerifyPhaseKind_PR_VERIFY_PHASE_KIND_WAITING || p.GetReason() != "merge-state-unknown" {
		t.Fatalf("verify phase = %v", p)
	}
	if !resp.GetHasBuildReceipt() || resp.HasSettledWorkflowRuns == nil || resp.GetHasSettledWorkflowRuns() {
		t.Fatalf("receipt/settled = %v / %v", resp.GetHasBuildReceipt(), resp.HasSettledWorkflowRuns)
	}
	if runs := resp.GetHeldWorkflowRuns(); len(runs) != 1 || runs[0].GetName() != "deploy" || runs[0].GetClass() != prstate.HeldClassInFlight {
		t.Fatalf("held runs = %v", runs)
	}
	if rc := resp.GetReviewComments(); len(rc) != 1 || rc[0].GetPath() != "main.go" || rc[0].GetLine() != 12 || rc[0].GetState() != pb.ReviewState_REVIEW_STATE_CHANGES_REQUESTED {
		t.Fatalf("review comments = %v", rc)
	}
	if !resp.GetObservedAt().AsTime().Equal(observed) {
		t.Fatalf("observed_at = %v", resp.GetObservedAt().AsTime())
	}
}

func TestGetPRStateCarriesEveryHeadWorkflowRun_Callbacks_AC231201(t *testing.T) {
	runs := []vcs.WorkflowRun{
		{Name: "CI", Status: "completed", Conclusion: "success", Event: "pull_request", HeadSHA: "abc"},
		{Name: "Deploy", Status: "in_progress", Event: "push", HeadSHA: "abc"},
	}
	reader := &fakePRStateReader{snap: prstate.Snapshot{WorkflowRuns: runs, RunsRead: true}}
	resp, err := getPRState(prStateServer(reader), &pb.GetPRStateRequest{Repo: "acme/widgets", PrNumber: 7})
	if err != nil {
		t.Fatalf("GetPRState: %v", err)
	}
	got := resp.GetWorkflowRuns()
	if !resp.GetIsWorkflowRunsRead() || len(got) != 2 {
		t.Fatalf("is_workflow_runs_read = %v, workflow_runs = %v", resp.GetIsWorkflowRunsRead(), got)
	}
	for i, want := range runs {
		r := got[i]
		if r.GetName() != want.Name || r.GetStatus() != want.Status || r.GetConclusion() != want.Conclusion || r.GetEvent() != want.Event || r.GetHeadSha() != want.HeadSHA {
			t.Fatalf("run %d = %v, want %+v", i, r, want)
		}
	}
	unread, err := getPRState(prStateServer(&fakePRStateReader{}), &pb.GetPRStateRequest{Repo: "acme/widgets", PrNumber: 7})
	if err != nil {
		t.Fatalf("GetPRState: %v", err)
	}
	if unread.GetIsWorkflowRunsRead() || len(unread.GetWorkflowRuns()) != 0 {
		t.Fatalf("a snapshot without a runs read carried runs: %v / %v", unread.GetIsWorkflowRunsRead(), unread.GetWorkflowRuns())
	}
}

func TestGetPRStateLeavesSettlementUnsetWhenNotComputed(t *testing.T) {
	reader := &fakePRStateReader{snap: prstate.Snapshot{Verdict: vcs.CheckVerdict{State: vcs.CheckVerdictPending}}}
	resp, err := getPRState(prStateServer(reader), &pb.GetPRStateRequest{Repo: "acme/widgets", PrNumber: 7})
	if err != nil {
		t.Fatalf("GetPRState: %v", err)
	}
	if resp.HasSettledWorkflowRuns != nil {
		t.Fatalf("has_settled_workflow_runs = %v, want unset for a non-green verdict", resp.GetHasSettledWorkflowRuns())
	}
	if resp.GetVerifyPhase().GetKind() != pb.PRVerifyPhaseKind_PR_VERIFY_PHASE_KIND_NONE {
		t.Fatalf("verify phase = %v, want NONE", resp.GetVerifyPhase().GetKind())
	}
}

// TestPRStateEnumNumbering pins the numbering the converter's casts rely on.
func TestPRStateEnumNumbering(t *testing.T) {
	pairs := []struct {
		vcs   int
		proto int32
	}{
		{int(vcs.PRStateOpen), int32(pb.PRState_PR_STATE_OPEN)},
		{int(vcs.PRStateClosed), int32(pb.PRState_PR_STATE_CLOSED)},
		{int(vcs.PRStateMerged), int32(pb.PRState_PR_STATE_MERGED)},
		{int(vcs.CheckStatusQueued), int32(pb.CheckStatus_CHECK_STATUS_QUEUED)},
		{int(vcs.CheckStatusInProgress), int32(pb.CheckStatus_CHECK_STATUS_IN_PROGRESS)},
		{int(vcs.CheckStatusCompleted), int32(pb.CheckStatus_CHECK_STATUS_COMPLETED)},
		{int(vcs.CheckConclusionSuccess), int32(pb.CheckConclusion_CHECK_CONCLUSION_SUCCESS)},
		{int(vcs.CheckConclusionFailure), int32(pb.CheckConclusion_CHECK_CONCLUSION_FAILURE)},
		{int(vcs.CheckConclusionNeutral), int32(pb.CheckConclusion_CHECK_CONCLUSION_NEUTRAL)},
		{int(vcs.CheckConclusionCancelled), int32(pb.CheckConclusion_CHECK_CONCLUSION_CANCELLED)},
		{int(vcs.CheckConclusionSkipped), int32(pb.CheckConclusion_CHECK_CONCLUSION_SKIPPED)},
		{int(vcs.CheckConclusionTimedOut), int32(pb.CheckConclusion_CHECK_CONCLUSION_TIMED_OUT)},
		{int(vcs.ReviewStateApproved), int32(pb.ReviewState_REVIEW_STATE_APPROVED)},
		{int(vcs.ReviewStateChangesRequested), int32(pb.ReviewState_REVIEW_STATE_CHANGES_REQUESTED)},
		{int(vcs.ReviewStateCommented), int32(pb.ReviewState_REVIEW_STATE_COMMENTED)},
		{int(vcs.ReviewStateDismissed), int32(pb.ReviewState_REVIEW_STATE_DISMISSED)},
		{int(vcs.ReviewStateRequired), int32(pb.ReviewState_REVIEW_STATE_REQUIRED)},
		{int(vcs.CheckVerdictUnknown), int32(pb.PRCheckVerdictState_PR_CHECK_VERDICT_STATE_UNKNOWN)},
		{int(vcs.CheckVerdictGreen), int32(pb.PRCheckVerdictState_PR_CHECK_VERDICT_STATE_GREEN)},
		{int(vcs.CheckVerdictFailing), int32(pb.PRCheckVerdictState_PR_CHECK_VERDICT_STATE_FAILING)},
		{int(vcs.CheckVerdictPending), int32(pb.PRCheckVerdictState_PR_CHECK_VERDICT_STATE_PENDING)},
		{int(vcs.VerifyPhaseNone) + 1, int32(pb.PRVerifyPhaseKind_PR_VERIFY_PHASE_KIND_NONE)},
		{int(vcs.VerifyPhaseVerifying) + 1, int32(pb.PRVerifyPhaseKind_PR_VERIFY_PHASE_KIND_VERIFYING)},
		{int(vcs.VerifyPhaseNeedsHuman) + 1, int32(pb.PRVerifyPhaseKind_PR_VERIFY_PHASE_KIND_NEEDS_HUMAN)},
		{int(vcs.VerifyPhaseWaiting) + 1, int32(pb.PRVerifyPhaseKind_PR_VERIFY_PHASE_KIND_WAITING)},
	}
	for i, p := range pairs {
		if clampInt32(p.vcs) != p.proto {
			t.Errorf("pair %d: vcs %d != proto %d", i, p.vcs, p.proto)
		}
	}
}
