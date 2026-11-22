package server

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

const recentAgentModelLimit = 10

// ModelUsageReader lists the model IDs an agent recently ran with here.
type ModelUsageReader interface {
	ListRecent(ctx context.Context, agentName string, limit int) ([]string, error)
}

type agentModelLister interface {
	ListModels(context.Context, *pb.ListModelsRequest) (*pb.ListModelsResponse, error)
}

// ListAgentModels answers the model picker for one loaded agent: the agent's
// own list, empty when its plugin cannot list models, and the models this
// daemon recently ran it with.
func (s *Server) ListAgentModels(ctx context.Context, agentName string) (*pb.AgentModelsResult, error) {
	client, ok := s.agentClients[agentName]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("agent %q is not loaded on this machine", agentName))
	}
	out := &pb.AgentModelsResult{}
	if lister, ok := client.(agentModelLister); ok {
		resp, err := lister.ListModels(ctx, &pb.ListModelsRequest{})
		if err != nil && status.Code(err) != codes.Unimplemented {
			return nil, fmt.Errorf("list %s models: %w", agentName, err)
		}
		out.Models = resp.GetModels()
	}
	if s.modelUsage == nil {
		return out, nil
	}
	recent, err := s.modelUsage.ListRecent(ctx, agentName, recentAgentModelLimit)
	if err != nil {
		// The picker still works without history, so a failed read only logs.
		s.logger.Warn().Err(err).Str("agent", agentName).Msg("ListAgentModels: recent models unavailable")
		return out, nil
	}
	out.RecentModelIds = recent
	return out, nil
}
