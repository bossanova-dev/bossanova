package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"

	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/prstate"
)

// PRStateReader reads one PR's state through the daemon's cached GitHub
// provider (*prstate.Reader).
type PRStateReader interface {
	Read(ctx context.Context, repo string, pr int, opts prstate.Options) (prstate.Snapshot, error)
}

// GetPRState reads a PR in a repo registered on this daemon. An unregistered
// repo answers NotFound before any GitHub read, so a caller can tell "not
// mine" from "daemon down".
func (s *Server) GetPRState(ctx context.Context, req *connect.Request[pb.GetPRStateRequest]) (*connect.Response[pb.GetPRStateResponse], error) {
	if s.prState == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("PR state reader not configured"))
	}
	slug, pr, err := s.registeredPR(ctx, req.Msg.GetRepo(), req.Msg.GetPrNumber())
	if err != nil {
		return nil, err
	}
	snap, err := s.prState.Read(ctx, slug, pr, prstate.Options{
		Fresh:                 req.Msg.GetShouldBypassCache(),
		IncludeReviewComments: req.Msg.GetShouldIncludeReviewComments(),
		IncludeWorkflowRuns:   req.Msg.GetShouldIncludeWorkflowRuns(),
	})
	if err != nil {
		return nil, prReadError("read PR state", slug, pr, err)
	}
	return connect.NewResponse(prStateResponse(slug, pr, snap)), nil
}

// registeredPR validates a PR request's repo and number and resolves the repo
// to its registered slug, with no GitHub read.
func (s *Server) registeredPR(ctx context.Context, repo string, prNumber int32) (string, int, error) {
	repoArg := strings.TrimSpace(repo)
	if repoArg == "" {
		return "", 0, connect.NewError(connect.CodeInvalidArgument, errors.New("repo is required (owner/name or an origin URL)"))
	}
	pr := int(prNumber)
	if pr < 1 {
		return "", 0, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("pr_number must be a positive integer, got %d", pr))
	}
	slug, err := s.registeredRepoSlug(ctx, repoArg)
	if err != nil {
		return "", 0, err
	}
	return slug, pr, nil
}

// prReadError maps a failed PR read: a GitHub rate limit is ResourceExhausted,
// anything else Unavailable.
func prReadError(action, slug string, pr int, err error) error {
	code := connect.CodeUnavailable
	if vcs.IsRateLimited(err) {
		code = connect.CodeResourceExhausted
	}
	return connect.NewError(code, fmt.Errorf("%s %s#%d: %w", action, slug, pr, err))
}

// registeredRepoSlug resolves repoArg (owner/name or an origin URL) to the
// lowercase slug of the registered repo it names.
func (s *Server) registeredRepoSlug(ctx context.Context, repoArg string) (string, error) {
	repo, err := s.registeredRepo(ctx, repoArg)
	if err != nil {
		return "", err
	}
	return strings.ToLower(vcs.RepoSlug(repo.OriginURL)), nil
}

