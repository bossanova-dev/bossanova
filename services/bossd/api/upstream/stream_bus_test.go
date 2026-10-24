package upstream

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"
)

// makeEvent builds a StreamEvent populated enough to be distinguishable
// across publishes (kind is enough — we never inspect the inner Session).
func makeEvent(kind pb.SessionDelta_Kind) StreamEvent {
	return StreamEvent{Session: &SessionEvent{Kind: kind}}
}

func TestStreamBus_PublishToSubscriber(t *testing.T) {
	t.Parallel()
	bus := NewStreamBus(zerolog.Nop())
	defer bus.Close()

	ch := bus.Subscribe(t.Context())

	want := makeEvent(pb.SessionDelta_KIND_CREATED)
	bus.Publish(want)

	select {
	case got, ok := <-ch:
		if !ok {
			t.Fatal("channel unexpectedly closed")
		}
		if got.Session == nil || got.Session.Kind != want.Session.Kind {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for event")
	}
}

func TestStreamBus_CtxCancelClosesChannel(t *testing.T) {
	t.Parallel()
	bus := NewStreamBus(zerolog.Nop())
	defer bus.Close()

	// This test deliberately cancels the ctx mid-body to assert the
	// channel closes on cancel. t.Context() auto-cancels at test end and
	// can't be used here.
	ctx, cancel := context.WithCancel(context.Background())
	ch := bus.Subscribe(ctx)
	cancel()

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected channel closed after ctx cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for ctx-driven close")
	}
}

func TestStreamBus_CloseClosesAllSubscribers(t *testing.T) {
	t.Parallel()
	bus := NewStreamBus(zerolog.Nop())

	ctx := t.Context()
	a := bus.Subscribe(ctx)
	b := bus.Subscribe(ctx)

	bus.Close()

	for i, ch := range []<-chan StreamEvent{a, b} {
		select {
		case _, ok := <-ch:
			if ok {
				t.Fatalf("subscriber %d: expected closed channel", i)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("subscriber %d: timeout waiting for close", i)
		}
	}
}

func TestStreamBus_CloseIdempotent(t *testing.T) {
	t.Parallel()
	bus := NewStreamBus(zerolog.Nop())
	bus.Close()
	bus.Close() // must not panic
	// Publish after close must be a no-op.
	bus.Publish(makeEvent(pb.SessionDelta_KIND_DELETED))
}

func TestStreamBus_SubscribeAfterClose(t *testing.T) {
	t.Parallel()
	bus := NewStreamBus(zerolog.Nop())
	bus.Close()

	ch := bus.Subscribe(t.Context())
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected closed channel for subscribe-after-close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for closed channel")
	}
}

// TestStreamBus_DropOnFull verifies the wrapper preserves drop-oldest
// behaviour from the underlying pubsub.Bus. With a non-draining
// subscriber, publishing more than the buffer holds must drop the oldest
// events so the most recent burst is the one delivered.
func TestStreamBus_DropOnFull(t *testing.T) {
	t.Parallel()
	bus := NewStreamBus(zerolog.Nop())
	defer bus.Close()

	ch := bus.Subscribe(t.Context())

	// Publish more than the default 256 buffer. Use distinguishable kinds
	// by index so we can assert which were retained.
	const total = 300
	kinds := []pb.SessionDelta_Kind{
		pb.SessionDelta_KIND_CREATED,
		pb.SessionDelta_KIND_UPDATED,
		pb.SessionDelta_KIND_DELETED,
	}
	for i := range total {
		bus.Publish(StreamEvent{Session: &SessionEvent{Kind: kinds[i%len(kinds)]}})
	}

	// Drain.
	got := 0
DRAIN:
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				break DRAIN
			}
			got++
		case <-time.After(50 * time.Millisecond):
			break DRAIN
		}
	}

	// We expect exactly the buffer size (256) — the rest were dropped.
	if got != 256 {
		t.Fatalf("expected 256 events after overflow, got %d", got)
	}
}

