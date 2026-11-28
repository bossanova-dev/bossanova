package daemonrpc

import (
	"context"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/adapter/rpcerror"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// RefreshRepoOrigin re-reads a repo's origin remote into its stored origin.
func (c *LocalClient) RefreshRepoOrigin(ctx context.Context, id string) (*pb.RefreshRepoOriginResponse, error) {
	resp, err := c.rpc.RefreshRepoOrigin(ctx, connect.NewRequest(&pb.RefreshRepoOriginRequest{Id: id}))
	if err != nil {
		return nil, rpcerror.WrapError(err)
	}
	return resp.Msg, nil
}
