package pty

import (
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestManagerConcurrentGetGetOrStartCleanup drives Get, GetOrStart, and
// Cleanup from many goroutines against overlapping IDs. It exists to lock
// in the process-map invariants against future refactors: the whole map
// access path runs under m.mu, so a check-then-delete sequence cannot race
// with another goroutine observing the about-to-be-deleted entry.
//
// Run under -race; failure is a race report, a panic, or a stale entry.
func TestManagerConcurrentGetGetOrStartCleanup(t *testing.T) {
	m := NewManager()

	const (
		goroutines = 50
		iterations = 20
		idPoolSize = 4
	)

	ids := make([]string, idPoolSize)
	for i := range ids {
		ids[i] = "id-" + strconv.Itoa(i)
	}

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(gi int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				id := ids[(gi+i)%idPoolSize]
				// Rotate through the three map-touching entry points.
				switch i % 3 {
				case 0:
					// Short-lived shell command so the process exits quickly
					// and later iterations hit the "done" branch.
					cmd := exec.Command("sh", "-c", "exit 0")
					p, err := m.GetOrStart(id, cmd)
					if err == nil && p != nil {
						select {
						case <-p.Done():
						case <-time.After(500 * time.Millisecond):
						}
					}
				case 1:
					_, _ = m.Get(id)
				case 2:
					m.Cleanup(id)
				}
			}
		}(g)
	}
	wg.Wait()

	// Final cleanup: every process that's still in the map is reaped to avoid
	// leaking fds into subsequent tests.
	for _, id := range ids {
		if p, ok := m.Get(id); ok {
			select {
			case <-p.Done():
			case <-time.After(500 * time.Millisecond):
			}
			m.Cleanup(id)
		}
	}
}

// TestManagerGetDeletesExitedEntry confirms that Get removes a dead process
// from the map so that a subsequent GetOrStart starts a fresh one rather
// than returning the corpse.
func TestManagerGetDeletesExitedEntry(t *testing.T) {
	m := NewManager()
	const id = "id-exit"

	cmd := exec.Command("sh", "-c", "exit 0")
	p, err := m.GetOrStart(id, cmd)
	if err != nil {
		t.Fatalf("GetOrStart: %v", err)
	}

	select {
	case <-p.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("process did not exit in time")
	}

	// Post-exit Get should report not-found and scrub the entry.
	if _, ok := m.Get(id); ok {
		t.Fatal("Get returned an exited process")
	}

	// Re-start at the same ID should succeed with a brand-new Process.
	cmd2 := exec.Command("sh", "-c", "exit 0")
	p2, err := m.GetOrStart(id, cmd2)
	if err != nil {
		t.Fatalf("second GetOrStart: %v", err)
	}
	if p2 == p {
		t.Fatal("GetOrStart returned the old, exited process instance")
	}
	select {
	case <-p2.Done():
	case <-time.After(2 * time.Second):
	}
	m.Cleanup(id)
}
