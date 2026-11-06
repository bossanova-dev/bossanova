package upstream

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// gatedWebhookDispatcher reports each Dispatch on started (by event type) and,
// when release is non-nil, holds it until release closes or ctx is cancelled.
type gatedWebhookDispatcher struct {
	started chan string
	release chan struct{}
	err     error
}

func newGatedWebhookDispatcher(gated bool) *gatedWebhookDispatcher {
	d := &gatedWebhookDispatcher{started: make(chan string, 2*webhookLaneDepth)}
	if gated {
		d.release = make(chan struct{})
	}
	return d
}

func (d *gatedWebhookDispatcher) Dispatch(ctx context.Context, ev *pb.WebhookEvent) error {
	select {
	case d.started <- ev.GetEventType():
	case <-ctx.Done():
		return ctx.Err()
	}
	if d.release != nil {
		select {
		case <-d.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return d.err
}

func webhookCmd(id string) *pb.OrchestratorCommand {
	return &pb.OrchestratorCommand{
		CommandId: id,
		Cmd:       &pb.OrchestratorCommand_Webhook{Webhook: &pb.WebhookEvent{Provider: "github", EventType: id}},
	}
}

// startTestWebhookLane starts client's webhook lane on outbound and tears it
// down at cleanup. The returned ctx is the lane's, for handleCommand calls.
func startTestWebhookLane(t *testing.T, client *StreamClient, outbound chan<- *pb.DaemonEvent) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	client.startWebhookLane(ctx, outbound)
	t.Cleanup(func() {
		cancel()
		client.joinWebhookLane()
	})
	return ctx
}

func recvStarted(t *testing.T, d *gatedWebhookDispatcher) string {
	t.Helper()
	select {
	case id := <-d.started:
		return id
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for webhook Dispatch to start")
		return ""
	}
}

func recvAck(t *testing.T, out <-chan *pb.DaemonEvent) *pb.WebhookAck {
	t.Helper()
	ev := recvEvent(t, out)
	if ev.GetAck() == nil {
		t.Fatalf("expected WebhookAck, got %+v", ev)
	}
	return ev.GetAck()
}

// TestWebhookLane_BurstDoesNotStarveAccountSync reproduces BOS-2252: webhook
// commands whose Dispatch is stuck on GitHub must not keep a following
// ApplyAgentAccountSync from being applied and answered.
func TestWebhookLane_BurstDoesNotStarveAccountSync(t *testing.T) {
	gate := newGatedWebhookDispatcher(true)
	applier := &recordingAccountSync{}
	client := newDispatcherClient(nil, gate, nil)
	client.commandHandler = &CommandHandlerAdapter{AccountSync: applier}
	out := make(chan *pb.DaemonEvent, 16)
	ctx := startTestWebhookLane(t, client, out)

	webhookIDs := []string{"wh-0", "wh-1", "wh-2"}
	fed := make(chan struct{})
	go func() {
		defer close(fed)
		for _, id := range webhookIDs {
			client.handleCommand(ctx, webhookCmd(id), out)
		}
		client.handleCommand(ctx, &pb.OrchestratorCommand{
			CommandId: "sync",
			Cmd: &pb.OrchestratorCommand_ApplyAgentAccountSync{ApplyAgentAccountSync: &pb.ApplyAgentAccountSyncCommand{
				LocalAgentAccountId: "local", SyncedAgentAccountId: "synced", Version: 9,
			}},
		}, out)
	}()

	// Every webhook is still blocked in Dispatch: the gate is not released.
	result := recvEvent(t, out).GetResult()
	if result == nil || !result.GetOk() || result.GetCommandId() != "sync" {
		t.Fatalf("expected ok account-sync result while webhooks are blocked, got %+v", result)
	}
	if applier.cmd.SyncedAgentAccountID != "synced" || applier.cmd.Version != 9 {
		t.Fatalf("applier saw %+v, want the sync command", applier.cmd)
	}
	waitClosed(t, fed, "the command reader to finish feeding")

	close(gate.release)
	for _, want := range webhookIDs {
		if ack := recvAck(t, out); !ack.GetOk() || ack.GetCommandId() != want {
			t.Fatalf("ack = %+v, want ok ack for %s", ack, want)
		}
	}
}

func TestWebhookLane_DispatchesInArrivalOrder(t *testing.T) {
	gate := newGatedWebhookDispatcher(false)
	client := newDispatcherClient(nil, gate, nil)
	out := make(chan *pb.DaemonEvent, 8)
	ctx := startTestWebhookLane(t, client, out)

	ids := []string{"A", "B", "C"}
	for _, id := range ids {
		client.handleCommand(ctx, webhookCmd(id), out)
	}
	for _, want := range ids {
		if got := recvStarted(t, gate); got != want {
			t.Fatalf("Dispatch order: got %s, want %s", got, want)
		}
	}
	for _, want := range ids {
		if ack := recvAck(t, out); ack.GetCommandId() != want {
			t.Fatalf("ack order: got %s, want %s", ack.GetCommandId(), want)
		}
	}
}

func TestWebhookLane_FullQueueNacksWithoutBlocking(t *testing.T) {
	gate := newGatedWebhookDispatcher(true)
	client := newDispatcherClient(nil, gate, nil)
	out := make(chan *pb.DaemonEvent, webhookLaneDepth+4)
	ctx := startTestWebhookLane(t, client, out)

	client.handleCommand(ctx, webhookCmd("head"), out)
	recvStarted(t, gate) // the worker holds the head, so the queue is empty
	for i := range webhookLaneDepth {
		client.handleCommand(ctx, webhookCmd(fmt.Sprintf("q-%d", i)), out)
	}

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		client.handleCommand(ctx, webhookCmd("overflow"), out)
	}()
	waitClosed(t, returned, "handleCommand to return on a full webhook queue")
	ack := recvAck(t, out)
	if ack.GetOk() || ack.GetCommandId() != "overflow" || ack.GetError() != "daemon webhook queue full" {
		t.Fatalf("expected immediate queue-full nack, got %+v", ack)
	}

	close(gate.release)
	for range webhookLaneDepth + 1 {
		if ack := recvAck(t, out); !ack.GetOk() {
			t.Fatalf("queued webhook acked not ok: %+v", ack)
		}
	}
}

