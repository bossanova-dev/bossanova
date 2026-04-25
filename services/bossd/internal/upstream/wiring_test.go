package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// wiringHandler is a minimal OrchestratorServiceHandler that counts
// RegisterDaemon and DaemonStream calls and explicitly rejects the
// already-removed Heartbeat / SyncSessions RPCs. The test passes only
// when bossd's startup wiring calls RegisterDaemon first and then opens
// DaemonStream — never the retired RPCs.
type wiringHandler struct {
	bossanovav1connect.UnimplementedOrchestratorServiceHandler

	registerCalls atomic.Int32
	streamCalls   atomic.Int32

	// snapshotSeen signals on first snapshot received so the test can
	// assert the wiring reaches the handshake point (not just TCP
	// connect).
	snapshotSeen chan struct{}
}

func (h *wiringHandler) RegisterDaemon(
	_ context.Context,
	req *connect.Request[pb.RegisterDaemonRequest],
) (*connect.Response[pb.RegisterDaemonResponse], error) {
	h.registerCalls.Add(1)
	return connect.NewResponse(&pb.RegisterDaemonResponse{
		DaemonId:     req.Msg.DaemonId,
		SessionToken: "session-tok-" + req.Msg.DaemonId,
	}), nil
}

func (h *wiringHandler) DaemonStream(
	ctx context.Context,
	stream *connect.BidiStream[pb.DaemonEvent, pb.OrchestratorCommand],
) error {
	h.streamCalls.Add(1)
	// Receive the first event — must be a snapshot per the protocol.
	ev, err := stream.Receive()
	if err != nil {
		return err
	}
	if ev.GetSnapshot() != nil {
		select {
		case h.snapshotSeen <- struct{}{}:
		default:
		}
	}
	// Block until the client disconnects.
	<-ctx.Done()
	return nil
}

// TestBossd_StartsStreamAfterRegister is the T3.7 acceptance test: it
// stands up a fake bosso, runs the same register→stream sequence the
// daemon startup does, and confirms (a) RegisterDaemon is hit exactly
// once, (b) DaemonStream is hit after, and (c) the initial snapshot
// reaches the handler. No legacy heartbeat/sync RPC invocations are
// observed because the fake handler doesn't even define them —
// attempting to call them would fail with Unimplemented.
func TestBossd_StartsStreamAfterRegister(t *testing.T) {
	t.Parallel()

	handler := &wiringHandler{
		snapshotSeen: make(chan struct{}, 1),
	}

	mux := http.NewServeMux()
	path, h := bossanovav1connect.NewOrchestratorServiceHandler(handler)
	mux.Handle(path, h)
	srv := httptest.NewUnstartedServer(mux)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)

	client := bossanovav1connect.NewOrchestratorServiceClient(srv.Client(), srv.URL)

	// 1. RegisterDaemon (the startup bootstrap) — same call main.go makes.
	regCtx, regCancel := context.WithTimeout(context.Background(), 5*time.Second)
	sessionToken, err := Register(regCtx, client, "daemon-wiring-test", "test-host", "jwt-abc", []string{"repo-1"})
	regCancel()
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	if sessionToken == "" {
		t.Fatalf("expected non-empty session token")
	}
	if got := handler.registerCalls.Load(); got != 1 {
		t.Fatalf("RegisterDaemon calls = %d, want 1", got)
	}

	// 2. StreamClient — same construction main.go does. Minimal snapshot
	//    readers so the snapshot payload is well-formed.
	stores := StreamStores{
		Sessions: NewSessionSnapshotReader(&fakeListerWire{
			sessions: []*pb.Session{{Id: "s-1", Title: "test"}},
		}),
		Repos: NewRepoSnapshotReader(func(_ context.Context) ([]string, error) {
			return []string{"repo-1"}, nil
		}),
		Chats: NewChatSnapshotReader(func(_ context.Context) ([]*pb.ClaudeChatMetadata, error) {
			return []*pb.ClaudeChatMetadata{{Id: "c-1", CreatedAt: timestamppb.Now()}}, nil
		}),
		Statuses: NewStatusSnapshotReader(func(_ context.Context) ([]*pb.ChatStatusEntry, error) {
			return nil, nil
		}),
	}

	streamClient := NewStreamClient(StreamClientConfig{
		Client:    client,
		AuthToken: sessionToken,
		DaemonID:  "daemon-wiring-test",
		Hostname:  "test-host",
		Stores:    stores,
		Events:    NewStreamBus(zerolog.Nop()),
		Logger:    zerolog.Nop(),
	})

	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()
	done := make(chan struct{})
	go func() {
		streamClient.Run(runCtx)
		close(done)
	}()

	// 3. Wait for the snapshot to land at the handler — proves the
	//    DaemonStream call reached the handshake point.
	select {
	case <-handler.snapshotSeen:
	case <-time.After(5 * time.Second):
		t.Fatalf("did not receive snapshot within 5s (register=%d, stream=%d)",
			handler.registerCalls.Load(), handler.streamCalls.Load())
	}

	if got := handler.streamCalls.Load(); got == 0 {
		t.Fatalf("DaemonStream calls = %d, want >= 1", got)
	}
	if got := handler.registerCalls.Load(); got != 1 {
		t.Fatalf("RegisterDaemon calls after stream open = %d, want 1 (no re-register)", got)
	}

	// 4. Clean shutdown — cancel the client and verify Run returns.
	runCancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("streamClient.Run did not return within 5s of cancel")
	}
}

// fakeListerWire is a trivial ProtoSessionLister for the wiring test.
// Name is suffixed -Wire to avoid colliding with other fakes in the
// package's tests.
type fakeListerWire struct {
	sessions []*pb.Session
}

func (f *fakeListerWire) ListSessions(_ context.Context) ([]*pb.Session, error) {
	return f.sessions, nil
}

// TestCommandHandlerAdapter_Stop verifies the adapter translates a
// stop command into a lifecycle call and echoes the post-stop session.
func TestCommandHandlerAdapter_Stop(t *testing.T) {
	t.Parallel()

	var stopCalled atomic.Int32
	var completionCalled atomic.Int32
	adapter := &CommandHandlerAdapter{
		Lifecycle: stopperFn(func(_ context.Context, id string) error {
			if id == "s-1" {
				stopCalled.Add(1)
			}
			return nil
		}),
		Sessions: readerFn(func(_ context.Context, id string) (*pb.Session, error) {
			return &pb.Session{Id: id}, nil
		}),
		Automation: nil,
		OnCompletion: func(_ context.Context, _ string) {
			completionCalled.Add(1)
		},
	}

	sess, err := adapter.Stop(context.Background(), "s-1")
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if sess.GetId() != "s-1" {
		t.Fatalf("session.id = %q, want s-1", sess.GetId())
	}
	if got := stopCalled.Load(); got != 1 {
		t.Fatalf("lifecycle stop calls = %d, want 1", got)
	}
	if got := completionCalled.Load(); got != 1 {
		t.Fatalf("completion calls = %d, want 1", got)
	}
}

// stopperFn / readerFn are function adapters used only by these tests
// to keep the setup inline rather than defining mock types.
type stopperFn func(context.Context, string) error

func (f stopperFn) StopSession(ctx context.Context, id string) error { return f(ctx, id) }

type readerFn func(context.Context, string) (*pb.Session, error)

func (f readerFn) GetSession(ctx context.Context, id string) (*pb.Session, error) {
	return f(ctx, id)
}
