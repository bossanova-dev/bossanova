//go:build darwin

package daemonlife

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// BOS-1519: `boss daemon start` must not quietly create the unsupervised
// shape when a service manager is installed for this profile. These tests pin
// the refusing policy on every launchd route that used to fall through to
// startDetachedBossd, and that the allow policy (every other caller) still
// spawns exactly as before.

// countDetachedSpawns replaces the detached-spawn seam with a recorder. When
// serves is non-nil the spawn "brings up" the socket by flipping it, so the
// allow policy's post-spawn wait returns at once.
func countDetachedSpawns(t *testing.T, serves *bool) *int {
	t.Helper()
	spawns := 0
	original := startDetachedBossd
	startDetachedBossd = func(string) error {
		spawns++
		if serves != nil {
			*serves = true
		}
		return nil
	}
	t.Cleanup(func() { startDetachedBossd = original })
	return &spawns
}

// TestEnsureRunningWithPolicyLaunchAgentLoadDoesNotServe is the 2026-10-11
// incident shape: the LaunchAgent is installed, `launchctl load` succeeds, and
// the socket never appears.
func TestEnsureRunningWithPolicyLaunchAgentLoadDoesNotServe(t *testing.T) {
	for _, tc := range []struct {
		name      string
		policy    DetachedFallbackPolicy
		wantMode  StartMode
		wantSpawn int
		wantRefus bool
	}{
		{name: "refuse", policy: RefuseDetachedFallbackWhenServiceInstalled, wantMode: StartModeUnknown, wantSpawn: 0, wantRefus: true},
		{name: "allow", policy: AllowDetachedFallback, wantMode: StartModeDetached, wantSpawn: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, socketPath := prepareEnsureRunningEnvironment(t)
			shrinkServiceManagerStartWait(t, 20*time.Millisecond)

			served := false
			stubDialReachableWhen(t, func() bool { return served })
			// load succeeds but serves nothing.
			calls := stubRestartLaunchctl(t, func([]string) ([]byte, error) { return nil, nil })
			spawns := countDetachedSpawns(t, &served)

			mode, err := EnsureRunningWithPolicy(socketPath, tc.policy)
			if tc.wantRefus {
				if !errors.Is(err, ErrServiceInstalledNotServing) {
					t.Fatalf("err = %v, want ErrServiceInstalledNotServing", err)
				}
			} else if err != nil {
				t.Fatalf("EnsureRunningWithPolicy: %v", err)
			}
			if mode != tc.wantMode {
				t.Errorf("StartMode = %v, want %v", mode, tc.wantMode)
			}
			if *spawns != tc.wantSpawn {
				t.Errorf("detached spawns = %d, want %d", *spawns, tc.wantSpawn)
			}
			// Vacuity guard: the LaunchAgent arm really was taken.
			if got := countLaunchctlVerb(*calls, "load"); got != 1 {
				t.Fatalf("`launchctl load` invocations = %d, want 1", got)
			}
		})
	}
}

// TestEnsureRunningWithPolicyRefusesARunningLaunchAgentWithNoSocket covers the
// st.Running shape: launchd reports a live PID for the job, so the load is
// skipped, and the socket is still not served.
func TestEnsureRunningWithPolicyRefusesARunningLaunchAgentWithNoSocket(t *testing.T) {
	_, _, socketPath := prepareEnsureRunningEnvironment(t)
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
	shrinkServiceManagerStartWait(t, 20*time.Millisecond)

	served := false
	stubDialReachableWhen(t, func() bool { return served })
	calls := stubRestartLaunchctl(t, func(args []string) ([]byte, error) {
		if args[0] == "list" {
			return []byte("{\n\t\"PID\" = 4242;\n\t\"Label\" = \"" + Label + "\";\n};\n"), nil
		}
		return nil, nil
	})
	spawns := countDetachedSpawns(t, &served)

	st, err := platformGetStatus()
	if err != nil || !st.Installed || !st.Running {
		t.Fatalf("fixture status = %+v (err %v), want Installed && Running", st, err)
	}

	mode, err := EnsureRunningWithPolicy(socketPath, RefuseDetachedFallbackWhenServiceInstalled)
	if !errors.Is(err, ErrServiceInstalledNotServing) {
		t.Fatalf("err = %v, want ErrServiceInstalledNotServing", err)
	}
	if mode != StartModeUnknown {
		t.Errorf("StartMode = %v, want %v", mode, StartModeUnknown)
	}
	if *spawns != 0 {
		t.Errorf("detached spawns = %d, want 0", *spawns)
	}
	if got := countLaunchctlVerb(*calls, "load"); got != 0 {
		t.Errorf("`launchctl load` invocations = %d, want 0 for a job launchd reports running", got)
	}
}

