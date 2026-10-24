package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/recurser/boss/api/cli/settings"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

type startupCloudAccessClient struct {
	status    *pb.CloudAccessStatus
	statusErr error
}

func (f *startupCloudAccessClient) GetCloudAccessStatus(context.Context) (*pb.CloudAccessStatus, error) {
	return f.status, f.statusErr
}
func (*startupCloudAccessClient) CreateCheckoutSession(context.Context, string, string) (string, error) {
	return "", nil
}
func (*startupCloudAccessClient) RefreshCloudEntitlements(context.Context) (*pb.CloudAccessStatus, error) {
	return nil, nil
}
func (*startupCloudAccessClient) ListOrganizations(context.Context) ([]*pb.Organization, error) {
	return nil, nil
}
func TestRemoteCloudAccessBypassesDaemonPreflight(t *testing.T) {
	err := settings.RequireActiveCloudAccess(context.Background(), &startupCloudAccessClient{
		status: &pb.CloudAccessStatus{
			State: pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION,
		},
	})
	if err == nil {
		t.Fatal("expected inactive cloud access error")
	}

	waitCalled := false
	_, gotErr := handleClientStartupError(nil, err, func() (startupClient, error) {
		waitCalled = true
		return nil, errors.New("daemon preflight")
	})
	if waitCalled {
		t.Fatal("daemon preflight fallback was invoked for cloud access denial")
	}
	if gotErr == nil {
		t.Fatal("expected cloud access error")
	}
	if !strings.Contains(gotErr.Error(), "Bossanova Cloud requires an active subscription.") {
		t.Fatal(gotErr)
	}
}
func TestRemoteCloudAccessStatusErrorBypassesDaemonPreflight(t *testing.T) {
	err := settings.RequireActiveCloudAccess(context.Background(), &startupCloudAccessClient{
		statusErr: errors.New("status rpc unavailable"),
	})
	if err == nil {
		t.Fatal("expected cloud access status error")
	}

	waitCalled := false
	_, gotErr := handleClientStartupError(nil, err, func() (startupClient, error) {
		waitCalled = true
		return nil, errors.New("daemon preflight")
	})
	if waitCalled {
		t.Fatal("daemon preflight fallback was invoked for cloud status error")
	}
	if gotErr == nil {
		t.Fatal("expected cloud status error")
	}
	if !strings.Contains(gotErr.Error(), "Bossanova Cloud status unavailable: status rpc unavailable") {
		t.Fatalf("error = %q", gotErr.Error())
	}
	if !strings.Contains(gotErr.Error(), "Local sessions are still available.") {
		t.Fatalf("error = %q missing local fallback", gotErr.Error())
	}
}
func TestRemoteStartupErrorBypassesDaemonPreflight(t *testing.T) {
	startupErr := errors.New("auth: no stored credentials")
	waitCalled := false

	_, gotErr := handleClientStartupError(remoteTestCommand(t), startupErr, func() (startupClient, error) {
		waitCalled = true
		return nil, errors.New("daemon preflight")
	})
	if waitCalled {
		t.Fatal("daemon preflight fallback was invoked for remote startup error")
	}
	if !errors.Is(gotErr, startupErr) {
		t.Fatalf("error = %v, want %v", gotErr, startupErr)
	}
}
func remoteTestCommand(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "boss"}
	cmd.Flags().String("remote", "https://orchestrator.example.test", "")
	return cmd
}
