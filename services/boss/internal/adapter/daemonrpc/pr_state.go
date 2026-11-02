package daemonrpc

import (
	"context"

	"connectrpc.com/connect"

	"github.com/recurser/boss/internal/adapter/rpcerror"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// GetPRState reads one PR's state through the daemon's GitHub read cache.
func (c *LocalClient) GetPRState(ctx context.Context, req *pb.GetPRStateRequest) (*pb.GetPRStateResponse, error) {
	resp, err := c.rpc.GetPRState(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, rpcerror.WrapError(err)
	}
	return resp.Msg, nil
}
