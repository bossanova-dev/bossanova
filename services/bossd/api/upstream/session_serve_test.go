package upstream

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type fakeServeCommandServer struct {
	SessionCommandServer
	startID string
	stopID  string
	serve   *pb.SessionServe
	err     error
}

func (f *fakeServeCommandServer) StartSessionServe(_ context.Context, req *connect.Request[pb.StartSessionServeRequest]) (*connect.Response[pb.StartSessionServeResponse], error) {
	f.startID = req.Msg.GetSessionId()
	if f.err != nil {
		return nil, f.err
	}
	return connect.NewResponse(&pb.StartSessionServeResponse{Serve: f.serve}), nil
}

func (f *fakeServeCommandServer) StopSessionServe(_ context.Context, req *connect.Request[pb.StopSessionServeRequest]) (*connect.Response[pb.StopSessionServeResponse], error) {
	f.stopID = req.Msg.GetSessionId()
	if f.err != nil {
		return nil, f.err
	}
	return connect.NewResponse(&pb.StopSessionServeResponse{Serve: f.serve}), nil
}

func TestCommandHandlerAdapterSessionServe(t *testing.T) {
	want := &pb.SessionServe{IsRunning: true, ServiceLinks: []*pb.ServiceLink{{Name: "api", Url: "https://api.example"}}}
	srv := &fakeServeCommandServer{serve: want}
	adapter := &CommandHandlerAdapter{Commands: srv}

	got, err := adapter.StartSessionServe(context.Background(), "sess-1")
	if err != nil || srv.startID != "sess-1" || !got.GetIsRunning() {
		t.Fatalf("StartSessionServe = (%v, %v), startID=%q", got, err, srv.startID)
	}
	srv.serve = &pb.SessionServe{IsRunning: false}
	got, err = adapter.StopSessionServe(context.Background(), "sess-1")
	if err != nil || srv.stopID != "sess-1" || got.GetIsRunning() {
		t.Fatalf("StopSessionServe = (%v, %v), stopID=%q", got, err, srv.stopID)
	}

	srv.err = connect.NewError(connect.CodeFailedPrecondition, errors.New("no serve command"))
	if _, err := adapter.StartSessionServe(context.Background(), "sess-1"); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("err = %v, want FailedPrecondition", err)
	}
}

func TestDispatchSessionServe(t *testing.T) {
	srv := &fakeServeCommandServer{
		serve: &pb.SessionServe{IsRunning: true, ServiceLinks: []*pb.ServiceLink{{Name: "web", Url: "https://web.example"}}},
	}
	c := newTestClient(StreamStores{})
	c.commandHandler = &CommandHandlerAdapter{Commands: srv}
	c.cloudControl = fixedCloudControl("full")
	c.targetObjects = &targetFixture{objects: map[targetKey]CommandTargetObject{
		{"session", "sess-1"}: {RepoID: "repo-1", SessionID: "sess-1"},
		{"repo", "repo-1"}:    {RepoID: "repo-1", Name: "repo-1"},
	}}
	out := make(chan *pb.DaemonEvent, 2)

	if ev := c.dispatchCommand(context.Background(), &pb.OrchestratorCommand{
		CommandId: "cmd-start",
		Cmd:       &pb.OrchestratorCommand_StartSessionServe{StartSessionServe: &pb.StartSessionServeCommand{SessionId: "sess-1"}},
	}, out); ev != nil {
		t.Fatalf("expected async nil, got %v", ev)
	}
	res := (<-out).GetResult()
	if !res.GetOk() || res.GetCommandId() != "cmd-start" || !res.GetSessionServe().GetIsRunning() {
		t.Fatalf("start result = %+v", res)
	}

	srv.serve = &pb.SessionServe{IsRunning: false}
	if ev := c.dispatchCommand(context.Background(), &pb.OrchestratorCommand{
		CommandId: "cmd-stop",
		Cmd:       &pb.OrchestratorCommand_StopSessionServe{StopSessionServe: &pb.StopSessionServeCommand{SessionId: "sess-1"}},
	}, out); ev != nil {
		t.Fatalf("expected async nil, got %v", ev)
	}
	res = (<-out).GetResult()
	if !res.GetOk() || res.GetCommandId() != "cmd-stop" || res.GetSessionServe().GetIsRunning() {
		t.Fatalf("stop result = %+v", res)
	}

	// Read-only cloud control refuses serve mutations.
	c.cloudControl = fixedCloudControl("read-only")
	ev := c.dispatchCommand(context.Background(), &pb.OrchestratorCommand{
		CommandId: "cmd-refused",
		Cmd:       &pb.OrchestratorCommand_StartSessionServe{StartSessionServe: &pb.StartSessionServeCommand{SessionId: "sess-1"}},
	}, out)
	if ev == nil || ev.GetResult().GetErrorCode() != pb.CommandResult_ERROR_CODE_CLOUD_CONTROL_DISABLED {
		t.Fatalf("read-only result = %+v, want CLOUD_CONTROL_DISABLED", ev)
	}
}
