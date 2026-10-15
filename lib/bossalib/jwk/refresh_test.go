package jwk

import (
	"context"
	"crypto/rsa"
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
			errs <- gate.refreshUnknown(context.Background(), "forged-kid", fetch)
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

	if err := gate.refreshUnknown(context.Background(), "kid-a", fetch); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	clock.Advance(DefaultUnknownKIDInterval - time.Second)
	if err := gate.refreshUnknown(context.Background(), "kid-b", fetch); !errors.Is(err, ErrRefreshThrottled) {
		t.Fatalf("second refresh err = %v, want ErrRefreshThrottled", err)
	}
	if fetches != 1 {
		t.Fatalf("fetches = %d, want 1", fetches)
	}

	clock.Advance(2 * time.Second)
	// Throttled attempts are not negatively cached (the interval already
	// bounds them), so kid-b fetches once the interval has passed...
	if err := gate.refreshUnknown(context.Background(), "kid-b", fetch); err != nil {
		t.Fatalf("refresh after interval: %v", err)
	}
	// ...and that fetch restarts the interval for every other unknown kid.
	if err := gate.refreshUnknown(context.Background(), "kid-c", fetch); !errors.Is(err, ErrRefreshThrottled) {
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

	if err := gate.refreshUnknown(context.Background(), "kid-a", fetch); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	gate.markUnknown("kid-a")
	clock.Advance(DefaultUnknownKIDInterval - time.Second)
	if err := gate.refreshUnknown(context.Background(), "kid-a", fetch); !errors.Is(err, ErrRefreshThrottled) {
		t.Fatalf("err = %v, want ErrRefreshThrottled", err)
	}
	clock.Advance(time.Second)
	if err := gate.refreshUnknown(context.Background(), "kid-a", fetch); err != nil {
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

	if err := gate.refresh(context.Background(), fetch); !errors.Is(err, boom) {
		t.Fatalf("first refresh err = %v, want %v", err, boom)
	}
	if err := gate.refresh(context.Background(), fetch); !errors.Is(err, ErrRefreshThrottled) {
		t.Fatalf("refresh inside backoff err = %v, want ErrRefreshThrottled", err)
	}
	if fetches != 1 {
		t.Fatalf("fetches = %d, want 1", fetches)
	}
	clock.Advance(DefaultFailureBackoff)
	fetch = func(context.Context) error { fetches++; return nil }
	if err := gate.refresh(context.Background(), fetch); err != nil {
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

	if err := gate.refreshUnknown(context.Background(), "kid-a", fetch); err != nil {
		t.Fatalf("unknown refresh: %v", err)
	}
	if err := gate.refresh(context.Background(), fetch); err != nil {
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
		leaderDone <- gate.refresh(context.Background(), func(context.Context) error {
			close(inFetch)
			<-release
			return nil
		})
	}()
	<-inFetch

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := gate.refresh(ctx, func(context.Context) error {
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
		_ = gate.refresh(context.Background(), func(context.Context) error {
			close(inFetch)
			<-release
			panic("fetch exploded")
		})
	}()
	<-inFetch
	go func() {
		waiterErr <- gate.refresh(context.Background(), func(context.Context) error { return nil })
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
	if err := gate.refreshUnknown(context.Background(), "first", fetch); err != nil {
		t.Fatalf("first RefreshUnknown: %v", err)
	}
	// Throttled lookups inside the interval must not grow the negative cache.
	for i := range 5000 {
		_ = gate.refreshUnknown(context.Background(), fmt.Sprintf("forged-%d", i), fetch)
	}
	if n := len(gate.unknown); n != 0 {
		t.Fatalf("throttled lookups grew negative cache to %d entries", n)
	}
	for i := range 5000 {
		gate.markUnknown(fmt.Sprintf("marked-%d", i))
	}
	if n := len(gate.unknown); n > maxUnknownKIDs {
		t.Fatalf("negative cache holds %d entries, want <= %d", n, maxUnknownKIDs)
	}
}

// Legacy gate assertions exercise the internal scheduling paths directly.
func (g *RefreshGate) refresh(ctx context.Context, fetch func(context.Context) error) error {
	g.mu.Lock()
	return g.refreshLocked(ctx, "", true, fetch)
}
func (g *RefreshGate) refreshUnknown(ctx context.Context, kid string, fetch func(context.Context) error) error {
	g.mu.Lock()
	return g.refreshLocked(ctx, kid, false, fetch)
}

func TestResolveKey_TTLBoundaryRechecksCache(t *testing.T) {
	testResolveKeyBoundary(t, true)
}

func TestResolveKey_UnknownKIDBoundaryRechecksCache(t *testing.T) {
	testResolveKeyBoundary(t, false)
}

func testResolveKeyBoundary(t *testing.T, expired bool) {
	t.Helper()
	gate, _ := newTestGate()
	key := &rsa.PublicKey{E: 65537}
	cache := KeyLookup{Expired: expired}
	if expired {
		cache.Key = key
	}
	fetches := 0
	fetch := func(context.Context) error { //nolint:unparam // must match the gate's fetch signature
		fetches++
		cache = KeyLookup{Key: key}
		return nil
	}
	lookup := func(string) KeyLookup { return cache }
	first := true
	boundaryLookup := func(kid string) KeyLookup {
		snapshot := lookup(kid)
		if first {
			first = false
			// Caller A finishes a TTL refresh after B captures its initial view.
			cache.Expired = true
			if _, err := gate.ResolveKey(context.Background(), kid, lookup, fetch); err != nil {
				t.Fatalf("caller A: %v", err)
			}
		}
		return snapshot
	}
	got, err := gate.ResolveKey(context.Background(), "kid", boundaryLookup, fetch)
	if err != nil || got != key {
		t.Fatalf("caller B = (%v, %v), want key", got, err)
	}
	if fetches != 1 {
		t.Fatalf("fetches = %d, want 1", fetches)
	}
	if len(gate.unknown) != 0 {
		t.Fatalf("negative cache = %v, want empty", gate.unknown)
	}
}

func TestResolveKey_Protocol(t *testing.T) {
	key := &rsa.PublicKey{E: 65537}
	boom := errors.New("jwks down")
	tests := []struct {
		name        string
		initial     KeyLookup
		after       KeyLookup
		fetchErr    error
		interval    bool
		backoff     bool
		wantKey     *rsa.PublicKey
		wantErr     string
		wantCause   error
		wantFetches int
		wantUnknown bool
	}{
		{name: "fresh hit", initial: KeyLookup{Key: key}, wantKey: key},
		{name: "expired fetch succeeds", initial: KeyLookup{Expired: true}, after: KeyLookup{Key: key}, wantKey: key, wantFetches: 1},
		{name: "expired fetch fails with stale key", initial: KeyLookup{Key: key, Expired: true}, fetchErr: boom, wantKey: key, wantFetches: 1},
		{name: "expired fetch fails without key", initial: KeyLookup{Expired: true}, fetchErr: boom, wantErr: "fetch jwks: jwks down", wantCause: boom, wantFetches: 1},
		{name: "failure backoff serves stale key", initial: KeyLookup{Key: key, Expired: true}, backoff: true, wantKey: key},
		{name: "unknown interval throttle", interval: true, wantErr: `unknown kid "kid"`},
		{name: "unknown fetch stays missing", wantErr: `unknown kid "kid"`, wantFetches: 1, wantUnknown: true},
		{name: "published rejected", after: KeyLookup{Rejected: "weak modulus"}, wantErr: `signing key "kid" was published but rejected: weak modulus`, wantFetches: 1, wantUnknown: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gate, clock := newTestGate()
			if tt.interval {
				gate.lastUnknownFetch = clock.Now()
			}
			if tt.backoff {
				gate.lastFailure = clock.Now()
			}
			cache := tt.initial
			fetches := 0
			fetch := func(context.Context) error {
				fetches++
				if tt.fetchErr == nil {
					cache = tt.after
				}
				return tt.fetchErr
			}
			lookup := func(string) KeyLookup { return cache }
			got, err := gate.ResolveKey(context.Background(), "kid", lookup, fetch)
			if got != tt.wantKey {
				t.Fatalf("key = %v, want %v", got, tt.wantKey)
			}
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("error = %v", err)
				}
			} else if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
			if tt.wantCause != nil && !errors.Is(err, tt.wantCause) {
				t.Fatalf("error %v does not wrap %v", err, tt.wantCause)
			}
			if fetches != tt.wantFetches {
				t.Fatalf("fetches = %d, want %d", fetches, tt.wantFetches)
			}
			_, marked := gate.unknown["kid"]
			if marked != tt.wantUnknown {
				t.Fatalf("negative cache = %v, want marked %t", gate.unknown, tt.wantUnknown)
			}
			if tt.wantUnknown {
				_, _ = gate.ResolveKey(context.Background(), "kid", lookup, fetch)
				if fetches != tt.wantFetches {
					t.Fatalf("negative cache refetched: %d", fetches)
				}
			}
		})
	}
}
