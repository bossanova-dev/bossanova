package client

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

type phaseRPC struct {
	bossanovav1connect.DaemonServiceClient
	req *pb.SetChatPhaseRequest
	err error
}

func (f *phaseRPC) SetChatPhase(_ context.Context, req *connect.Request[pb.SetChatPhaseRequest]) (*connect.Response[pb.SetChatPhaseResponse], error) {
	f.req = req.Msg
	return connect.NewResponse(&pb.SetChatPhaseResponse{}), f.err
}
func TestLocalSetChatPhase(t *testing.T) {
	failure := errors.New("daemon failed")
	for _, tt := range []struct {
		name, phase string
		err         error
	}{{"set", "reviewing", nil}, {"clear", "", nil}, {"failure", "building", failure}} {
		t.Run(tt.name, func(t *testing.T) {
			rpc := &phaseRPC{err: tt.err}
			c := &LocalClient{rpc: rpc}
			err := c.SetChatPhase(context.Background(), "session", "chat", tt.phase)
			if !errors.Is(err, tt.err) {
				t.Fatalf("error=%v, want %v", err, tt.err)
			}
			if rpc.req.GetSessionId() != "session" || rpc.req.GetAgentSessionId() != "chat" || rpc.req.GetPhase() != tt.phase {
				t.Fatalf("request = %v", rpc.req)
			}
		})
	}
}
func TestRemoteSetChatPhaseLocalOnly(t *testing.T) {
	err := (&RemoteClient{}).SetChatPhase(context.Background(), "session", "chat", "reviewing")
	if connect.CodeOf(err) != connect.CodeUnimplemented || err.Error() != errLocalOnly("set chat phase").Error() {
		t.Fatalf("error=%v, want local-only unimplemented", err)
	}
}
