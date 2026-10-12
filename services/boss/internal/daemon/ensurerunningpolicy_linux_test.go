//go:build linux

package daemon

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// BOS-1519 on the systemd path: with a unit installed, the refusing policy
// returns ErrServiceInstalledNotServing before resolving or spawning bossd;
// with no unit, both policies spawn as before.

func TestEnsureRunningWithPolicyRefusesAnUnservedSystemdUnit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Installed && !Running without asking systemd.
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
	writeBossdUnitFile(t, home)
	// No bossd anywhere: a resolve would fail with a different error, so the
	// sentinel proves the refusal fired before ResolveBossdPath.
	t.Setenv("PATH", t.TempDir())
	original := executablePath
	t.Cleanup(func() { executablePath = original })
	executablePath = func() (string, error) { return filepath.Join(t.TempDir(), "boss"), nil }

	shrinkServiceManagerStartWait(t, 20*time.Millisecond)
	stubSocketReachableAfter(t, 1<<30)
	var calls [][]string
	originalRunSystemctl := runSystemctl
	t.Cleanup(func() { runSystemctl = originalRunSystemctl })
	runSystemctl = func(args ...string) ([]byte, error) {
		calls = append(calls, args)
		return nil, nil
	}

	mode, err := EnsureRunningWithPolicy(filepath.Join(home, "bossd.sock"), RefuseDetachedFallbackWhenServiceInstalled)
	if !errors.Is(err, ErrServiceInstalledNotServing) {
		t.Fatalf("err = %v, want ErrServiceInstalledNotServing", err)
	}
	if mode != StartModeUnknown {
		t.Errorf("StartMode = %v, want %v", mode, StartModeUnknown)
	}
	if len(calls) != 1 || len(calls[0]) < 2 || calls[0][1] != "start" {
		t.Errorf("systemctl calls = %v, want exactly one `start`", calls)
	}
}

func TestEnsureRunningWithPolicySpawnsWhenNoUnitIsInstalled(t *testing.T) {
	for _, policy := range []DetachedFallbackPolicy{AllowDetachedFallback, RefuseDetachedFallbackWhenServiceInstalled} {
		t.Run(policy.String(), func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
			writeFakeBossd(t)
			// Three failures: EnsureRunningWithPolicy's probe, the platform
			// re-probe, and the final pre-spawn guard.
			stubSocketReachableAfter(t, 3)
			originalRunSystemctl := runSystemctl
			t.Cleanup(func() { runSystemctl = originalRunSystemctl })
			runSystemctl = func(args ...string) ([]byte, error) {
				t.Fatalf("systemctl invoked with no unit installed: %v", args)
				return nil, nil
			}

			mode, err := EnsureRunningWithPolicy(filepath.Join(home, "bossd.sock"), policy)
			if err != nil {
				t.Fatalf("EnsureRunningWithPolicy: %v", err)
			}
			if mode != StartModeDetached {
				t.Errorf("StartMode = %v, want %v", mode, StartModeDetached)
			}
		})
	}
}
