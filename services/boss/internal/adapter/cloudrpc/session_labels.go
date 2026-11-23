package cloudrpc

import (
	"context"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/adapter/rpcerror"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func (c *RemoteClient) ListSessionLabels(ctx context.Context) ([]*pb.SessionLabel, error) {
	resp, err := c.rpc.ProxyListSessionLabels(ctx, connect.NewRequest(&pb.ProxyListSessionLabelsRequest{}))
	if err != nil {
		return nil, rpcerror.WrapError(err)
	}
	return resp.Msg.GetLabels(), nil
}

func (c *RemoteClient) CreateSessionLabel(ctx context.Context, req *pb.CreateSessionLabelRequest) (*pb.SessionLabel, error) {
	resp, err := c.rpc.ProxyCreateSessionLabel(ctx, connect.NewRequest(&pb.ProxyCreateSessionLabelRequest{
		Name:        req.GetName(),
		Color:       req.GetColor(),
		Description: req.GetDescription(),
	}))
	if err != nil {
		return nil, rpcerror.WrapError(err)
	}
	return resp.Msg.GetLabel(), nil
}

func (c *RemoteClient) UpdateSessionLabel(ctx context.Context, req *pb.UpdateSessionLabelRequest) (*pb.SessionLabel, error) {
	resp, err := c.rpc.ProxyUpdateSessionLabel(ctx, connect.NewRequest(&pb.ProxyUpdateSessionLabelRequest{
		Id:          req.GetId(),
		Name:        req.Name,
		Color:       req.Color,
		Description: req.Description,
	}))
	if err != nil {
		return nil, rpcerror.WrapError(err)
	}
	return resp.Msg.GetLabel(), nil
}

func (c *RemoteClient) DeleteSessionLabel(ctx context.Context, id string) error {
	_, err := c.rpc.ProxyDeleteSessionLabel(ctx, connect.NewRequest(&pb.ProxyDeleteSessionLabelRequest{Id: id}))
	return rpcerror.WrapError(err)
}

func (c *RemoteClient) SetSessionLabels(ctx context.Context, sessionID string, labelIDs []string) (*pb.Session, error) {
	resp, err := c.rpc.ProxySetSessionLabels(ctx, connect.NewRequest(&pb.ProxySetSessionLabelsRequest{
		SessionId: sessionID,
		LabelIds:  labelIDs,
	}))
	if err != nil {
		return nil, rpcerror.WrapError(err)
	}
	return resp.Msg.GetSession(), nil
}
