package relay

import (
	"sync"
	"testing"
)

func TestPool_RegisterAndGet(t *testing.T) {
	p := NewPool()

	// Get returns nil for unregistered daemon.
	if got := p.Get("d1"); got != nil {
		t.Errorf("Get(unregistered) = %v, want nil", got)
	}

	// Register and retrieve.
	p.Register("d1", "http://localhost:9001")
	if got := p.Get("d1"); got == nil {
		t.Fatal("Get(d1) = nil after Register")
	}
}

func TestPool_Remove(t *testing.T) {
	p := NewPool()
	p.Register("d1", "http://localhost:9001")

	p.Remove("d1")
	if got := p.Get("d1"); got != nil {
		t.Errorf("Get(d1) after Remove = %v, want nil", got)
	}
}

func TestPool_RegisterReplaces(t *testing.T) {
	p := NewPool()
	p.Register("d1", "http://localhost:9001")
	first := p.Get("d1")

	p.Register("d1", "http://localhost:9002")
	second := p.Get("d1")

	// The client should be replaced (different instance).
	if first == second {
		t.Error("Register should replace existing client")
	}
}

func TestPool_ConcurrentAccess(t *testing.T) {
	p := NewPool()
	var wg sync.WaitGroup

	// Concurrent registrations.
	for i := range 100 {
		wg.Go(func() {
			id := "d" + string(rune('0'+i%10))
			p.Register(id, "http://localhost:9000")
			p.Get(id)
		})
	}
	wg.Wait()

	// Concurrent reads.
	for range 100 {
		wg.Go(func() {
			p.Get("d0")
		})
	}
	wg.Wait()

	// Concurrent removes.
	for i := range 10 {
		wg.Go(func() {
			id := "d" + string(rune('0'+i))
			p.Remove(id)
		})
	}
	wg.Wait()
}

func TestPool_SharedHTTPClient(t *testing.T) {
	p := NewPool()
	if p.httpClient == nil {
		t.Fatal("NewPool().httpClient should not be nil")
	}
}
