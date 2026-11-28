package server

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"

	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/repoorigin"
)

// RefreshRepoOrigin persists the repo checkout's origin remote as its stored
// origin and reports the origin it replaced.
func (s *Server) RefreshRepoOrigin(ctx context.Context, req *connect.Request[pb.RefreshRepoOriginRequest]) (*connect.Response[pb.RefreshRepoOriginResponse], error) {
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}
	repo, err := s.repos.Get(ctx, req.Msg.Id)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("repo not found: %s", req.Msg.Id))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get repo: %w", err))
	}
	previous, err := repoorigin.Refresh(ctx, s.worktrees, s.repos, repo, s.logger)
	switch {
	case errors.Is(err, repoorigin.ErrNoRemote):
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s has no origin remote; stored origin %q kept", repo.LocalPath, previous))
	case errors.Is(err, repoorigin.ErrUnhostedRemote):
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s: %w; stored origin %q kept", repo.LocalPath, err, previous))
	case errors.Is(err, domain.ErrNotFound):
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("repo not found: %s", req.Msg.Id))
	case errors.Is(err, domain.ErrAmbiguousOrigin):
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("refresh origin: %w", err))
	case err != nil:
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("refresh origin: %w", err))
	}
	if previous != repo.OriginURL {
		s.republishRepoSessions(ctx, repo.ID)
	}
	return connect.NewResponse(&pb.RefreshRepoOriginResponse{Repo: s.liveRepoToProto(repo), PreviousOriginUrl: previous}), nil
}
