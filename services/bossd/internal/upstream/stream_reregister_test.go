package upstream

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// reRegisterHandler models bosso's behaviour when a bossd presents a
// stale daemon session_token: the first N DaemonStream opens are
// rejected with Unauthenticated; once RegisterDaemon issues a fresh
// token bossd retries and is accepted. Matches the real daemons-table
// UPSERT semantics where a second registration rotates the token and
// invalidates the previous one.
type reRegisterHandler struct {
	bossanovav1connect.UnimplementedOrchestratorServiceHandler

	mu            sync.Mutex
	validToken    string // the only session_token bosso currently accepts
	registerCalls int32
	streamCalls   int32
	streamRejects int32
	snapshotSeen  chan struct{}
}

func (h *reRegisterHandler) RegisterDaemon(
	_ context.Context,
	req *connect.Request[pb.RegisterDaemonRequest],
) (*connect.Response[pb.RegisterDaemonResponse], error) {
	n := atomic.AddInt32(&h.registerCalls, 1)
	// Rotate the accepted token on every Register call (matching the
	// UPSERT behaviour). n=1 issues the initial token, n=2 the fresh
	// one.
	tok := "session-tok-" + req.Msg.DaemonId + "-v" + itoa(int(n))
	h.mu.Lock()
	h.validToken = tok
	h.mu.Unlock()
	return connect.NewResponse(&pb.RegisterDaemonResponse{
		DaemonId:     req.Msg.DaemonId,
		SessionToken: tok,
	}), nil
}

func (h *reRegisterHandler) DaemonStream(
	ctx context.Context,
	stream *connect.BidiStream[pb.DaemonEvent, pb.OrchestratorCommand],
) error {
	atomic.AddInt32(&h.streamCalls, 1)
	presented := stream.RequestHeader().Get("X-Daemon-Token")
	h.mu.Lock()
	valid := h.validToken
	h.mu.Unlock()
	if presented != valid {
		atomic.AddInt32(&h.streamRejects, 1)
		return connect.NewError(connect.CodeUnauthenticated, errors.New("invalid daemon token"))
	}
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
	<-ctx.Done()
	return nil
}

// TestStreamClient_ReRegistersOnStaleSessionToken verifies the
// self-healing flow: bossd starts with a session_token that bosso
// rejects (e.g. another bossd with the same daemon_id rotated it),
// Run sees CodeUnauthenticated, calls ReRegister, and the next
// reconnect authenticates with the fresh token.
func TestStreamClient_ReRegistersOnStaleSessionToken(t *testing.T) {
	t.Parallel()

	handler := &reRegisterHandler{
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

	// Seed bosso with a valid token owned by a (hypothetical) peer
	// bossd. The StreamClient starts with a DIFFERENT, stale token —
	// the sort of state we'd be in if a peer daemon rotated the token
	// right before us.
	_, err := Register(context.Background(), client, "daemon-reregister-test", "host", "jwt", []string{"repo-1"})
	if err != nil {
		t.Fatalf("seed Register failed: %v", err)
	}

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
		Client:       client,
		AuthToken:    "jwt",
		SessionToken: NewSessionTokenHolder("stale-token-that-bosso-does-not-know"), // deliberately wrong
		DaemonID:     "daemon-reregister-test",
		Hostname:     "host",
		Stores:       stores,
		Events:       NewStreamBus(zerolog.Nop()),
		ReRegister: func(ctx context.Context) (string, error) {
			return Register(ctx, client, "daemon-reregister-test", "host", "jwt", []string{"repo-1"})
		},
		Logger: zerolog.Nop(),
	})

	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()
	done := make(chan struct{})
	go func() {
		streamClient.Run(runCtx)
		close(done)
	}()

	select {
	case <-handler.snapshotSeen:
	case <-time.After(10 * time.Second):
		t.Fatalf("did not recover within 10s (register=%d, stream=%d, rejects=%d)",
			atomic.LoadInt32(&handler.registerCalls),
			atomic.LoadInt32(&handler.streamCalls),
			atomic.LoadInt32(&handler.streamRejects))
	}

	if got := atomic.LoadInt32(&handler.streamRejects); got < 1 {
		t.Fatalf("expected at least 1 stream rejection before re-register, got %d", got)
	}
	// Seed + one re-register call driven by the auth failure = 2.
	// Allow for up to one extra call to absorb backoff-window timing.
	if got := atomic.LoadInt32(&handler.registerCalls); got < 2 {
		t.Fatalf("RegisterDaemon calls = %d, want >= 2 (seed + re-register)", got)
	}

	runCancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("streamClient.Run did not return within 5s of cancel")
	}
}

// itoa is a tiny stdlib-free int-to-string helper so the test file
// doesn't pull in strconv for one call site. Non-negative only.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
