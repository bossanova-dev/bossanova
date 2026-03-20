package eventbus

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func testBus() *Bus {
	return New(zerolog.Nop())
}

func testEvent(source string) *bossanovav1.EventNotification {
	return &bossanovav1.EventNotification{Source: source}
}

func TestPublishSubscribe(t *testing.T) {
	bus := testBus()
	defer bus.Close()

	ch := bus.Subscribe(t.Context())

	event := testEvent("test-plugin")
	bus.Publish(event)

	select {
	case got := <-ch:
		if got.GetSource() != "test-plugin" {
			t.Errorf("expected source %q, got %q", "test-plugin", got.GetSource())
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestMultipleSubscribers(t *testing.T) {
	bus := testBus()
	defer bus.Close()

	ch1 := bus.Subscribe(t.Context())
	ch2 := bus.Subscribe(t.Context())

	event := testEvent("multi")
	bus.Publish(event)

	for i, ch := range []<-chan *bossanovav1.EventNotification{ch1, ch2} {
		select {
		case got := <-ch:
			if got.GetSource() != "multi" {
				t.Errorf("subscriber %d: expected source %q, got %q", i, "multi", got.GetSource())
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d: timed out waiting for event", i)
		}
	}
}

func TestContextCancelUnsubscribes(t *testing.T) {
	bus := testBus()
	defer bus.Close()

	ctx, cancel := context.WithCancel(context.Background())
	ch := bus.Subscribe(ctx)

	cancel()

	// Wait for the goroutine to unsubscribe and close the channel.
	deadline := time.After(time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				// Channel closed — success.
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for channel to close after context cancel")
		}
	}
}

func TestBufferOverflowDropsEvent(t *testing.T) {
	bus := New(zerolog.Nop())
	defer bus.Close()

	ch := bus.Subscribe(t.Context())

	// Fill the buffer.
	for i := range subscriberBufSize {
		bus.Publish(testEvent("fill-" + string(rune('0'+i%10))))
	}

	// This event should be dropped (buffer full).
	bus.Publish(testEvent("overflow"))

	// Drain the buffer — all should be the "fill" events.
	for range subscriberBufSize {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatal("timed out draining buffer")
		}
	}

	// No more events should be available.
	select {
	case ev := <-ch:
		t.Errorf("expected no more events, got source=%q", ev.GetSource())
	default:
		// OK — buffer was drained, overflow was dropped.
	}
}

func TestCloseIdempotent(t *testing.T) {
	bus := testBus()
	bus.Close()
	bus.Close() // should not panic
}

func TestCloseClosesSubscriberChannels(t *testing.T) {
	bus := testBus()

	ch := bus.Subscribe(t.Context())
	bus.Close()

	// Channel should be closed.
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("expected channel to be closed after bus.Close")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for channel close")
	}
}

func TestPublishAfterCloseIsNoop(t *testing.T) {
	bus := testBus()
	bus.Close()

	// Should not panic.
	bus.Publish(testEvent("after-close"))
}

func TestSubscribeAfterCloseReturnsClosedChannel(t *testing.T) {
	bus := testBus()
	bus.Close()

	ch := bus.Subscribe(context.Background())

	select {
	case _, ok := <-ch:
		if ok {
			t.Error("expected closed channel from subscribe after bus close")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out — channel should be immediately closed")
	}
}

func TestConcurrentPublishSubscribe(t *testing.T) {
	bus := testBus()
	defer bus.Close()

	const numSubscribers = 10
	const numEvents = 100

	var wg sync.WaitGroup
	counts := make([]int, numSubscribers)

	for i := range numSubscribers {
		ch := bus.Subscribe(t.Context())
		wg.Go(func() {
			for range ch {
				counts[i]++
			}
		})
	}

	// Publish events concurrently.
	var pubWg sync.WaitGroup
	for range numEvents {
		pubWg.Go(func() {
			bus.Publish(testEvent("concurrent"))
		})
	}
	pubWg.Wait()

	// Close the bus to close all subscriber channels.
	bus.Close()
	wg.Wait()

	for i, count := range counts {
		if count == 0 {
			t.Errorf("subscriber %d received 0 events", i)
		}
	}
}