func TestDispatchCommand_Webhook_EmitsAck(t *testing.T) {
	gate := newGatedWebhookDispatcher(false)
	client := newDispatcherClient(nil, gate, nil)
	client.cloudControl = nil // Missing local policy is read-only; webhook hints still dispatch.
	out := make(chan *pb.DaemonEvent, 4)
	ctx := startTestWebhookLane(t, client, out)
	if ev := client.dispatchCommand(ctx, webhookCmd("c-5"), out); ev != nil {
		t.Fatalf("expected no synchronous result for an enqueued webhook, got %+v", ev)
	}
	if ack := recvAck(t, out); !ack.GetOk() || ack.GetCommandId() != "c-5" {
		t.Fatalf("expected webhook ack ok=true, got %+v", ack)
	}
	if got := recvStarted(t, gate); got != "c-5" {
		t.Fatalf("dispatched %q, want c-5", got)
	}
}

func TestDispatchCommand_Webhook_FailureAckWithError(t *testing.T) {
	gate := newGatedWebhookDispatcher(false)
	gate.err = errors.New("route not found")
	client := newDispatcherClient(nil, gate, nil)
	out := make(chan *pb.DaemonEvent, 4)
	ctx := startTestWebhookLane(t, client, out)
	client.dispatchCommand(ctx, webhookCmd("c-5b"), out)
	ack := recvAck(t, out)
	if ack.GetOk() || ack.GetCommandId() != "c-5b" || ack.GetError() != "route not found" {
		t.Fatalf("expected webhook ack ok=false with the Dispatch error, got %+v", ack)
	}
}

func TestDispatchCommand_Webhook_NoLaneNacks(t *testing.T) {
	gate := newGatedWebhookDispatcher(false)
	client := newDispatcherClient(nil, gate, nil)
	ev := client.dispatchCommand(context.Background(), webhookCmd("c-6"), make(chan *pb.DaemonEvent, 1))
	ack := ev.GetAck()
	if ack == nil || ack.GetOk() || ack.GetCommandId() != "c-6" || ack.GetError() != "webhook lane not running" {
		t.Fatalf("expected lane-not-running nack, got %+v", ev)
	}
	if len(gate.started) != 0 {
		t.Fatal("webhook dispatched without a running lane")
	}
}

// TestWebhookLane_TeardownJoinsWorker cancels the lane while a Dispatch is in
// flight and the queue holds more, then closes outbound after the join, as
// openStream does. A worker that outlived the join would panic on the send.
func TestWebhookLane_TeardownJoinsWorker(t *testing.T) {
	gate := newGatedWebhookDispatcher(true)
	client := newDispatcherClient(nil, gate, nil)
	out := make(chan *pb.DaemonEvent) // nobody reads: an ack send can only stall
	ctx, cancel := context.WithCancel(context.Background())
	client.startWebhookLane(ctx, out)
	lane := client.webhookLane

	client.handleCommand(ctx, webhookCmd("in-flight"), out)
	recvStarted(t, gate)
	client.handleCommand(ctx, webhookCmd("queued-1"), out)
	client.handleCommand(ctx, webhookCmd("queued-2"), out)

	cancel()
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		client.joinWebhookLane()
	}()
	waitClosed(t, joined, "the webhook worker to be joined")
	waitClosed(t, lane.done, "the webhook worker done channel")
	close(out)

	if client.webhookLane != nil {
		t.Fatal("joinWebhookLane left the lane set")
	}
	if len(gate.started) != 0 {
		t.Fatal("queued webhooks were dispatched after the lane was cancelled")
	}
}
