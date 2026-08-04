package tuidriver

import (
	"testing"
	"time"
)

// TestWaitClosedReportsAClosedChannel pins the happy path: an already-closed
// channel is observed immediately and reported as closed.
func TestWaitClosedReportsAClosedChannel(t *testing.T) {
	ch := make(chan struct{})
	close(ch)

	start := time.Now()
	if !waitClosed(ch, 5*time.Second) {
		t.Fatal("waitClosed reported false for a closed channel; want true")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("waitClosed on a closed channel took %s; want ~0", elapsed)
	}
}

// TestWaitClosedReportsAChannelClosedDuringTheWait covers the ordinary
// teardown shape: the loop exits shortly after Close starts waiting.
func TestWaitClosedReportsAChannelClosedDuringTheWait(t *testing.T) {
	ch := make(chan struct{})
	go func() {
		time.Sleep(10 * time.Millisecond)
		close(ch)
	}()

	if !waitClosed(ch, 5*time.Second) {
		t.Fatal("waitClosed reported false for a channel closed during the wait; want true")
	}
}

// TestWaitClosedGivesUpOnAChannelThatNeverCloses is the BOS-698 guard: a
// readLoop wedged in a pty read never closes d.done, and Close must not
// deadlock its caller waiting for it.
func TestWaitClosedGivesUpOnAChannelThatNeverCloses(t *testing.T) {
	ch := make(chan struct{}) // never closed

	start := time.Now()
	if waitClosed(ch, 20*time.Millisecond) {
		t.Fatal("waitClosed reported true for a channel that never closes; want false")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("waitClosed took %s; want it bounded near 20ms", elapsed)
	}
}
