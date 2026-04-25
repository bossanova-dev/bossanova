package upstream

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"
)

type fakeCommandHandler struct {
	stopCalls   atomic.Int32
	pauseCalls  atomic.Int32
	resumeCalls atomic.Int32
	returnErr   error
	session     *pb.Session
}

func (f *fakeCommandHandler) Stop(_ context.Context, _ string) (*pb.Session, error) {
	f.stopCalls.Add(1)
	return f.session, f.returnErr
}
func (f *fakeCommandHandler) Pause(_ context.Context, _ string) (*pb.Session, error) {
	f.pauseCalls.Add(1)
	return f.session, f.returnErr
}
func (f *fakeCommandHandler) Resume(_ context.Context, _ string) (*pb.Session, error) {
	f.resumeCalls.Add(1)
	return f.session, f.returnErr
}

type fakeWebhookDispatcher struct {
	calls atomic.Int32
	err   error
}

func (f *fakeWebhookDispatcher) Dispatch(_ context.Context, _ *pb.WebhookEvent) error {
	f.calls.Add(1)
	return f.err
}

type fakeAttacher struct {
	calls     atomic.Int32
	chunks    []*pb.SessionAttachChunk
	attachErr error
}

func (f *fakeAttacher) Attach(_ context.Context, sessionID, commandID string) (<-chan *pb.SessionAttachChunk, error) {
	f.calls.Add(1)
	if f.attachErr != nil {
		return nil, f.attachErr
	}
	ch := make(chan *pb.SessionAttachChunk, len(f.chunks)+1)
	for _, c := range f.chunks {
		ch <- c
	}
	close(ch)
	_ = sessionID
	_ = commandID
	return ch, nil
}

// newDispatcherClient wires a StreamClient with just the command-side
// collaborators. Other fields stay nil; the dispatcher functions under
// test never touch them.
func newDispatcherClient(
	handler SessionCommandHandler,
	webhooks WebhookDispatcher,
	attacher SessionAttacher,
) *StreamClient {
	return NewStreamClient(StreamClientConfig{
		CommandHandler: handler,
		Webhooks:       webhooks,
		Attacher:       attacher,
		Logger:         zerolog.Nop(),
	})
}

func TestDispatchCommand_Stop_CallsHandler(t *testing.T) {
	sess := &pb.Session{Id: "s1"}
	handler := &fakeCommandHandler{session: sess}
	client := newDispatcherClient(handler, nil, nil)

	out := make(chan *pb.DaemonEvent, 4)
	cmd := &pb.OrchestratorCommand{
		CommandId: "c-1",
		Cmd:       &pb.OrchestratorCommand_Stop{Stop: &pb.StopSessionCommand{SessionId: "s1"}},
	}
	ev := client.dispatchCommand(context.Background(), cmd, out)

	if handler.stopCalls.Load() != 1 {
		t.Fatalf("stop calls = %d, want 1", handler.stopCalls.Load())
	}
	if r := ev.GetResult(); r == nil || !r.GetOk() || r.GetCommandId() != "c-1" {
		t.Fatalf("unexpected result: %+v", ev)
	}
}

func TestDispatchCommand_Pause_CallsHandler(t *testing.T) {
	handler := &fakeCommandHandler{session: &pb.Session{Id: "s1"}}
	client := newDispatcherClient(handler, nil, nil)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-2",
			Cmd:       &pb.OrchestratorCommand_Pause{Pause: &pb.PauseSessionCommand{SessionId: "s1"}},
		}, make(chan *pb.DaemonEvent, 4))
	if handler.pauseCalls.Load() != 1 {
		t.Fatalf("pause calls = %d", handler.pauseCalls.Load())
	}
	if r := ev.GetResult(); r == nil || !r.GetOk() {
		t.Fatalf("expected ok result: %+v", ev)
	}
}

