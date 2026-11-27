package daemonrpc

import (
	"context"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/adapter/rpcerror"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func (c *LocalClient) ListSetupChecks(ctx context.Context, req *pb.ListSetupChecksRequest) (*pb.ListSetupChecksResponse, error) {
	resp, err := c.rpc.ListSetupChecks(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, rpcerror.WrapError(err)
	}
	return resp.Msg, nil
}
