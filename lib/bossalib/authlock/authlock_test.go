package authlock

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireSerializesLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "refresh.lock")
	first, err := acquire(context.Background(), path, time.Millisecond)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer func() { _ = first.Unlock() }()

	acquired := make(chan struct{})
	errCh := make(chan error, 1)
	blocked := notifyBlockedAcquire(t)
	go func() {
		second, err := acquire(context.Background(), path, time.Millisecond)
		if err != nil {
			errCh <- err
			return
		}
		defer func() { _ = second.Unlock() }()
		close(acquired)
		errCh <- nil
	}()

	waitForBlockedAcquire(t, blocked)
	select {
	case <-acquired:
		t.Fatal("second acquire succeeded while first lock was held")
	default:
	}

	if err := first.Unlock(); err != nil {
		t.Fatalf("unlock first: %v", err)
	}

	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("second acquire did not proceed after unlock")
	}
	if err := <-errCh; err != nil {
		t.Fatalf("second acquire returned error: %v", err)
	}
}

func notifyBlockedAcquire(t *testing.T) <-chan struct{} {
	t.Helper()

	blocked := make(chan struct{})
	afterFailedTryLock = func() {
		select {
		case <-blocked:
		default:
			close(blocked)
		}
	}
	t.Cleanup(func() { afterFailedTryLock = nil })
	return blocked
}

func waitForBlockedAcquire(t *testing.T, blocked <-chan struct{}) {
	t.Helper()

	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("second acquire did not block on the held lock")
	}
}

// retryAfterRelease measures how long a second acquire takes to succeed after
// the first lock is released, given a poll interval supplied to acquire. The
// effective retry cadence is governed by the interval-defaulting branch in
// acquire (interval <= 0 -> pollInterval).
func retryAfterRelease(t *testing.T, interval time.Duration) time.Duration {
	t.Helper()
	path := filepath.Join(t.TempDir(), "refresh.lock")
	first, err := acquire(context.Background(), path, time.Millisecond)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	done := make(chan time.Duration, 1)
	errCh := make(chan error, 1)
	blocked := notifyBlockedAcquire(t)
	go func() {
		start := time.Now()
		second, err := acquire(context.Background(), path, interval)
		if err != nil {
			errCh <- err
			return
		}
		_ = second.Unlock()
		done <- time.Since(start)
	}()

	waitForBlockedAcquire(t, blocked)
	if err := first.Unlock(); err != nil {
		t.Fatalf("unlock first: %v", err)
	}

	select {
	case d := <-done:
		return d
	case err := <-errCh:
		t.Fatalf("second acquire: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("second acquire did not complete")
	}
	return 0
}

// TestAcquireIntervalDefaulting pins the boundary and both sides of the
// `interval <= 0` defaulting branch on authlock.go:31.
//
//   - interval == 0 and interval < 0 (boundary value) MUST default to the large
//     pollInterval, so the retry after release is slow.
//   - interval > 0 MUST be used verbatim (no defaulting), so a small positive
//     interval retries quickly even though pollInterval is large.
func TestAcquireIntervalDefaulting(t *testing.T) {
	const bigPoll = 400 * time.Millisecond
	prev := pollInterval
	pollInterval = bigPoll
	t.Cleanup(func() { pollInterval = prev })

	// Threshold safely between the fast (small positive interval) and slow
	// (defaulted to bigPoll) regimes.
	const threshold = 200 * time.Millisecond

	t.Run("zero defaults to pollInterval (slow)", func(t *testing.T) {
		// Kills boundary mutant (< 0): with `interval < 0`, interval==0 would
		// NOT default, leaving a 0-duration busy-retry that returns fast.
		// Kills negation mutant (> 0): with `interval > 0`, interval==0 would
		// NOT default, same fast busy-retry.
		got := retryAfterRelease(t, 0)
		if got < threshold {
			t.Fatalf("interval==0 retry took %v, want >= %v (should default to pollInterval=%v)", got, threshold, bigPoll)
		}
	})

	t.Run("negative defaults to pollInterval (slow)", func(t *testing.T) {
		// Kills negation mutant (> 0): with `interval > 0`, a negative interval
		// would NOT default, leaving a negative-duration busy-retry that returns
		// fast.
		got := retryAfterRelease(t, -time.Second)
		if got < threshold {
			t.Fatalf("negative interval retry took %v, want >= %v (should default to pollInterval=%v)", got, threshold, bigPoll)
		}
	})

	t.Run("positive interval is used verbatim (fast)", func(t *testing.T) {
		// Kills negation mutant (> 0): with `interval > 0`, this small positive
		// interval WOULD be overwritten by the large pollInterval, making the
		// retry slow.
		got := retryAfterRelease(t, time.Millisecond)
		if got >= threshold {
			t.Fatalf("positive interval retry took %v, want < %v (small interval must not default to pollInterval=%v)", got, threshold, bigPoll)
		}
	})
}

func TestAcquireHonorsContextCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "refresh.lock")
	first, err := acquire(context.Background(), path, time.Millisecond)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer func() { _ = first.Unlock() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	_, err = acquire(ctx, path, time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquire error = %v, want deadline exceeded", err)
	}
}
