package upstream

import (
	"context"
	"errors"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"
)

// alwaysFailStream is a bidirectionalStream whose Send fails immediately,
// so openStream returns before spinning up any forwarder goroutines. This
// keeps the reconnect test focused purely on the outer Run loop's
// backoff/wake behaviour.
type alwaysFailStream struct{}

func (alwaysFailStream) Send(*pb.DaemonEvent) error { return errors.New("send boom") }
func (alwaysFailStream) Receive() (*pb.OrchestratorCommand, error) {
	return nil, errors.New("recv boom")
}
func (alwaysFailStream) CloseRequest() error { return nil }

// countingOpener records every DaemonStream open and signals on openCh.
type countingOpener struct {
	openCh chan struct{}
}

func (o *countingOpener) DaemonStream(_ context.Context) bidirectionalStream {
	select {
	case o.openCh <- struct{}{}:
	default:
	}
	return alwaysFailStream{}
}

// TestStreamClient_ReconnectWakesRunLoopFromBackoff proves that Reconnect
// pulls the Run loop out of its backoff sleep and forces an immediate
// re-open, without the backoff timer ever firing. A fake clock guarantees
// determinism: its After() channel never fires unless the test advances
// it, so the only thing that can cause a second open is Reconnect().
func TestStreamClient_ReconnectWakesRunLoopFromBackoff(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	opener := &countingOpener{openCh: make(chan struct{}, 8)}
	c := NewStreamClient(StreamClientConfig{
		Opener: opener,
		Events: NewStreamBus(zerolog.Nop()),
		Clock:  clock,
		Logger: zerolog.Nop(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()

	// First open happens right away and fails.
	select {
	case <-opener.openCh:
	case <-time.After(2 * time.Second):
		t.Fatal("first stream open never happened")
	}

	// The loop is now parked in its backoff sleep on the fake clock, whose
	// After() will not fire until we advance it (we never do). Wait until
	// the timer is registered so we know the loop reached the select.
	waitForTimer(clock, 2*time.Second)

	// No second open should occur on its own.
	select {
	case <-opener.openCh:
		t.Fatal("stream re-opened without Reconnect or a clock advance")
	case <-time.After(100 * time.Millisecond):
	}

	// Reconnect must wake the loop and force an immediate re-open.
	c.Reconnect()
	select {
	case <-opener.openCh:
	case <-time.After(2 * time.Second):
		t.Fatal("Reconnect did not wake the Run loop out of backoff")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// TestStreamClient_ReconnectNilReceiverIsNoop guards the nil-safety
// contract: a nil *StreamClient must absorb Reconnect without panicking,
// so callers never have to nil-check before signalling.
func TestStreamClient_ReconnectNilReceiverIsNoop(t *testing.T) {
	t.Parallel()

	var c *StreamClient
	c.Reconnect() // must not panic
}

// TestStreamClient_ReconnectIsNonBlockingAndCoalesced proves repeated
// Reconnect calls never block, even when the Run loop is not draining the
// channel — the signal is coalesced into the single-slot buffer.
func TestStreamClient_ReconnectIsNonBlockingAndCoalesced(t *testing.T) {
	t.Parallel()

	c := NewStreamClient(StreamClientConfig{
		Opener: &countingOpener{openCh: make(chan struct{}, 1)},
		Events: NewStreamBus(zerolog.Nop()),
		Logger: zerolog.Nop(),
	})

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			c.Reconnect()
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Reconnect blocked instead of coalescing")
	}
}
