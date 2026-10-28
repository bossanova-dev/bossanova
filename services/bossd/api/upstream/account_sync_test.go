package upstream

import (
	"context"
	"errors"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/domain"
)

type recordingAccountSync struct {
	cmd domain.ApplyAgentAccountSync
	err error
}

func (s *recordingAccountSync) Apply(_ context.Context, cmd domain.ApplyAgentAccountSync) error {
	s.cmd = cmd
	return s.err
}

func TestDispatchApplyAgentAccountSync(t *testing.T) {
	applier := &recordingAccountSync{}
	client := newDispatcherClient(&fakeCommandHandler{}, nil, nil)
	client.commandHandler = &CommandHandlerAdapter{AccountSync: applier}
	cmd := &pb.OrchestratorCommand{CommandId: "sync", Cmd: &pb.OrchestratorCommand_ApplyAgentAccountSync{ApplyAgentAccountSync: &pb.ApplyAgentAccountSyncCommand{LocalAgentAccountId: "local", SyncedAgentAccountId: "synced", Version: 7, DesiredStatus: "disabled", Priority: 12, IsQuarantined: true, ShouldVerifyAuth: true}}}
	out := make(chan *pb.DaemonEvent, 2)
	client.dispatchCommand(context.Background(), cmd, out)
	result := recvEvent(t, out).GetResult()
	if !result.GetOk() || result.GetCommandId() != "sync" {
		t.Fatalf("result=%+v", result)
	}
	want := domain.ApplyAgentAccountSync{LocalAgentAccountID: "local", SyncedAgentAccountID: "synced", Version: 7, DesiredStatus: "disabled", Priority: 12, IsQuarantined: true, ShouldVerifyAuth: true}
	if applier.cmd != want {
		t.Fatalf("cmd=%+v want=%+v", applier.cmd, want)
	}
	applier.err = errors.New("write failed")
	client.dispatchCommand(context.Background(), cmd, out)
	if recvEvent(t, out).GetResult().GetOk() {
		t.Fatal("failed apply reported success")
	}
}
