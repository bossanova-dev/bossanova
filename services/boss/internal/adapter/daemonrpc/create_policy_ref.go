package daemonrpc

import (
	"context"

	"github.com/recurser/boss/internal/adapter/rpcerror"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/daemoncaps"
)

// requirePolicyRefCapability probes the daemon before a trial create, so an
// older daemon cannot drop policy_ref and launch a session that verifies
// under the base policy. A create without a policy ref needs no probe.
func (c *LocalClient) requirePolicyRefCapability(ctx context.Context, req *pb.CreateSessionRequest) error {
	if req.GetPolicyRef() == "" {
		return nil
	}
	if err := daemoncaps.Require(ctx, c.rpc, daemoncaps.CreateSessionPolicyRef); err != nil {
		return rpcerror.WrapError(err)
	}
	return nil
}
