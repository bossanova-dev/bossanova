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

// fakeRlimit is an in-memory RLIMIT_NOFILE used to drive raiseFileLimitWith
// deterministically. hardCeiling models the highest Max value a set is allowed
// to reach: a set that tries to raise Max above it fails with EPERM (an
// unprivileged process under a lowered hard cap); 0 means no ceiling
// (privileged / unlimited, so any hard raise succeeds).
type fakeRlimit struct {
	cur, max    uint64
	hardCeiling uint64
	getErr      error
}

func (f *fakeRlimit) get(_ int, lim *syscall.Rlimit) error {
	if f.getErr != nil {
		return f.getErr
	}
	lim.Cur = f.cur
	lim.Max = f.max
	return nil
}

func (f *fakeRlimit) set(_ int, lim *syscall.Rlimit) error {
	if f.hardCeiling != 0 && lim.Max > f.hardCeiling {
		return syscall.EPERM
	}
	f.cur = lim.Cur
	f.max = lim.Max
	return nil
}

func constMaxPerProc(v uint64, ok bool) func() (uint64, bool) {
	return func() (uint64, bool) { return v, ok }
}

// TestRaiseFileLimitWith exercises every branch of the pure core against a fake
// syscall backend: high/unlimited hard, a raisable finite hard, an EPERM hard
// (soft-only fallback), an already-high soft, the sysctl clamp path (both above
// and below the warn floor), and a Getrlimit error.
func TestRaiseFileLimitWith(t *testing.T) {
	tests := []struct {
		name       string
		fake       fakeRlimit
		maxPerProc func() (uint64, bool)
		wantSoft   uint64
		wantHardUp bool
		wantWarn   bool
	}{
		{
			name:       "low soft, unlimited hard",
			fake:       fakeRlimit{cur: 128, max: 0},
			maxPerProc: constMaxPerProc(0, false),
			wantSoft:   65536,
			wantHardUp: false,
			wantWarn:   false,
		},
		{
			name:       "finite hard raisable",
			fake:       fakeRlimit{cur: 128, max: 4096, hardCeiling: 0},
			maxPerProc: constMaxPerProc(0, false),
			wantSoft:   65536,
			wantHardUp: true,
			wantWarn:   false,
		},
		{
			name:       "finite hard EPERM, soft-only fallback below floor",
			fake:       fakeRlimit{cur: 128, max: 4096, hardCeiling: 4096},
			maxPerProc: constMaxPerProc(0, false),
			wantSoft:   4096,
			wantHardUp: false,
			wantWarn:   true,
		},
		{
			name:       "already-high soft unchanged",
			fake:       fakeRlimit{cur: 65536, max: 0},
			maxPerProc: constMaxPerProc(0, false),
			wantSoft:   65536,
			wantHardUp: false,
			wantWarn:   false,
		},
		{
			name:       "sysctl clamp above floor",
			fake:       fakeRlimit{cur: 128, max: 0},
			maxPerProc: constMaxPerProc(20000, true),
			wantSoft:   20000,
			wantHardUp: false,
			wantWarn:   false,
		},
		{
			name:       "sysctl clamp triggers floor warn",
			fake:       fakeRlimit{cur: 128, max: 0},
			maxPerProc: constMaxPerProc(4000, true),
			wantSoft:   4000,
			wantHardUp: false,
			wantWarn:   true,
		},
		{
			name:       "getrlimit error yields zero outcome",
			fake:       fakeRlimit{getErr: syscall.EPERM},
			maxPerProc: constMaxPerProc(0, false),
			wantSoft:   0,
			wantHardUp: false,
			wantWarn:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := tt.fake
			out := raiseFileLimitWith(f.get, f.set, tt.maxPerProc)
			if out.achievedSoft != tt.wantSoft {
				t.Errorf("achievedSoft = %d, want %d", out.achievedSoft, tt.wantSoft)
			}
			if out.hardRaised != tt.wantHardUp {
				t.Errorf("hardRaised = %v, want %v", out.hardRaised, tt.wantHardUp)
			}
			if out.warn != tt.wantWarn {
				t.Errorf("warn = %v, want %v", out.warn, tt.wantWarn)
			}
		})
	}
}
