package cloudrpc

import (
	"context"
	"strings"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestCredentialDevicesRequireLocalDaemon(t *testing.T) {
	c := &RemoteClient{}
	for name, call := range map[string]func() error{
		"status": func() error { _, err := c.GetAgentCredentialDeviceStatus(context.Background()); return err },
		"list":   func() error { _, err := c.ListAgentCredentialDevices(context.Background()); return err },
		"approve": func() error {
			return c.ApproveAgentCredentialDevice(context.Background(), &pb.ApproveAgentCredentialDeviceRequest{})
		},
		"revoke": func() error {
			return c.RevokeAgentCredentialDevice(context.Background(), &pb.RevokeAgentCredentialDeviceRequest{})
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := call()
			if err == nil || !strings.Contains(err.Error(), "local daemon") || !strings.Contains(err.Error(), "--remote") {
				t.Fatalf("got %v", err)
			}
		})
	}
}
