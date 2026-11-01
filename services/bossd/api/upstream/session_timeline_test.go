package upstream

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (f *fakeSessionCommandServer) GetSessionTimeline(context.Context, *connect.Request[pb.GetSessionTimelineRequest]) (*connect.Response[pb.GetSessionTimelineResponse], error) {
	return connect.NewResponse(&pb.GetSessionTimelineResponse{}), nil
}

func (e *errCommandServer) GetSessionTimeline(context.Context, *connect.Request[pb.GetSessionTimelineRequest]) (*connect.Response[pb.GetSessionTimelineResponse], error) {
	return nil, e.err
}

func (f *fakeCommandHandler) GetSessionTimeline(context.Context, string) (*pb.GetSessionTimelineResponse, error) {
	return &pb.GetSessionTimelineResponse{}, f.returnErr
}

type timelineCommandServer struct {
	*fakeSessionCommandServer
	sessionID string
	response  *pb.GetSessionTimelineResponse
	err       error
	calls     int
}

func (s *timelineCommandServer) GetSessionTimeline(_ context.Context, req *connect.Request[pb.GetSessionTimelineRequest]) (*connect.Response[pb.GetSessionTimelineResponse], error) {
	s.calls++
	s.sessionID = req.Msg.GetSessionId()
	return connect.NewResponse(s.response), s.err
}

func TestDispatchGetSessionTimeline(t *testing.T) {
	want := &pb.GetSessionTimelineResponse{Steps: []*pb.TimelineStep{
		{At: &timestamppb.Timestamp{Seconds: 100}, Title: "Build", Detail: "compiled", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE},
		{At: &timestamppb.Timestamp{Seconds: 200}, Title: "Verify", Detail: "checking", State: pb.TimelineStepState_TIMELINE_STEP_STATE_CURRENT},
	}}
	server := &timelineCommandServer{response: want}
	handler := &CommandHandlerAdapter{Commands: server}
	command := &pb.OrchestratorCommand{CommandId: "timeline", Cmd: &pb.OrchestratorCommand_GetSessionTimeline{
		GetSessionTimeline: &pb.GetSessionTimelineCommand{SessionId: "session-1"},
	}}
	out := make(chan *pb.DaemonEvent, 1)
	if event := newDispatcherClient(handler, nil, nil).dispatchCommand(context.Background(), command, out); event != nil {
		t.Fatalf("answered on reader: %v", event)
	}
	result := recvEvent(t, out).GetResult()
	if !result.GetOk() || result.GetCommandId() != "timeline" || !proto.Equal(result.GetGetSessionTimeline(), want) {
		t.Fatalf("result = %v, want ordered timeline %v", result, want)
	}
	if server.sessionID != "session-1" || server.calls != 1 {
		t.Fatalf("forwarded session = %q, calls = %d", server.sessionID, server.calls)
	}
}

func TestGetSessionTimelineAdapterValidation(t *testing.T) {
	server := &timelineCommandServer{}
	adapter := &CommandHandlerAdapter{Commands: server}
	if _, err := adapter.GetSessionTimeline(context.Background(), ""); err == nil {
		t.Fatal("empty session id accepted")
	}
	if server.calls != 0 {
		t.Fatal("empty session id reached command server")
	}
	if _, err := (&CommandHandlerAdapter{}).GetSessionTimeline(context.Background(), "session-1"); err == nil {
		t.Fatal("unwired command server accepted")
	}
}

func TestDispatchGetSessionTimelineNotFound(t *testing.T) {
	server := &timelineCommandServer{err: connect.NewError(connect.CodeNotFound, errors.New("missing session"))}
	handler := &CommandHandlerAdapter{Commands: server}
	command := &pb.OrchestratorCommand{CommandId: "missing", Cmd: &pb.OrchestratorCommand_GetSessionTimeline{
		GetSessionTimeline: &pb.GetSessionTimelineCommand{SessionId: "missing-session"},
	}}
	out := make(chan *pb.DaemonEvent, 1)
	if event := newDispatcherClient(handler, nil, nil).dispatchCommand(context.Background(), command, out); event != nil {
		t.Fatalf("answered on reader: %v", event)
	}
	result := recvEvent(t, out).GetResult()
	if result.GetOk() || result.GetCommandId() != "missing" || result.GetErrorCode() != pb.CommandResult_ERROR_CODE_NOT_FOUND {
		t.Fatalf("result = %v, want typed not-found error", result)
	}
}

func TestGetSessionTimelineMissingCapability(t *testing.T) {
	adapter := &CommandHandlerAdapter{Commands: struct{ SessionCommandServer }{&fakeSessionCommandServer{}}}
	if _, err := adapter.GetSessionTimeline(context.Background(), "session-1"); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("unsupported server error = %v, want Unimplemented", err)
	}
	handlers := []SessionCommandHandler{
		struct{ SessionCommandHandler }{&fakeCommandHandler{}}, adapter,
	}
	for _, handler := range handlers {
		command := &pb.OrchestratorCommand{CommandId: "unsupported", Cmd: &pb.OrchestratorCommand_GetSessionTimeline{
			GetSessionTimeline: &pb.GetSessionTimelineCommand{SessionId: "session-1"},
		}}
		out := make(chan *pb.DaemonEvent, 1)
		if event := newDispatcherClient(handler, nil, nil).dispatchCommand(context.Background(), command, out); event != nil {
			t.Fatalf("answered on reader: %v", event)
		}
		result := recvEvent(t, out).GetResult()
		if result.GetOk() || result.GetErrorCode() != pb.CommandResult_ERROR_CODE_UNIMPLEMENTED {
			t.Fatalf("result = %v, want Unimplemented", result)
		}
	}
}
