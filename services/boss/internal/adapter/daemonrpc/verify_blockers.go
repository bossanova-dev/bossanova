package daemonrpc

import (
	"context"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/adapter/rpcerror"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// AcceptSessionPRBlocker accepts a review finding on the displayed PR head.
func (c *LocalClient) AcceptSessionPRBlocker(ctx context.Context, req *pb.AcceptSessionPRBlockerRequest) (*pb.AcceptSessionPRBlockerResponse, error) {
	resp, err := c.rpc.AcceptSessionPRBlocker(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, rpcerror.WrapError(err)
	}
	return resp.Msg, nil
}

// ListSessionPRBlockers lists acceptable findings on the displayed PR head.
func (c *LocalClient) ListSessionPRBlockers(ctx context.Context, id string) (*pb.ListSessionPRBlockersResponse, error) {
	resp, err := c.rpc.ListSessionPRBlockers(ctx, connect.NewRequest(&pb.ListSessionPRBlockersRequest{Id: id}))
	if err != nil {
		return nil, rpcerror.WrapError(err)
	}
	return resp.Msg, nil
}
