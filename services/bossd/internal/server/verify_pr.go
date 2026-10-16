package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/dotenv"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/verifybridge"
)

// verifyVerbs is the boss-verify toolbox's operator surface;
// *verifybridge.Bridge is the real one.
type verifyVerbs interface {
	Approve(context.Context, verifybridge.Target, *verifybridge.MergeApproval) (verifybridge.ApproveResult, error)
	MarkMerged(context.Context, verifybridge.Target) (verifybridge.MarkMergedResult, error)
}

// verifyVerbsFor returns the verbs that run with env as the child's whole
// environment. Tests substitute Server.newVerifyVerbs.
func (s *Server) verifyVerbsFor(env []string) verifyVerbs {
	if s.newVerifyVerbs != nil {
		return s.newVerifyVerbs(env)
	}
	return &verifybridge.Bridge{Env: env}
}

// ApproveSessionPR runs `verify-gate.mjs approve` for the session's PR on this
// host, so the operator need not be on the machine holding the worktree.
func (s *Server) ApproveSessionPR(ctx context.Context, req *connect.Request[pb.ApproveSessionPRRequest]) (*connect.Response[pb.ApproveSessionPRResponse], error) {
	target, env, err := s.verifyTarget(ctx, req.Msg.GetId(), req.Msg.GetIsDryRun())
	if err != nil {
		return nil, err
	}
	r, err := s.verifyVerbsFor(env).Approve(ctx, target, nil)
	if err != nil {
		return nil, s.verifyBridgeError("approve", target, err)
	}
	return connect.NewResponse(&pb.ApproveSessionPRResponse{Result: approveResultProto(r)}), nil
}

func approveResultProto(r verifybridge.ApproveResult) *pb.VerifyApproveResult {
	return &pb.VerifyApproveResult{
		IsApproved:    r.Approved,
		IsPlanned:     r.Planned,
		IsDryRun:      r.DryRun,
		Reason:        r.Reason,
		Waives:        r.Waives,
		Lifted:        r.Lifted,
		HeadSha:       r.HeadSHA,
		TrackerWrites: r.TrackerWrites,
		ApprovalUrl:   r.ApprovalURL,
		Why:           r.Why,
		Park:          r.Park,
		State:         r.State,
		SignOff:       r.SignOff,
	}
}

// operatorMergeBudget splits OperatorMergeSession's time. The whole flow must
// answer inside the daemon's 120s WriteTimeout, or a merge that landed reaches
// the operator as a dropped reply. approve gets the first slice; mark-merged
// gets whatever is left of total after the merge, and is skipped when nothing is.
type operatorMergeBudget struct {
	total, approve time.Duration
}

var defaultOperatorMergeBudget = operatorMergeBudget{total: 110 * time.Second, approve: 60 * time.Second}

func (s *Server) mergeBudget() operatorMergeBudget {
	if s.operatorMergeBudget.total > 0 {
		return s.operatorMergeBudget
	}
	return defaultOperatorMergeBudget
}

// OperatorMergeSession is the operator's merge from a review or merge row, in
// one order that never merges what the operator did not see: refuse a FIX,
// check the live gate at the pinned head, record the head-bound approval
// (review rows only), merge through MergeSession at that head, then run the
// post-merge tracker bookkeeping. Nothing is recorded when the gate refuses or
// the head moved.
func (s *Server) OperatorMergeSession(ctx context.Context, req *connect.Request[pb.OperatorMergeSessionRequest]) (*connect.Response[pb.OperatorMergeSessionResponse], error) {
	budget := s.mergeBudget()
	deadline := time.Now().Add(budget.total)
	pin, ok := vcs.NormalizeHeadSHA(req.Msg.GetExpectedHeadSha())
	if !ok {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("expected_head_sha must be a 40-character hex commit SHA"))
	}
	id := req.Msg.GetId()
	target, env, err := s.verifyTarget(ctx, id, false)
	if err != nil {
		return nil, err
	}
	if err := s.refuseFixAttention(ctx, id, target.PR); err != nil {
		return nil, err
	}
	verbs := s.verifyVerbsFor(env)
	resp := &pb.OperatorMergeSessionResponse{}
	if req.Msg.GetShouldApprove() {
		if err := s.operatorMergeGate(ctx, id, target.PR, pin); err != nil {
			return nil, err
		}
		approveCtx, cancel := context.WithTimeout(ctx, budget.approve)
		r, err := verbs.Approve(approveCtx, target, &verifybridge.MergeApproval{Head: pin, Waives: req.Msg.GetWaives()})
		cancel()
		if err != nil {
			return nil, s.verifyBridgeError("approve", target, err)
		}
		resp.Approval = approveResultProto(r)
		if !r.Approved {
			return connect.NewResponse(resp), nil
		}
	}
	merged, err := s.MergeSession(ctx, connect.NewRequest(&pb.MergeSessionRequest{Id: id, ExpectedHeadSha: pin}))
	if err != nil {
		if resp.Approval != nil {
			return nil, approvalStandsError(err, resp.Approval)
		}
		return nil, err
	}
	resp.IsMerged = true
	resp.Session = merged.Msg.GetSession()
	resp.MergeDetail = merged.Msg.GetDetail()
	resp.TrackerWrites, resp.TrackerWhy = s.markMerged(ctx, verbs, target, deadline)
	return connect.NewResponse(resp), nil
}

