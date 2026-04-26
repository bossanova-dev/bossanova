package pubsub_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/recurser/bossalib/pubsub"
)

// recv pulls one value from ch with a timeout so a hung test fails fast
// rather than blocking the whole test binary.
func recv[T any](t *testing.T, ch <-chan T) (T, bool) {
	t.Helper()
	select {
	case v, ok := <-ch:
		return v, ok
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting on channel")
		var zero T
		return zero, false
	}
}

func TestSubscribe_SingleSubscriberReceives(t *testing.T) {
	t.Parallel()
	bus := pubsub.New[int]()
	defer bus.Close()

	sub, cancel := bus.Subscribe()
	defer cancel()

	bus.Publish(42)

	got, ok := recv(t, sub)
	if !ok {
		t.Fatal("channel unexpectedly closed")
	}
	if got != 42 {
		t.Fatalf("expected 42, got %d", got)
	}
}

func TestSubscribe_MultipleSubscribersAllReceive(t *testing.T) {
	t.Parallel()
	bus := pubsub.New[int]()
	defer bus.Close()

	const n = 5
	subs := make([]<-chan int, n)
	cancels := make([]func(), n)
	for i := range n {
		subs[i], cancels[i] = bus.Subscribe()
	}
	defer func() {
		for _, c := range cancels {
			c()
		}
	}()

	bus.Publish(7)
	bus.Publish(8)

	for i, s := range subs {
		got1, _ := recv(t, s)
		got2, _ := recv(t, s)
		if got1 != 7 || got2 != 8 {
			t.Fatalf("subscriber %d got (%d, %d), want (7, 8)", i, got1, got2)
		}
	}
}

