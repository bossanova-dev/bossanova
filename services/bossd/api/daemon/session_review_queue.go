package server

import (
	"context"
	"errors"

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
	rows, counts := reviewqueue.Build(listed.Msg.GetSessions(), req.Msg.GetSort())
	return connect.NewResponse(&pb.ListReviewQueueResponse{Rows: rows, LevelCounts: counts}), nil
}
