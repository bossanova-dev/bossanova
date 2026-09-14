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

	const waitBudget = 5 * time.Second
	start := time.Now()
	if !waitClosed(ch, waitBudget) {
		t.Fatal("waitClosed reported false for a closed channel; want true")
	}
	// A fifth of the budget handed to waitClosed, which is the 1s ceiling this assertion always
	// carried, now expressed as a derivation: an implementation that polled the whole budget out
	// instead of observing the closed channel immediately would exceed it by 5x.
	if elapsed := time.Since(start); elapsed > waitBudget/5 {
		t.Errorf("waitClosed on a closed channel took %s; want ~0, ceiling %s", elapsed, waitBudget/5)
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

	const waitBudget = 20 * time.Millisecond
	start := time.Now()
	if waitClosed(ch, waitBudget) {
		t.Fatal("waitClosed reported true for a channel that never closes; want false")
	}
	// 100x the budget: a waitClosed that never gave up would not return at all.
	if elapsed := time.Since(start); elapsed > 100*waitBudget {
		t.Errorf("waitClosed took %s; want it bounded near 20ms", elapsed)
	}
}
