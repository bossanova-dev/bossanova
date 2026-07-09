package tuitest_test

import (
	"context"
	"testing"
	"time"

	"github.com/recurser/boss/internal/client"
	"github.com/recurser/boss/internal/tuitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestSessionAttachedLifecycle asserts SessionAttached is false before any
// AttachSession stream opens, flips true while a real client stream is live, and
// returns to false after the stream closes. This is the attach-state signal the
// BOS-217 daemon-op pushers gate on.
//
// The connect server-streaming client blocks in AttachSession until the server
// flushes response headers, and the mock's handler blocks on its (empty) event
// channel without flushing — so the open call MUST run in its own goroutine. The
// server-side handler is still dispatched immediately (incrementing the active
// count at entry), so SessionAttached observes the live stream while the client
// call is parked. Cancelling the context unwinds both sides.
func TestSessionAttachedLifecycle(t *testing.T) {
	d := tuitest.NewMockDaemon(t)
	const id = "sess-attach-lifecycle"

	if d.SessionAttached(id) {
		t.Fatal("SessionAttached should be false before any attach")
	}

	c := client.NewLocal(d.SocketPath())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		stream, err := c.AttachSession(ctx, id)
		if err != nil {
			return
		}
		defer func() { _ = stream.Close() }()
		<-ctx.Done() // hold the stream open until the test cancels
	}()

	// The AttachSession RPC increments the active count at handler entry; poll
	// until the server goroutine has entered it.
	waitUntil(t, 3*time.Second, func() bool { return d.SessionAttached(id) },
		"SessionAttached did not become true while a stream was live")

	// Cancelling unwinds the RPC (client + server), decrementing the count.
	cancel()
	<-done
	waitUntil(t, 3*time.Second, func() bool { return !d.SessionAttached(id) },
		"SessionAttached did not return to false after the stream closed")
}

// TestTryPushTimesOutWhenChannelFull fills a session's 64-event attach buffer
// with no reader, then asserts a further TryPushStateChange returns an error
// within ~timeout rather than blocking forever — the property that keeps the
// bridge's single-threaded serve loop alive when the TUI stalls.
func TestTryPushTimesOutWhenChannelFull(t *testing.T) {
	d := tuitest.NewMockDaemon(t)
	const id = "sess-full"

	// The buffer is 64; nothing drains it because no AttachSession stream is open.
	for i := 0; i < 64; i++ {
		if err := d.TryPushStateChange(id,
			pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
			pb.SessionState_SESSION_STATE_READY_FOR_REVIEW,
			time.Second); err != nil {
			t.Fatalf("prefill send %d unexpectedly failed: %v", i, err)
		}
	}

	start := time.Now()
	err := d.TryPushStateChange(id,
		pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
		pb.SessionState_SESSION_STATE_READY_FOR_REVIEW,
		50*time.Millisecond)
	if err == nil {
		t.Fatal("TryPushStateChange should error when the buffer is full")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("TryPushStateChange blocked too long (%v); it must give up near the timeout", elapsed)
	}
}

// waitUntil polls cond until it returns true or the deadline elapses.
func waitUntil(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(msg)
}
