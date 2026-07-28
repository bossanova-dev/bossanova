package questionsignal

import (
	"sync"
	"testing"
	"time"
)

func TestStore_SetGetClear(t *testing.T) {
	s := NewStore(time.Minute)

	if _, ok := s.Get("a"); ok {
		t.Fatalf("Get on empty store returned ok=true")
	}

	s.SetPending("a", "claude-notification")
	rec, ok := s.Get("a")
	if !ok {
		t.Fatalf("Get after SetPending returned ok=false")
	}
	if rec.Source != "claude-notification" {
		t.Errorf("Source = %q, want claude-notification", rec.Source)
	}
	if rec.At.IsZero() {
		t.Errorf("At is zero, want the set time")
	}

	s.Clear("a")
	if _, ok := s.Get("a"); ok {
		t.Fatalf("Get after Clear returned ok=true")
	}
}

func TestStore_KeyedIndependently(t *testing.T) {
	s := NewStore(time.Minute)
	s.SetPending("a", "src-a")
	s.SetPending("b", "src-b")

	s.Clear("a")
	if _, ok := s.Get("a"); ok {
		t.Errorf("a should be cleared")
	}
	if rec, ok := s.Get("b"); !ok || rec.Source != "src-b" {
		t.Errorf("b should still be pending with its own source, got %+v ok=%v", rec, ok)
	}
}

func TestStore_TTLExpiry(t *testing.T) {
	now := time.Unix(1000, 0)
	clock := &fakeClock{t: now}
	s := NewStoreWithClock(30*time.Second, clock.now)

	s.SetPending("a", "src")
	if _, ok := s.Get("a"); !ok {
		t.Fatalf("fresh record should be pending")
	}

	// Advance to exactly the TTL boundary — still fresh (age == ttl is not expired).
	clock.advance(30 * time.Second)
	if _, ok := s.Get("a"); !ok {
		t.Fatalf("record at exactly ttl boundary should still be pending")
	}

	// One tick past the TTL — aged out.
	clock.advance(time.Nanosecond)
	if _, ok := s.Get("a"); ok {
		t.Fatalf("record past ttl should have aged out")
	}
}

func TestStore_ZeroTTLFallsBackToDefault(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1000, 0)}
	s := NewStoreWithClock(0, clock.now)
	s.SetPending("a", "src")

	clock.advance(time.Nanosecond)
	if _, ok := s.Get("a"); !ok {
		t.Fatalf("record should remain pending when zero ttl falls back to the default")
	}
}

func TestStore_GetPurgesExpired(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	s := NewStoreWithClock(time.Second, clock.now)
	s.SetPending("a", "src")
	clock.advance(2 * time.Second)

	// First Get sees it expired (ok=false) and purges it.
	if _, ok := s.Get("a"); ok {
		t.Fatalf("expired record should read as not-pending")
	}
	if got := s.len(); got != 0 {
		t.Errorf("expired record should be purged on Get, len=%d", got)
	}
}

func TestStore_ConcurrentAccess(t *testing.T) {
	s := NewStore(time.Minute)
	const workers = 16
	const iters = 500
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			key := "k"
			for i := 0; i < iters; i++ {
				switch i % 3 {
				case 0:
					s.SetPending(key, "src")
				case 1:
					_, _ = s.Get(key)
				case 2:
					s.Clear(key)
				}
			}
		}()
	}
	wg.Wait()
}

// fakeClock is a monotonic test clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}
