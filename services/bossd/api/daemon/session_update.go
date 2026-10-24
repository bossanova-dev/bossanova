package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/session"
)

func (s *Server) RemoveSession(ctx context.Context, req *connect.Request[pb.RemoveSessionRequest]) (*connect.Response[pb.RemoveSessionResponse], error) {
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	// Get session first to find branch name and repo for git cleanup.
	sess, err := s.sessions.Get(ctx, req.Msg.Id)
	if err != nil {
		// If not found, nothing to delete.
		return connect.NewResponse(&pb.RemoveSessionResponse{}), nil
	}

	// Notify the task orchestrator BEFORE deleting, because sessions.Delete
	// nullifies task_mappings.session_id, making GetBySessionID unable to
	// find the mapping afterward.
	if s.completionNotifier != nil {
		s.completionNotifier.HandleSessionCompleted(ctx, req.Msg.Id, domain.TaskMappingStatusFailed)
	}

	// Stop any in-flight background draft-PR create before touching the branch
	// (BOS-540). CreateSession now returns mid-create, so a remove issued
	// seconds later races a goroutine about to push this branch and open a PR
	// for it — which would survive the row we are about to delete.
	if s.lifecycle != nil {
		s.lifecycle.StopBackgroundDraftPR(ctx, req.Msg.Id)
		// Re-read after the join: the step can correct a drifted BranchName on
		// its way out (attachPRMetadata does), and reaping the pre-stop name
		// would delete nothing and leave the real branch behind.
		if fresh, freshErr := s.sessions.Get(ctx, req.Msg.Id); freshErr == nil {
			sess = fresh
		}
	}

	// Best-effort git cleanup: delete branch + prune worktree. Logged rather than
	// discarded: since BOS-717 this can fail by being REFUSED the per-clone gate
	// (a concurrent create in the same repo), and the row is deleted immediately
	// below, so a silent refusal orphans the branch with nothing left naming it.
	var branches []string
	if sess.RepoID != "" && sess.BranchName != "" {
		branches = s.unsharedBranches(ctx, sess.RepoID, sess.ID, []string{sess.BranchName})
	}
	if len(branches) > 0 {
		repo, err := s.repos.Get(ctx, sess.RepoID)
		if err == nil {
			if reapErr := s.worktrees.ReapLocalBranches(ctx, repo.LocalPath, branches); reapErr != nil {
				s.logger.Warn().Err(reapErr).
					Str("session", req.Msg.Id).
					Str("repo", repo.LocalPath).
					Str("branch", sess.BranchName).
					Msg("remove session: could not delete the local branch; it is left behind")
			}
		}
	}

	// Delete from DB.
	if err := s.sessions.Delete(ctx, req.Msg.Id); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("remove session: %w", err))
	}
	if s.onSessionDeleted != nil {
		s.onSessionDeleted(ctx, req.Msg.Id)
	}

	return connect.NewResponse(&pb.RemoveSessionResponse{}), nil
}

func (s *Server) UpdateSession(ctx context.Context, req *connect.Request[pb.UpdateSessionRequest]) (*connect.Response[pb.UpdateSessionResponse], error) {
	msg := req.Msg
	if msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	params := domain.UpdateSessionParams{}
	if msg.Title != nil {
		title := strings.TrimSpace(*msg.Title)
		if title == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("title cannot be empty"))
		}
		params.Title = &title
	}
	// tracker_url/tracker_id let a running session (e.g. a cron boss-build run)
	// link itself to the Linear ticket it selected after creation, so the TUI
	// [l]inear shortcut — gated on a non-empty tracker URL — becomes available.
	// UpdateSessionParams uses **string so nil means "leave unchanged"; take the
	// address of the request's *string only when the field was supplied.
	if msg.TrackerUrl != nil {
		params.TrackerURL = &msg.TrackerUrl
	}
	if msg.TrackerId != nil {
		params.TrackerID = &msg.TrackerId
	}

	sess, err := s.sessions.Update(ctx, msg.Id, params)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("update session: %w", err))
	}

	// Best-effort: update the PR title if this session has a PR.
	if msg.Title != nil && sess.PRNumber != nil && *sess.PRNumber > 0 {
		repo, repoErr := s.repos.Get(ctx, sess.RepoID)
		if repoErr == nil && repo.OriginURL != "" {
			if prErr := s.provider.UpdatePRTitle(ctx, repo.OriginURL, *sess.PRNumber, *params.Title); prErr != nil {
				s.logger.Warn().Err(prErr).
					Str("session", sess.ID).
					Int("pr", *sess.PRNumber).
					Msg("failed to update PR title (best-effort)")
			}
		}
	}

	p := SessionToProto(sess)
	if repo, err := s.repos.Get(ctx, sess.RepoID); err == nil {
		p.RepoDisplayName = repo.DisplayName
		p.RepoOriginUrl = CanonicalRepoOriginURL(repo.OriginURL)
	}

	// Propagate the update (e.g. a rename) to the cloud/web via the upstream
	// stream, matching LinkSessionPR/MarkArchived/Resurrect. Without this the local
	// DB and TUI reflect the new title but bosso never sees it until the next
	// full daemon snapshot.
	if s.onSessionUpdated != nil {
		s.onSessionUpdated(ctx, p)
	}

	return connect.NewResponse(&pb.UpdateSessionResponse{Session: p}), nil
}

func (s *Server) LinkSessionPR(ctx context.Context, req *connect.Request[pb.LinkSessionPRRequest]) (*connect.Response[pb.LinkSessionPRResponse], error) {
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}
	if strings.TrimSpace(req.Msg.Pr) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("pr is required"))
	}

	sess, err := s.lifecycle.LinkPR(ctx, req.Msg.Id, req.Msg.Pr)
	if session.IsDuplicateActiveSessionError(err) {
		return nil, connect.NewError(connect.CodeAlreadyExists, err)
	}
	if errors.Is(err, session.ErrStartPathBusy) {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("link PR: %w", err))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("link PR: %w", err))
	}
	s.refreshLinkedPRDisplay(ctx, sess)

	p := s.sessionProtoWithRepo(ctx, sess)
	if s.onSessionUpdated != nil {
		s.onSessionUpdated(ctx, p)
	}

	return connect.NewResponse(&pb.LinkSessionPRResponse{
		Session:        p,
		WorktreeBranch: s.lifecycle.WorktreeBranchMismatch(ctx, sess),
	}), nil
}

// refreshLinkedPRDisplay replaces the display entry of the session's previous
// PR with the linked one's. A closed or merged entry left behind would make the
// display poller skip the session as settled and keep it hidden as done. When
// the fresh read fails the entry is dropped, so the next poll reads the linked
// PR instead.
func (s *Server) refreshLinkedPRDisplay(ctx context.Context, sess *models.Session) {
	if s.displayTracker == nil || sess.PRNumber == nil {
		return
	}
	repo, err := s.repos.Get(ctx, sess.RepoID)
	if err == nil {
		refreshCtx, cancel := context.WithTimeout(ctx, refreshSessionPRTimeout)
		err = s.refreshSessionPRDisplay(refreshCtx, repo, sess.ID, *sess.PRNumber)
		cancel()
	}
	if err != nil {
		s.logger.Warn().Err(err).Str("session", sess.ID).Msg("link PR: refresh display status; dropping the stale entry")
		s.displayTracker.Remove(sess.ID)
	}
}
