package cloudrpc

import (
	"context"
	"fmt"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func credentialDeviceLocalOnly() error {
	return fmt.Errorf("credential device enrollment requires a local daemon; run without --remote")
}
func (*RemoteClient) GetAgentCredentialDeviceStatus(context.Context) (*pb.GetAgentCredentialDeviceStatusResponse, error) {
	return nil, credentialDeviceLocalOnly()
}
func (*RemoteClient) ListAgentCredentialDevices(context.Context) (*pb.ListAgentCredentialDevicesResponse, error) {
	return nil, credentialDeviceLocalOnly()
}
func (*RemoteClient) ApproveAgentCredentialDevice(context.Context, *pb.ApproveAgentCredentialDeviceRequest) error {
	return credentialDeviceLocalOnly()
}
func (*RemoteClient) RevokeAgentCredentialDevice(context.Context, *pb.RevokeAgentCredentialDeviceRequest) error {
	return credentialDeviceLocalOnly()
}