// refuseFixAttention refuses a session whose attention is a FIX: a blocked
// verify park or a failure. The TUI offers no merge on a fix row; this keeps a
// direct call from merging over one too. The escape hatch for a blocked park
// is an approval through the CLI, and for a failure a plain merge.
func (s *Server) refuseFixAttention(ctx context.Context, id string, pr int) error {
	got, err := s.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: id}))
	if err != nil {
		return err
	}
	a := got.Msg.GetSession().GetAttention()
	if a.GetKind() != pb.AttentionKind_ATTENTION_KIND_FIX {
		return nil
	}
	override := fmt.Sprintf("use boss merge %s to override", id)
	if len(a.GetParkCodes()) > 0 {
		override = fmt.Sprintf("use boss pr approve %d to override", pr)
	}
	return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("PR #%d is blocked: %s; %s", pr, a.GetSentence(), override))
}

// operatorMergeGate runs a stricter live gate than MergeSession's before an
// approval is recorded, so an approval is never left standing on a head that
// could not merge, is a draft, has checks still running, or moved since the
// operator looked.
func (s *Server) operatorMergeGate(ctx context.Context, id string, pr int, pin string) error {
	sess, err := s.sessions.Get(ctx, id)
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("get session: %w", err))
	}
	repo, err := s.repos.Get(ctx, sess.RepoID)
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("get repo: %w", err))
	}
	mb, err := s.liveMergeBlock(ctx, repo.OriginURL, pr, pin, operatorApprovalBlocks)
	switch {
	case errors.Is(err, vcs.ErrHeadMismatch):
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%w; nothing was approved or merged", err))
	case err != nil:
		return connect.NewError(connect.CodeCanceled, fmt.Errorf("merge abandoned while checking the live merge gate: %w", err))
	case mb != nil:
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("merge blocked: gate=%s; %s; nothing was approved or merged", mb.Gate.Slug(), mb.Detail))
	}
	return nil
}

// approvalStandsError keeps a refused merge's code and says that the approval
// recorded just before it still stands on that head.
func approvalStandsError(err error, approval *pb.VerifyApproveResult) error {
	msg := err.Error()
	var ce *connect.Error
	if errors.As(err, &ce) {
		msg = ce.Message()
	}
	return connect.NewError(connect.CodeOf(err), fmt.Errorf("%s; the approval of %s (%s) stays recorded", msg, approval.GetHeadSha(), approval.GetApprovalUrl()))
}

// markMerged runs the toolbox's post-merge tracker bookkeeping. The merge has
// landed, so it runs past the caller's cancellation but stops at deadline, so
// the reply that says the merge landed is never dropped. A failure is
// reported, never returned.
func (s *Server) markMerged(ctx context.Context, verbs verifyVerbs, target verifybridge.Target, deadline time.Time) (writes, why string) {
	if time.Until(deadline) <= 0 {
		s.logger.Warn().Int("pr", target.PR).Str("repo", target.Repo).Msg("operator merge: no time left for tracker bookkeeping")
		return "failed", "no time was left to update the tracker"
	}
	markCtx, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
	defer cancel()
	r, err := verbs.MarkMerged(markCtx, target)
	switch {
	case err != nil:
		s.logger.Warn().Err(err).Int("pr", target.PR).Str("repo", target.Repo).Msg("operator merge: tracker bookkeeping failed")
		return "failed", err.Error()
	case !r.Merged:
		return "skipped", r.Reason
	}
	return r.TrackerWrites, r.Why
}

// verifyTarget resolves the session's PR, repo slug and worktree, and the
// environment the verb runs under: this daemon's, then the worktree .env and
// the repo's stored secrets. The repo's LINEAR_API_KEY is what lets the verb
// remove the needs-human label; an operator's shell rarely carries it.
func (s *Server) verifyTarget(ctx context.Context, id string, dryRun bool) (verifybridge.Target, []string, error) {
	if id == "" {
		return verifybridge.Target{}, nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}
	sess, err := s.sessions.Get(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return verifybridge.Target{}, nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session not found: %s", id))
	}
	if err != nil {
		return verifybridge.Target{}, nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get session: %w", err))
	}
	if sess.PRNumber == nil || *sess.PRNumber < 1 {
		return verifybridge.Target{}, nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("session %s has no pull request", id))
	}
	if sess.WorktreePath == "" {
		return verifybridge.Target{}, nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("session %s has no worktree", id))
	}
	repo := session.RepoForSessionEnv(ctx, s.repos, sess.RepoID, sess.ID, "verify verb", s.logger)
	var slug string
	if repo != nil {
		slug = strings.ToLower(vcs.RepoSlug(repo.OriginURL))
	}
	env := os.Environ()
	for k, v := range dotenv.OverlayWithRepo(map[string]string{}, sess.WorktreePath, repo) {
		env = append(env, k+"="+v)
	}
	return verifybridge.Target{PR: *sess.PRNumber, Repo: slug, Dir: sess.WorktreePath, DryRun: dryRun}, env, nil
}

// verifyBridgeError maps a bridge failure onto a connect code. A timeout stays
// distinguishable so the caller can say an approval may already be posted.
func (s *Server) verifyBridgeError(verb string, t verifybridge.Target, err error) error {
	s.logger.Warn().Err(err).Str("verb", verb).Int("pr", t.PR).Str("repo", t.Repo).Msg("verify verb failed")
	code := connect.CodeInternal
	switch {
	case errors.Is(err, verifybridge.ErrNodeTimeout):
		code = connect.CodeDeadlineExceeded
	case errors.Is(err, verifybridge.ErrInvalidTarget),
		errors.Is(err, verifybridge.ErrToolboxMissing),
		errors.Is(err, verifybridge.ErrNodeMissing):
		code = connect.CodeFailedPrecondition
	}
	return connect.NewError(code, err)
}
