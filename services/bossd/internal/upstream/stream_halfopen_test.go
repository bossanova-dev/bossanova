package upstream

import (
	"context"
	"crypto/x509"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossd/internal/tmux"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/proto"
)

// BOS-1375. These tests reproduce the half-open reverse stream that wedged a
// daemon's TerminalStream for ten hours: bosso's Connect end-stream envelope
// arrives, but the HTTP/2 stream never gets END_STREAM or RST_STREAM. The
// client reader then parks in connect's end-of-stream discard on the HTTP/2
// response pipe, the HTTP/2 request writer parks reading connect's request
// io.Pipe, and cancelling the stream context wakes neither — x/net/http2 only
// looks at ctx after the request body is done. Every recovery path
// (heartbeat watchdog, CycleStream, logout) funnels through that cancel.
//
// A connect handler cannot express the shape (returning from it ends the
// stream), so halfOpenServer mounts a raw http.Handler at the procedure paths
// and drives the wire format by hand. The client side is the production stack:
// buildHTTPSUpstreamTransport (http.Transport + http2.ConfigureTransports) →
// the generated connect client → the real openers.

// halfOpenDeadline bounds every wait in this file, so a regression fails the
// test instead of hanging the package.
const halfOpenDeadline = 5 * time.Second

// connectStreamContentType is what connect's validateResponse requires on a
// Connect-protocol streaming response. Anything else makes Receive fail
// immediately, which would let the tests pass without ever reaching the
// half-open shape.
const connectStreamContentType = "application/connect+proto"

// halfOpenServer is an HTTP/2-over-TLS server whose stream handlers write
// their data frames and the Connect end-stream envelope, flush, and then hold
// the HTTP/2 stream open — never reading the request body — until the client
// aborts the stream or the test releases them.
type halfOpenServer struct {
	srv         *httptest.Server
	client      bossanovav1connect.OrchestratorServiceClient
	release     chan struct{}
	releaseOnce sync.Once
	// opened receives one entry per stream the server has fully written (data
	// frames + end-stream envelope flushed), so a test knows the half-open
	// shape is on the wire before it cancels.
	opened chan string
	// opens counts streams per procedure path.
	terminalOpens atomic.Int32
	daemonOpens   atomic.Int32
}

// halfOpenEnvelope frames payload as one Connect streaming envelope.
func halfOpenEnvelope(flags byte, payload []byte) []byte {
	buf := make([]byte, 5+len(payload))
	buf[0] = flags
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(payload)))
	copy(buf[5:], payload)
	return buf
}

func newHalfOpenServer(t *testing.T) *halfOpenServer {
	t.Helper()
	h := &halfOpenServer{
		release: make(chan struct{}),
		opened:  make(chan string, 16),
	}
	hold := func(firstFrame proto.Message, counter *atomic.Int32) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			counter.Add(1)
			payload, err := proto.Marshal(firstFrame)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", connectStreamContentType)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(halfOpenEnvelope(0x00, payload))
			// The Connect end-stream envelope (flag 0x02) is the only JSON part
			// of the wire format; an empty object is a clean OK end-of-stream.
			_, _ = w.Write(halfOpenEnvelope(0x02, []byte("{}")))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			h.opened <- r.URL.Path
			// Hold the HTTP/2 stream open: returning would send END_STREAM.
			select {
			case <-h.release:
			case <-r.Context().Done():
			}
		}
	}
	mux := http.NewServeMux()
	mux.Handle(bossanovav1connect.OrchestratorServiceTerminalStreamProcedure, hold(&pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_Ready{Ready: &pb.TerminalReady{}},
	}, &h.terminalOpens))
	mux.Handle(bossanovav1connect.OrchestratorServiceDaemonStreamProcedure, hold(&pb.OrchestratorCommand{
		CommandId: "half-open",
	}, &h.daemonOpens))

	h.srv = httptest.NewUnstartedServer(mux)
	h.srv.EnableHTTP2 = true
	h.srv.StartTLS()

	// The production HTTPS stack. Add the test certificate to the existing
	// TLS config rather than replacing it: replacing drops the h2 ALPN entry
	// http2.ConfigureTransports installed, and connect then refuses the bidi
	// stream as HTTP/1.1.
	tr, _ := buildHTTPSUpstreamTransport()
	pool := x509.NewCertPool()
	pool.AddCert(h.srv.Certificate())
	tr.TLSClientConfig.RootCAs = pool
	h.client = bossanovav1connect.NewOrchestratorServiceClient(&http.Client{Transport: tr}, h.srv.URL)

	t.Cleanup(func() {
		// Release any handler still holding a stream (the red case), then let
		// Close join every handler before the transport is dropped.
		h.releaseAll()
		h.srv.Close()
		tr.CloseIdleConnections()
	})
	return h
}