func TestDispatchCommand_Resume_CallsHandler(t *testing.T) {
	handler := &fakeCommandHandler{session: &pb.Session{Id: "s1"}}
	client := newDispatcherClient(handler, nil, nil)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-3",
			Cmd:       &pb.OrchestratorCommand_Resume{Resume: &pb.ResumeSessionCommand{SessionId: "s1"}},
		}, make(chan *pb.DaemonEvent, 4))
	if handler.resumeCalls.Load() != 1 {
		t.Fatalf("resume calls = %d", handler.resumeCalls.Load())
	}
	if r := ev.GetResult(); r == nil || !r.GetOk() {
		t.Fatalf("expected ok result: %+v", ev)
	}
}

func TestDispatchCommand_Transfer_NotYetImplemented(t *testing.T) {
	// T4.6 lands the coordinated transfer protocol on the bosso side.
	// Daemon-side session-lifecycle participation is a follow-up; when
	// no TransferHandler is wired, the dispatcher ACKs a structured
	// error so bosso's command waiter resolves promptly.
	client := newDispatcherClient(nil, nil, nil)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-4",
			Cmd:       &pb.OrchestratorCommand_Transfer{Transfer: &pb.TransferSessionCommand{SessionId: "s1"}},
		}, make(chan *pb.DaemonEvent, 4))
	if r := ev.GetResult(); r == nil || r.GetOk() || r.GetError() == "" {
		t.Fatalf("expected error result for transfer, got %+v", ev)
	}
}

// --- Coordinated transfer protocol (decision #14, T4.6) ---

// fakeTransferHandler records which protocol hook bosso invoked. Tests
// configure the per-call return values to simulate the source-role
// (nil TransferConfirmed) and target-role (non-nil TransferConfirmed)
// outcomes.
type fakeTransferHandler struct {
	transferCalls  atomic.Int32
	confirmedCalls atomic.Int32
	cancelCalls    atomic.Int32
	transferResult *pb.TransferConfirmed
	transferErr    error
	confirmedErr   error
	cancelErr      error
}

func (f *fakeTransferHandler) Transfer(_ context.Context, _ *pb.TransferSessionCommand) (*pb.TransferConfirmed, error) {
	f.transferCalls.Add(1)
	return f.transferResult, f.transferErr
}
func (f *fakeTransferHandler) Confirmed(_ context.Context, _ *pb.TransferConfirmed) error {
	f.confirmedCalls.Add(1)
	return f.confirmedErr
}
func (f *fakeTransferHandler) Cancel(_ context.Context, _ *pb.TransferCancel) error {
	f.cancelCalls.Add(1)
	return f.cancelErr
}

// newDispatcherClientWithTransfer is a constructor shim the transfer
// tests use. Keeps the original three-arg newDispatcherClient intact for
// the non-transfer cases so they stay diff-minimal.
func newDispatcherClientWithTransfer(transfer TransferHandler) *StreamClient {
	return NewStreamClient(StreamClientConfig{
		TransferHandler: transfer,
		Logger:          zerolog.Nop(),
	})
}

func TestDispatchCommand_Transfer_SourceRole_ReturnsOkNoPayload(t *testing.T) {
	// Source role: handler returns (nil, nil). Dispatcher ACKs Ok:true
	// with no TransferConfirmed — bosso reads this as "source accepted,
	// has emitted the SessionDelta{UPDATED, transferring_to=target}".
	handler := &fakeTransferHandler{}
	client := newDispatcherClientWithTransfer(handler)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-tx1",
			Cmd:       &pb.OrchestratorCommand_Transfer{Transfer: &pb.TransferSessionCommand{SessionId: "s1"}},
		}, make(chan *pb.DaemonEvent, 4))
	r := ev.GetResult()
	if r == nil || !r.GetOk() || r.GetCommandId() != "c-tx1" {
		t.Fatalf("expected ok handshake, got %+v", ev)
	}
	if r.GetTransferConfirmed() != nil {
		t.Errorf("source-role result must not carry TransferConfirmed, got %+v", r.GetTransferConfirmed())
	}
	if handler.transferCalls.Load() != 1 {
		t.Errorf("transfer calls = %d, want 1", handler.transferCalls.Load())
	}
}

