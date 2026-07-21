//go:build unix

package main

import (
	"syscall"
	"testing"
)

// TestRaiseFileLimit verifies raiseFileLimit raises a low soft limit toward the
// 65536 target, never lowers an already-higher soft limit, and clamps to a
// finite hard cap. All sub-cases restore the original RLIMIT_NOFILE so the test
// is hermetic, and skip gracefully when the environment forbids the setrlimit
// the case depends on (e.g. a locked-down CI sandbox).
func TestRaiseFileLimit(t *testing.T) {
	const target = 65536

	var orig syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &orig); err != nil {
		t.Fatalf("Getrlimit: %v", err)
	}
	t.Cleanup(func() {
		restore := orig
		_ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &restore)
	})

	// expectedTarget is min(target, hardCap), treating a RLIM_INFINITY/0 max as
	// "no finite cap" so the raise clamps to the 65536 target.
	expectedTarget := uint64(target)
	if orig.Max != 0 && expectedTarget > orig.Max {
		expectedTarget = orig.Max
	}

	t.Run("raises low soft limit", func(t *testing.T) {
		low := orig
		low.Cur = 128
		if low.Max != 0 && low.Cur > low.Max {
			t.Skipf("hard cap %d below test soft value 128", low.Max)
		}
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &low); err != nil {
			t.Skipf("cannot lower soft limit to 128 in this environment: %v", err)
		}

		raiseFileLimit()

		var got syscall.Rlimit
		if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &got); err != nil {
			t.Fatalf("Getrlimit: %v", err)
		}
		if got.Cur != expectedTarget {
			t.Errorf("soft limit = %d, want %d", got.Cur, expectedTarget)
		}
		if got.Cur <= 128 {
			t.Errorf("soft limit %d was not raised above 128", got.Cur)
		}
	})

	t.Run("never lowers an already-higher soft limit", func(t *testing.T) {
		// Only meaningful when the target is achievable; set Cur to the target
		// (or Max) and confirm raiseFileLimit leaves it untouched.
		high := orig
		high.Cur = expectedTarget
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &high); err != nil {
			t.Skipf("cannot raise soft limit to %d in this environment: %v", expectedTarget, err)
		}

		raiseFileLimit()

		var got syscall.Rlimit
		if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &got); err != nil {
			t.Fatalf("Getrlimit: %v", err)
		}
		if got.Cur != expectedTarget {
			t.Errorf("soft limit = %d, want unchanged %d", got.Cur, expectedTarget)
		}
	})

	t.Run("clamps to a finite hard cap", func(t *testing.T) {
		// Lower the hard cap below the 65536 target so the raise must clamp to
		// it. Lowering Max is privileged-free but irreversible for this process,
		// so run it last (Cleanup restores Cur; Max stays lowered for the rest
		// of the process, which is fine — no later test raises above it).
		const cap = 4096
		if orig.Max != 0 && orig.Max < cap {
			t.Skipf("existing hard cap %d already below %d", orig.Max, cap)
		}
		capped := orig
		capped.Cur = 128
		capped.Max = cap
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &capped); err != nil {
			t.Skipf("cannot set finite hard cap in this environment: %v", err)
		}

		raiseFileLimit()

		var got syscall.Rlimit
		if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &got); err != nil {
			t.Fatalf("Getrlimit: %v", err)
		}
		if got.Cur != cap {
			t.Errorf("soft limit = %d, want clamped to hard cap %d", got.Cur, cap)
		}
	})
}
