package daemonrpc

import (
	"context"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/contract/daemon/callerorigin"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"

	"github.com/recurser/boss/internal/adapter/rpcerror"
)

// NewForwarded is NewLocalWithToken for a socket forwarded from another host's
// daemon (`boss --host`). Every call asserts origin, the caller this process
// runs for, so the remote daemon can cap what the forward would otherwise
// attribute to the operator.
func NewForwarded(socketPath, token string, origin callerorigin.Assertion) *LocalClient {
	return newLocalClient(socketPath, token, callerorigin.NewClientInterceptor(origin))
}

// DescribeCaller asks the daemon which principal it attributes this process to.
func (c *LocalClient) DescribeCaller(ctx context.Context) (*pb.DescribeCallerResponse, error) {
	resp, err := c.rpc.DescribeCaller(ctx, connect.NewRequest(&pb.DescribeCallerRequest{}))
	if err != nil {
		return nil, rpcerror.WrapError(err)
	}
	return resp.Msg, nil
}