func TestCancel_RemovesSubscriber(t *testing.T) {
	t.Parallel()
	bus := pubsub.New[int]()
	defer bus.Close()

	keep, keepCancel := bus.Subscribe()
	defer keepCancel()

	drop, dropCancel := bus.Subscribe()

	bus.Publish(1)
	if got, _ := recv(t, drop); got != 1 {
		t.Fatalf("dropped subscriber missed pre-cancel publish: got %d", got)
	}
	if got, _ := recv(t, keep); got != 1 {
		t.Fatalf("kept subscriber missed pre-cancel publish: got %d", got)
	}

	dropCancel()

	// Channel should now be closed (eventually — cancel is synchronous in
	// our impl, but assert with a small timeout to be safe).
	select {
	case _, ok := <-drop:
		if ok {
			t.Fatal("expected channel closed after cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for channel close after cancel")
	}

	// Publishes after cancel must not panic and must not reach the dropped
	// subscriber. The kept subscriber still receives.
	bus.Publish(2)
	if got, _ := recv(t, keep); got != 2 {
		t.Fatalf("kept subscriber missed post-cancel publish: got %d", got)
	}

	// Calling cancel again must be a no-op.
	dropCancel()
}

func TestClose_ClosesAllSubscribers(t *testing.T) {
	t.Parallel()
	bus := pubsub.New[int]()

	a, _ := bus.Subscribe()
	b, _ := bus.Subscribe()

	bus.Close()

	for i, s := range []<-chan int{a, b} {
		select {
		case _, ok := <-s:
			if ok {
				t.Fatalf("subscriber %d: expected closed channel", i)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("subscriber %d: timeout waiting for close", i)
		}
	}
}

func TestClose_Idempotent(t *testing.T) {
	t.Parallel()
	bus := pubsub.New[int]()
	bus.Close()
	bus.Close() // must not panic
	bus.Close()
}

func TestPublish_AfterClose_NoOp(t *testing.T) {
	t.Parallel()
	bus := pubsub.New[int]()
	bus.Close()
	// Must not panic.
	bus.Publish(1)
}

func TestSubscribe_AfterClose_ReturnsClosedChannel(t *testing.T) {
	t.Parallel()
	bus := pubsub.New[int]()
	bus.Close()

	ch, cancel := bus.Subscribe()
	defer cancel()

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected channel closed after subscribe-after-close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for closed channel")
	}

	// Cancel returned for closed-bus subscribe must be a safe no-op.
	cancel()
	cancel()
}

// TestPublish_DropsOldestOnOverflow validates the drop-oldest contract.
// With a non-draining subscriber, publishing more events than the buffer
// holds must drop the OLDEST (so the subscriber sees the most recent
// burst, never a frozen-at-time-of-overflow snapshot).
func TestPublish_DropsOldestOnOverflow(t *testing.T) {
	t.Parallel()
	bus := pubsub.New[int]()
	defer bus.Close()

	sub, cancel := bus.Subscribe()
	defer cancel()

	// The default buffer is 256. Publish 256 + 3 = 259 values without
	// draining: values 0..258. After overflow, the buffer should hold the
	// most-recent 256 values (3..258).
	const total = 259
	for i := range total {
		bus.Publish(i)
	}

	// Drain and verify the sequence is 3..258 in order.
	got := make([]int, 0, 256)
DRAIN:
	for {
		select {
		case v, ok := <-sub:
			if !ok {
				break DRAIN
			}
			got = append(got, v)
		case <-time.After(50 * time.Millisecond):
			break DRAIN
		}
	}

	if len(got) != 256 {
		t.Fatalf("expected 256 values after overflow, got %d", len(got))
	}
	if got[0] != total-256 {
		t.Fatalf("expected first value %d (oldest dropped), got %d", total-256, got[0])
	}
	if got[len(got)-1] != total-1 {
		t.Fatalf("expected last value %d (newest retained), got %d", total-1, got[len(got)-1])
	}
	// Verify monotonic.
	for i := 1; i < len(got); i++ {
		if got[i] != got[i-1]+1 {
			t.Fatalf("non-monotonic at %d: %d then %d", i, got[i-1], got[i])
		}
	}
}

// TestPublish_OnDropCalledOnOverflow validates the OnDrop hook fires
// exactly once for every dropped event when a subscriber's buffer is full.
// With a non-draining subscriber and 3 publishes past capacity, OnDrop
// should be invoked 3 times.
func TestPublish_OnDropCalledOnOverflow(t *testing.T) {
	t.Parallel()

	var drops atomic.Int64
	bus := pubsub.New[int](pubsub.WithOnDrop[int](func() {
		drops.Add(1)
	}))
	defer bus.Close()

	sub, cancel := bus.Subscribe()
	defer cancel()

	// Fill the buffer exactly (256 events) — no drops yet.
	for i := range 256 {
		bus.Publish(i)
	}
	if got := drops.Load(); got != 0 {
		t.Fatalf("expected 0 drops at exact capacity, got %d", got)
	}

	// Three more publishes — each must trigger OnDrop once.
	for i := 256; i < 259; i++ {
		bus.Publish(i)
	}
	if got := drops.Load(); got != 3 {
		t.Fatalf("expected 3 drops, got %d", got)
	}

	// Drain a few values and confirm the channel still works.
	<-sub
}

// TestPublish_OnDropNilIsNoOp ensures a bus constructed without WithOnDrop
// behaves identically to the pre-hook implementation.
func TestPublish_OnDropNilIsNoOp(t *testing.T) {
	t.Parallel()
	bus := pubsub.New[int]()
	defer bus.Close()

	_, cancel := bus.Subscribe()
	defer cancel()

	// Force overflow with a non-draining subscriber. Must not panic.
	for i := range 300 {
		bus.Publish(i)
	}
}

// TestPublish_NeverBlocks ensures Publish remains non-blocking even when
// every subscriber is full. A blocking Publish would deadlock the daemon's
// lifecycle hooks.
func TestPublish_NeverBlocks(t *testing.T) {
	t.Parallel()
	bus := pubsub.New[int]()
	defer bus.Close()

	// Three non-draining subscribers.
	for range 3 {
		_, _ = bus.Subscribe()
	}

	done := make(chan struct{})
	go func() {
		// Publish way more than the buffer can hold — must complete fast.
		for i := range 10_000 {
			bus.Publish(i)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked: 10k publishes did not complete in 5s")
	}
}

// TestConcurrent_PublishSubscribeCancel exercises the mutex paths under
// the race detector. The assertion is "no race / no panic / no deadlock";
// event ordering is intentionally unverified because subscribers come and
// go.
func TestConcurrent_PublishSubscribeCancel(t *testing.T) {
	t.Parallel()
	bus := pubsub.New[int]()
	defer bus.Close()

	const publishers = 4
	const subscribers = 8
	const perPublisher = 2_000

	var wg sync.WaitGroup

	// Publishers.
	for p := range publishers {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := range perPublisher {
				bus.Publish(base*perPublisher + i)
			}
		}(p)
	}

	// Subscribers that subscribe, drain a bit, then cancel.
	var totalReceived atomic.Int64
	for range subscribers {
		wg.Go(func() {
			ch, cancel := bus.Subscribe()
			deadline := time.After(500 * time.Millisecond)
			for {
				select {
				case _, ok := <-ch:
					if !ok {
						return
					}
					totalReceived.Add(1)
				case <-deadline:
					cancel()
					// Drain remaining without counting — cancel closes the
					// channel; the loop exits on !ok next iteration.
				}
			}
		})
	}

	wg.Wait()
	// We don't assert on totalReceived: with drop-oldest semantics and
	// overlapping subscribe/cancel, the count is inherently nondeterministic.
	// The test exists for the race detector and to verify no deadlock.
	_ = totalReceived.Load()
}

// TestConcurrent_CloseDuringPublish stresses the closed-bus race: Close
// and Publish racing must never panic. (A naive impl that closes channels
// without holding the publish mutex can hit "send on closed channel".)
func TestConcurrent_CloseDuringPublish(t *testing.T) {
	t.Parallel()
	for range 100 {
		bus := pubsub.New[int]()
		_, _ = bus.Subscribe()
		_, _ = bus.Subscribe()

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := range 1_000 {
				bus.Publish(i)
			}
		}()
		go func() {
			defer wg.Done()
			time.Sleep(time.Microsecond)
			bus.Close()
		}()
		wg.Wait()
	}
}
