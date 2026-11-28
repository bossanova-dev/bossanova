package server

import (
	"context"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func (s *Server) CheckAgentLogins(
	ctx context.Context,
	_ *connect.Request[pb.CheckAgentLoginsRequest],
) (*connect.Response[pb.CheckAgentLoginsResponse], error) {
	results := s.agentLogins(ctx)
	if len(results) == 0 {
		return connect.NewResponse(&pb.CheckAgentLoginsResponse{}), nil
	}
	agents := make([]*pb.AgentLogin, len(results))
	for i, r := range results {
		agents[i] = agentLoginToProto(r)
	}
	return connect.NewResponse(&pb.CheckAgentLoginsResponse{Agents: agents}), nil
}

func agentLoginToProto(r agentLoginResult) *pb.AgentLogin {
	switch r.State {
	case agentLoginLoggedIn:
		return &pb.AgentLogin{
			AgentName: r.Agent,
			Status:    pb.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_IN,
		}
	case agentLoginLoggedOut:
		return &pb.AgentLogin{
			AgentName: r.Agent,
			Status:    pb.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT,
			Remedy:    agentLoginRemedy(r),
		}
	case agentLoginManaged:
		return &pb.AgentLogin{
			AgentName:         r.Agent,
			Status:            pb.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
			HasManagedAccount: true,
		}
	default:
		return &pb.AgentLogin{
			AgentName: r.Agent,
			Status:    pb.AgentLoginStatus_AGENT_LOGIN_STATUS_UNKNOWN,
		}
	}
}
