package upstream

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

func (f *fakeSessionCommandServer) SendBackToRepair(_ context.Context, req *connect.Request[pb.SendBackToRepairRequest]) (*connect.Response[pb.SendBackToRepairResponse], error) {
	return connect.NewResponse(&pb.SendBackToRepairResponse{IsSentBack: true, CommentUrl: req.Msg.GetNote()}), nil
}
func (e *errCommandServer) SendBackToRepair(context.Context, *connect.Request[pb.SendBackToRepairRequest]) (*connect.Response[pb.SendBackToRepairResponse], error) {
	return nil, e.err
}
func (f *fakeCommandHandler) SendBackToRepair(_ context.Context, req *pb.SendBackToRepairRequest) (*pb.SendBackToRepairResponse, error) {
	f.sendBackReq = req
	return &pb.SendBackToRepairResponse{IsSentBack: true, CommentUrl: "https://example/comment", IsAutoRepairEnabled: true}, f.returnErr
}

func TestDispatchSendBackToRepair(t *testing.T) {
	h := &fakeCommandHandler{}
	command := &pb.OrchestratorCommand{CommandId: "send-back", Cmd: &pb.OrchestratorCommand_SendBackToRepair{SendBackToRepair: &pb.SendBackToRepairCommand{SessionId: "s1", ExpectedHeadSha: "head", ExpectedProofDigest: "digest", Note: "note"}}}
	out := make(chan *pb.DaemonEvent, 1)
	if ev := newDispatcherClient(h, nil, nil).dispatchCommand(context.Background(), command, out); ev != nil {
		t.Fatalf("answered on reader: %v", ev)
	}
	result := recvEvent(t, out).GetResult()
	if !result.GetOk() || !result.GetSendBackToRepair().GetIsSentBack() || result.GetCommandId() != "send-back" {
		t.Fatalf("result: %v", result)
	}
	if want := (&pb.SendBackToRepairRequest{Id: "s1", ExpectedHeadSha: "head", ExpectedProofDigest: "digest", Note: "note"}); !proto.Equal(h.sendBackReq, want) {
		t.Fatalf("request: %v", h.sendBackReq)
	}
}
