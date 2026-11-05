package daemonrpc

import (
	"context"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/adapter/rpcerror"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// AcquireTrackerIssueLease takes or renews a tracker issue lease through the
// local daemon, which forwards signed-in work and falls back to its own lease.
func (c *LocalClient) AcquireTrackerIssueLease(ctx context.Context, req *pb.AcquireTrackerIssueLeaseRequest) (*pb.AcquireTrackerIssueLeaseResponse, error) {
	resp, err := c.rpc.AcquireTrackerIssueLease(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, rpcerror.WrapError(err)
	}
	return resp.Msg, nil
}

// ReleaseTrackerIssueLease releases a tracker issue lease through the local daemon.
func (c *LocalClient) ReleaseTrackerIssueLease(ctx context.Context, req *pb.ReleaseTrackerIssueLeaseRequest) (*pb.ReleaseTrackerIssueLeaseResponse, error) {
	resp, err := c.rpc.ReleaseTrackerIssueLease(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, rpcerror.WrapError(err)
	}
	return resp.Msg, nil
}
