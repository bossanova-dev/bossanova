package daemonrpc

import (
	"context"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/adapter/rpcerror"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// SetSessionTrackerRank stores rank without changing the session's tracker link.
func (c *LocalClient) SetSessionTrackerRank(ctx context.Context, id, trackerID string, rank *pb.TrackerRank) (*pb.Session, error) {
	resp, err := c.rpc.SetSessionTrackerRank(ctx, connect.NewRequest(&pb.SetSessionTrackerRankRequest{Id: id, TrackerId: trackerID, TrackerRank: rank}))
	if err != nil {
		return nil, rpcerror.WrapError(err)
	}
	return resp.Msg.GetSession(), nil
}