// releaseAll unblocks every handler still holding a stream open. Idempotent.
func (h *halfOpenServer) releaseAll() {
	h.releaseOnce.Do(func() { close(h.release) })
}

// joinOnCleanup registers a cleanup that releases the server's held streams
// and then joins done. Registered after newHalfOpenServer, so it runs before
// the server's own cleanup: in a red run the goroutine is still parked on a
// held stream, and releasing first is what lets it unwind and be joined.
func (h *halfOpenServer) joinOnCleanup(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	t.Cleanup(func() {
		h.releaseAll()
		select {
		case <-done:
		case <-time.After(halfOpenDeadline):
			t.Errorf("%s was not reclaimed after the server released its streams", what)
		}
	})
}

// waitOpened blocks until the server has flushed a stream on path.
func (h *halfOpenServer) waitOpened(t *testing.T, path string) {
	t.Helper()
	deadline := time.After(halfOpenDeadline)
	for {
		select {
		case got := <-h.opened:
			if got == path {
				return
			}
		case <-deadline:
			t.Fatalf("server did not open a half-open %s stream within %s", path, halfOpenDeadline)
		}
	}
}

// cancelReturnBound is how promptly a parked call must return once its stream
// context is cancelled.
const cancelReturnBound = time.Second

// returnsPromptly waits for done, failing the test (rather than hanging it) if
// the goroutine is still running cancelReturnBound later. A goroutine reported
// here is still joined by joinOnCleanup once the server releases its streams.
func returnsPromptly(t *testing.T, done <-chan struct{}, what string) bool {
	t.Helper()
	select {
	case <-done:
		return true
	case <-time.After(cancelReturnBound):
		t.Errorf("%s did not return within %s", what, cancelReturnBound)
		return false
	}
}

// receiveParked starts recv on its own goroutine and asserts it is still
// blocked after a short settle window — the half-open shape, not a stream
// that simply ended. It returns the goroutine's done channel; cleanup joins it
// after the server releases its streams, so a red run does not leak it.
func (h *halfOpenServer) receiveParked(t *testing.T, recv func() error, gotErr *atomic.Value) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	h.joinOnCleanup(t, done, "parked Receive goroutine")
	go func() {
		defer close(done)
		if err := recv(); err != nil {
			gotErr.Store(err)
		}
	}()
	select {
	case <-done:
		t.Fatalf("Receive returned before cancel (err=%v); the server is not holding the stream half-open", gotErr.Load())
	case <-time.After(150 * time.Millisecond):
	}
	return done
}

// TestTerminalStreamOpener_CancelAbortsHalfOpenStream covers R1 for the
// TerminalStream opener: once the stream is half-open and Receive is parked in
// connect's end-of-stream discard, cancelling the stream context must make
// Receive return.
func TestTerminalStreamOpener_CancelAbortsHalfOpenStream(t *testing.T) {
	t.Parallel()
	srv := newHalfOpenServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opener := &terminalConnectOpener{client: srv.client, logger: zerolog.Nop()}
	stream := opener.TerminalStream(ctx)
	if err := stream.Send(nil); err != nil {
		t.Fatalf("flush headers: %v", err)
	}
	srv.waitOpened(t, bossanovav1connect.OrchestratorServiceTerminalStreamProcedure)

	// The first frame is TerminalReady: proof the response is live and the
	// next Receive will reach the end-stream envelope.
	first, err := stream.Receive()
	if err != nil {
		t.Fatalf("first Receive: %v", err)
	}
	if first.GetReady() == nil {
		t.Fatalf("first frame = %v, want TerminalReady", first)
	}

	var recvErr atomic.Value
	done := srv.receiveParked(t, func() error { _, err := stream.Receive(); return err }, &recvErr)

	cancel()
	if !returnsPromptly(t, done, "Receive after cancelling a half-open TerminalStream") {
		return
	}
	if recvErr.Load() == nil {
		t.Fatal("Receive returned nil error after cancel; want a cancellation/abort error")
	}
}

