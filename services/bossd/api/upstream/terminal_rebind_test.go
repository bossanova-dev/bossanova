package upstream

// Tests for the terminal-stream rebind-on-re-register defense (2026-07-11
// incident): bosso builds a fresh DaemonState on every DaemonStream
// registration, so a TerminalStream sender bound to the prior state is
// permanently stranded — the daemon keeps a healthy-looking bidi open while
// every web attach fails ErrNoTerminalSender. The daemon-side defense is to
// cycle the terminal stream whenever the control-plane DaemonStream
// completes a (re-)registration handshake, guaranteeing the terminal sender
// always rebinds to the current DaemonState.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"
)

// handshakeStream is a bidirectionalStream whose Send always succeeds (so
// the snapshot handshake completes) and whose Receive blocks until the test
// ends the stream or the opener's ctx cancels.
type handshakeStream struct {
	ctx   context.Context
	ended chan struct{}
	once  sync.Once
}

func (s *handshakeStream) Send(*pb.DaemonEvent) error { return nil }

func (s *handshakeStream) Receive() (*pb.OrchestratorCommand, error) {
	select {
	case <-s.ended:
		return nil, errors.New("stream ended by test")
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
}

func (s *handshakeStream) CloseRequest() error { return nil }

func (s *handshakeStream) end() {
	s.once.Do(func() { close(s.ended) })
}

// handshakeOpener hands out handshakeStreams and records them.
type handshakeOpener struct {
	mu      sync.Mutex
	streams []*handshakeStream
}

func (o *handshakeOpener) DaemonStream(ctx context.Context) bidirectionalStream {
	o.mu.Lock()
	defer o.mu.Unlock()
	s := &handshakeStream{ctx: ctx, ended: make(chan struct{})}
	o.streams = append(o.streams, s)
	return s
}

func (o *handshakeOpener) latest() *handshakeStream {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.streams) == 0 {
		return nil
	}
	return o.streams[len(o.streams)-1]
}

// TestStreamClient_OnHandshakeFiresPerSuccessfulHandshake asserts the
// OnHandshake hook fires once per completed snapshot handshake — i.e. once
// per registration bosso observes — including reconnects. The production
// wiring points this hook at TerminalStreamClient.CycleStream so the
// terminal sender rebinds to the DaemonState each registration creates.
func TestStreamClient_OnHandshakeFiresPerSuccessfulHandshake(t *testing.T) {
	t.Parallel()

	opener := &handshakeOpener{}
	hookCh := make(chan struct{}, 8)
	c := NewStreamClient(StreamClientConfig{
		Opener: opener,
		Events: NewStreamBus(zerolog.Nop()),
		Logger: zerolog.Nop(),
		OnHandshake: func() {
			select {
			case hookCh <- struct{}{}:
			default:
			}
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()

	// First handshake completes → hook fires.
	select {
	case <-hookCh:
	case <-time.After(2 * time.Second):
		t.Fatal("OnHandshake did not fire after first handshake")
	}

	// End the stream, then force an immediate reconnect (skipping the
	// backoff sleep). The second handshake must fire the hook again.
	opener.latest().end()
	c.Reconnect()
	select {
	case <-hookCh:
	case <-time.After(2 * time.Second):
		t.Fatal("OnHandshake did not fire after reconnect handshake")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// TestTerminalStreamClient_CycleStreamReconnects asserts CycleStream
// abandons the current healthy TerminalStream attempt and reconnects
// immediately (no backoff ramp) — the fake stream never errors, so the
// only thing that can cause a second open is the cycle.
func TestTerminalStreamClient_CycleStreamReconnects(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)

	stop := runClient(t, h.client)
	defer stop()

	// Wait for the first open.
	deadline := time.After(2 * time.Second)
	for h.opener.calls() == 0 {
		select {
		case <-deadline:
			t.Fatal("first terminal stream open never happened")
		case <-time.After(10 * time.Millisecond):
		}
	}
	first := h.opener.calls()

	h.client.CycleStream()

	// A fresh open must follow promptly (initial backoff is 1s; the cycle
	// path must not ramp it).
	deadline = time.After(3 * time.Second)
	for h.opener.calls() <= first {
		select {
		case <-deadline:
			t.Fatalf("no reconnect after CycleStream; opens still %d", h.opener.calls())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestTerminalStreamClient_CycleStreamNilReceiverIsNoop mirrors the
// StreamClient.Reconnect nil-safety contract so production wiring never
// has to nil-check before signalling.
func TestTerminalStreamClient_CycleStreamNilReceiverIsNoop(t *testing.T) {
	t.Parallel()

	var c *TerminalStreamClient
	c.CycleStream() // must not panic
}
