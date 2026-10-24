package server

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func (s *Server) NotifyAuthChange(ctx context.Context, req *connect.Request[pb.NotifyAuthChangeRequest]) (*connect.Response[pb.NotifyAuthChangeResponse], error) {
	action := req.Msg.Action
	if action != "login" && action != "logout" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("action must be \"login\" or \"logout\""))
	}

	if s.authNotifier == nil {
		// No orchestrator configured — nothing to do.
		return connect.NewResponse(&pb.NotifyAuthChangeResponse{}), nil
	}

	switch action {
	case "login":
		repos, err := s.repos.List(ctx)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list repos: %w", err))
		}
		repoIDs := make([]string, len(repos))
		for i, r := range repos {
			repoIDs[i] = r.ID
		}
		verdict, err := s.authNotifier.NotifyLogin(ctx, repoIDs)
		if err != nil {
			s.logger.Warn().Err(err).Msg("upstream connect after login failed")
			// Non-fatal for the RPC: the daemon still works in local mode, and
			// the caller is `boss login`, which must not fail because a
			// separate process is unhappy. The failure is no longer silent
			// though — it rides back in the verdict below so the operator's
			// terminal can say so.
		}
		return connect.NewResponse(loginVerdictToProto(verdict)), nil
	case "logout":
		s.authNotifier.NotifyLogout()
	}

	// Logout evaluated no credentials, so it has no verdict to report.
	return connect.NewResponse(&pb.NotifyAuthChangeResponse{}), nil
}
