package daemonrpc

import (
	"context"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/adapter/rpcerror"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// GetGitHubReadCredential fetches a local repository read credential.
// The response contains a secret and must never be logged.
func (c *LocalClient) GetGitHubReadCredential(ctx context.Context, req *pb.GetGitHubReadCredentialRequest) (*pb.GetGitHubReadCredentialResponse, error) {
	response, err := c.rpc.GetGitHubReadCredential(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, rpcerror.WrapError(err)
	}
	return response.Msg, nil
}
