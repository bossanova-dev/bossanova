package daemonrpc

import (
	"context"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

type setupWatchDaemon struct {
	bossanovav1connect.UnimplementedDaemonServiceHandler
}

func (setupWatchDaemon) WatchSessionSetup(_ context.Context, req *connect.Request[pb.WatchSessionSetupRequest], stream *connect.ServerStream[pb.WatchSessionSetupResponse]) error {
	if req.Msg.SessionId != "session" {
		return connect.NewError(connect.CodeNotFound, nil)
	}
	return stream.Send(&pb.WatchSessionSetupResponse{Event: &pb.WatchSessionSetupResponse_SetupOutput{SetupOutput: &pb.SetupScriptOutput{Text: "replayed"}}})
}

func TestLocalWatchSessionSetup(t *testing.T) {
	_, handler := bossanovav1connect.NewDaemonServiceHandler(setupWatchDaemon{})
	server := httptest.NewServer(handler)
	defer server.Close()
	c := &LocalClient{rpc: bossanovav1connect.NewDaemonServiceClient(server.Client(), server.URL)}
	stream, err := c.WatchSessionSetup(t.Context(), "session")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if !stream.Receive() || stream.Msg().GetSetupOutput().GetText() != "replayed" {
		t.Fatalf("frame = %v, err %v", stream.Msg(), stream.Err())
	}
	if stream.Receive() || stream.Err() != nil {
		t.Fatalf("end of stream = %v", stream.Err())
	}
	missing, err := c.WatchSessionSetup(t.Context(), "missing")
	if err == nil {
		defer func() { _ = missing.Close() }()
		missing.Receive()
		err = missing.Err()
	}
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("error = %v", err)
	}
}
