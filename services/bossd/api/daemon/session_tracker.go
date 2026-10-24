package server

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	"github.com/recurser/bossalib/contract/daemon/trackerref"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/session"
)

// SetSessionTracker links a session to a tracker issue through the same
// resolver the periodic tracker-link pass uses: the id must name an issue the
// repo's own tracker returns, and the stored URL is the tracker's. It
// overwrites an existing link — this is the correction path for a link the
// pass got wrong or could not find.
func (s *Server) SetSessionTracker(ctx context.Context, req *connect.Request[pb.SetSessionTrackerRequest]) (*connect.Response[pb.SetSessionTrackerResponse], error) {
	msg := req.Msg
	if msg.GetId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id is required"))
	}
	trackerID, ok := trackerref.ParseID(msg.GetTrackerId())
	if !ok {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("tracker_id %q is not a tracker issue id (e.g. ABC-123)", msg.GetTrackerId()))
	}

	sess, err := s.sessions.Get(ctx, msg.GetId())
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session %q not found", msg.GetId()))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get session: %w", err))
	}
	repo, err := s.repos.Get(ctx, sess.RepoID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get repo: %w", err))
	}
	if s.trackerResolver == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("no Linear tracker configured for this repo"))
	}

	canonicalID, url, err := s.trackerResolver.ResolveTrackerIssue(ctx, repo, trackerID)
	switch {
	case errors.Is(err, session.ErrTrackerNotConfigured):
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("no Linear tracker configured for this repo: %w", err))
	case errors.Is(err, session.ErrTrackerIssueNotFound):
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("%s not found among the tracker's open issues", trackerID))
	case err != nil:
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("resolve tracker issue %s: %w", trackerID, err))
	}

	idPtr := &canonicalID
	urlPtr := &url
	updated, err := s.sessions.Update(ctx, sess.ID, domain.UpdateSessionParams{
		TrackerID:  &idPtr,
		TrackerURL: &urlPtr,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("update session: %w", err))
	}

	p := s.sessionProtoWithRepo(ctx, updated)
	if s.onSessionUpdated != nil {
		s.onSessionUpdated(ctx, p)
	}
	return connect.NewResponse(&pb.SetSessionTrackerResponse{Session: p}), nil
}
