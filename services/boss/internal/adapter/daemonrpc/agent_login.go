package daemonrpc

import (
	"context"

	"connectrpc.com/connect"

	"github.com/recurser/boss/internal/adapter/rpcerror"
	"github.com/recurser/boss/internal/domain/rpc"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func (c *LocalClient) CheckAgentLogins(ctx context.Context) ([]rpc.AgentLogin, error) {
	resp, err := c.rpc.CheckAgentLogins(ctx, connect.NewRequest(&pb.CheckAgentLoginsRequest{}))
	if err != nil {
		return nil, rpcerror.WrapError(err)
	}
	out := make([]rpc.AgentLogin, 0, len(resp.Msg.GetAgents()))
	for _, a := range resp.Msg.GetAgents() {
		out = append(out, rpc.AgentLoginFromProto(a))
	}
	return out, nil
}
