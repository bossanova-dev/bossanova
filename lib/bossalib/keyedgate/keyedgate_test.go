package keyedgate

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestAcquireSerializesSameKey(t *testing.T) {
	t.Parallel()
	r := &Registry{Name: "test"}

	release, _, err := r.Acquire(context.Background(), "k", time.Second)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	// A second acquire for the same key must not succeed while the first holds
	// it — it should time out rather than return.
	_, waited, err := r.Acquire(context.Background(), "k", 50*time.Millisecond)
	if err == nil {
		t.Fatal("second acquire for a held key succeeded; the gate is not serializing")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second acquire error = %v, want DeadlineExceeded", err)
	}
	if waited < 50*time.Millisecond {
		t.Fatalf("waited %s, want >= the 50ms timeout", waited)
	}

	release()

	// Once released, the same key is immediately available again.
	release2, _, err := r.Acquire(context.Background(), "k", time.Second)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	release2()
}

func TestAcquireDistinctKeysDoNotContend(t *testing.T) {
	t.Parallel()
	r := &Registry{Name: "test"}

	releaseA, _, err := r.Acquire(context.Background(), "a", time.Second)
	if err != nil {
		t.Fatalf("acquire a: %v", err)
	}
	defer releaseA()

	// This is the property AC 1 rests on: one wedged key must not block another.
	releaseB, waited, err := r.Acquire(context.Background(), "b", time.Second)
	if err != nil {
		t.Fatalf("acquire b while a is held: %v", err)
	}
	defer releaseB()
	if waited > 100*time.Millisecond {
		t.Fatalf("acquiring a distinct key waited %s; keys are contending", waited)
	}
}

func TestAcquireRejectsAlreadyCancelledContext(t *testing.T) {
	t.Parallel()
	r := &Registry{Name: "test"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// The gate is free, so a naive select could pick the ready sem-send over the
	// ready ctx.Done. A caller that has given up must never take the gate.
	if _, _, err := r.Acquire(ctx, "k", time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("acquire with cancelled ctx = %v, want Canceled", err)
	}

	// And the gate must still be free afterwards.
	release, _, err := r.Acquire(context.Background(), "k", time.Second)
	if err != nil {
		t.Fatalf("acquire after a cancelled attempt: %v (the abandoned attempt leaked the gate)", err)
	}
	release()
}

func TestReleaseIsIdempotent(t *testing.T) {
	t.Parallel()
	r := &Registry{Name: "test"}

	release, _, err := r.Acquire(context.Background(), "k", time.Second)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// A second release must not block on the now-empty channel, and must not
	// drive refs negative — which would leave the entry in the map and hand the
	// next acquirer a second live gate for the same key.
	done := make(chan struct{})
	go func() {
		release()
		release()
		release()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("repeated release blocked; release is not idempotent")
	}

	if got := r.gateCount(); got != 0 {
		t.Fatalf("registry holds %d gates after release; entry was not reaped", got)
	}

	// Mutual exclusion must survive the extra releases.
	first, _, err := r.Acquire(context.Background(), "k", time.Second)
	if err != nil {
		t.Fatalf("acquire after repeated release: %v", err)
	}
	defer first()
	if _, _, err := r.Acquire(context.Background(), "k", 50*time.Millisecond); err == nil {
		t.Fatal("key acquired twice concurrently; repeated release destroyed mutual exclusion")
	}
}

func TestRegistryReapsEntriesAfterUse(t *testing.T) {
	t.Parallel()
	r := &Registry{Name: "test"}

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, _, err := r.Acquire(context.Background(), "shared", 5*time.Second)
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			release()
		}()
	}
	wg.Wait()

	if got := r.gateCount(); got != 0 {
		t.Fatalf("registry holds %d gates after all releases, want 0", got)
	}
}

func (r *Registry) gateCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.gates)
}
