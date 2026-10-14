package jwk

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a manually stepped clock for gate tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestGate() (*RefreshGate, *fakeClock) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	return NewRefreshGate(RefreshGateConfig{Now: clock.Now}), clock
}

func TestRefreshGate_ConcurrentUnknownKIDsShareOneFetch(t *testing.T) {
	gate, _ := newTestGate()
	var fetches atomic.Int32
	release := make(chan struct{})
	fetch := func(context.Context) error { //nolint:unparam // must match the gate's fetch signature
		fetches.Add(1)
		<-release
		return nil
	}

	const callers = 100
	var started, wg sync.WaitGroup
	started.Add(callers)
	wg.Add(callers)
	errs := make(chan error, callers)
	for range callers {
		go func() {
			defer wg.Done()
			started.Done()
			errs <- gate.RefreshUnknown(context.Background(), "forged-kid", fetch)
		}()
	}
	started.Wait()
	close(release)
	wg.Wait()
	close(errs)

	if got := fetches.Load(); got != 1 {
		t.Fatalf("fetches = %d, want 1", got)
	}
	for err := range errs {
		if err != nil && !errors.Is(err, ErrRefreshThrottled) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
}

func TestRefreshGate_SecondUnknownKIDInsideIntervalDoesNotFetch(t *testing.T) {
	gate, clock := newTestGate()
	var fetches int
	fetch := func(context.Context) error { fetches++; return nil }

	if err := gate.RefreshUnknown(context.Background(), "kid-a", fetch); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	clock.Advance(DefaultUnknownKIDInterval - time.Second)
	if err := gate.RefreshUnknown(context.Background(), "kid-b", fetch); !errors.Is(err, ErrRefreshThrottled) {
		t.Fatalf("second refresh err = %v, want ErrRefreshThrottled", err)
	}
	if fetches != 1 {
		t.Fatalf("fetches = %d, want 1", fetches)
	}

	clock.Advance(2 * time.Second)
	// Throttled attempts are not negatively cached (the interval already
	// bounds them), so kid-b fetches once the interval has passed...
	if err := gate.RefreshUnknown(context.Background(), "kid-b", fetch); err != nil {
		t.Fatalf("refresh after interval: %v", err)
	}
	// ...and that fetch restarts the interval for every other unknown kid.
	if err := gate.RefreshUnknown(context.Background(), "kid-c", fetch); !errors.Is(err, ErrRefreshThrottled) {
		t.Fatalf("kid-c inside new interval err = %v, want ErrRefreshThrottled", err)
	}
	if fetches != 2 {
		t.Fatalf("fetches = %d, want 2", fetches)
	}
}

func TestRefreshGate_MarkUnknownSuppressesRepeatKID(t *testing.T) {
	gate, clock := newTestGate()
	var fetches int
	fetch := func(context.Context) error { fetches++; return nil }

	if err := gate.RefreshUnknown(context.Background(), "kid-a", fetch); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	gate.MarkUnknown("kid-a")
	clock.Advance(DefaultUnknownKIDInterval - time.Second)
	if err := gate.RefreshUnknown(context.Background(), "kid-a", fetch); !errors.Is(err, ErrRefreshThrottled) {
		t.Fatalf("err = %v, want ErrRefreshThrottled", err)
	}
	clock.Advance(time.Second)
	if err := gate.RefreshUnknown(context.Background(), "kid-a", fetch); err != nil {
		t.Fatalf("refresh after negative entry expired: %v", err)
	}
	if fetches != 2 {
		t.Fatalf("fetches = %d, want 2", fetches)
	}
}

func TestRefreshGate_FailureBacksOffTTLRefresh(t *testing.T) {
	gate, clock := newTestGate()
	var fetches int
	boom := errors.New("jwks down")
	fetch := func(context.Context) error { fetches++; return boom }

	if err := gate.Refresh(context.Background(), fetch); !errors.Is(err, boom) {
		t.Fatalf("first refresh err = %v, want %v", err, boom)
	}
	if err := gate.Refresh(context.Background(), fetch); !errors.Is(err, ErrRefreshThrottled) {
		t.Fatalf("refresh inside backoff err = %v, want ErrRefreshThrottled", err)
	}
	if fetches != 1 {
		t.Fatalf("fetches = %d, want 1", fetches)
	}
	clock.Advance(DefaultFailureBackoff)
	fetch = func(context.Context) error { fetches++; return nil }
	if err := gate.Refresh(context.Background(), fetch); err != nil {
		t.Fatalf("refresh after backoff: %v", err)
	}
	if fetches != 2 {
		t.Fatalf("fetches = %d, want 2", fetches)
	}
}

func TestRefreshGate_TTLRefreshIsNotSpacedByUnknownInterval(t *testing.T) {
	gate, _ := newTestGate()
	var fetches int
	fetch := func(context.Context) error { fetches++; return nil }

	if err := gate.RefreshUnknown(context.Background(), "kid-a", fetch); err != nil {
		t.Fatalf("unknown refresh: %v", err)
	}
	if err := gate.Refresh(context.Background(), fetch); err != nil {
		t.Fatalf("ttl refresh: %v", err)
	}
	if fetches != 2 {
		t.Fatalf("fetches = %d, want 2", fetches)
	}
}

func TestRefreshGate_WaiterHonoursOwnContext(t *testing.T) {
	gate, _ := newTestGate()
	release := make(chan struct{})
	leaderDone := make(chan error, 1)
	inFetch := make(chan struct{})
	go func() {
		leaderDone <- gate.Refresh(context.Background(), func(context.Context) error {
			close(inFetch)
			<-release
			return nil
		})
	}()
	<-inFetch

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := gate.Refresh(ctx, func(context.Context) error {
		t.Error("waiter must not start a second fetch")
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter err = %v, want context.Canceled", err)
	}
	close(release)
	if err := <-leaderDone; err != nil {
		t.Fatalf("leader err = %v", err)
	}
}

func TestRefreshGate_PanicReleasesWaiters(t *testing.T) {
	gate, _ := newTestGate()
	inFetch := make(chan struct{})
	release := make(chan struct{})
	waiterErr := make(chan error, 1)

	go func() {
		defer func() { _ = recover() }()
		_ = gate.Refresh(context.Background(), func(context.Context) error {
			close(inFetch)
			<-release
			panic("fetch exploded")
		})
	}()
	<-inFetch
	go func() {
		waiterErr <- gate.Refresh(context.Background(), func(context.Context) error { return nil })
	}()
	// Give the waiter a chance to join the in-flight call before the panic.
	time.Sleep(10 * time.Millisecond)
	close(release)

	select {
	case err := <-waiterErr:
		// Either the waiter joined the panicked call, or it arrived after the
		// leader settled and was throttled by the failure backoff.
		if err == nil {
			t.Fatal("waiter err = nil, want a failure")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter was not released after the leader panicked")
	}
}

func TestRefreshGate_NegativeCacheIsBounded(t *testing.T) {
	gate := NewRefreshGate(RefreshGateConfig{})
	fetch := func(context.Context) error { return nil }
	if err := gate.RefreshUnknown(context.Background(), "first", fetch); err != nil {
		t.Fatalf("first RefreshUnknown: %v", err)
	}
	// Throttled lookups inside the interval must not grow the negative cache.
	for i := range 5000 {
		_ = gate.RefreshUnknown(context.Background(), fmt.Sprintf("forged-%d", i), fetch)
	}
	if n := len(gate.unknown); n != 0 {
		t.Fatalf("throttled lookups grew negative cache to %d entries", n)
	}
	for i := range 5000 {
		gate.MarkUnknown(fmt.Sprintf("marked-%d", i))
	}
	if n := len(gate.unknown); n > maxUnknownKIDs {
		t.Fatalf("negative cache holds %d entries, want <= %d", n, maxUnknownKIDs)
	}
}
