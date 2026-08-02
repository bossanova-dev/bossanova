package termreset

import (
	"sync"
	"syscall"
	"testing"
	"time"
)

// noopSignal is the do-nothing seam a test installs when it does not care about
// that half of the re-raise.
func noopSignal(syscall.Signal) {}

// TestInstallHangupRescueRestoresDispositionThenCleansUpThenReraises delivers a
// real SIGHUP to the test process with both signal seams swapped out, and
// asserts the rescue restores SIGHUP's default disposition *first*, then runs
// the cleanup, then re-raises. The reset must come first: cleanup shells out to
// tmux with no timeout, and while signal.Notify is still registered a second
// hangup would be swallowed by this package's channel instead of killing boss.
// The seams are what make this testable without killing the test binary.
func TestInstallHangupRescueRestoresDispositionThenCleansUpThenReraises(t *testing.T) {
	var (
		mu          sync.Mutex
		events      []string
		restoredSig syscall.Signal
		raisedSig   syscall.Signal
	)
	raised := make(chan struct{})
	var raiseOnce sync.Once
	defer swapSignalSeams(
		func(sig syscall.Signal) {
			mu.Lock()
			events = append(events, "restore-default")
			restoredSig = sig
			mu.Unlock()
		},
		func(sig syscall.Signal) {
			mu.Lock()
			events = append(events, "raise")
			raisedSig = sig
			mu.Unlock()
			raiseOnce.Do(func() { close(raised) })
		},
	)()

	stop := InstallHangupRescue(func() {
		mu.Lock()
		events = append(events, "cleanup")
		mu.Unlock()
	})
	defer stop()

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("kill -HUP self: %v", err)
	}

	select {
	case <-raised:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the rescue to re-raise SIGHUP")
	}

	// A second hangup must not run the cleanup again. The handler goroutine has
	// already returned by now, so this pins the single-shot shape rather than the
	// sync.Once itself — but a handler that looped without the Once would fail
	// here.
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("second kill -HUP self: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	want := []string{"restore-default", "cleanup", "raise"}
	if len(events) != len(want) {
		t.Fatalf("got event order %v, want %v", events, want)
	}
	for i, w := range want {
		if events[i] != w {
			t.Fatalf("got event order %v, want %v", events, want)
		}
	}
	if restoredSig != syscall.SIGHUP {
		t.Errorf("restored the default disposition for %v, want SIGHUP", restoredSig)
	}
	if raisedSig != syscall.SIGHUP {
		t.Errorf("re-raised %v, want SIGHUP", raisedSig)
	}
}

// TestInstallHangupRescueStopIsIdempotent guards the deregistration func
// against the double-close panic a naive implementation would hit.
func TestInstallHangupRescueStopIsIdempotent(t *testing.T) {
	defer swapSignalSeams(noopSignal, noopSignal)()

	stop := InstallHangupRescue(func() {
		t.Error("cleanup ran without a SIGHUP")
	})
	stop()
	stop()
	stop()
}

// TestInstallHangupRescueReraisesAfterCleanupPanics pins the guard that keeps a
// panicking cleanup from stealing the re-raise. Without the recover the panic
// escapes a goroutine and aborts the whole test binary, so this cannot rot into
// a false pass: boss would die with the wrong status and dump a goroutine trace
// into the very terminal the rescue exists to clean up.
func TestInstallHangupRescueReraisesAfterCleanupPanics(t *testing.T) {
	raised := make(chan struct{})
	var once sync.Once
	defer swapSignalSeams(
		noopSignal,
		func(syscall.Signal) { once.Do(func() { close(raised) }) },
	)()

	stop := InstallHangupRescue(func() { panic("cleanup blew up") })
	defer stop()

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("kill -HUP self: %v", err)
	}
	select {
	case <-raised:
	case <-time.After(10 * time.Second):
		t.Fatal("a panicking cleanup swallowed the SIGHUP re-raise")
	}
}

// TestInstallHangupRescueToleratesNilCleanup keeps the handler from panicking
// in a signal goroutine if a caller wires it without a cleanup.
func TestInstallHangupRescueToleratesNilCleanup(t *testing.T) {
	raised := make(chan struct{})
	var once sync.Once
	defer swapSignalSeams(
		noopSignal,
		func(syscall.Signal) { once.Do(func() { close(raised) }) },
	)()

	stop := InstallHangupRescue(nil)
	defer stop()

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("kill -HUP self: %v", err)
	}
	select {
	case <-raised:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the rescue to re-raise SIGHUP")
	}
}
