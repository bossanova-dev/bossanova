package daemonrpc

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func (d wedgedDaemon) GetAgentCredentialDeviceStatus(context.Context, *connect.Request[pb.GetAgentCredentialDeviceStatusRequest]) (*connect.Response[pb.GetAgentCredentialDeviceStatusResponse], error) {
	<-d.release
	return nil, context.Canceled
}
func (d wedgedDaemon) ListAgentCredentialDevices(context.Context, *connect.Request[pb.ListAgentCredentialDevicesRequest]) (*connect.Response[pb.ListAgentCredentialDevicesResponse], error) {
	<-d.release
	return nil, context.Canceled
}
func (d wedgedDaemon) ApproveAgentCredentialDevice(context.Context, *connect.Request[pb.ApproveAgentCredentialDeviceRequest]) (*connect.Response[pb.ApproveAgentCredentialDeviceResponse], error) {
	<-d.release
	return nil, context.Canceled
}
func (d wedgedDaemon) RevokeAgentCredentialDevice(context.Context, *connect.Request[pb.RevokeAgentCredentialDeviceRequest]) (*connect.Response[pb.RevokeAgentCredentialDeviceResponse], error) {
	<-d.release
	return nil, context.Canceled
}

func TestCredentialDeviceRPCsBounded(t *testing.T) {
	shrinkDefaultRPCDeadline(t, 50*time.Millisecond)
	socket, _ := startWedgedDaemon(t, time.Second)
	c := NewLocal(socket)
	for name, call := range map[string]func() error{
		"status": func() error { _, err := c.GetAgentCredentialDeviceStatus(context.Background()); return err },
		"list":   func() error { _, err := c.ListAgentCredentialDevices(context.Background()); return err },
		"approve": func() error {
			return c.ApproveAgentCredentialDevice(context.Background(), &pb.ApproveAgentCredentialDeviceRequest{DaemonId: "target", Fingerprint: "abcd"})
		},
		"revoke": func() error {
			return c.RevokeAgentCredentialDevice(context.Background(), &pb.RevokeAgentCredentialDeviceRequest{DaemonId: "target"})
		},
	} {
		t.Run(name, func(t *testing.T) { _, err := callWithinBudget(t, call); assertWedgedDaemonError(t, err, socket) })
	}
}
