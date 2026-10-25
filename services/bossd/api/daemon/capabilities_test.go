package server

import (
	"context"
	"slices"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/daemoncaps"
)

func TestGetDaemonCapabilities(t *testing.T) {
	server := &Server{}
	response, err := server.GetDaemonCapabilities(context.Background(), connect.NewRequest(&pb.GetDaemonCapabilitiesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(response.Msg.GetCapabilities(), daemoncaps.MergeExpectedHeadSHA) {
		t.Fatalf("capabilities=%v, want merge head pin support", response.Msg.GetCapabilities())
	}
}
