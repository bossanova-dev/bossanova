package server

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/reviewqueue"
)

// ListReviewQueue serves the repo's PRs that verify has parked for a person.
// It reads the repo's sessions through ListSessions, so every row's session is
// hydrated exactly as the session list serves it.
func (s *Server) ListReviewQueue(ctx context.Context, req *connect.Request[pb.ListReviewQueueRequest]) (*connect.Response[pb.ListReviewQueueResponse], error) {
	repoID := req.Msg.GetRepoId()
	if repoID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("repo_id is required"))
	}
	listed, err := s.ListSessions(ctx, connect.NewRequest(&pb.ListSessionsRequest{RepoId: &repoID}))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	archived, err := s.loadSessionsForList(ctx, repoID, true, []pb.SessionState{pb.SessionState_SESSION_STATE_MERGED})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("load archived audit sessions"))
	}
	sessions := listed.Msg.GetSessions()
	for _, row := range archived {
		if row.ArchivedAt == nil || row.PRNumber == nil || row.StateEnteredAt == nil || now.Sub(*row.StateEnteredAt) >= reviewqueue.AuditWindow {
			continue
		}
		resp, err := s.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: row.ID}))
		if connect.CodeOf(err) == connect.CodeNotFound {
			continue
		}
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, resp.Msg.GetSession())
	}
	rows, counts := reviewqueue.Build(sessions, req.Msg.GetSort(), now)
	return connect.NewResponse(&pb.ListReviewQueueResponse{Rows: rows, LevelCounts: counts}), nil
}
