package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

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
	Approve(context.Context, verifybridge.Target) (verifybridge.ApproveResult, error)
	SendBack(context.Context, verifybridge.Target, string) (verifybridge.SendBackResult, error)
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
	r, err := s.verifyVerbsFor(env).Approve(ctx, target)
	if err != nil {
		return nil, s.verifyBridgeError("approve", target, err)
	}
	return connect.NewResponse(&pb.ApproveSessionPRResponse{Result: &pb.VerifyApproveResult{
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
	}}), nil
}

// SendBackSessionPR runs `verify-gate.mjs send-back` for the session's PR on
// this host. Starting the repair chat stays with the caller.
func (s *Server) SendBackSessionPR(ctx context.Context, req *connect.Request[pb.SendBackSessionPRRequest]) (*connect.Response[pb.SendBackSessionPRResponse], error) {
	note := req.Msg.GetNote()
	if strings.TrimSpace(note) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("send-back needs a non-empty note"))
	}
	target, env, err := s.verifyTarget(ctx, req.Msg.GetId(), req.Msg.GetIsDryRun())
	if err != nil {
		return nil, err
	}
	r, err := s.verifyVerbsFor(env).SendBack(ctx, target, note)
	if err != nil {
		return nil, s.verifyBridgeError("send-back", target, err)
	}
	return connect.NewResponse(&pb.SendBackSessionPRResponse{
		IsPosted:   r.Posted,
		IsPlanned:  r.Planned,
		IsDryRun:   r.DryRun,
		Reason:     r.Reason,
		CommentUrl: r.CommentURL,
		HeadSha:    r.HeadSHA,
		State:      r.State,
	}), nil
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
