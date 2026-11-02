package cloudrpc

import (
	"context"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/adapter/rpcerror"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// ListSyncedAgentAccounts reads the caller's hosted synced-account registry:
// each row with its member daemons, plus the pending link suggestions. It is
// user-authenticated, so the boss client calls it with the user's own login.
func (c *RemoteClient) ListSyncedAgentAccounts(ctx context.Context) (*pb.ListSyncedAgentAccountsResponse, error) {
	resp, err := c.rpc.ListSyncedAgentAccounts(ctx, connect.NewRequest(&pb.ListSyncedAgentAccountsRequest{}))
	if err != nil {
		return nil, rpcerror.WrapError(err)
	}
	return resp.Msg, nil
}
