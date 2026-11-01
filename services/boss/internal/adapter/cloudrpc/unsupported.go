package cloudrpc

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/adapter/rpcerror"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// errLocalOnly is returned for operations that only work on a local daemon.
func errLocalOnly(op string) error {
	return rpcerror.WrapError(connect.NewError(connect.CodeUnimplemented, fmt.Errorf("%s is only available on a local daemon", op)))
}

// GetGitHubUsage reads one daemon's own usage ledger, so it has no remote form.
func (c *RemoteClient) GetGitHubUsage(context.Context, *pb.GetGitHubUsageRequest) (*pb.GetGitHubUsageResponse, error) {
	return nil, errLocalOnly("GitHub usage")
}
