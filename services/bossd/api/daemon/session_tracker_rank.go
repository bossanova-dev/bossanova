package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/contract/daemon/trackerref"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/domain"
)

// SetSessionTrackerRank records rank without resolving or changing the tracker link.
func (s *Server) SetSessionTrackerRank(ctx context.Context, req *connect.Request[pb.SetSessionTrackerRankRequest]) (*connect.Response[pb.SetSessionTrackerRankResponse], error) {
	msg := req.Msg
	rank := msg.GetTrackerRank()
	if msg.GetId() == "" || rank == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id and tracker_rank are required"))
	}
	if rank.Tier < 0 || rank.Tier > 4 || rank.UnblockedTicketCount < 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("tier must be 0..4 and unblocked_ticket_count must be nonnegative"))
	}
	if rank.DeadlineOn != nil {
		if _, err := time.Parse("2006-01-02", *rank.DeadlineOn); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("deadline_on must be YYYY-MM-DD: %w", err))
		}
	}
	trackerID := strings.TrimSpace(msg.GetTrackerId())
	repository := ""
	if id, ok := trackerref.ParseID(trackerID); ok {
		trackerID = id
	} else if ref, ok := trackerref.ParseGitHubIssue(trackerID); ok {
		trackerID = fmt.Sprintf("GH-%d", ref.Number)
		repository = ref.Repository
	} else {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("tracker_id %q is not a tracker issue id", trackerID))
	}
	sess, err := s.sessions.Get(ctx, msg.GetId())
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session %q not found", msg.GetId()))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get session: %w", err))
	}
	storedID := ""
	if sess.TrackerID != nil {
		storedID = *sess.TrackerID
	}
	if storedID != trackerID {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("session tracker_id %q differs from rank tracker_id %q", storedID, trackerID))
	}
	if repository != "" && !rankRepositoryMatches(sess.TrackerURL, repository) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("rank issue repository %q does not match the session tracker link", repository))
	}
	applied, err := s.sessions.SetTrackerRank(ctx, sess.ID, trackerID, sess.TrackerURL, models.TrackerRank{Tier: rank.Tier, TierSource: rank.TierSource, DeadlineOn: rank.DeadlineOn, DeadlineSource: rank.DeadlineSource, IsScheduled: rank.IsScheduled, UnblockedTicketCount: rank.UnblockedTicketCount, RankedAt: time.Now().UTC()})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("set tracker rank: %w", err))
	}
	if !applied {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("session tracker link changed while storing rank for %q (previous link %q)", trackerID, storedID))
	}
	updated, err := s.sessions.Get(ctx, sess.ID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("reload session: %w", err))
	}
	p := s.sessionProtoWithRepo(ctx, updated)
	if s.onSessionUpdated != nil {
		s.onSessionUpdated(ctx, p)
	}
	return connect.NewResponse(&pb.SetSessionTrackerRankResponse{Session: p}), nil
}

func rankRepositoryMatches(storedURL *string, repository string) bool {
	if storedURL == nil {
		return false
	}
	ref, ok := trackerref.ParseGitHubIssue(*storedURL)
	return ok && strings.EqualFold(ref.Repository, repository)
}
