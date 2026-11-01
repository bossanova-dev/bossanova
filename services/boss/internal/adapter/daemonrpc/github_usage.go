package daemonrpc

import (
	"context"

	"connectrpc.com/connect"

	"github.com/recurser/boss/internal/adapter/rpcerror"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// GetGitHubUsage reads the daemon's per-label, per-quota GitHub call counts.
func (c *LocalClient) GetGitHubUsage(ctx context.Context, req *pb.GetGitHubUsageRequest) (*pb.GetGitHubUsageResponse, error) {
	resp, err := c.rpc.GetGitHubUsage(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, rpcerror.WrapError(err)
	}
	return resp.Msg, nil
}
