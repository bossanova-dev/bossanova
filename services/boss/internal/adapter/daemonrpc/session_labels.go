package daemonrpc

import (
	"context"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/adapter/rpcerror"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func (c *LocalClient) ListSessionLabels(ctx context.Context) ([]*pb.SessionLabel, error) {
	resp, err := c.rpc.ListSessionLabels(ctx, connect.NewRequest(&pb.ListSessionLabelsRequest{}))
	if err != nil {
		return nil, rpcerror.WrapError(err)
	}
	return resp.Msg.GetLabels(), nil
}

func (c *LocalClient) CreateSessionLabel(ctx context.Context, req *pb.CreateSessionLabelRequest) (*pb.SessionLabel, error) {
	resp, err := c.rpc.CreateSessionLabel(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, rpcerror.WrapError(err)
	}
	return resp.Msg.GetLabel(), nil
}

func (c *LocalClient) UpdateSessionLabel(ctx context.Context, req *pb.UpdateSessionLabelRequest) (*pb.SessionLabel, error) {
	resp, err := c.rpc.UpdateSessionLabel(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, rpcerror.WrapError(err)
	}
	return resp.Msg.GetLabel(), nil
}

func (c *LocalClient) DeleteSessionLabel(ctx context.Context, id string) error {
	_, err := c.rpc.DeleteSessionLabel(ctx, connect.NewRequest(&pb.DeleteSessionLabelRequest{Id: id}))
	return rpcerror.WrapError(err)
}

func (c *LocalClient) SetSessionLabels(ctx context.Context, sessionID string, labelIDs []string) (*pb.Session, error) {
	resp, err := c.rpc.SetSessionLabels(ctx, connect.NewRequest(&pb.SetSessionLabelsRequest{
		SessionId: sessionID,
		LabelIds:  labelIDs,
	}))
	if err != nil {
		return nil, rpcerror.WrapError(err)
	}
	return resp.Msg.GetSession(), nil
}
