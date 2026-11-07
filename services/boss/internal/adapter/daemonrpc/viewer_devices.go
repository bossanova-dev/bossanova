package daemonrpc

import (
	"context"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/adapter/rpcerror"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func (c *LocalClient) GetViewerDeviceStatus(ctx context.Context) (*pb.GetViewerDeviceStatusResponse, error) {
	response, err := c.rpc.GetViewerDeviceStatus(ctx, connect.NewRequest(&pb.GetViewerDeviceStatusRequest{}))
	if err != nil {
		return nil, rpcerror.WrapError(err)
	}
	return response.Msg, nil
}
func (c *LocalClient) ListViewerDevices(ctx context.Context) (*pb.ListLocalViewerDevicesResponse, error) {
	response, err := c.rpc.ListViewerDevices(ctx, connect.NewRequest(&pb.ListLocalViewerDevicesRequest{}))
	if err != nil {
		return nil, rpcerror.WrapError(err)
	}
	return response.Msg, nil
}
func (c *LocalClient) ApproveViewerDevice(ctx context.Context, request *pb.ApproveViewerDeviceRequest) error {
	_, err := c.rpc.ApproveViewerDevice(ctx, connect.NewRequest(request))
	return rpcerror.WrapError(err)
}
func (c *LocalClient) RevokeViewerDevice(ctx context.Context, request *pb.RevokeViewerDeviceRequest) error {
	_, err := c.rpc.RevokeViewerDevice(ctx, connect.NewRequest(request))
	return rpcerror.WrapError(err)
}