// TestStreamBus_DropEmitsWarning verifies the OnDrop hook plumbed through
// from pubsub.Bus produces a structured-log warning. Without this, a slow
// subscriber would silently lose events and users would see UI gaps with
// no signal in the logs.
func TestStreamBus_DropEmitsWarning(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	bus := NewStreamBus(logger)
	defer bus.Close()

	// Non-draining subscriber so the buffer fills up.
	_ = bus.Subscribe(t.Context())

	// Publish past the buffer to force at least one drop.
	const total = 300
	for i := range total {
		bus.Publish(makeEvent(pb.SessionDelta_Kind(i % 3)))
	}

	out := buf.String()
	if !strings.Contains(out, "stream bus subscriber buffer full, dropping event") {
		t.Fatalf("expected drop warning in log output, got: %s", out)
	}
	if !strings.Contains(out, "\"component\":\"stream-bus\"") {
		t.Fatalf("expected component=stream-bus tag in log output, got: %s", out)
	}
	if !strings.Contains(out, "\"level\":\"warn\"") {
		t.Fatalf("expected warn level in log output, got: %s", out)
	}
}

// TestStreamBus_CloseUnblocksWatcher is a regression test for the
// watcher-goroutine leak: when callers pass a non-cancellable ctx (here,
// context.Background) and Close() is called, the bridge goroutines must
// exit promptly via the stop channel. Before the fix, those watchers
// leaked until the process exited.
//
// We exercise multiple subscribers so a missed close manifests as a
// stuck channel rather than a single timing fluke.
func TestStreamBus_CloseUnblocksWatcher(t *testing.T) {
	t.Parallel()

	bus := NewStreamBus(zerolog.Nop())

	const subs = 8
	chs := make([]<-chan StreamEvent, subs)
	for i := range subs {
		// All subscribers use background ctx — only Close() can release
		// their watcher goroutines.
		chs[i] = bus.Subscribe(context.Background())
	}

	bus.Close()

	deadline := time.After(1 * time.Second)
	var closed atomic.Int64
	for i, ch := range chs {
		select {
		case _, ok := <-ch:
			if ok {
				t.Fatalf("subscriber %d: expected closed channel after Close", i)
			}
			closed.Add(1)
		case <-deadline:
			t.Fatalf("subscriber %d: timeout waiting for Close to close channel", i)
		}
	}
	if got := closed.Load(); got != subs {
		t.Fatalf("expected %d subscriber channels closed, got %d", subs, got)
	}
}

// TestStreamBus_ConcurrentPublishSubscribe exercises the wrapper's
// goroutine-spawning Subscribe and the bridging ctx watcher under -race.
func TestStreamBus_ConcurrentPublishSubscribe(t *testing.T) {
	t.Parallel()
	bus := NewStreamBus(zerolog.Nop())
	defer bus.Close()

	const publishers = 4
	const subscribers = 8
	const perPublisher = 1_000

	var wg sync.WaitGroup
	for range publishers {
		wg.Go(func() {
			for i := range perPublisher {
				bus.Publish(makeEvent(pb.SessionDelta_Kind(i % 3)))
			}
		})
	}

	for range subscribers {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer cancel()
			ch := bus.Subscribe(ctx)
			for {
				select {
				case _, ok := <-ch:
					if !ok {
						return
					}
				case <-ctx.Done():
					return
				}
			}
		})
	}

	wg.Wait()
}

// TestStreamBus_PublishDoesNotBlock guards against a regression to a
// blocking publisher. Lifecycle hooks (display compute, session
// transitions) call Publish synchronously and must never deadlock.
func TestStreamBus_PublishDoesNotBlock(t *testing.T) {
	t.Parallel()
	bus := NewStreamBus(zerolog.Nop())
	defer bus.Close()

	// Three non-draining subscribers.
	for range 3 {
		_ = bus.Subscribe(t.Context())
	}

	done := make(chan struct{})
	go func() {
		for i := range 5_000 {
			bus.Publish(makeEvent(pb.SessionDelta_Kind(i % 3)))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked: 5k publishes did not complete in 5s")
	}
}
