package upstream

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

type modelListingServer struct {
	asked  string
	result *pb.AgentModelsResult
	err    error
}

func (s *modelListingServer) ListAgentModels(_ context.Context, agentName string) (*pb.AgentModelsResult, error) {
	s.asked = agentName
	return s.result, s.err
}

func dispatchListAgentModels(t *testing.T, handler SessionCommandHandler, agentName string) *pb.CommandResult {
	t.Helper()
	out := make(chan *pb.DaemonEvent, 4)
	ev := newDispatcherClient(handler, nil, nil).dispatchCommand(context.Background(), &pb.OrchestratorCommand{
		CommandId: "c-models",
		Cmd:       &pb.OrchestratorCommand_ListAgentModels{ListAgentModels: &pb.ListAgentModelsCommand{AgentName: agentName}},
	}, out)
	if ev == nil {
		ev = recvEvent(t, out)
	}
	return ev.GetResult()
}

func TestDispatchListAgentModelsRepliesWithTheServersAnswer(t *testing.T) {
	want := &pb.AgentModelsResult{Models: []*pb.AgentModel{{Id: "opus"}}, RecentModelIds: []string{"opus"}}
	server := &modelListingServer{result: want}

	res := dispatchListAgentModels(t, &CommandHandlerAdapter{AgentModels: server}, "claude")
	if !res.GetOk() || res.GetCommandId() != "c-models" || !proto.Equal(res.GetAgentModels(), want) {
		t.Fatalf("result = %v, want ok agent_models %v", res, want)
	}
	if server.asked != "claude" {
		t.Fatalf("asked for %q, want claude", server.asked)
	}
}

func TestDispatchListAgentModelsClassifiesAnUnknownAgent(t *testing.T) {
	server := &modelListingServer{err: connect.NewError(connect.CodeNotFound, errors.New(`agent "nope" is not loaded`))}

	res := dispatchListAgentModels(t, &CommandHandlerAdapter{AgentModels: server}, "nope")
	if res.GetOk() || res.GetErrorCode() != pb.CommandResult_ERROR_CODE_NOT_FOUND {
		t.Fatalf("result = %v, want NOT_FOUND failure", res)
	}
}

func TestDispatchListAgentModelsFailsWhenUnwired(t *testing.T) {
	for name, handler := range map[string]SessionCommandHandler{
		"handler without the capability": &fakeCommandHandler{},
		"adapter without a server":       &CommandHandlerAdapter{},
	} {
		t.Run(name, func(t *testing.T) {
			if res := dispatchListAgentModels(t, handler, "claude"); res.GetOk() {
				t.Fatalf("result = %v, want a failure", res)
			}
		})
	}
}
