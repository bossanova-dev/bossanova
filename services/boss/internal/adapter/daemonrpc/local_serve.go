package daemonrpc

import (
	"context"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/adapter/rpcerror"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// StartSessionServe launches the repository serve command on the local daemon.
func (c *LocalClient) StartSessionServe(ctx context.Context, id string) (*pb.SessionServe, error) {
	resp, err := c.rpc.StartSessionServe(ctx, connect.NewRequest(&pb.StartSessionServeRequest{SessionId: id}))
	if err != nil {
		return nil, rpcerror.WrapError(err)
	}
	return resp.Msg.Serve, nil
}

// StopSessionServe stops the repository serve command on the local daemon.
func (c *LocalClient) StopSessionServe(ctx context.Context, id string) (*pb.SessionServe, error) {
	resp, err := c.rpc.StopSessionServe(ctx, connect.NewRequest(&pb.StopSessionServeRequest{SessionId: id}))
	if err != nil {
		return nil, rpcerror.WrapError(err)
	}
	return resp.Msg.Serve, nil
}

// GetSessionServe reads the local repository serve command's state.
func (c *LocalClient) GetSessionServe(ctx context.Context, id string) (*pb.SessionServe, error) {
	resp, err := c.rpc.GetSessionServe(ctx, connect.NewRequest(&pb.GetSessionServeRequest{SessionId: id}))
	if err != nil {
		return nil, rpcerror.WrapError(err)
	}
	return resp.Msg.Serve, nil
}
