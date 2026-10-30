package account

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

type credentialDeviceCLIStub struct {
	Client
	approve *pb.ApproveAgentCredentialDeviceRequest
	revoked string
	devices []*pb.AgentCredentialDevice
}

func (*credentialDeviceCLIStub) GetAgentCredentialDeviceStatus(context.Context) (*pb.GetAgentCredentialDeviceStatusResponse, error) {
	return &pb.GetAgentCredentialDeviceStatusResponse{Device: &pb.AgentCredentialDevice{DaemonId: "local", Status: "pending", Fingerprint: "abcd", DevicePublicKey: []byte("hidden-key")}}, nil
}
func (s *credentialDeviceCLIStub) ListAgentCredentialDevices(context.Context) (*pb.ListAgentCredentialDevicesResponse, error) {
	return &pb.ListAgentCredentialDevicesResponse{Devices: s.devices}, nil
}
func (s *credentialDeviceCLIStub) ApproveAgentCredentialDevice(_ context.Context, r *pb.ApproveAgentCredentialDeviceRequest) error {
	s.approve = r
	return nil
}
func (s *credentialDeviceCLIStub) RevokeAgentCredentialDevice(_ context.Context, r *pb.RevokeAgentCredentialDeviceRequest) error {
	s.revoked = r.GetDaemonId()
	return nil
}
func executeDeviceCLI(args ...string) (*credentialDeviceCLIStub, string, error) {
	s := &credentialDeviceCLIStub{}
	cmd := New(func(*cobra.Command) (Client, error) { return s, nil }, nil, nil)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs(args)
	err := cmd.Execute()
	return s, out.String(), err
}
func TestDevicesStatusJSON_Accounts_AC20(t *testing.T) {
	_, out, err := executeDeviceCLI("devices", "status", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got["daemon_id"] != "local" || got["fingerprint"] != "abcd" || got["status"] != "pending" {
		t.Fatalf("status: %v", got)
	}
	if _, ok := got["device_public_key"]; ok {
		t.Fatalf("public-key bytes leaked in stable schema: %v", got)
	}
}
func TestDevicesEmptyListJSON_Accounts_AC20(t *testing.T) {
	_, out, err := executeDeviceCLI("devices", "ls", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Fatalf("empty list = %q", out)
	}
}
func TestDevicesApproveFingerprint_Accounts_AC21(t *testing.T) {
	s, _, err := executeDeviceCLI("devices", "approve", "target", "--fingerprint", "abcd")
	if err != nil {
		t.Fatal(err)
	}
	if s.approve.GetDaemonId() != "target" || s.approve.GetFingerprint() != "abcd" {
		t.Fatalf("approval: %v", s.approve)
	}
	s, _, err = executeDeviceCLI("devices", "approve", "target")
	if err == nil || !strings.Contains(err.Error(), "--fingerprint") || s.approve != nil {
		t.Fatalf("missing fingerprint: %v, %v", err, s.approve)
	}
}
func TestDevicesRevokeWarnsToRotate_Accounts_AC22(t *testing.T) {
	s, out, err := executeDeviceCLI("devices", "revoke", "target")
	if err != nil {
		t.Fatal(err)
	}
	if s.revoked != "target" || !strings.Contains(out, "rotate") || !strings.Contains(out, "upstream") {
		t.Fatalf("revoke: %q %q", s.revoked, out)
	}
}

func TestDevicesListShowsExpiry_Accounts_AC20(t *testing.T) {
	expiry := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	stub := &credentialDeviceCLIStub{devices: []*pb.AgentCredentialDevice{{DaemonId: "target", Status: "pending", Fingerprint: "abcd", ExpiresAt: timestamppb.New(expiry)}}}
	cmd := New(func(*cobra.Command) (Client, error) { return stub, nil }, nil, nil)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"devices", "ls"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"DAEMON", "STATUS", "FINGERPRINT", "EXPIRES", "target", "pending", "abcd", expiry.Format(time.RFC3339)} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q missing %q", out.String(), want)
		}
	}
}
