package upstream

import (
	"context"
	"errors"
	"strings"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestCommandHandlerAdapter_MergeSession(t *testing.T) {
	t.Parallel()

	t.Run("empty session id is rejected", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &fakeSessionCommandServer{}}
		if _, err := adapter.MergeSession(context.Background(), "", ""); err == nil || !strings.Contains(err.Error(), "merge: session_id required") {
			t.Fatalf("MergeSession error = %v, want merge: session_id required", err)
		}
	})

	t.Run("missing command server is rejected", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{}
		if _, err := adapter.MergeSession(context.Background(), "s1", ""); err == nil || !strings.Contains(err.Error(), "merge: command server not wired") {
			t.Fatalf("MergeSession error = %v, want command server not wired", err)
		}
	})

	t.Run("command error is wrapped", func(t *testing.T) {
		t.Parallel()
		adapter := &CommandHandlerAdapter{Commands: &errCommandServer{err: errors.New("boom")}}
		if _, err := adapter.MergeSession(context.Background(), "s1", ""); err == nil || !strings.Contains(err.Error(), "merge session: boom") {
			t.Fatalf("MergeSession error = %v, want merge session: boom", err)
		}
	})

	t.Run("returns successfully when the command server succeeds", func(t *testing.T) {
		t.Parallel()
		fake := &fakeSessionCommandServer{}
		adapter := &CommandHandlerAdapter{Commands: fake}
		pin := strings.Repeat("a", 40)
		if _, err := adapter.MergeSession(context.Background(), "s1", pin); err != nil {
			t.Fatalf("MergeSession returned error: %v", err)
		}
		if fake.lastMergeReq.GetId() != "s1" || fake.lastMergeReq.GetExpectedHeadSha() != pin {
			t.Fatalf("merge request = %v", fake.lastMergeReq)
		}
	})
}

func TestDispatchCommand_Merge_CallsHandler(t *testing.T) {
	sess := &pb.Session{Id: "s-merge"}
	handler := &fakeCommandHandler{mergeSession: sess}
	client := newDispatcherClient(handler, nil, nil)
	out := make(chan *pb.DaemonEvent, 4)
	// Merge is dispatched asynchronously (it can queue behind another merge on
	// the same repo), so dispatchCommand returns nil and the result arrives on
	// outbound.
	if ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-m1",
			Cmd:       &pb.OrchestratorCommand_Merge{Merge: &pb.MergeSessionCommand{SessionId: "s-merge", ExpectedHeadSha: strings.Repeat("a", 40)}},
		}, out); ev != nil {
		t.Fatalf("expected nil synchronous result for async merge command, got %+v", ev)
	}
	ev := recvEvent(t, out)
	if handler.mergeHead != strings.Repeat("a", 40) {
		t.Fatalf("merge pin = %q", handler.mergeHead)
	}
	if handler.mergeCalls.Load() != 1 {
		t.Fatalf("merge calls = %d, want 1", handler.mergeCalls.Load())
	}
	r := ev.GetResult()
	if r == nil || !r.GetOk() || r.GetCommandId() != "c-m1" {
		t.Fatalf("expected ok result with command_id, got %+v", ev)
	}
	if r.GetSession().GetId() != "s-merge" {
		t.Fatalf("expected session id s-merge, got %q", r.GetSession().GetId())
	}
}