// TestEnsureRunningWithPolicyWaitsForARunningLaunchAgentStillBinding covers a
// job launchd reports running whose bossd binds its socket during the startup
// budget (a concurrent start, a supervised respawn): the refusing policy waits
// for it and reports the already-running daemon instead of refusing.
func TestEnsureRunningWithPolicyWaitsForARunningLaunchAgentStillBinding(t *testing.T) {
	_, _, socketPath := prepareEnsureRunningEnvironment(t)
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
	shrinkServiceManagerStartWait(t, 5*time.Second)

	bindsAt := time.Now().Add(150 * time.Millisecond)
	stubDialReachableWhen(t, func() bool { return time.Now().After(bindsAt) })
	calls := stubRestartLaunchctl(t, func(args []string) ([]byte, error) {
		if args[0] == "list" {
			return []byte("{\n\t\"PID\" = 4242;\n\t\"Label\" = \"" + Label + "\";\n};\n"), nil
		}
		return nil, nil
	})
	spawns := countDetachedSpawns(t, nil)

	mode, err := EnsureRunningWithPolicy(socketPath, RefuseDetachedFallbackWhenServiceInstalled)
	if err != nil {
		t.Fatalf("err = %v, want nil once the running job binds its socket", err)
	}
	if mode != StartModeAlreadyRunning {
		t.Errorf("StartMode = %v, want %v", mode, StartModeAlreadyRunning)
	}
	if *spawns != 0 {
		t.Errorf("detached spawns = %d, want 0", *spawns)
	}
	if got := countLaunchctlVerb(*calls, "load"); got != 0 {
		t.Errorf("`launchctl load` invocations = %d, want 0 for a job launchd reports running", got)
	}
}

// TestEnsureRunningWithPolicyRefusesAnUnservedWatchdog covers both watchdog
// routes for a non-root caller: the waiting route whose budget expires, and the
// insecure no-wait route. Neither may spawn under the refusing policy, and the
// cannot-serve warning — which announces an unsupervised start — must not fire
// for a start that is refused.
func TestEnsureRunningWithPolicyRefusesAnUnservedWatchdog(t *testing.T) {
	for _, tc := range []struct {
		name     string
		insecure bool
	}{
		{name: "watchdog wait expires"},
		{name: "insecure watchdog, no wait", insecure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socketPath, layout := prepareWatchdogEnsureRunningEnvironment(t, "unattended")
			if tc.insecure {
				if err := os.Remove(layout.BinaryPath); err != nil {
					t.Fatalf("remove watchdog binary: %v", err)
				}
				if got := observeUnattendedInstall().State; got != UnattendedInstallInsecure {
					t.Fatalf("install state = %d, want UnattendedInstallInsecure", got)
				}
			}
			stubNonRootEUID(t)
			shrinkWatchdogRespawnWait(t, 0)
			captureResidueWarnings(t)
			warned := captureWatchdogCannotServeWarnings(t)

			served := false
			stubDialReachableWhen(t, func() bool { return served })
			calls := stubLaunchctlWithDeadAgent(t, nil)
			spawns := countDetachedSpawns(t, &served)

			mode, err := EnsureRunningWithPolicy(socketPath, RefuseDetachedFallbackWhenServiceInstalled)
			if !errors.Is(err, ErrServiceInstalledNotServing) {
				t.Fatalf("err = %v, want ErrServiceInstalledNotServing", err)
			}
			if errors.Is(err, ErrWatchdogRecoveryFailed) {
				t.Errorf("err = %v took the root arm; this test is about a non-root caller", err)
			}
			if mode != StartModeUnknown {
				t.Errorf("StartMode = %v, want %v", mode, StartModeUnknown)
			}
			if *spawns != 0 {
				t.Errorf("detached spawns = %d, want 0", *spawns)
			}
			if len(*warned) != 0 {
				t.Errorf("cannot-serve warnings = %d, want 0 — it announces an unsupervised start that was refused", len(*warned))
			}
			if got := countLaunchctlVerb(*calls, "load"); got != 0 {
				t.Errorf("`launchctl load` invocations = %d, want 0 beside a watchdog", got)
			}
		})
	}
}

// TestEnsureRunningWithPolicySpawnsWhenNoServiceIsInstalled: with no service
// manager to prefer, refusing would leave the host with no way to start a
// daemon, so both policies spawn exactly as before.
func TestEnsureRunningWithPolicySpawnsWhenNoServiceIsInstalled(t *testing.T) {
	for _, policy := range []DetachedFallbackPolicy{AllowDetachedFallback, RefuseDetachedFallbackWhenServiceInstalled} {
		t.Run(policy.String(), func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "1")
			stubDefaultServiceSettings(t)
			useTempWatchdogRoot(t)
			sourcePath := writeFakeCellarBossd(t, home, "fallback version")
			stubExecutableNextTo(t, sourcePath)
			socketPath := filepath.Join(home, "bossd.sock")

			if st, err := platformGetStatus(); err != nil || st.Installed {
				t.Fatalf("fixture status = %+v (err %v), want nothing installed", st, err)
			}

			served := false
			stubDialReachableWhen(t, func() bool { return served })
			calls := stubRestartLaunchctl(t, func([]string) ([]byte, error) { return nil, nil })
			spawns := countDetachedSpawns(t, &served)

			mode, err := EnsureRunningWithPolicy(socketPath, policy)
			if err != nil {
				t.Fatalf("EnsureRunningWithPolicy: %v", err)
			}
			if mode != StartModeDetached {
				t.Errorf("StartMode = %v, want %v", mode, StartModeDetached)
			}
			if *spawns != 1 {
				t.Errorf("detached spawns = %d, want 1", *spawns)
			}
			if len(*calls) != 0 {
				t.Errorf("launchctl calls = %v, want none with no service installed", *calls)
			}
		})
	}
}