// TestDaemonStreamOpener_CancelAbortsHalfOpenStream covers R1 for the
// DaemonStream opener's existing cancel sources (logout, refresher error,
// shutdown).
func TestDaemonStreamOpener_CancelAbortsHalfOpenStream(t *testing.T) {
	t.Parallel()
	srv := newHalfOpenServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opener := &connectOpener{client: srv.client, logger: zerolog.Nop()}
	stream := opener.DaemonStream(ctx)
	if err := stream.Send(nil); err != nil {
		t.Fatalf("flush headers: %v", err)
	}
	srv.waitOpened(t, bossanovav1connect.OrchestratorServiceDaemonStreamProcedure)

	first, err := stream.Receive()
	if err != nil {
		t.Fatalf("first Receive: %v", err)
	}
	if first.GetCommandId() != "half-open" {
		t.Fatalf("first frame = %v, want the half-open command", first)
	}

	var recvErr atomic.Value
	done := srv.receiveParked(t, func() error { _, err := stream.Receive(); return err }, &recvErr)

	cancel()
	if !returnsPromptly(t, done, "Receive after cancelling a half-open DaemonStream") {
		return
	}
	if recvErr.Load() == nil {
		t.Fatal("Receive returned nil error after cancel; want a cancellation/abort error")
	}
}

// TestTerminalStreamOpener_CancelAbortsFlowControlParkedWriter covers R1 when
// the HTTP/2 request writer is parked on flow control rather than on connect's
// request pipe: the half-open peer never reads the request body, so it never
// grants WINDOW_UPDATE, and once the client has sent more than the window the
// writer waits in awaitFlowControl. Closing the request pipe does not wake
// that wait; closing the response (abortStream) does — this is the case the
// CloseResponse half of the opener's cancel hook exists for.
func TestTerminalStreamOpener_CancelAbortsFlowControlParkedWriter(t *testing.T) {
	t.Parallel()
	srv := newHalfOpenServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opener := &terminalConnectOpener{client: srv.client, logger: zerolog.Nop()}
	stream := opener.TerminalStream(ctx)
	if err := stream.Send(nil); err != nil {
		t.Fatalf("flush headers: %v", err)
	}
	srv.waitOpened(t, bossanovav1connect.OrchestratorServiceTerminalStreamProcedure)
	if _, err := stream.Receive(); err != nil {
		t.Fatalf("first Receive: %v", err)
	}

	var recvErr atomic.Value
	recvDone := srv.receiveParked(t, func() error { _, err := stream.Receive(); return err }, &recvErr)

	// Push well past Go's default 1 MiB per-stream receive window. The sender
	// stalls once the window is spent: connect's Send blocks on the request
	// pipe because the HTTP/2 writer stopped reading it to wait for credit.
	const chunk = 256 << 10
	const chunks = 16
	var sent atomic.Int32
	sendDone := make(chan struct{})
	srv.joinOnCleanup(t, sendDone, "flow-control sender goroutine")
	go func() {
		defer close(sendDone)
		data := make([]byte, chunk)
		for range chunks {
			if err := stream.Send(&pb.TerminalServerMessage{Msg: &pb.TerminalServerMessage_Data{
				Data: &pb.TerminalDataChunk{AttachId: "flow", Data: data},
			}}); err != nil {
				return
			}
			sent.Add(1)
		}
	}()

	// Wait for the sender to stall short of the full payload.
	stalled := false
	deadline := time.Now().Add(halfOpenDeadline)
	for !stalled && time.Now().Before(deadline) {
		before := sent.Load()
		select {
		case <-sendDone:
			t.Fatalf("sender finished all %d chunks; the server granted flow-control credit, so the writer never parked", chunks)
		case <-time.After(200 * time.Millisecond):
		}
		stalled = sent.Load() == before
	}
	if !stalled {
		t.Fatalf("sender never stalled on flow control (sent %d chunks)", sent.Load())
	}

	cancel()
	if !returnsPromptly(t, recvDone, "Receive after cancelling a flow-control-parked stream") {
		return
	}
	returnsPromptly(t, sendDone, "Send after cancelling a flow-control-parked stream")
}

