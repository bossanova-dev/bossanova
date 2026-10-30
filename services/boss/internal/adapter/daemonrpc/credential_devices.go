package daemonrpc

import (
	"context"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/adapter/rpcerror"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func (c *LocalClient) GetAgentCredentialDeviceStatus(ctx context.Context) (*pb.GetAgentCredentialDeviceStatusResponse, error) {
	resp, err := c.rpc.GetAgentCredentialDeviceStatus(ctx, connect.NewRequest(&pb.GetAgentCredentialDeviceStatusRequest{}))
	if err != nil {
		return nil, rpcerror.WrapError(err)
	}
	return resp.Msg, nil
}
func (c *LocalClient) ListAgentCredentialDevices(ctx context.Context) (*pb.ListAgentCredentialDevicesResponse, error) {
	resp, err := c.rpc.ListAgentCredentialDevices(ctx, connect.NewRequest(&pb.ListAgentCredentialDevicesRequest{}))
	if err != nil {
		return nil, rpcerror.WrapError(err)
	}
	return resp.Msg, nil
}
func (c *LocalClient) ApproveAgentCredentialDevice(ctx context.Context, req *pb.ApproveAgentCredentialDeviceRequest) error {
	_, err := c.rpc.ApproveAgentCredentialDevice(ctx, connect.NewRequest(req))
	return rpcerror.WrapError(err)
}
func (c *LocalClient) RevokeAgentCredentialDevice(ctx context.Context, req *pb.RevokeAgentCredentialDeviceRequest) error {
	_, err := c.rpc.RevokeAgentCredentialDevice(ctx, connect.NewRequest(req))
	return rpcerror.WrapError(err)
}
