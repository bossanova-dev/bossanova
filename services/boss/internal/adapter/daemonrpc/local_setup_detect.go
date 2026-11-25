package daemonrpc

import (
	"context"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/adapter/rpcerror"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// DetectSetupScript asks the local daemon to inspect localPath for a worktree setup command.
func (c *LocalClient) DetectSetupScript(ctx context.Context, localPath string) (*pb.DetectSetupScriptResponse, error) {
	resp, err := c.rpc.DetectSetupScript(ctx, connect.NewRequest(&pb.DetectSetupScriptRequest{
		LocalPath: localPath,
	}))
	if err != nil {
		return nil, rpcerror.WrapError(err)
	}
	return resp.Msg, nil
}
