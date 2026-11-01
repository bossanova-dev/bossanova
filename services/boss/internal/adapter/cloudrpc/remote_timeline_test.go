package cloudrpc

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

type timelineOrchestrator struct {
	bossanovav1connect.UnimplementedOrchestratorServiceHandler
	sessionID string
	fail      bool
}

func (f *timelineOrchestrator) ProxyGetSessionTimeline(_ context.Context, req *connect.Request[pb.ProxyGetSessionTimelineRequest]) (*connect.Response[pb.ProxyGetSessionTimelineResponse], error) {
	f.sessionID = req.Msg.GetSessionId()
	if f.fail {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("session not found"))
	}
	return connect.NewResponse(&pb.ProxyGetSessionTimelineResponse{Steps: []*pb.TimelineStep{{Title: "Build started"}, {Title: "Verify started"}}}), nil
}

func TestRemoteClient_GetSessionTimeline(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "steps", true: "typed error"}[fail], func(t *testing.T) {
			fake := &timelineOrchestrator{fail: fail}
			_, handler := bossanovav1connect.NewOrchestratorServiceHandler(fake)
			server := httptest.NewServer(handler)
			t.Cleanup(server.Close)
			got, err := NewRemote(server.URL, "tok").GetSessionTimeline(context.Background(), "session-123")
			if fail {
				if connect.CodeOf(err) != connect.CodeNotFound {
					t.Fatalf("error = %v, want NotFound", err)
				}
			} else if err != nil || len(got) != 2 || got[0].GetTitle() != "Build started" || got[1].GetTitle() != "Verify started" {
				t.Fatalf("steps = %v, error = %v", got, err)
			}
			if fake.sessionID != "session-123" {
				t.Fatalf("session id = %q", fake.sessionID)
			}
		})
	}
}
