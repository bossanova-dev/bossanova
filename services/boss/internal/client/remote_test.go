package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

// fakeChatOrchestrator records the proxy chat calls it receives and returns
// canned responses. Unimplemented RPCs inherit CodeUnimplemented from the
// embedded base.
type fakeChatOrchestrator struct {
	bossanovav1connect.UnimplementedOrchestratorServiceHandler
	transcriptReq *pb.ProxyGetChatTranscriptRequest
	sendReq       *pb.ProxySendChatMessageRequest
}

func (f *fakeChatOrchestrator) ProxyGetChatTranscript(_ context.Context, req *connect.Request[pb.ProxyGetChatTranscriptRequest]) (*connect.Response[pb.ProxyGetChatTranscriptResponse], error) {
	f.transcriptReq = req.Msg
	return connect.NewResponse(&pb.ProxyGetChatTranscriptResponse{
		Messages:           []*pb.ChatMessage{{Text: "line"}},
		FinalAssistantText: "final",
		Exists:             true,
	}), nil
}

func (f *fakeChatOrchestrator) ProxySendChatMessage(_ context.Context, req *connect.Request[pb.ProxySendChatMessageRequest]) (*connect.Response[pb.ProxySendChatMessageResponse], error) {
	f.sendReq = req.Msg
	return connect.NewResponse(&pb.ProxySendChatMessageResponse{TmuxSessionName: "tmux-x", Delivered: true}), nil
}

func newTestRemote(t *testing.T) (*RemoteClient, *fakeChatOrchestrator) {
	t.Helper()
	fake := &fakeChatOrchestrator{}
	path, handler := bossanovav1connect.NewOrchestratorServiceHandler(fake)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return NewRemote(srv.URL, "tok"), fake
}

func TestRemoteClient_GetChatTranscript(t *testing.T) {
	t.Parallel()
	c, fake := newTestRemote(t)

	resp, err := c.GetChatTranscript(context.Background(), &pb.GetChatTranscriptRequest{
		SessionId:      "sess-1",
		AgentSessionId: "agent-9",
		MaxMessages:    7,
	})
	if err != nil {
		t.Fatalf("GetChatTranscript: %v", err)
	}
	if !resp.GetExists() || resp.GetFinalAssistantText() != "final" || len(resp.GetMessages()) != 1 {
		t.Fatalf("unexpected response: %+v", resp)
	}
	got := fake.transcriptReq
	if got.GetSessionId() != "sess-1" || got.GetAgentSessionId() != "agent-9" || got.GetMaxMessages() != 7 {
		t.Fatalf("fields not forwarded: %+v", got)
	}
}

func TestRemoteClient_SendChatMessage(t *testing.T) {
	t.Parallel()
	c, fake := newTestRemote(t)

	resp, err := c.SendChatMessage(context.Background(), &pb.SendChatMessageRequest{
		AgentSessionId: "agent-9",
		Message:        "hello",
		WakeIfAsleep:   true,
	})
	if err != nil {
		t.Fatalf("SendChatMessage: %v", err)
	}
	if !resp.GetDelivered() || resp.GetTmuxSessionName() != "tmux-x" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	got := fake.sendReq
	if got.GetAgentSessionId() != "agent-9" || got.GetMessage() != "hello" || !got.GetWakeIfAsleep() {
		t.Fatalf("fields not forwarded: %+v", got)
	}
}

// TestRemoteClient_SendChatMessage_PropagatesSubmit asserts the BOS-242 submit
// intent survives the SendChatMessageRequest → ProxySendChatMessageRequest
// conversion, set as present so an explicit false (prefill) is not defaulted to
// submit=true server-side.
func TestRemoteClient_SendChatMessage_PropagatesSubmit(t *testing.T) {
	t.Parallel()
	c, fake := newTestRemote(t)

	for _, submit := range []bool{true, false} {
		if _, err := c.SendChatMessage(context.Background(), &pb.SendChatMessageRequest{
			AgentSessionId: "agent-9",
			Message:        "hello",
			Submit:         submit,
		}); err != nil {
			t.Fatalf("SendChatMessage(submit=%v): %v", submit, err)
		}
		got := fake.sendReq
		if got.Submit == nil {
			t.Fatalf("submit=%v: expected submit set (present), got nil", submit)
		}
		if got.GetSubmit() != submit {
			t.Fatalf("submit forwarded = %v, want %v", got.GetSubmit(), submit)
		}
	}
}
