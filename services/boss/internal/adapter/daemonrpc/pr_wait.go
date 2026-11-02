package daemonrpc

import (
	"context"
	"time"

	"connectrpc.com/connect"

	"github.com/recurser/boss/internal/adapter/rpcerror"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
)

// prWaitGrace is how much longer than the wait the client listens, so the
// daemon's own timed_out answer arrives before the client gives up.
const prWaitGrace = 30 * time.Second

// WaitPRState blocks on one PR condition through the daemon's webhook-woken
// wait. Without a caller deadline the 30s default bound would cut the wait
// off, so the call is bounded by the timeout the daemon applies plus a grace.
func (c *LocalClient) WaitPRState(ctx context.Context, req *pb.WaitPRStateRequest) (*pb.WaitPRStateResponse, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, config.PRWaitTimeout(req.GetTimeoutSeconds())+prWaitGrace)
		defer cancel()
	}
	resp, err := c.rpc.WaitPRState(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, rpcerror.WrapError(err)
	}
	return resp.Msg, nil
}
