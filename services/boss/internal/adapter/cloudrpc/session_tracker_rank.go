package cloudrpc

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/adapter/rpcerror"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// SetSessionTrackerRank is local-daemon only, like SetSessionTracker.
func (c *RemoteClient) SetSessionTrackerRank(context.Context, string, string, *pb.TrackerRank) (*pb.Session, error) {
	return nil, rpcerror.WrapError(connect.NewError(connect.CodeUnimplemented, errors.New("setting a tracker rank is only available against a local daemon")))
}
