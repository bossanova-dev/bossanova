package cloudrpc

import (
	"context"

	"github.com/recurser/boss/internal/adapter/rpcerror"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// DetectSetupScript inspects a local directory and is only available on a local daemon.
func (c *RemoteClient) DetectSetupScript(_ context.Context, _ string) (*pb.DetectSetupScriptResponse, error) {
	return nil, rpcerror.WrapError(errLocalOnly("DetectSetupScript"))
}
