package account

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

type credentialDeviceCLIStub struct {
	Client
	approve   *pb.ApproveAgentCredentialDeviceRequest
	revoked   string
	requests  int
	devices   []*pb.AgentCredentialDevice
	readiness *pb.CredentialDeviceReadiness
}

func (s *credentialDeviceCLIStub) GetAgentCredentialDeviceStatus(context.Context) (*pb.GetAgentCredentialDeviceStatusResponse, error) {
	return &pb.GetAgentCredentialDeviceStatusResponse{Device: &pb.AgentCredentialDevice{DaemonId: "local", Status: "pending", Fingerprint: "abcd", DevicePublicKey: []byte("hidden-key")}, Readiness: s.readiness}, nil
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
	return executeDeviceCLIWith(&credentialDeviceCLIStub{}, args...)
}
func executeDeviceCLIWith(s *credentialDeviceCLIStub, args ...string) (*credentialDeviceCLIStub, string, error) {
	cmd := New(func(*cobra.Command) (Client, error) { return s, nil }, nil, nil, nil, nil)
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
	cmd := New(func(*cobra.Command) (Client, error) { return stub, nil }, nil, nil, nil, nil)
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

func (s *credentialDeviceCLIStub) RenewAgentCredentialDeviceEnrollment(context.Context) (*pb.RenewAgentCredentialDeviceEnrollmentResponse, error) {
	s.requests++
	return &pb.RenewAgentCredentialDeviceEnrollmentResponse{Device: &pb.AgentCredentialDevice{DaemonId: "local", Status: "pending", Fingerprint: "abcd", DevicePublicKey: []byte("hidden-key"), ExpiresAt: timestamppb.New(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))}}, nil
}

func TestDevicesRequest_Accounts_AC224501(t *testing.T) {
	s, out, err := executeDeviceCLI("devices", "request")
	if err != nil {
		t.Fatal(err)
	}
	if s.requests != 1 {
		t.Fatalf("renewals=%d", s.requests)
	}
	for _, want := range []string{"Status: pending", "Fingerprint: abcd", "Expires: 2026-11-01T12:00:00Z", "On an enrolled machine run: boss account devices approve local --fingerprint abcd"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q missing %q", out, want)
		}
	}
}

func TestDevicesRequestJSON_Accounts_AC224501(t *testing.T) {
	s, out, err := executeDeviceCLI("devices", "request", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"daemon_id": "local", "status": "pending", "fingerprint": "abcd", "requested_at": "", "expires_at": "2026-11-01T12:00:00Z", "approved_at": "", "approved_by_daemon_id": "", "revoked_at": ""}
	if s.requests != 1 || len(got) != len(want) {
		t.Fatalf("renewals=%d JSON=%v", s.requests, got)
	}
	for key, value := range want {
		if actual, ok := got[key]; !ok || actual != value {
			t.Errorf("%s=%q, want %q (present=%t)", key, actual, value, ok)
		}
	}
}

var deviceReadinessChecked = timestamppb.New(time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC))