func TestDispatchCommand_Transfer_TargetRole_EmbedsConfirmed(t *testing.T) {
	// Target role: handler returns a non-nil TransferConfirmed. The
	// dispatcher MUST embed it in CommandResult.Payload so bosso can
	// proceed to step 4 (forward TransferConfirmed to source).
	handler := &fakeTransferHandler{
		transferResult: &pb.TransferConfirmed{SessionId: "s1", TargetDaemonId: "d-b"},
	}
	client := newDispatcherClientWithTransfer(handler)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-tx2",
			Cmd:       &pb.OrchestratorCommand_Transfer{Transfer: &pb.TransferSessionCommand{SessionId: "s1"}},
		}, make(chan *pb.DaemonEvent, 4))
	r := ev.GetResult()
	if r == nil || !r.GetOk() {
		t.Fatalf("expected ok result, got %+v", ev)
	}
	tc := r.GetTransferConfirmed()
	if tc == nil || tc.GetSessionId() != "s1" || tc.GetTargetDaemonId() != "d-b" {
		t.Fatalf("target-role result missing TransferConfirmed payload: %+v", r.GetPayload())
	}
}

func TestDispatchCommand_TransferConfirmed_AcksOk(t *testing.T) {
	// Step 4 on source: emit DELETED session delta, ACK Ok:true.
	handler := &fakeTransferHandler{}
	client := newDispatcherClientWithTransfer(handler)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-tx3",
			Cmd: &pb.OrchestratorCommand_TransferConfirmed{
				TransferConfirmed: &pb.TransferConfirmed{SessionId: "s1", TargetDaemonId: "d-b"},
			},
		}, make(chan *pb.DaemonEvent, 4))
	if r := ev.GetResult(); r == nil || !r.GetOk() {
		t.Fatalf("expected ok result, got %+v", ev)
	}
	if handler.confirmedCalls.Load() != 1 {
		t.Errorf("confirmed calls = %d, want 1", handler.confirmedCalls.Load())
	}
}

func TestDispatchCommand_TransferConfirmed_NoHandler_AcksOk(t *testing.T) {
	// No handler wired: TransferConfirmed is idempotent-no-op semantics.
	// Still ACK Ok so bosso's waiter doesn't trip.
	client := newDispatcherClient(nil, nil, nil)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-tx4",
			Cmd: &pb.OrchestratorCommand_TransferConfirmed{
				TransferConfirmed: &pb.TransferConfirmed{SessionId: "s1"},
			},
		}, make(chan *pb.DaemonEvent, 4))
	if r := ev.GetResult(); r == nil || !r.GetOk() {
		t.Fatalf("expected ok no-op result, got %+v", ev)
	}
}

func TestDispatchCommand_TransferCancel_AcksOk(t *testing.T) {
	handler := &fakeTransferHandler{}
	client := newDispatcherClientWithTransfer(handler)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-tx5",
			Cmd: &pb.OrchestratorCommand_TransferCancel{
				TransferCancel: &pb.TransferCancel{SessionId: "s1", Reason: "target create failed"},
			},
		}, make(chan *pb.DaemonEvent, 4))
	if r := ev.GetResult(); r == nil || !r.GetOk() {
		t.Fatalf("expected ok result, got %+v", ev)
	}
	if handler.cancelCalls.Load() != 1 {
		t.Errorf("cancel calls = %d, want 1", handler.cancelCalls.Load())
	}
}

func TestDispatchCommand_TransferCancel_NoHandler_AcksOk(t *testing.T) {
	// Like TransferConfirmed — no handler means idempotent no-op, still ACK.
	client := newDispatcherClient(nil, nil, nil)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-tx6",
			Cmd: &pb.OrchestratorCommand_TransferCancel{
				TransferCancel: &pb.TransferCancel{SessionId: "s1"},
			},
		}, make(chan *pb.DaemonEvent, 4))
	if r := ev.GetResult(); r == nil || !r.GetOk() {
		t.Fatalf("expected ok no-op result, got %+v", ev)
	}
}