func (s *Server) registeredRepo(ctx context.Context, repoArg string) (*domain.Repo, error) {
	want := repoArg
	if parsed := vcs.RepoSlug(repoArg); parsed != "" {
		want = parsed
	}
	want = strings.ToLower(strings.TrimSuffix(strings.Trim(want, "/"), ".git"))
	repos, err := s.repos.List(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list repos: %w", err))
	}
	for _, r := range repos {
		if slug := strings.ToLower(vcs.RepoSlug(r.OriginURL)); slug != "" && slug == want {
			return r, nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("repo %s is not registered on this daemon", repoArg))
}

// prStateResponse converts a snapshot to the wire. The vcs enums share the
// proto enums' numbering (pinned by TestPRStateEnumNumbering).
func prStateResponse(slug string, pr int, snap prstate.Snapshot) *pb.GetPRStateResponse {
	st := snap.Status
	resp := &pb.GetPRStateResponse{
		Repo:                   slug,
		PrNumber:               clampInt32(pr),
		State:                  pb.PRState(clampInt32(int(st.State))),
		IsDraft:                st.Draft,
		HeadSha:                st.HeadSHA,
		HeadBranch:             st.HeadBranch,
		BaseBranch:             st.BaseBranch,
		IsMergeable:            st.Mergeable,
		MergeStateStatus:       mergeStateName(st.MergeStateStatus),
		ReviewDecision:         pb.ReviewState(clampInt32(int(st.ReviewDecisionState))),
		LatestReviewState:      pb.ReviewState(clampInt32(int(st.LatestReviewState))),
		CheckVerdict:           prCheckVerdict(snap.Verdict),
		VerifyPhase:            &pb.PRVerifyPhase{Kind: pb.PRVerifyPhaseKind(clampInt32(int(snap.VerifyPhase.Kind) + 1)), Reason: snap.VerifyPhase.Reason, Label: snap.VerifyPhase.Label},
		HasBuildReceipt:        snap.HasBuildReceipt,
		HasSettledWorkflowRuns: snap.RunsSettled,
		ObservedAt:             timestamppb.New(snap.ObservedAt),
		IsWorkflowRunsRead:     snap.RunsRead,
	}
	for _, c := range snap.Checks {
		check := &pb.PRCheck{Name: c.Name, Status: pb.CheckStatus(clampInt32(int(c.Status))), Description: c.Description, IsUnclassified: c.Unclassified, WorkflowName: c.Workflow}
		if c.Conclusion != nil {
			conclusion := pb.CheckConclusion(clampInt32(int(*c.Conclusion)))
			check.Conclusion = &conclusion
		}
		resp.Checks = append(resp.Checks, check)
	}
	for _, run := range snap.HeldRuns {
		resp.HeldWorkflowRuns = append(resp.HeldWorkflowRuns, &pb.PRHeldWorkflowRun{Name: run.Name, Status: run.Status, Event: run.Event, Class: run.Class})
	}
	for _, run := range snap.WorkflowRuns {
		resp.WorkflowRuns = append(resp.WorkflowRuns, &pb.PRWorkflowRun{Name: run.Name, Status: run.Status, Conclusion: run.Conclusion, Event: run.Event, HeadSha: run.HeadSHA})
	}
	for _, rc := range snap.ReviewComments {
		comment := &pb.ReviewComment{Author: rc.Author, Body: rc.Body, State: pb.ReviewState(clampInt32(int(rc.State))), Path: rc.Path}
		if rc.Line != nil {
			line := clampInt32(*rc.Line)
			comment.Line = &line
		}
		resp.ReviewComments = append(resp.ReviewComments, comment)
	}
	return resp
}

func prCheckVerdict(v vcs.CheckVerdict) *pb.PRCheckVerdict {
	return &pb.PRCheckVerdict{
		State:             pb.PRCheckVerdictState(clampInt32(int(v.State))),
		Reason:            v.Reason,
		TotalCount:        clampInt32(v.Total),
		PassedCount:       clampInt32(v.Passed),
		FailedCount:       clampInt32(v.Failed),
		PendingCount:      clampInt32(v.Pending),
		SkippedCount:      clampInt32(v.Skipped),
		UnclassifiedCount: clampInt32(v.Unclassified),
	}
}

// mergeStateNames spells GitHub's mergeStateStatus values in lowercase.
var mergeStateNames = map[vcs.MergeStateStatus]string{
	vcs.MergeStateStatusClean:    "clean",
	vcs.MergeStateStatusBlocked:  "blocked",
	vcs.MergeStateStatusBehind:   "behind",
	vcs.MergeStateStatusDirty:    "dirty",
	vcs.MergeStateStatusDraft:    "draft",
	vcs.MergeStateStatusHasHooks: "has_hooks",
	vcs.MergeStateStatusUnknown:  "unknown",
	vcs.MergeStateStatusUnstable: "unstable",
}

func mergeStateName(m vcs.MergeStateStatus) string { return mergeStateNames[m] }
