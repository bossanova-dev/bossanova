package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
)

const refreshSessionPRTimeout = 20 * time.Second

func (s *Server) RefreshSessionPR(ctx context.Context, req *connect.Request[pb.RefreshSessionPRRequest]) (*connect.Response[pb.RefreshSessionPRResponse], error) {
	msg := req.Msg
	sess, repo, prNumber, err := s.resolveRefreshSessionPRTarget(ctx, msg)
	if err != nil {
		return nil, err
	}

	refreshCtx, cancel := context.WithTimeout(ctx, refreshSessionPRTimeout)
	defer cancel()

	if err := s.refreshSessionPRDisplay(refreshCtx, repo, sess.ID, prNumber); err != nil {
		return nil, err
	}

	refreshed, err := s.sessions.Get(ctx, sess.ID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get refreshed session: %w", err))
	}
	p := s.sessionProtoWithRepo(ctx, refreshed)
	if s.displayTracker != nil {
		HydrateDisplayEntry(p, s.displayTracker.Get(refreshed.ID))
	}
	if s.onSessionUpdated != nil {
		s.onSessionUpdated(ctx, p)
	}
	return connect.NewResponse(&pb.RefreshSessionPRResponse{Session: p}), nil
}

func (s *Server) resolveRefreshSessionPRTarget(
	ctx context.Context, msg *pb.RefreshSessionPRRequest,
) (*models.Session, *models.Repo, int, error) {
	if msg == nil {
		return nil, nil, 0, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id or pr_number is required"))
	}
	sessionID := strings.TrimSpace(msg.GetId())
	hasSessionID := msg != nil && msg.Id != nil && sessionID != ""
	hasPRNumber := msg != nil && msg.PrNumber != nil
	prNumber := int(msg.GetPrNumber())

	if !hasSessionID && !hasPRNumber {
		return nil, nil, 0, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id or pr_number is required"))
	}
	if msg != nil && msg.Id != nil && strings.TrimSpace(msg.GetId()) == "" {
		return nil, nil, 0, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id cannot be empty"))
	}
	if hasPRNumber && prNumber <= 0 {
		return nil, nil, 0, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("pr_number must be positive"))
	}

	var sess *models.Session
	var err error
	if hasSessionID {
		sess, err = s.sessions.Get(ctx, sessionID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nil, 0, connect.NewError(connect.CodeNotFound, fmt.Errorf("session not found: %w", err))
			}
			return nil, nil, 0, connect.NewError(connect.CodeInternal, fmt.Errorf("get session: %w", err))
		}
		if sess.PRNumber == nil || *sess.PRNumber <= 0 {
			return nil, nil, 0, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("session has no linked PR"))
		}
		if hasPRNumber && *sess.PRNumber != prNumber {
			return nil, nil, 0, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id %q is linked to PR #%d, not PR #%d", sessionID, *sess.PRNumber, prNumber))
		}
		prNumber = *sess.PRNumber
	} else {
		sess, err = s.sessionByPRNumber(ctx, prNumber)
		if err != nil {
			return nil, nil, 0, err
		}
	}

	repo, err := s.repos.Get(ctx, sess.RepoID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, 0, connect.NewError(connect.CodeNotFound, fmt.Errorf("repo not found: %w", err))
		}
		return nil, nil, 0, connect.NewError(connect.CodeInternal, fmt.Errorf("get repo: %w", err))
	}
	if strings.TrimSpace(repo.OriginURL) == "" {
		return nil, nil, 0, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("session repo has no origin URL"))
	}
	return sess, repo, prNumber, nil
}

func (s *Server) sessionByPRNumber(ctx context.Context, prNumber int) (*models.Session, error) {
	rows, err := s.sessions.ListActive(ctx, "")
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list sessions: %w", err))
	}
	var match *models.Session
	for _, sess := range rows {
		if sess == nil || sess.PRNumber == nil || *sess.PRNumber != prNumber {
			continue
		}
		if match != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("PR #%d is linked to multiple sessions; pass id", prNumber))
		}
		match = sess
	}
	if match == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("no session linked to PR #%d", prNumber))
	}
	return match, nil
}

func (s *Server) refreshSessionPRDisplay(ctx context.Context, repo *models.Repo, sessionID string, prNumber int) error {
	if s.provider == nil {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("refresh session PR: VCS provider is unavailable"))
	}
	if s.displayTracker == nil {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("refresh session PR: display tracker is unavailable"))
	}

	prStatus, err := s.provider.GetPRStatus(ctx, repo.OriginURL, prNumber)
	if err != nil {
		return providerRefreshError(ctx, "PR status", err)
	}
	if prStatus == nil {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("refresh session PR: provider returned no PR status"))
	}

	var checks []vcs.CheckResult
	var reviews []vcs.ReviewComment
	if prStatus.State != vcs.PRStateMerged && prStatus.State != vcs.PRStateClosed && !prStatus.Draft {
		checks, err = s.provider.GetCheckResults(ctx, repo.OriginURL, prNumber)
		if err != nil {
			return providerRefreshError(ctx, "check results", err)
		}
		reviews, err = s.provider.GetReviewComments(ctx, repo.OriginURL, prNumber)
		if err != nil {
			return providerRefreshError(ctx, "review comments", err)
		}
	}

	info := vcs.ComputeDisplayStatus(prStatus, checks, reviews)
	info.HeadSHA = prStatus.HeadSHA
	info.Mergeable = prStatus.Mergeable
	s.displayTracker.Set(sessionID, info)
	return nil
}

func providerRefreshError(ctx context.Context, read string, err error) error {
	code := connect.CodeUnavailable
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = ctxErr
		code = connect.CodeCanceled
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			code = connect.CodeDeadlineExceeded
		}
	}
	return connect.NewError(code, fmt.Errorf("refresh session PR: fetch %s: %w", read, err))
}