func TestDispatchCommand_Webhook_EmitsAck(t *testing.T) {
	dispatcher := &fakeWebhookDispatcher{}
	client := newDispatcherClient(nil, dispatcher, nil)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-5",
			Cmd:       &pb.OrchestratorCommand_Webhook{Webhook: &pb.WebhookEvent{Provider: "github"}},
		}, make(chan *pb.DaemonEvent, 4))
	if dispatcher.calls.Load() != 1 {
		t.Fatalf("dispatcher calls = %d, want 1", dispatcher.calls.Load())
	}
	ack := ev.GetAck()
	if ack == nil || !ack.GetOk() || ack.GetCommandId() != "c-5" {
		t.Fatalf("expected webhook ack ok=true, got %+v", ev)
	}
}

func TestDispatchCommand_Webhook_FailureAckWithError(t *testing.T) {
	dispatcher := &fakeWebhookDispatcher{err: errors.New("route not found")}
	client := newDispatcherClient(nil, dispatcher, nil)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{
			CommandId: "c-5b",
			Cmd:       &pb.OrchestratorCommand_Webhook{Webhook: &pb.WebhookEvent{}},
		}, make(chan *pb.DaemonEvent, 4))
	ack := ev.GetAck()
	if ack == nil || ack.GetOk() || ack.GetError() == "" {
		t.Fatalf("expected webhook ack ok=false with error, got %+v", ev)
	}
}

func TestDispatchCommand_UnknownOneof_LogsAndSkips(t *testing.T) {
	// Nil oneof is the only portable "unknown" we can construct here —
	// a zero-initialized OrchestratorCommand has no Cmd set and so
	// exercises the default branch of dispatchCommand.
	client := newDispatcherClient(nil, nil, nil)
	ev := client.dispatchCommand(context.Background(),
		&pb.OrchestratorCommand{CommandId: "c-u"},
		make(chan *pb.DaemonEvent, 4))
	if ev != nil {
		t.Fatalf("expected nil DaemonEvent for unknown command, got %+v", ev)
	}
}

func TestDispatchCommand_AttachSession_StreamsChunksUntilClose(t *testing.T) {
	// The attacher fires two chunks and then closes its channel. The
	// dispatcher returns an immediate ok CommandResult (handshake)
	// and a background goroutine pumps the chunks onto outbound.
	chunks := []*pb.SessionAttachChunk{
		{SessionId: "s1", CommandId: "c-att", Event: &pb.SessionAttachChunk_OutputLine{OutputLine: &pb.OutputLine{Text: "hello"}}},
		{SessionId: "s1", CommandId: "c-att", Event: &pb.SessionAttachChunk_SessionEnded{SessionEnded: &pb.SessionEnded{}}},
	}
	attacher := &fakeAttacher{chunks: chunks}
	client := newDispatcherClient(nil, nil, attacher)

	out := make(chan *pb.DaemonEvent, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ev := client.dispatchCommand(ctx,
		&pb.OrchestratorCommand{
			CommandId: "c-att",
			Cmd:       &pb.OrchestratorCommand_Attach{Attach: &pb.AttachSessionCommand{SessionId: "s1"}},
		}, out)

	// Handshake result must be ok, no session payload.
	r := ev.GetResult()
	if r == nil || !r.GetOk() || r.GetCommandId() != "c-att" {
		t.Fatalf("expected ok handshake result, got %+v", ev)
	}

	// Collect chunks arriving asynchronously.
	got := 0
	deadline := time.After(500 * time.Millisecond)
	for got < len(chunks) {
		select {
		case ev := <-out:
			if c := ev.GetAttachChunk(); c != nil {
				got++
			}
		case <-deadline:
			t.Fatalf("expected %d chunks, got %d", len(chunks), got)
		}
	}
	if attacher.calls.Load() != 1 {
		t.Fatalf("attacher calls = %d, want 1", attacher.calls.Load())
	}
}
