package viewer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type viewerCommandStub struct {
	status  *pb.GetViewerDeviceStatusResponse
	devices []*pb.LocalViewerDevice
	approve *pb.ApproveViewerDeviceRequest
	revoke  *pb.RevokeViewerDeviceRequest
	err     error
}

func (s *viewerCommandStub) GetViewerDeviceStatus(context.Context) (*pb.GetViewerDeviceStatusResponse, error) {
	return s.status, s.err
}
func (s *viewerCommandStub) ListViewerDevices(context.Context) (*pb.ListLocalViewerDevicesResponse, error) {
	return &pb.ListLocalViewerDevicesResponse{Devices: s.devices}, s.err
}
func (s *viewerCommandStub) ApproveViewerDevice(_ context.Context, r *pb.ApproveViewerDeviceRequest) error {
	s.approve = r
	return s.err
}
func (s *viewerCommandStub) RevokeViewerDevice(_ context.Context, r *pb.RevokeViewerDeviceRequest) error {
	s.revoke = r
	return s.err
}
func executeViewer(t *testing.T, s *viewerCommandStub, args ...string) (string, error) {
	t.Helper()
	c := New(func(*cobra.Command) (Client, error) { return s, nil })
	var b bytes.Buffer
	c.SetOut(&b)
	c.SetErr(&b)
	c.SetArgs(args)
	returnOutput := func() (string, error) { err := c.Execute(); return b.String(), err }
	return returnOutput()
}
func TestViewerCommands(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		available bool
		want      string
	}{
		{"status", []string{"status"}, true, "Daemon fingerprint: abcd"},
		{"unavailable", []string{"status"}, false, "unavailable"},
		{"approve", []string{"approve", "device", "--fingerprint", "1234"}, true, "Daemon fingerprint: abcd"},
		{"revoke", []string{"revoke", "device"}, false, "Revoked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &viewerCommandStub{status: &pb.GetViewerDeviceStatusResponse{DaemonFingerprint: "abcd", IsAvailable: tc.available}}
			out, err := executeViewer(t, s, tc.args...)
			if err != nil || !strings.Contains(out, tc.want) {
				t.Fatalf("out=%q err=%v", out, err)
			}
			if tc.name == "approve" && (s.approve.GetViewerDeviceId() != "device" || s.approve.GetFingerprint() != "1234") {
				t.Fatalf("request=%v", s.approve)
			}
			if tc.name == "revoke" && s.revoke.GetViewerDeviceId() != "device" {
				t.Fatalf("request=%v", s.revoke)
			}
		})
	}
}
func TestViewerList(t *testing.T) {
	s := &viewerCommandStub{devices: []*pb.LocalViewerDevice{{ViewerDeviceId: "device", Label: "Laptop\t\n\x1b[31m", Fingerprint: "abcd", IsApproved: true, ApprovedAt: timestamppb.New(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))}}}
	out, err := executeViewer(t, s, "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0]) != 5 || got[0]["viewer_device_id"] != "device" || got[0]["is_approved"] != true || got[0]["approved_at"] != "2026-11-01T12:00:00Z" {
		t.Fatalf("json=%s", out)
	}
	out, err = executeViewer(t, s, "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "device") || !strings.Contains(out, "abcd") || strings.ContainsAny(out, "\x1b\r") {
		t.Fatalf("table=%q", out)
	}
	if strings.Count(out, "\n") != 2 {
		t.Fatalf("unsafe rows: %q", out)
	}
	s.devices = nil
	out, err = executeViewer(t, s, "list", "--json")
	if err != nil || strings.TrimSpace(out) != "[]" {
		t.Fatalf("empty=%q err=%v", out, err)
	}
}
func TestViewerFingerprintRequired(t *testing.T) {
	for _, args := range [][]string{{"approve", "device"}, {"approve", "device", "--fingerprint", ""}, {"approve", "device", "--fingerprint", "   "}} {
		s := &viewerCommandStub{}
		_, err := executeViewer(t, s, args...)
		if err == nil || s.approve != nil {
			t.Fatalf("args=%v request=%v err=%v", args, s.approve, err)
		}
	}
}
func TestViewerErrors(t *testing.T) {
	sentinel := errors.New("daemon unavailable")
	for _, args := range [][]string{{"status"}, {"list"}, {"approve", "device", "--fingerprint", "abcd"}, {"revoke", "device"}} {
		_, err := executeViewer(t, &viewerCommandStub{err: sentinel}, args...)
		if !errors.Is(err, sentinel) {
			t.Fatalf("args=%v err=%v", args, err)
		}
	}
	c := New(func(*cobra.Command) (Client, error) { return nil, sentinel })
	c.SetArgs([]string{"status"})
	if err := c.Execute(); !errors.Is(err, sentinel) {
		t.Fatalf("factory err=%v", err)
	}
}
