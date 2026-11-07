package main

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/adapter/daemontest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

type viewerWiringDaemon struct {
	bossanovav1connect.UnimplementedDaemonServiceHandler
	statusCalls int
}

func (d *viewerWiringDaemon) GetViewerDeviceStatus(context.Context, *connect.Request[pb.GetViewerDeviceStatusRequest]) (*connect.Response[pb.GetViewerDeviceStatusResponse], error) {
	d.statusCalls++
	return connect.NewResponse(&pb.GetViewerDeviceStatusResponse{IsAvailable: true, DaemonFingerprint: "abcd"}), nil
}
func TestViewerLocalWiring(t *testing.T) {
	daemon := &viewerWiringDaemon{}
	socket := daemontest.Serve(t, daemon)
	out, status := executeBoss(t, socket, "viewer", "status")
	if status != 0 || !strings.Contains(out, "abcd") || daemon.statusCalls != 1 {
		t.Fatalf("local: out=%q status=%d calls=%d", out, status, daemon.statusCalls)
	}
	out, status = executeBoss(t, socket, "--remote", "https://example.invalid", "viewer", "status")
	if status == 0 || out != "" || daemon.statusCalls != 1 {
		t.Fatalf("remote: out=%q status=%d calls=%d", out, status, daemon.statusCalls)
	}
}