// TestTerminalStreamClient_CycleStreamRebindsHalfOpenStream covers R2: with
// the real opener against a half-open server, CycleStream must lead Run to
// open a fresh stream. The heartbeat watchdog and logout funnel through the
// same stream-context cancel, so the R1 tests plus the existing fake-based
// Run-loop tests cover them.
func TestTerminalStreamClient_CycleStreamRebindsHalfOpenStream(t *testing.T) {
	t.Parallel()
	srv := newHalfOpenServer(t)

	health := NewTerminalHealth()
	client := NewTerminalStreamClient(TerminalStreamClientConfig{
		Client:     srv.client,
		TmuxClient: tmux.NewClient(tmux.WithCommandFactory((&recordingCmdFactory{}).factory)),
		Chats:      &fakeChatLookup{},
		Logger:     zerolog.Nop(),
		Health:     health,
		// Long enough that neither the ready gate nor the heartbeat fires on
		// its own: CycleStream is the only cancel source in this test.
		ReadyTimeout: time.Minute,
		PingInterval: time.Minute,
	})

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	srv.joinOnCleanup(t, runDone, "TerminalStreamClient.Run")
	t.Cleanup(cancel) // LIFO: cancel Run before releasing and joining it.
	go func() {
		defer close(runDone)
		_ = client.Run(ctx)
	}()

	srv.waitOpened(t, bossanovav1connect.OrchestratorServiceTerminalStreamProcedure)
	waitForN(t, "first stream ready-confirmed", halfOpenDeadline, health.Healthy)
	// Let the reader consume the end-stream envelope and park in the discard.
	time.Sleep(150 * time.Millisecond)

	client.CycleStream()
	waitForN(t, "Run opens a second stream after CycleStream", halfOpenDeadline, func() bool {
		return srv.terminalOpens.Load() >= 2
	})
}

// TestTerminalStreamClient_BoundedTeardownClosesHalfOpenConn covers R3 against
// the real transport: with the opener's cancel hook suppressed (the test-only
// skipAbortOnCancel seam), the heartbeat watchdog's cancel leaves the reader
// parked on the half-open stream exactly as in the incident, and openStream's
// bounded teardown must recover by closing the connection it captured via
// GotConn — not by abandoning the reader.
func TestTerminalStreamClient_BoundedTeardownClosesHalfOpenConn(t *testing.T) {
	t.Parallel()
	srv := newHalfOpenServer(t)

	health := NewTerminalHealth()
	client := NewTerminalStreamClient(TerminalStreamClientConfig{
		Opener: &terminalConnectOpener{
			client:            srv.client,
			logger:            zerolog.Nop(),
			skipAbortOnCancel: true,
		},
		TmuxClient: tmux.NewClient(tmux.WithCommandFactory((&recordingCmdFactory{}).factory)),
		Chats:      &fakeChatLookup{},
		Logger:     zerolog.Nop(),
		Health:     health,
		// The server never pings, so the watchdog cancels the stream one
		// interval after TerminalReady; the bound's first stage is the same
		// interval.
		ReadyTimeout:      halfOpenDeadline,
		PingInterval:      100 * time.Millisecond,
		MissedBeatsBudget: 1,
	})

	errCh := make(chan error, 1)
	done := make(chan struct{})
	srv.joinOnCleanup(t, done, "openStream")
	go func() {
		defer close(done)
		errCh <- client.openStream(context.Background())
	}()

	srv.waitOpened(t, bossanovav1connect.OrchestratorServiceTerminalStreamProcedure)
	// Well inside terminalReaderAbandonGrace: only the conn close can have
	// released the reader this quickly.
	select {
	case <-errCh:
	case <-time.After(3 * time.Second):
		t.Fatal("openStream did not return after the watchdog cancelled a half-open stream with the cancel hook suppressed")
	}
	snap := health.Snapshot()
	if snap.ReaderConnCloses != 1 {
		t.Fatalf("ReaderConnCloses = %d, want 1 (the bound must have closed the captured connection)", snap.ReaderConnCloses)
	}
	if snap.ReadersAbandoned != 0 {
		t.Fatalf("ReadersAbandoned = %d, want 0 (closing the connection should release the reader)", snap.ReadersAbandoned)
	}
}
