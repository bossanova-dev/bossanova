package server

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/recurser/bossd/internal/session"
	"github.com/rs/zerolog"
)

// syncBuffer is a mutex-guarded log sink. The daemon logs from the RPC
// goroutine, the lifecycle goroutine and the background draft-PR step, so a bare
// bytes.Buffer would race.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestCreateSessionLogsBeforeAcquiringStartLocks is the AC-3 proof: lock waits
// are visible in the daemon log BEFORE they become hangs.
//
// The target lock is taken out from under the create, so the create is provably
// still waiting when the assertion runs — which is what makes this a statement
// about ordering rather than about the line merely existing. On pre-BOS-717 code
// the only lock signal was `start_lock_wait`, emitted after acquisition on the
// success path, so a create that waited forever logged nothing at all.
func TestCreateSessionLogsBeforeAcquiringStartLocks(t *testing.T) {
	t.Parallel()

	sink := &syncBuffer{}
	h := newCreateSessionStreamHarnessWithProvider(t,
		&setupStreamWorktree{}, &setupStreamAgent{}, setupStreamProvider{}, zerolog.New(sink))

	const branch = "bos-717-hygiene"
	release, _, err := session.AcquireTargetStart(context.Background(), h.repo.ID, branch, nil)
	if err != nil {
		t.Fatalf("pre-acquire target lock: %v", err)
	}
	released := false
	t.Cleanup(func() {
		if !released {
			release()
		}
	})

	done := make(chan error, 1)
	go func() {
		done <- h.createBranchSession(context.Background(), h.repo.ID, "blocked on lock", branch)
	}()

	// The pre-acquire line must appear while the create is still blocked.
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(sink.String(), "acquiring session start locks") {
		select {
		case err := <-done:
			t.Fatalf("create returned (%v) before the lock-wait line was logged; log:\n%s", err, sink.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no lock-wait line logged while the create was blocked; log:\n%s", sink.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Still blocked: the line really was emitted before acquisition, not after.
	select {
	case err := <-done:
		t.Fatalf("create completed while the target lock was held: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	release()
	released = true
	if err := <-done; err != nil {
		t.Fatalf("create after lock release = %v, want nil", err)
	}
}

// TestCreateSessionWarnsOnContendedStartLocks proves a slow-but-successful
// acquire is surfaced at Warn — the early warning for the wedge, distinct from
// the Info line that records the wait only once the create has finished.
func TestCreateSessionWarnsOnContendedStartLocks(t *testing.T) {
	// Not parallel: it lowers the package-level warn threshold.
	original := session.SlowStartLockWaitThreshold
	session.SlowStartLockWaitThreshold = 10 * time.Millisecond
	t.Cleanup(func() { session.SlowStartLockWaitThreshold = original })

	sink := &syncBuffer{}
	h := newCreateSessionStreamHarnessWithProvider(t,
		&setupStreamWorktree{}, &setupStreamAgent{}, setupStreamProvider{}, zerolog.New(sink))

	const branch = "bos-717-contended"
	release, _, err := session.AcquireTargetStart(context.Background(), h.repo.ID, branch, nil)
	if err != nil {
		t.Fatalf("pre-acquire target lock: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- h.createBranchSession(context.Background(), h.repo.ID, "contended", branch)
	}()

	time.Sleep(100 * time.Millisecond)
	release()

	if err := <-done; err != nil {
		t.Fatalf("create = %v, want nil", err)
	}
	if !strings.Contains(sink.String(), "session start locks were contended") {
		t.Fatalf("no contention warning after a %s wait; log:\n%s", 100*time.Millisecond, sink.String())
	}
}

// TestCreateSessionFailsFastOnAnAlreadyCancelledContext proves the bounded
// acquire refuses rather than blocking: a caller that has already given up never
// takes the lock.
func TestCreateSessionFailsFastOnAnAlreadyCancelledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := session.AcquireTargetStart(ctx, "repo-x", "branch-x", nil); err == nil {
		t.Fatal("AcquireTargetStart on a cancelled context returned nil error")
	}
	if _, _, err := session.AcquireStartPath(ctx, "repo-x"); err == nil {
		t.Fatal("AcquireStartPath on a cancelled context returned nil error")
	}
}
