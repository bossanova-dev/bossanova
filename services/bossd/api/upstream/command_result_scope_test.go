package upstream

import (
	"context"
	"testing"
	"testing/synctest"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
)

func TestCommandResultScopeDuplicateIDs(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(map[bool]string{false: "original-order", true: "reverse-order"}[reverse], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c, handler, _ := targetClient()
			modes := map[string]config.CloudControlMode{"a": config.CloudControlFull, "b": config.CloudControlFull}
			c.cloudControl = targetPolicy{machine: config.CloudControlFull, repos: modes}
			handler.transcript = &pb.GetChatTranscriptResponse{FinalAssistantText: "body", Messages: []*pb.ChatMessage{{Text: "body"}}}
			out := make(chan *pb.DaemonEvent, 2)
			var results []*pb.DaemonEvent
			for _, ids := range [][2]string{{"sa", "ca"}, {"sb", "cb"}} {
				c.handleCommand(ctx, &pb.OrchestratorCommand{CommandId: "reused", Cmd: &pb.OrchestratorCommand_GetChatTranscript{GetChatTranscript: &pb.GetChatTranscriptCommand{SessionId: ids[0], AgentSessionId: ids[1]}}}, out)
				ev := recvEvent(t, out)
				if !ev.GetResult().GetOk() {
					t.Fatalf("transcript refused: %v", ev)
				}
				results = append(results, ev)
			}
			modes["a"] = config.CloudControlPrivate
			if reverse {
				results[0], results[1] = results[1], results[0]
			}
			for _, ev := range results {
				out <- ev
			}
			close(out)
			stream := &policyDaemonStream{sent: make(chan *pb.DaemonEvent, 2)}
			c.runWriter(ctx, stream, out)
			for i := range results {
				got := (<-stream.sent).GetResult().GetGetChatTranscript()
				private := (i == 0) != reverse
				if private && (got.GetFinalAssistantText() != "" || len(got.GetMessages()) != 0) {
					t.Fatalf("reused ID exposed downgraded repo transcript: %v", got)
				}
				if !private && got.GetFinalAssistantText() != "body" {
					t.Fatalf("full repo transcript redacted: %v", got)
				}
			}
		})
	}
}

func TestCommandResultScopeCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, _, _ := targetClient()
		ctx, cancel := context.WithCancel(context.WithValue(context.Background(), resultScopeKey{}, []string{"a"}))
		ev := commandErr("id", "error")
		c.bindResultScope(ctx, ev)
		if got := c.takeResultScope(ev); len(got) != 1 || got[0] != "a" {
			t.Fatalf("consumed scope = %v", got)
		}
		if got := c.takeResultScope(ev); got != nil {
			t.Fatalf("consumed scope retained: %v", got)
		}
		c.bindResultScope(ctx, ev)
		cancel()
		synctest.Wait()
		if _, exists := c.resultRepos.Load(ev.GetResult()); exists {
			t.Fatal("disconnect retained pending result scope")
		}
		// An asynchronous handler finishing after stream teardown cannot retain it.
		c.bindResultScope(ctx, ev)
		synctest.Wait()
		if _, exists := c.resultRepos.Load(ev.GetResult()); exists {
			t.Fatal("late completion retained canceled result scope")
		}
		c.sendCommandResult(ctx, nil, ev)
	})
}