func TestDevicesStatusPrintsReadiness_Accounts_AC224601(t *testing.T) {
	request := &pb.CredentialDeviceRemediationStep{MachineDaemonId: "local", IsThisMachine: true, Command: "boss account devices request"}
	approve := &pb.CredentialDeviceRemediationStep{Command: "boss account devices approve local --fingerprint abcd"}
	head := "Daemon: local\nStatus: pending\nFingerprint: abcd\n"
	for _, tc := range []struct {
		name      string
		readiness *pb.CredentialDeviceReadiness
		want      string
	}{
		{"enrollment_expired", &pb.CredentialDeviceReadiness{State: "enrollment_expired", ApproverDaemonIds: []string{"boot"}, ExpiresAt: timestamppb.New(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)), RemediationSteps: []*pb.CredentialDeviceRemediationStep{request, approve}},
			"Readiness: enrollment_expired\nExpires: 2026-11-01T12:00:00Z\nApprovers: boot\nNext steps:\n  1. On this machine run: boss account devices request\n  2. On an enrolled machine run: boss account devices approve local --fingerprint abcd\n"},
		{"unenrolled", &pb.CredentialDeviceReadiness{State: "unenrolled", ApproverDaemonIds: []string{"boot", "peer"}, RemediationSteps: []*pb.CredentialDeviceRemediationStep{request, approve}},
			"Readiness: unenrolled\nApprovers: boot, peer\nNext steps:\n  1. On this machine run: boss account devices request\n  2. On an enrolled machine run: boss account devices approve local --fingerprint abcd\n"},
		{"enrollment_pending", &pb.CredentialDeviceReadiness{State: "enrollment_pending", ApproverDaemonIds: []string{"boot"}, ExpiresAt: timestamppb.New(time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)), RemediationSteps: []*pb.CredentialDeviceRemediationStep{approve}},
			"Readiness: enrollment_pending\nExpires: 2026-11-03T12:00:00Z\nApprovers: boot\nNext steps:\n  1. On an enrolled machine run: boss account devices approve local --fingerprint abcd\n"},
		{"revoked", &pb.CredentialDeviceReadiness{State: "revoked", ApproverDaemonIds: []string{"boot"}, RemediationSteps: []*pb.CredentialDeviceRemediationStep{request}},
			"Readiness: revoked\nApprovers: boot\nNext steps:\n  1. On this machine run: boss account devices request\n"},
		{"bootstrap_unpinned", &pb.CredentialDeviceReadiness{State: "bootstrap_unpinned", BootstrapDaemonId: "boot", ApproverDaemonIds: []string{"boot"}, RemediationSteps: []*pb.CredentialDeviceRemediationStep{{MachineDaemonId: "boot", Command: "boss account devices status"}, {MachineDaemonId: "local", IsThisMachine: true, Command: "boss account devices approve boot --fingerprint <fingerprint-shown-on-boot>"}}},
			"Readiness: bootstrap_unpinned\nApprover to pin: boot\nApprovers: boot\nNext steps:\n  1. On boot run: boss account devices status\n  2. On this machine run: boss account devices approve boot --fingerprint <fingerprint-shown-on-boot>\n"},
		{"ready", &pb.CredentialDeviceReadiness{State: "ready", BootstrapDaemonId: "boot", ApproverDaemonIds: []string{"boot"}}, "Readiness: ready\nBootstrap: boot\nApprovers: boot\n"},
		{"unknown", &pb.CredentialDeviceReadiness{State: "unknown"}, "Readiness: unknown\n"},
		{"older daemon", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.readiness != nil {
				tc.readiness.CheckedAt = deviceReadinessChecked
			}
			_, out, err := executeDeviceCLIWith(&credentialDeviceCLIStub{readiness: tc.readiness}, "devices", "status")
			if err != nil {
				t.Fatal(err)
			}
			if out != head+tc.want {
				t.Fatalf("output:\n%s\nwant:\n%s", out, head+tc.want)
			}
		})
	}
}

func TestDevicesStatusReadinessJSON_Accounts_AC224601(t *testing.T) {
	readiness := &pb.CredentialDeviceReadiness{State: "bootstrap_unpinned", BootstrapDaemonId: "boot", ApproverDaemonIds: []string{"boot"}, CheckedAt: deviceReadinessChecked, RemediationSteps: []*pb.CredentialDeviceRemediationStep{{MachineDaemonId: "boot", Command: "boss account devices status"}, {MachineDaemonId: "local", IsThisMachine: true, Command: "boss account devices approve boot --fingerprint <fingerprint-shown-on-boot>"}}}
	_, out, err := executeDeviceCLIWith(&credentialDeviceCLIStub{readiness: readiness}, "devices", "status", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		DaemonID    string         `json:"daemon_id"`
		Fingerprint string         `json:"fingerprint"`
		Readiness   *readinessJSON `json:"readiness"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	want := readinessJSON{State: "bootstrap_unpinned", BootstrapDaemonID: "boot", ApproverDaemonIDs: []string{"boot"}, CheckedAt: "2026-11-02T12:00:00Z", RemediationSteps: []remediationStepJSON{{MachineDaemonID: "boot", Command: "boss account devices status"}, {MachineDaemonID: "local", IsThisMachine: true, Command: "boss account devices approve boot --fingerprint <fingerprint-shown-on-boot>"}}}
	if got.DaemonID != "local" || got.Fingerprint != "abcd" || got.Readiness == nil || !reflect.DeepEqual(*got.Readiness, want) {
		t.Fatalf("status JSON=%s", out)
	}
	_, out, err = executeDeviceCLI("devices", "status", "--json")
	if err != nil || strings.Contains(out, "readiness") {
		t.Fatalf("readiness without a daemon verdict: %v %s", err, out)
	}
}
