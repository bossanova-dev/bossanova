package daemonrpc

import (
	"context"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// ListRepoCloudControls reads local operator grants through the authenticated socket.
func (c *LocalClient) ListRepoCloudControls(ctx context.Context) ([]*pb.RepoCloudControl, error) {
	response, err := c.rpc.ListRepoCloudControls(ctx, connect.NewRequest(&pb.ListRepoCloudControlsRequest{}))
	if err != nil {
		return nil, err
	}
	return response.Msg.GetRepos(), nil
}
