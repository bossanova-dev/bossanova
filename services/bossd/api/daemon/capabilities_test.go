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
	for _, want := range []string{daemoncaps.MergeExpectedHeadSHA, daemoncaps.CreateSessionPolicyRef} {
		if !slices.Contains(response.Msg.GetCapabilities(), want) {
			t.Fatalf("capabilities=%v, want %s", response.Msg.GetCapabilities(), want)
		}
	}
}
