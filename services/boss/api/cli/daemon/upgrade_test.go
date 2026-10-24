package daemon

import (
	"reflect"
	"strings"
	"testing"
	"time"

	daemon "github.com/recurser/boss/internal/daemonlife"
)

func TestRestartDaemonAfterUpgradeWaitsForShutdownBeforeBootstrap(t *testing.T) {
	deps := newTestGroup()

	deps.pinDaemonNotStandaloneServed(t)
	oldDefaultSocketPath := deps.defaultSocketPath
	oldDaemonGetStatus := deps.daemonGetStatus
	oldDaemonStop := deps.daemonStop
	oldRestartDaemon := deps.restartDaemon
	oldTerminateCurrentProfileBossd := deps.terminateCurrentProfileBossd
	oldWaitForSocketGone := deps.waitForDaemonSocketGone
	oldDaemonSocketReachable := deps.daemonSocketReachable
	oldDaemonRestartReadyTimeout := deps.daemonRestartReadyTimeout
	oldDaemonRestartPollInterval := deps.daemonRestartPollInterval
	defer func() {
		deps.defaultSocketPath = oldDefaultSocketPath
		deps.daemonGetStatus = oldDaemonGetStatus
		deps.daemonStop = oldDaemonStop
		deps.restartDaemon = oldRestartDaemon
		deps.terminateCurrentProfileBossd = oldTerminateCurrentProfileBossd
		deps.waitForDaemonSocketGone = oldWaitForSocketGone
		deps.daemonSocketReachable = oldDaemonSocketReachable
		deps.daemonRestartReadyTimeout = oldDaemonRestartReadyTimeout
		deps.daemonRestartPollInterval = oldDaemonRestartPollInterval
	}()

	const socketPath = "/tmp/bossd.sock"
	var events []string
	deps.defaultSocketPath = func() (string, error) { return socketPath, nil }
	deps.daemonGetStatus = func() (*daemon.Status, error) {
		events = append(events, "status")
		return &daemon.Status{Installed: true, Running: true}, nil
	}
	deps.daemonStop = func() error {
		events = append(events, "stop")
		return nil
	}
	deps.waitForDaemonSocketGone = func(path string) bool {
		if path != socketPath {
			t.Fatalf("wait socket path = %q, want %q", path, socketPath)
		}
		events = append(events, "wait")
		return true
	}
	deps.restartDaemon = func() error {
		events = append(events, "restart")
		return nil
	}
	deps.daemonSocketReachable = func(path string) bool {
		if path != socketPath {
			t.Fatalf("socket path = %q, want %q", path, socketPath)
		}
		events = append(events, "ready")
		return true
	}
	deps.terminateCurrentProfileBossd = func() (int, error) {
		t.Fatal("terminateCurrentProfileBossd called for a running installed daemon")
		return 0, nil
	}

	if err := deps.restartDaemonAfterUpgrade(); err != nil {
		t.Fatalf("restartDaemonAfterUpgrade() error = %v", err)
	}
	if want := []string{"status", "stop", "wait", "restart", "ready"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestRestartDaemonAfterUpgradeReportsWhenSocketNeverBecomesReachable(t *testing.T) {
	deps := newTestGroup()

	deps.pinDaemonNotStandaloneServed(t)
	oldDefaultSocketPath := deps.defaultSocketPath
	oldDaemonGetStatus := deps.daemonGetStatus
	oldDaemonStop := deps.daemonStop
	oldRestartDaemon := deps.restartDaemon
	oldTerminateCurrentProfileBossd := deps.terminateCurrentProfileBossd
	oldWaitForSocketGone := deps.waitForDaemonSocketGone
	oldDaemonSocketReachable := deps.daemonSocketReachable
	oldDaemonRestartReadyTimeout := deps.daemonRestartReadyTimeout
	oldDaemonRestartPollInterval := deps.daemonRestartPollInterval
	defer func() {
		deps.defaultSocketPath = oldDefaultSocketPath
		deps.daemonGetStatus = oldDaemonGetStatus
		deps.daemonStop = oldDaemonStop
		deps.restartDaemon = oldRestartDaemon
		deps.terminateCurrentProfileBossd = oldTerminateCurrentProfileBossd
		deps.waitForDaemonSocketGone = oldWaitForSocketGone
		deps.daemonSocketReachable = oldDaemonSocketReachable
		deps.daemonRestartReadyTimeout = oldDaemonRestartReadyTimeout
		deps.daemonRestartPollInterval = oldDaemonRestartPollInterval
	}()

	deps.defaultSocketPath = func() (string, error) { return "/tmp/bossd.sock", nil }
	deps.daemonGetStatus = func() (*daemon.Status, error) { return &daemon.Status{Installed: true, Running: true}, nil }
	deps.daemonStop = func() error { return nil }
	deps.waitForDaemonSocketGone = func(string) bool { return true }
	deps.restartDaemon = func() error { return nil }
	deps.terminateCurrentProfileBossd = func() (int, error) { return 0, nil }
	deps.daemonSocketReachable = func(string) bool { return false }
	deps.daemonRestartReadyTimeout = 20 * time.Millisecond
	deps.daemonRestartPollInterval = time.Millisecond

	err := deps.restartDaemonAfterUpgrade()
	if err == nil || !strings.Contains(err.Error(), "did not become reachable") {
		t.Fatalf("restartDaemonAfterUpgrade() error = %v, want readiness error", err)
	}
}

func TestRestartDaemonAfterUpgradeScopesStandaloneCleanupToCurrentProfile(t *testing.T) {
	deps := newTestGroup()

	deps.pinDaemonNotStandaloneServed(t)
	oldDefaultSocketPath := deps.defaultSocketPath
	oldDaemonGetStatus := deps.daemonGetStatus
	oldDaemonStop := deps.daemonStop
	oldRestartDaemon := deps.restartDaemon
	oldTerminateCurrentProfileBossd := deps.terminateCurrentProfileBossd
	oldWaitForSocketGone := deps.waitForDaemonSocketGone
	oldWaitForStandaloneBossdExit := deps.waitForStandaloneBossdExit
	oldDaemonSocketReachable := deps.daemonSocketReachable
	defer func() {
		deps.waitForStandaloneBossdExit = oldWaitForStandaloneBossdExit
		deps.defaultSocketPath = oldDefaultSocketPath
		deps.daemonGetStatus = oldDaemonGetStatus
		deps.daemonStop = oldDaemonStop
		deps.restartDaemon = oldRestartDaemon
		deps.terminateCurrentProfileBossd = oldTerminateCurrentProfileBossd
		deps.waitForDaemonSocketGone = oldWaitForSocketGone
		deps.daemonSocketReachable = oldDaemonSocketReachable
	}()

	const socketPath = "/tmp/bossd.sock"
	var events []string
	deps.defaultSocketPath = func() (string, error) { return socketPath, nil }
	deps.daemonGetStatus = func() (*daemon.Status, error) {
		events = append(events, "status")
		return &daemon.Status{Installed: true, Running: false}, nil
	}
	deps.daemonStop = func() error {
		t.Fatal("daemon.Stop called for an inactive installed daemon")
		return nil
	}
	deps.terminateCurrentProfileBossd = func() (int, error) {
		events = append(events, "terminate-current-profile")
		return 1, nil
	}
	deps.waitForDaemonSocketGone = func(path string) bool {
		if path != socketPath {
			t.Fatalf("wait socket path = %q, want %q", path, socketPath)
		}
		events = append(events, "wait")
		return true
	}
	// Unstubbed, this reaches waitForDaemonLockRelease, which flocks the running
	// profile's real bossd.lock and blocks for the whole 20s shutdown timeout on
	// any machine with a live daemon. restartDaemonAfterUpgrade reaches it through
	// waitForCurrentProfileBossdExit, a plain func; this var is the seam that func
	// delegates to, and stubbing it is what handlers_test.go already does.
	deps.waitForStandaloneBossdExit = func(string) bool {
		events = append(events, "exit")
		return true
	}
	deps.restartDaemon = func() error {
		events = append(events, "restart")
		return nil
	}
	deps.daemonSocketReachable = func(path string) bool {
		if path != socketPath {
			t.Fatalf("socket path = %q, want %q", path, socketPath)
		}
		events = append(events, "ready")
		return true
	}

	if err := deps.restartDaemonAfterUpgrade(); err != nil {
		t.Fatalf("restartDaemonAfterUpgrade() error = %v", err)
	}
	if want := []string{"status", "terminate-current-profile", "wait", "exit", "restart", "ready"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestRestartDaemonAfterUpgradeKeepsStandaloneDaemonUninstalled(t *testing.T) {
	deps := newTestGroup()

	deps.pinDaemonNotStandaloneServed(t)
	oldDefaultSocketPath := deps.defaultSocketPath
	oldDaemonGetStatus := deps.daemonGetStatus
	oldDaemonStop := deps.daemonStop
	oldDaemonEnsureRunning := deps.daemonEnsureRunning
	oldRestartDaemon := deps.restartDaemon
	oldTerminateCurrentProfileBossd := deps.terminateCurrentProfileBossd
	oldWaitForSocketGone := deps.waitForDaemonSocketGone
	oldDaemonSocketReachable := deps.daemonSocketReachable
	defer func() {
		deps.defaultSocketPath = oldDefaultSocketPath
		deps.daemonGetStatus = oldDaemonGetStatus
		deps.daemonStop = oldDaemonStop
		deps.daemonEnsureRunning = oldDaemonEnsureRunning
		deps.restartDaemon = oldRestartDaemon
		deps.terminateCurrentProfileBossd = oldTerminateCurrentProfileBossd
		deps.waitForDaemonSocketGone = oldWaitForSocketGone
		deps.daemonSocketReachable = oldDaemonSocketReachable
	}()

	const socketPath = "/tmp/bossd.sock"
	var events []string
	deps.defaultSocketPath = func() (string, error) { return socketPath, nil }
	deps.daemonGetStatus = func() (*daemon.Status, error) {
		events = append(events, "status")
		return &daemon.Status{Installed: false}, nil
	}
	deps.daemonStop = func() error {
		t.Fatal("daemon.Stop called for a standalone daemon")
		return nil
	}
	deps.terminateCurrentProfileBossd = func() (int, error) {
		events = append(events, "terminate-current-profile")
		return 0, nil
	}
	deps.waitForDaemonSocketGone = func(string) bool {
		t.Fatal("waitForDaemonSocketGone called without a standalone daemon to reap")
		return false
	}
	deps.restartDaemon = func() error {
		t.Fatal("daemon.Restart installs a LaunchAgent for standalone upgrades")
		return nil
	}
	deps.daemonEnsureRunning = func(path string) error {
		if path != socketPath {
			t.Fatalf("ensure path = %q, want %q", path, socketPath)
		}
		events = append(events, "start-standalone")
		return nil
	}
	deps.daemonSocketReachable = func(path string) bool {
		if path != socketPath {
			t.Fatalf("socket path = %q, want %q", path, socketPath)
		}
		events = append(events, "ready")
		return true
	}

	if err := deps.restartDaemonAfterUpgrade(); err != nil {
		t.Fatalf("restartDaemonAfterUpgrade() error = %v", err)
	}
	if want := []string{"status", "terminate-current-profile", "start-standalone", "ready"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

// pinDaemonNotStandaloneServed fixes the BOS-1181 serving-mode verdict for one
// test. Without it the real predicate reads the daemon state of whatever bossd
// is running on the machine executing the suite, so these upgrade paths would
// be decided by the host rather than by the status under test.
// TestRestartDaemonAfterUpgradeKeepsAStandaloneServedDaemonStandalone covers
// the opposite verdict.
func (deps *Group) pinDaemonNotStandaloneServed(t *testing.T) {
	t.Helper()
	old := deps.daemonStandaloneServed
	t.Cleanup(func() { deps.daemonStandaloneServed = old })
	deps.daemonStandaloneServed = func(*daemon.Status) bool { return false }
}
