package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/recurser/boss/api/cli/cliutil"

	"github.com/spf13/cobra"

	daemon "github.com/recurser/boss/internal/daemonlife"
	"github.com/recurser/bossalib/daemonstate"
	"github.com/recurser/bossalib/kit/config"
)

func TestRestartSocketPath(t *testing.T) {
	t.Run("returns path", func(t *testing.T) {
		got, err := restartSocketPath("/tmp/boss.sock", nil)
		if err != nil {
			t.Fatalf("restartSocketPath returned error: %v", err)
		}
		if got != "/tmp/boss.sock" {
			t.Fatalf("restartSocketPath returned %q, want /tmp/boss.sock", got)
		}
	})

	t.Run("surfaces path error", func(t *testing.T) {
		pathErr := errors.New("home unavailable")
		_, err := restartSocketPath("", pathErr)
		if !errors.Is(err, pathErr) {
			t.Fatalf("restartSocketPath error = %v, want %v", err, pathErr)
		}
	})

	t.Run("rejects empty path", func(t *testing.T) {
		_, err := restartSocketPath("", nil)
		if err == nil {
			t.Fatal("restartSocketPath returned nil error for empty path")
		}
	})
}

func TestWaitForSocketGoneWaitsForDelayedDaemonShutdown(t *testing.T) {
	// This test intentionally blocks ~3.5s waiting on a delayed socket close;
	// run it in parallel so it overlaps other slow tests instead of adding its
	// wall time serially.
	t.Parallel()
	sockPath := filepath.Join("/tmp", "boss-wait-gone-"+strconv.Itoa(os.Getpid())+"-"+strconv.FormatInt(time.Now().UnixNano(), 10)+".sock")
	if err := os.Remove(sockPath); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove stale socket: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(sockPath) })

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	if !daemon.IsSocketReachable(sockPath) {
		t.Fatalf("socket %q did not become reachable", sockPath)
	}

	time.AfterFunc(3500*time.Millisecond, func() {
		_ = ln.Close()
		_ = os.Remove(sockPath)
	})
	start := time.Now()
	if !waitForSocketGone(sockPath) {
		t.Fatal("waitForSocketGone returned false before delayed shutdown completed")
	}
	if elapsed := time.Since(start); elapsed < 3*time.Second {
		t.Fatalf("waitForSocketGone returned after %v, want it to wait past old 3s timeout", elapsed)
	}
}

func TestWaitForDaemonLockReleaseWaitsForSingletonLockRelease(t *testing.T) {
	appDataDir := t.TempDir()
	lock, err := os.OpenFile(filepath.Join(appDataDir, "bossd.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("open singleton lock: %v", err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("lock singleton file: %v", err)
	}

	const releaseDelay = 25 * time.Millisecond
	time.AfterFunc(releaseDelay, func() { _ = lock.Close() })
	start := time.Now()
	if !waitForDaemonLockRelease(appDataDir) {
		t.Fatal("waitForDaemonLockRelease() = false, want true after lock release")
	}
	if elapsed := time.Since(start); elapsed < releaseDelay {
		t.Fatalf("waitForDaemonLockRelease() returned after %s, before singleton lock release", elapsed)
	}
}

func TestCurrentDaemonProfileUsesConfiguredAppDataAndSocketPath(t *testing.T) {
	deps := newTestGroup()

	deps.restoreDaemonCommandStubs(t)
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	appDataDir := filepath.Join(dir, "data")
	socketPath := filepath.Join(dir, "bossd.sock")
	settings := config.DefaultSettings()
	settings.AppDataDir = appDataDir
	if err := config.SaveTo(settingsPath, settings); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	t.Setenv("BOSS_SETTINGS_PATH", settingsPath)
	deps.defaultSocketPath = func() (string, error) { return socketPath, nil }

	profile, err := deps.currentDaemonProfile()
	if err != nil {
		t.Fatalf("currentDaemonProfile: %v", err)
	}
	if profile.SettingsPath != settingsPath {
		t.Fatalf("SettingsPath = %q, want %q", profile.SettingsPath, settingsPath)
	}
	if profile.AppDataDir != appDataDir {
		t.Fatalf("AppDataDir = %q, want %q", profile.AppDataDir, appDataDir)
	}
	if profile.SocketPath != socketPath {
		t.Fatalf("SocketPath = %q, want %q", profile.SocketPath, socketPath)
	}
}

func TestRunDaemonStatusPrintsProfileMetadataWhenReachable(t *testing.T) {
	deps := newTestGroup()

	deps.restoreDaemonCommandStubs(t)
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	appDataDir := filepath.Join(dir, "data")
	socketPath := filepath.Join(dir, "bossd.sock")
	executablePath := filepath.Join(dir, "bossd")
	settings := config.DefaultSettings()
	settings.AppDataDir = appDataDir
	if err := config.SaveTo(settingsPath, settings); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	t.Setenv("BOSS_SETTINGS_PATH", settingsPath)
	if err := daemonstate.Write(appDataDir, daemonstate.Metadata{
		PID:            12345,
		ExecutablePath: executablePath,
		SettingsPath:   settingsPath,
		SocketPath:     socketPath,
		StartedAt:      time.Now(),
	}); err != nil {
		t.Fatalf("daemonstate.Write: %v", err)
	}
	deps.daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: true, PID: 99, ServicePath: "/tmp/service"}, nil
	}
	deps.defaultSocketPath = func() (string, error) { return socketPath, nil }
	deps.daemonSocketReachable = func(path string) bool {
		if path != socketPath {
			t.Fatalf("socket reachability path = %q, want %q", path, socketPath)
		}
		return true
	}

	out := captureStdout(t, func() {
		if err := deps.runDaemonStatus(&cobra.Command{}); err != nil {
			t.Fatalf("runDaemonStatus: %v", err)
		}
	})
	for _, want := range []string{
		"Daemon is running.",
		"settings: " + settingsPath,
		"app data: " + appDataDir,
		"socket:   " + socketPath,
		"socket reachable: true",
		"standalone PID: 12345",
		"standalone executable: " + executablePath,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want %q", out, want)
		}
	}
}

// daemonStatusRecordedPID is the PID writeDaemonStatusProfile records in the
// daemon state file. Tests pair it with the stubbed service-manager PID to
// select a rung of the supervision ladder.
const daemonStatusRecordedPID = 12345

// writeDaemonStatusProfile lays down the settings + daemon state record that
// runDaemonStatus reads and returns the socket path it will probe. It mirrors
// the setup TestRunDaemonStatusPrintsProfileMetadataWhenReachable does inline
// so the BOS-1183 header and supervision tests differ only in the stubbed
// service-manager status.
func (deps *Group) writeDaemonStatusProfile(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	appDataDir := filepath.Join(dir, "data")
	socketPath := filepath.Join(dir, "bossd.sock")
	executablePath := filepath.Join(dir, "bossd")
	settings := config.DefaultSettings()
	settings.AppDataDir = appDataDir
	if err := config.SaveTo(settingsPath, settings); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	t.Setenv("BOSS_SETTINGS_PATH", settingsPath)
	if err := daemonstate.Write(appDataDir, daemonstate.Metadata{
		PID:            daemonStatusRecordedPID,
		ExecutablePath: executablePath,
		SettingsPath:   settingsPath,
		SocketPath:     socketPath,
		StartedAt:      time.Now(),
	}); err != nil {
		t.Fatalf("daemonstate.Write: %v", err)
	}
	deps.defaultSocketPath = func() (string, error) { return socketPath, nil }
	return socketPath
}

// TestRunDaemonStatusDoesNotClaimRunningWhenSocketUnreachable is the BOS-1183
// headline regression test. `boss daemon status` chose its header from
// Installed/Running alone while probing the socket ten lines lower, so on
// 2026-09-06 a real machine printed "Daemon is running." directly above
// "socket reachable: false" — a launchd job registered but never spawned. One
// probe must now drive both, and the header must never assert health that the
// probe contradicts.
func TestRunDaemonStatusDoesNotClaimRunningWhenSocketUnreachable(t *testing.T) {
	deps := newTestGroup()

	deps.restoreDaemonCommandStubs(t)
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
	socketPath := deps.writeDaemonStatusProfile(t)
	deps.daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: true, PID: daemonStatusRecordedPID, ServicePath: "/tmp/service"}, nil
	}
	probes := 0
	deps.daemonSocketReachable = func(path string) bool {
		if path != socketPath {
			t.Fatalf("socket reachability path = %q, want %q", path, socketPath)
		}
		probes++
		return false
	}

	out := captureStdout(t, func() {
		if err := deps.runDaemonStatus(&cobra.Command{}); err != nil {
			t.Fatalf("runDaemonStatus: %v", err)
		}
	})
	if strings.Contains(out, "Daemon is running.") {
		t.Fatalf("output = %q, must not claim the daemon is running while its socket is unreachable", out)
	}
	for _, want := range []string{
		"not serving",
		"socket reachable: false",
		"boss daemon doctor",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want %q", out, want)
		}
	}
	if probes != 1 {
		t.Fatalf("socket probed %d times, want exactly 1 verdict driving both header and field", probes)
	}
}

// TestRunDaemonStatusLabelsUnsupervisedDaemon pins the BOS-1183 R4 half: a
// daemon the service manager does not own must be labelled where it is
// displayed. "standalone PID" is a misnomer — bossd writes that record on
// every start, supervised or not — so the label is the only line that
// separates the two.
func TestRunDaemonStatusLabelsUnsupervisedDaemon(t *testing.T) {
	deps := newTestGroup()

	deps.restoreDaemonCommandStubs(t)
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
	deps.writeDaemonStatusProfile(t)
	deps.stubDaemonDoctorProcess(t, nil)
	deps.daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: false, ServicePath: "/tmp/service"}, nil
	}
	deps.daemonSocketReachable = func(string) bool { return true }

	out := captureStdout(t, func() {
		if err := deps.runDaemonStatus(&cobra.Command{}); err != nil {
			t.Fatalf("runDaemonStatus: %v", err)
		}
	})
	for _, want := range []string{
		"standalone PID: 12345",
		"supervision: unsupervised",
		"will not survive reboot",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want %q", out, want)
		}
	}
}

// TestRunDaemonStatusLabelsSupervisedDaemon is the other half: when the
// service manager owns the recorded PID the label must say so, or the
// unsupervised warning becomes noise operators learn to ignore.
func TestRunDaemonStatusLabelsSupervisedDaemon(t *testing.T) {
	deps := newTestGroup()

	deps.restoreDaemonCommandStubs(t)
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
	deps.writeDaemonStatusProfile(t)
	deps.stubDaemonDoctorProcess(t, nil)
	deps.daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: true, PID: daemonStatusRecordedPID, ServicePath: "/tmp/service"}, nil
	}
	deps.daemonSocketReachable = func(string) bool { return true }

	out := captureStdout(t, func() {
		if err := deps.runDaemonStatus(&cobra.Command{}); err != nil {
			t.Fatalf("runDaemonStatus: %v", err)
		}
	})
	if !strings.Contains(out, "supervision: supervised") {
		t.Fatalf("output = %q, want a supervised label", out)
	}
	if strings.Contains(out, "unsupervised") {
		t.Fatalf("output = %q, must not warn about supervision for a service-manager-owned daemon", out)
	}
}

// TestRunDaemonStatusReportsUnknownSupervisionWithoutServicePID mirrors
// reportDaemonSupervision's st.PID == 0 rung (services/boss/api/cli/daemon/daemon_doctor.go):
// launchctl output that will not parse, or a systemd MainPID read that fails,
// leaves ownership unproven. Certifying supervision there would emit a false
// healthy verdict from the one line added to detect an ownership mismatch.
func TestRunDaemonStatusReportsUnknownSupervisionWithoutServicePID(t *testing.T) {
	deps := newTestGroup()

	deps.restoreDaemonCommandStubs(t)
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
	deps.writeDaemonStatusProfile(t)
	deps.stubDaemonDoctorProcess(t, nil)
	deps.daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: true, PID: 0, ServicePath: "/tmp/service"}, nil
	}
	deps.daemonSocketReachable = func(string) bool { return true }

	out := captureStdout(t, func() {
		if err := deps.runDaemonStatus(&cobra.Command{}); err != nil {
			t.Fatalf("runDaemonStatus: %v", err)
		}
	})
	if !strings.Contains(out, "supervision: unknown") {
		t.Fatalf("output = %q, want an unknown supervision verdict", out)
	}
	if strings.Contains(out, "supervision: supervised") {
		t.Fatalf("output = %q, must not certify supervision without a service PID", out)
	}
}

// TestRunDaemonStatusDoesNotCallAStaleRecordUnsupervised is the repair-pass
// regression for the disagreement BOS-1183 and BOS-1181 left between three
// surfaces. `boss daemon status` described the state RECORD without probing
// it, so a correctly supervised daemon whose record outlived its process read
// "unsupervised (two daemons, or a stale state record)" here while doctor said
// "unknown (recorded PID N is not running)" and restart classified the host as
// supervised. Status now makes the same signal-0 probe doctor does.
func TestRunDaemonStatusDoesNotCallAStaleRecordUnsupervised(t *testing.T) {
	deps := newTestGroup()

	deps.restoreDaemonCommandStubs(t)
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
	deps.writeDaemonStatusProfile(t)
	deps.stubDaemonDoctorProcess(t, syscall.ESRCH)
	deps.daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: true, PID: daemonStatusRecordedPID + 1, ServicePath: "/tmp/service"}, nil
	}
	deps.daemonSocketReachable = func(string) bool { return true }

	out := captureStdout(t, func() {
		if err := deps.runDaemonStatus(&cobra.Command{}); err != nil {
			t.Fatalf("runDaemonStatus: %v", err)
		}
	})
	if strings.Contains(out, "unsupervised") {
		t.Fatalf("output = %q, must not report a stale record as an unsupervised daemon", out)
	}
	if !strings.Contains(out, "supervision: unknown (recorded PID 12345 is not running)") {
		t.Fatalf("output = %q, want the stale-record unknown verdict doctor prints", out)
	}
}

func TestRunDaemonStartRefusesReachableSocket(t *testing.T) {
	deps := newTestGroup()

	deps.restoreDaemonCommandStubs(t)
	socketPath := filepath.Join(t.TempDir(), "bossd.sock")
	deps.defaultSocketPath = func() (string, error) { return socketPath, nil }
	deps.daemonSocketReachable = func(path string) bool {
		if path != socketPath {
			t.Fatalf("socket reachability path = %q, want %q", path, socketPath)
		}
		return true
	}
	deps.daemonEnsureRunning = func(string) error {
		t.Fatal("daemonEnsureRunning called for reachable socket")
		return nil
	}
	deps.daemonEnsureRunningWithPolicy = func(string, daemon.DetachedFallbackPolicy) (daemon.StartMode, error) {
		t.Fatal("daemonEnsureRunningWithPolicy called for reachable socket")
		return daemon.StartModeUnknown, nil
	}

	out := captureStdout(t, func() {
		if err := deps.runDaemonStart(&cobra.Command{}); err != nil {
			t.Fatalf("runDaemonStart: %v", err)
		}
	})
	if !strings.Contains(out, "Daemon is already running.") {
		t.Fatalf("output = %q, want already running message", out)
	}
}

// TestRunDaemonStartAnnouncesStandaloneFallback pins BOS-1183 R4 on the start
// path. platformEnsureRunning falls through to an unsupervised direct spawn
// whenever the LaunchAgent branch does not serve the socket — including the
// registered-but-never-spawned case, where Running==true skips that branch
// entirely — and `boss daemon start` printed the same "Daemon started." for
// both. Losing supervision has to be announced where it happens.
func TestRunDaemonStartAnnouncesStandaloneFallback(t *testing.T) {
	deps := newTestGroup()

	deps.restoreDaemonCommandStubs(t)
	socketPath := filepath.Join(t.TempDir(), "bossd.sock")
	deps.defaultSocketPath = func() (string, error) { return socketPath, nil }
	deps.daemonSocketReachable = func(string) bool { return false }
	deps.daemonEnsureRunningWithPolicy = func(path string, _ daemon.DetachedFallbackPolicy) (daemon.StartMode, error) {
		if path != socketPath {
			t.Fatalf("start socket path = %q, want %q", path, socketPath)
		}
		return daemon.StartModeDetached, nil
	}

	out := captureStdout(t, func() {
		if err := deps.runDaemonStart(&cobra.Command{}); err != nil {
			t.Fatalf("runDaemonStart: %v", err)
		}
	})
	// The existing line is load-bearing for scripts that parse this output, so
	// the notice is ADDED beneath it rather than replacing it.
	if !strings.Contains(out, "Daemon started.") {
		t.Fatalf("output = %q, want the unchanged started line", out)
	}
	for _, want := range []string{
		"not under the service manager",
		// The reboot consequence alone reads as a deferred, tomorrow problem,
		// so the operator moves on while gh is ALREADY falling back to
		// unauthenticated requests. This is the first surface that reports the
		// loss, so it must name both halves — as status and doctor do.
		"keychain",
		"unauthenticated",
		"will not survive reboot",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want %q", out, want)
		}
	}
	if strings.Contains(out, "Run 'boss daemon doctor'") {
		t.Fatalf("output = %q, must state the loss without telling the user what to run", out)
	}
}

// TestDaemonUnsupervisedConsequencesAreOneSentenceEverywhere pins what the
// shared constant buys: three surfaces reported three different consequence
// sets for one fact, and the operator learned the least from the surface that
// tells them first. Asserting the constant is USED, rather than re-asserting
// the wording, is what keeps this from being a copy of the string.
func TestDaemonUnsupervisedConsequencesAreOneSentenceEverywhere(t *testing.T) {
	deps := newTestGroup()

	for _, want := range []string{"keychain", "unauthenticated", "will not survive reboot"} {
		if !strings.Contains(daemonUnsupervisedConsequences, want) {
			t.Fatalf("shared consequence sentence = %q, want it to name %q", daemonUnsupervisedConsequences, want)
		}
	}

	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
	deps.stubDaemonDoctorProcess(t, nil)
	const recordedPID = 5150
	st := daemon.Status{Installed: true, Running: false}

	deps.stubDaemonSupervisionInputs(t, launchAgentSupervisionStatus(), daemon.WatchdogOwnership{})
	statusLine := deps.daemonSupervisionLine(&st, recordedPID, launchAgentSupervisionStatus(), daemon.WatchdogOwnership{})
	if !strings.Contains(statusLine, daemonUnsupervisedConsequences) {
		t.Fatalf("status line = %q, want the shared consequence sentence", statusLine)
	}

	previous := deps.daemonGetStatus
	deps.daemonGetStatus = func() (*daemon.Status, error) { return &st, nil }
	t.Cleanup(func() { deps.daemonGetStatus = previous })
	var doctorOut bytes.Buffer
	deps.reportDaemonSupervisionGathered(&doctorOut, daemonstate.Metadata{PID: recordedPID}, nil)
	if !strings.Contains(doctorOut.String(), daemonUnsupervisedConsequences) {
		t.Fatalf("doctor output = %q, want the shared consequence sentence", doctorOut.String())
	}
}

// TestRunDaemonStartOmitsFallbackNoticeWhenSupervised is the other half: a
// warning printed on the healthy path is a warning operators stop reading.
func TestRunDaemonStartOmitsFallbackNoticeWhenSupervised(t *testing.T) {
	deps := newTestGroup()

	deps.restoreDaemonCommandStubs(t)
	socketPath := filepath.Join(t.TempDir(), "bossd.sock")
	deps.defaultSocketPath = func() (string, error) { return socketPath, nil }
	deps.daemonSocketReachable = func(string) bool { return false }
	deps.daemonEnsureRunningWithPolicy = func(string, daemon.DetachedFallbackPolicy) (daemon.StartMode, error) {
		return daemon.StartModeServiceManager, nil
	}

	out := captureStdout(t, func() {
		if err := deps.runDaemonStart(&cobra.Command{}); err != nil {
			t.Fatalf("runDaemonStart: %v", err)
		}
	})
	if !strings.Contains(out, "Daemon started.") {
		t.Fatalf("output = %q, want the started line", out)
	}
	for _, unwanted := range []string{"not under the service manager", "will not survive reboot"} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("output = %q, must not warn about supervision when the service manager started the daemon", out)
		}
	}
}

// daemonStartCommand builds a `start` command carrying the --detached flag the
// way daemonCmd defines it, so a test can set it.
func daemonStartCommand(t *testing.T, detached bool) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	addDaemonStartFlags(cmd)
	if detached {
		if err := cmd.Flags().Set("detached", "true"); err != nil {
			t.Fatalf("set --detached: %v", err)
		}
	}
	return cmd
}

// TestRunDaemonStartRefusesWhenTheServiceDoesNotServe pins BOS-1519: an
// explicit start with an installed service that did not serve exits non-zero,
// and never prints the line scripts read as success.
func TestRunDaemonStartRefusesWhenTheServiceDoesNotServe(t *testing.T) {
	deps := newTestGroup()

	deps.restoreDaemonCommandStubs(t)
	socketPath := filepath.Join(t.TempDir(), "bossd.sock")
	deps.defaultSocketPath = func() (string, error) { return socketPath, nil }
	deps.daemonSocketReachable = func(string) bool { return false }
	var policies []daemon.DetachedFallbackPolicy
	deps.daemonEnsureRunningWithPolicy = func(_ string, policy daemon.DetachedFallbackPolicy) (daemon.StartMode, error) {
		policies = append(policies, policy)
		return daemon.StartModeUnknown, fmt.Errorf("%w: /tmp/com.example.bossd.plist", daemon.ErrServiceInstalledNotServing)
	}

	var err error
	out := captureStdout(t, func() {
		err = deps.runDaemonStart(daemonStartCommand(t, false))
	})
	if !errors.Is(err, daemon.ErrServiceInstalledNotServing) {
		t.Fatalf("runDaemonStart err = %v, want ErrServiceInstalledNotServing", err)
	}
	if strings.Contains(out, "Daemon started.") {
		t.Fatalf("output = %q, must not report a start that was refused", out)
	}
	want := []daemon.DetachedFallbackPolicy{daemon.RefuseDetachedFallbackWhenServiceInstalled}
	if !reflect.DeepEqual(policies, want) {
		t.Fatalf("policies = %v, want %v", policies, want)
	}
}

// TestRunDaemonStartDetachedAllowsTheFallback: --detached is the deliberate
// escape hatch, and keeps today's behaviour including the fallback notice.
func TestRunDaemonStartDetachedAllowsTheFallback(t *testing.T) {
	deps := newTestGroup()

	deps.restoreDaemonCommandStubs(t)
	socketPath := filepath.Join(t.TempDir(), "bossd.sock")
	deps.defaultSocketPath = func() (string, error) { return socketPath, nil }
	deps.daemonSocketReachable = func(string) bool { return false }
	var policies []daemon.DetachedFallbackPolicy
	deps.daemonEnsureRunningWithPolicy = func(_ string, policy daemon.DetachedFallbackPolicy) (daemon.StartMode, error) {
		policies = append(policies, policy)
		return daemon.StartModeDetached, nil
	}

	out := captureStdout(t, func() {
		if err := deps.runDaemonStart(daemonStartCommand(t, true)); err != nil {
			t.Fatalf("runDaemonStart: %v", err)
		}
	})
	want := []daemon.DetachedFallbackPolicy{daemon.AllowDetachedFallback}
	if !reflect.DeepEqual(policies, want) {
		t.Fatalf("policies = %v, want %v", policies, want)
	}
	if !strings.Contains(out, "Daemon started.") {
		t.Fatalf("output = %q, want the started line", out)
	}
	if !strings.Contains(out, "not under the service manager") {
		t.Fatalf("output = %q, want the existing fallback notice", out)
	}
}

// TestDaemonStartCommandDefinesDetached pins that the flag reaches the real
// command, not only the test helper.
func TestDaemonStartCommandDefinesDetached(t *testing.T) {
	deps := newTestGroup()

	start, _, err := deps.daemonCmd().Find([]string{"start"})
	if err != nil {
		t.Fatalf("find start: %v", err)
	}
	if start.Flags().Lookup("detached") == nil {
		t.Fatal("`boss daemon start` has no --detached flag")
	}
}

// TestDaemonStopAllStandaloneStillSweepsPlugins pins the one CLI path that is
// still allowed to signal plugin processes. `boss daemon stop --all-standalone`
// is the explicit cross-profile crash-cleanup escape hatch, so it must keep
// sweeping bossd-plugin-* processes (BOS-349). The plugin sweep is stubbed here
// so the test never invokes the real pgrep/SIGTERM path.
func TestDaemonStopAllStandaloneStillSweepsPlugins(t *testing.T) {
	deps := newTestGroup()

	deps.restoreDaemonCommandStubs(t)
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	appDataDir := filepath.Join(dir, "data")
	socketPath := filepath.Join(dir, "bossd.sock")
	settings := config.DefaultSettings()
	settings.AppDataDir = appDataDir
	if err := config.SaveTo(settingsPath, settings); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	t.Setenv("BOSS_SETTINGS_PATH", settingsPath)
	deps.daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: false, Running: false}, nil
	}
	deps.defaultSocketPath = func() (string, error) { return socketPath, nil }
	terminateCalled := false
	deps.terminateAllBossdProcesses = func() (int, error) {
		terminateCalled = true
		return 2, nil
	}
	pluginSweepCalled := false
	deps.terminateAllPluginProcesses = func(daemonProfile) (int, error) {
		pluginSweepCalled = true
		return 3, nil
	}
	deps.waitForDaemonSocketGone = func(path string) bool {
		if path != socketPath {
			t.Fatalf("wait socket path = %q, want %q", path, socketPath)
		}
		return true
	}
	cmd := &cobra.Command{}
	cmd.Flags().Bool("all-standalone", false, "")
	if err := cmd.Flags().Set("all-standalone", "true"); err != nil {
		t.Fatalf("set all-standalone: %v", err)
	}

	out := captureStdout(t, func() {
		if err := deps.runDaemonStop(cmd); err != nil {
			t.Fatalf("runDaemonStop: %v", err)
		}
	})
	if !terminateCalled {
		t.Fatal("terminateAllBossdProcesses was not called")
	}
	if !pluginSweepCalled {
		t.Fatal("terminateAllPluginProcesses was not called; --all-standalone must still sweep plugin processes (BOS-349 escape hatch)")
	}
	if !strings.Contains(out, "Stopped 2 bossd process(es) across all profiles.") {
		t.Fatalf("output = %q, want broad stop message", out)
	}
	if !strings.Contains(out, "Stopped 3 plugin process(es).") {
		t.Fatalf("output = %q, want plugin cleanup message", out)
	}
}

// TestDaemonStopNeverSweepsPluginsOnNormalPaths is the BOS-349 regression guard.
// The per-profile plugin sweep matched plugins by BINARY PATH, not by owning
// daemon, so profile B's stop SIGTERMed profile A's live plugin children (all
// six at once; the main bossd survived). Normal stop must not touch plugin
// processes at all — bossd's Host.Stop owns reaping its own children. Only
// --all-standalone may sweep. daemonProcesses.FindPluginPIDs is the discovery seam every
// sweep funnels through, so if a normal path never reaches it, it never sweeps.
func TestDaemonStopNeverSweepsPluginsOnNormalPaths(t *testing.T) {
	deps := newTestGroup()

	cases := []struct {
		name   string
		status *daemon.Status
	}{
		{"installed and running", &daemon.Status{Installed: true, Running: true}},
		{"installed but not running", &daemon.Status{Installed: true, Running: false}},
		{"not installed", &daemon.Status{Installed: false, Running: false}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps.restoreDaemonCommandStubs(t)
			dir := t.TempDir()
			settingsPath := filepath.Join(dir, "settings.json")
			appDataDir := filepath.Join(dir, "data")
			socketPath := filepath.Join(dir, "bossd.sock")
			settings := config.DefaultSettings()
			settings.AppDataDir = appDataDir
			if err := config.SaveTo(settingsPath, settings); err != nil {
				t.Fatalf("SaveTo: %v", err)
			}
			t.Setenv("BOSS_SETTINGS_PATH", settingsPath)
			deps.daemonGetStatus = func() (*daemon.Status, error) { return tc.status, nil }
			deps.defaultSocketPath = func() (string, error) { return socketPath, nil }
			deps.daemonStop = func() error { return nil }
			deps.waitForDaemonSocketGone = func(string) bool { return true }

			swept := false
			deps.daemonProcesses.FindPluginPIDs = func() ([]int, error) {
				swept = true
				return nil, nil
			}

			captureStdout(t, func() {
				if err := deps.runDaemonStop(&cobra.Command{}); err != nil {
					t.Fatalf("runDaemonStop: %v", err)
				}
			})
			if swept {
				t.Fatal("normal stop path discovered plugin PIDs; it must never sweep plugin processes (BOS-349)")
			}
		})
	}
}

// TestDaemonRestartNeverSweepsPlugins is the BOS-349 regression guard for the
// restart branches. Restart must stop then start the daemon without ever
// signalling plugin processes; the event order also pins that no "plugins"
// step sits between stop and start.
func TestDaemonRestartNeverSweepsPlugins(t *testing.T) {
	deps := newTestGroup()

	cases := []struct {
		name       string
		status     *daemon.Status
		wantEvents []string
	}{
		{"installed and running", &daemon.Status{Installed: true, Running: true}, []string{"stop", "restart"}},
		{"installed but not running", &daemon.Status{Installed: true, Running: false}, []string{"terminate-current-profile", "restart"}},
		{"not installed", &daemon.Status{Installed: false, Running: false}, []string{"start"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps.restoreDaemonCommandStubs(t)
			dir := t.TempDir()
			settingsPath := filepath.Join(dir, "settings.json")
			appDataDir := filepath.Join(dir, "data")
			socketPath := filepath.Join(dir, "bossd.sock")
			settings := config.DefaultSettings()
			settings.AppDataDir = appDataDir
			if err := config.SaveTo(settingsPath, settings); err != nil {
				t.Fatalf("SaveTo: %v", err)
			}
			t.Setenv("BOSS_SETTINGS_PATH", settingsPath)
			deps.daemonGetStatus = func() (*daemon.Status, error) { return tc.status, nil }
			deps.defaultSocketPath = func() (string, error) { return socketPath, nil }
			deps.daemonSocketReachable = func(path string) bool { return path == socketPath }
			deps.waitForDaemonSocketGone = func(string) bool { return true }
			deps.daemonRestartReadyTimeout = 200 * time.Millisecond
			deps.daemonRestartPollInterval = time.Millisecond

			var events []string
			deps.daemonStop = func() error {
				events = append(events, "stop")
				return nil
			}
			deps.restartDaemon = func() error {
				events = append(events, "restart")
				return nil
			}
			deps.terminateCurrentProfileBossd = func() (int, error) {
				events = append(events, "terminate-current-profile")
				return 1, nil
			}
			deps.daemonEnsureRunning = func(path string) error {
				if path != socketPath {
					t.Fatalf("ensure path = %q, want %q", path, socketPath)
				}
				events = append(events, "start")
				return nil
			}

			swept := false
			deps.daemonProcesses.FindPluginPIDs = func() ([]int, error) {
				swept = true
				return nil, nil
			}

			captureStdout(t, func() {
				if err := deps.runDaemonRestart(&cobra.Command{}); err != nil {
					t.Fatalf("runDaemonRestart: %v", err)
				}
			})
			if swept {
				t.Fatal("restart path discovered plugin PIDs; it must never sweep plugin processes (BOS-349)")
			}
			if !reflect.DeepEqual(events, tc.wantEvents) {
				t.Fatalf("events = %v, want %v", events, tc.wantEvents)
			}
		})
	}
}

func TestRunDaemonRestartReportsWhenSocketNeverBecomesReachable(t *testing.T) {
	deps := newTestGroup()

	deps.restoreDaemonCommandStubs(t)
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	appDataDir := filepath.Join(dir, "data")
	socketPath := filepath.Join(dir, "bossd.sock")
	settings := config.DefaultSettings()
	settings.AppDataDir = appDataDir
	if err := config.SaveTo(settingsPath, settings); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	t.Setenv("BOSS_SETTINGS_PATH", settingsPath)
	deps.daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: true}, nil
	}
	deps.defaultSocketPath = func() (string, error) { return socketPath, nil }
	// The new daemon never starts accepting connections, so the readiness loop
	// must exhaust its deadline and return an error.
	deps.daemonSocketReachable = func(string) bool { return false }
	deps.waitForDaemonSocketGone = func(string) bool { return true }
	deps.daemonStop = func() error { return nil }
	deps.daemonProcesses.FindPluginPIDs = func() ([]int, error) { return nil, nil }
	deps.restartDaemon = func() error { return nil }
	deps.daemonEnsureRunning = func(string) error { return nil }
	// Keep the readiness loop fast so the failure path doesn't wait 30s.
	deps.daemonRestartReadyTimeout = 20 * time.Millisecond
	deps.daemonRestartPollInterval = time.Millisecond

	err := deps.runDaemonRestart(&cobra.Command{})
	if err == nil {
		t.Fatal("runDaemonRestart returned nil, want a not-reachable error")
	}
	if !strings.Contains(err.Error(), "did not become reachable") {
		t.Fatalf("error = %v, want it to mention socket not reachable", err)
	}
}

func TestRunDaemonRestartWaitsForStandaloneExitBeforeStartingReplacement(t *testing.T) {
	deps := newTestGroup()

	deps.restoreDaemonCommandStubs(t)
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	appDataDir := filepath.Join(dir, "data")
	socketPath := filepath.Join(dir, "bossd.sock")
	settings := config.DefaultSettings()
	settings.AppDataDir = appDataDir
	if err := config.SaveTo(settingsPath, settings); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	t.Setenv("BOSS_SETTINGS_PATH", settingsPath)

	var events []string
	deps.daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: false}, nil
	}
	deps.defaultSocketPath = func() (string, error) { return socketPath, nil }
	deps.terminateStandaloneCurrentProfile = func(profile daemonProfile) (int, error) {
		if profile.AppDataDir != appDataDir {
			t.Fatalf("profile app data dir = %q, want %q", profile.AppDataDir, appDataDir)
		}
		events = append(events, "terminate")
		return 1, nil
	}
	deps.waitForDaemonSocketGone = func(path string) bool {
		if path != socketPath {
			t.Fatalf("socket path = %q, want %q", path, socketPath)
		}
		events = append(events, "socket-gone")
		return true
	}
	deps.waitForStandaloneBossdExit = func(dir string) bool {
		if dir != appDataDir {
			t.Fatalf("state dir = %q, want %q", dir, appDataDir)
		}
		events = append(events, "process-exited")
		return true
	}
	deps.daemonEnsureRunning = func(path string) error {
		if path != socketPath {
			t.Fatalf("ensure path = %q, want %q", path, socketPath)
		}
		events = append(events, "start")
		return nil
	}

	if err := deps.runDaemonRestart(&cobra.Command{}); err != nil {
		t.Fatalf("runDaemonRestart: %v", err)
	}
	if want := []string{"terminate", "socket-gone", "process-exited", "start"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

// daemonRestartProfileFixture points the current profile at a temp app-data
// directory, so the BOS-1181 serving-mode probe reads test state rather than
// whatever daemon happens to be running on the host.
func daemonRestartProfileFixture(t *testing.T) (appDataDir, socketPath string) {
	t.Helper()
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	appDataDir = filepath.Join(dir, "data")
	socketPath = filepath.Join(dir, "bossd.sock")
	settings := config.DefaultSettings()
	settings.AppDataDir = appDataDir
	if err := config.SaveTo(settingsPath, settings); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	t.Setenv("BOSS_SETTINGS_PATH", settingsPath)
	return appDataDir, socketPath
}

// recordLiveStandaloneDaemon writes the daemon-state record bossd leaves on
// startup and makes that PID answer as a live bossd, which is what the probe
// keys the standalone verdict on. Requires restoreDaemonCommandStubs.
func (deps *Group) recordLiveStandaloneDaemon(t *testing.T, appDataDir, socketPath string, pid int) {
	t.Helper()
	const executable = "/opt/homebrew/bin/bossd"
	if err := daemonstate.Write(appDataDir, daemonstate.Metadata{
		PID:            pid,
		ExecutablePath: executable,
		SocketPath:     socketPath,
	}); err != nil {
		t.Fatalf("write daemon state: %v", err)
	}
	deps.daemonProcesses.CommandLine = func(queried int) (string, error) {
		if queried == pid {
			return executable, nil
		}
		return "", errors.New("no such process")
	}
}

// TestRunDaemonRestartPreservesStandaloneDaemonBehindAnUnusableLaunchAgent is
// the BOS-1181 regression. It reproduces the reported incident exactly: a
// standalone daemon is serving, a LaunchAgent plist exists, and `launchctl
// list` exits 0 for a job launchd registered but never spawned (Running with
// no PID). The old predicate read Installed and Running and took the launchd
// path, which stopped the process that was serving and then could not produce
// a socket. Restart must instead restart the standalone daemon in kind and end
// with a reachable socket.
//
// This drives the real serving-mode probe rather than stubbing it, so the
// daemon-state record and the launchd PID are the actual inputs under test.
func TestRunDaemonRestartPreservesStandaloneDaemonBehindAnUnusableLaunchAgent(t *testing.T) {
	deps := newTestGroup()

	if !daemon.StandaloneServingSupported() {
		t.Skip("standalone serving is a macOS mode; the sibling test pins the unchanged path elsewhere")
	}
	deps.restoreDaemonCommandStubs(t)
	appDataDir, socketPath := daemonRestartProfileFixture(t)
	deps.recordLiveStandaloneDaemon(t, appDataDir, socketPath, 27923)

	var events []string
	serving := true
	// Installed AND Running are both true here, and neither is a statement
	// about what is serving: the plist exists and launchctl accepted the job,
	// but launchd never spawned it, so there is no PID.
	deps.daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: true}, nil
	}
	deps.defaultSocketPath = func() (string, error) { return socketPath, nil }
	deps.daemonSocketReachable = func(string) bool { return serving }
	deps.daemonStop = func() error {
		events = append(events, "launchd-stop")
		serving = false
		return nil
	}
	deps.restartDaemon = func() error {
		// launchd accepts the bootstrap and still spawns nothing — the half of
		// the incident that made the outcome worse than doing nothing.
		events = append(events, "launchd-restart")
		return nil
	}
	deps.terminateStandaloneCurrentProfile = func(profile daemonProfile) (int, error) {
		events = append(events, "terminate-standalone")
		serving = false
		return 1, nil
	}
	deps.waitForDaemonSocketGone = func(string) bool {
		events = append(events, "socket-gone")
		return true
	}
	deps.waitForStandaloneBossdExit = func(string) bool {
		events = append(events, "process-exited")
		return true
	}
	deps.daemonEnsureRunning = func(string) error {
		events = append(events, "start-standalone")
		serving = true
		return nil
	}
	deps.daemonRestartReadyTimeout = 20 * time.Millisecond
	deps.daemonRestartPollInterval = time.Millisecond

	captureStdout(t, func() {
		if err := deps.runDaemonRestart(&cobra.Command{}); err != nil {
			t.Fatalf("runDaemonRestart: %v", err)
		}
	})

	want := []string{"terminate-standalone", "socket-gone", "process-exited", "start-standalone"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v (a standalone-served daemon must restart in kind, never through launchd)", events, want)
	}
	if !deps.daemonSocketReachable(socketPath) {
		t.Fatal("restart ended with no reachable socket; it began with one")
	}
}

// TestRunDaemonRestartLeavesServiceManagerPathUnchangedWithoutStandaloneMode
// is the other half of the platform seam: where there is no standalone serving
// mode (Linux), the very same inputs must still restart through the service
// manager. BOS-1181 deliberately does not change Linux behaviour.
func TestRunDaemonRestartLeavesServiceManagerPathUnchangedWithoutStandaloneMode(t *testing.T) {
	deps := newTestGroup()

	if daemon.StandaloneServingSupported() {
		t.Skip("this platform has a standalone serving mode; the sibling test covers it")
	}
	deps.restoreDaemonCommandStubs(t)
	appDataDir, socketPath := daemonRestartProfileFixture(t)
	deps.recordLiveStandaloneDaemon(t, appDataDir, socketPath, 27923)

	var events []string
	deps.daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: true}, nil
	}
	deps.defaultSocketPath = func() (string, error) { return socketPath, nil }
	deps.daemonSocketReachable = func(string) bool { return true }
	deps.daemonStop = func() error {
		events = append(events, "service-stop")
		return nil
	}
	deps.restartDaemon = func() error {
		events = append(events, "service-restart")
		return nil
	}
	deps.terminateStandaloneCurrentProfile = func(daemonProfile) (int, error) {
		t.Fatal("the standalone path must not be reachable on a platform without a standalone serving mode")
		return 0, nil
	}
	deps.waitForDaemonSocketGone = func(string) bool {
		events = append(events, "socket-gone")
		return true
	}
	deps.daemonEnsureRunning = func(string) error { return nil }
	deps.daemonRestartReadyTimeout = 20 * time.Millisecond
	deps.daemonRestartPollInterval = time.Millisecond

	captureStdout(t, func() {
		if err := deps.runDaemonRestart(&cobra.Command{}); err != nil {
			t.Fatalf("runDaemonRestart: %v", err)
		}
	})

	if want := []string{"service-stop", "socket-gone", "service-restart"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

// TestRunDaemonRestartStillRestartsASupervisedDaemonThroughTheServiceManager
// pins the anti-downgrade half of the fix. bossd writes its daemon-state
// record however it was spawned, so a correctly supervised host also has a
// live recorded PID — reading the record alone would push every such host onto
// the standalone path permanently. Here the record names the same PID launchd
// reports, so the verdict must stay supervised.
func TestRunDaemonRestartStillRestartsASupervisedDaemonThroughTheServiceManager(t *testing.T) {
	deps := newTestGroup()

	deps.restoreDaemonCommandStubs(t)
	appDataDir, socketPath := daemonRestartProfileFixture(t)
	const launchdPID = 4242
	deps.recordLiveStandaloneDaemon(t, appDataDir, socketPath, launchdPID)

	var events []string
	deps.daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: true, PID: launchdPID}, nil
	}
	deps.defaultSocketPath = func() (string, error) { return socketPath, nil }
	deps.daemonSocketReachable = func(string) bool { return true }
	deps.daemonStop = func() error {
		events = append(events, "launchd-stop")
		return nil
	}
	deps.restartDaemon = func() error {
		events = append(events, "launchd-restart")
		return nil
	}
	deps.waitForDaemonSocketGone = func(string) bool {
		events = append(events, "socket-gone")
		return true
	}
	deps.terminateStandaloneCurrentProfile = func(daemonProfile) (int, error) {
		t.Fatal("a launchd-served daemon must not be restarted as a standalone one")
		return 0, nil
	}
	deps.daemonEnsureRunning = func(string) error {
		t.Fatal("a launchd-served daemon must not be started standalone while launchd can serve it")
		return nil
	}
	deps.daemonRestartReadyTimeout = 20 * time.Millisecond
	deps.daemonRestartPollInterval = time.Millisecond

	captureStdout(t, func() {
		if err := deps.runDaemonRestart(&cobra.Command{}); err != nil {
			t.Fatalf("runDaemonRestart: %v", err)
		}
	})

	if want := []string{"launchd-stop", "socket-gone", "launchd-restart"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

// TestRunDaemonRestartRestoresServiceWhenTheServiceManagerProducesNoSocket
// covers the other way BOS-1181's outcome is reached: the service manager
// accepts the restart, returns success, and never produces a reachable socket.
// Restoring service outranks honouring the requested supervision mode, so
// restart must fall back to the direct start `boss daemon start` uses instead
// of exiting non-zero with the host left with no daemon.
func TestRunDaemonRestartRestoresServiceWhenTheServiceManagerProducesNoSocket(t *testing.T) {
	deps := newTestGroup()

	deps.restoreDaemonCommandStubs(t)
	appDataDir, socketPath := daemonRestartProfileFixture(t)

	var events []string
	serving := true
	deps.daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: true, PID: 4242}, nil
	}
	deps.defaultSocketPath = func() (string, error) { return socketPath, nil }
	deps.daemonSocketReachable = func(string) bool { return serving }
	deps.daemonStop = func() error {
		events = append(events, "launchd-stop")
		serving = false
		return nil
	}
	deps.waitForDaemonSocketGone = func(string) bool { return true }
	deps.restartDaemon = func() error {
		events = append(events, "launchd-restart")
		return nil
	}
	deps.daemonEnsureRunning = func(string) error {
		events = append(events, "restore")
		// The fallback really did leave a directly-spawned bossd serving, so
		// the record it writes is what the announcement must be read from.
		// Off macOS that record is compiled out of the verdict, which is the
		// whole point of the platform-split assertion below.
		deps.recordLiveStandaloneDaemon(t, appDataDir, socketPath, 27923)
		serving = true
		return nil
	}
	deps.daemonRestartReadyTimeout = 20 * time.Millisecond
	deps.daemonRestartPollInterval = time.Millisecond

	out := captureStdout(t, func() {
		if err := deps.runDaemonRestart(&cobra.Command{}); err != nil {
			t.Fatalf("runDaemonRestart: %v", err)
		}
	})

	if want := []string{"launchd-stop", "launchd-restart", "restore"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	if !deps.daemonSocketReachable(socketPath) {
		t.Fatal("restart ended with no reachable socket")
	}
	// The announcement must be read from the serving-mode probe, never assumed
	// from the fact that the fallback ran — so what it has to say is decided by
	// the platform seam, and both arms are asserted rather than one skipped.
	//
	// On macOS the restore left a directly-spawned bossd whose recorded PID is
	// not launchd's, so the probe reads standalone: silently downgrading
	// supervision would leave the operator believing the LaunchAgent is still
	// serving, and the announcement has to say otherwise. Off macOS standalone
	// serving is compiled out, so the very same inputs classify supervised and
	// the announcement must say THAT — claiming a direct start there would be a
	// statement about the host that this path never observed.
	//
	// Each arm also asserts the sibling wording is absent, so neither the
	// neutral fallback branch nor the opposite verdict can satisfy this test.
	want, notWant := "under the service manager", "by starting bossd directly"
	if daemon.StandaloneServingSupported() {
		want, notWant = notWant, want
	}
	if !strings.Contains(out, want) {
		t.Fatalf("stdout = %q, want it to contain %q", out, want)
	}
	if strings.Contains(out, notWant) {
		t.Fatalf("stdout = %q, must not contain %q on this platform", out, notWant)
	}
}

// TestRunDaemonRestartNamesTheRecoveryCommandWhenRestoreFails pins R3: when
// neither the service manager nor the restore produces a socket, the error has
// to name `boss daemon start`. The reported incident's message pointed at
// `boss daemon status`, from which recovery is not discoverable.
func TestRunDaemonRestartNamesTheRecoveryCommandWhenRestoreFails(t *testing.T) {
	deps := newTestGroup()

	deps.restoreDaemonCommandStubs(t)
	_, socketPath := daemonRestartProfileFixture(t)

	deps.daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: true, PID: 4242}, nil
	}
	deps.defaultSocketPath = func() (string, error) { return socketPath, nil }
	deps.daemonSocketReachable = func(string) bool { return false }
	deps.daemonStop = func() error { return nil }
	deps.waitForDaemonSocketGone = func(string) bool { return true }
	deps.restartDaemon = func() error { return nil }
	deps.daemonEnsureRunning = func(string) error { return errors.New("no bossd binary") }
	deps.daemonRestartReadyTimeout = 20 * time.Millisecond
	deps.daemonRestartPollInterval = time.Millisecond

	err := deps.runDaemonRestart(&cobra.Command{})
	if err == nil {
		t.Fatal("runDaemonRestart returned nil, want an error naming the recovery command")
	}
	if !strings.Contains(err.Error(), "did not become reachable") {
		t.Fatalf("error = %v, want it to mention the socket never becoming reachable", err)
	}
	// The remediation has to name `boss daemon start`; it must NOT blanket-claim
	// the daemon is now stopped, which this path never verified — launchd still
	// reports PID 4242 here.
	if !strings.Contains(err.Error(), "boss daemon start") {
		t.Fatalf("error = %v, want it to name 'boss daemon start'", err)
	}
	if strings.Contains(err.Error(), "the daemon is now stopped") {
		t.Fatalf("error = %v, must not claim a stopped daemon it never probed", err)
	}
	// The failed restore's own error is the only diagnostic the operator gets.
	if !strings.Contains(err.Error(), "no bossd binary") {
		t.Fatalf("error = %v, want it to surface the direct-start failure", err)
	}
}

// TestRestartReachableDaemonForSettingsReloadTakesStandalonePathWhenStandaloneServed
// is half of R4: the settings-reload restart and `boss daemon restart` must
// decide standalone-vs-service-manager through the same helper, so an
// installed-and-running status that is nonetheless standalone-served does not
// get booted out here either.
func TestRestartReachableDaemonForSettingsReloadTakesStandalonePathWhenStandaloneServed(t *testing.T) {
	deps := newTestGroup()

	deps.restoreDaemonCommandStubs(t)
	deps.daemonStandaloneServed = func(*daemon.Status) bool { return true }

	var events []string
	err := restartReachableDaemonForSettingsReloadWith(
		"/tmp/boss.sock",
		func() (*daemon.Status, error) {
			return &daemon.Status{Installed: true, Running: true, PID: 0}, nil
		},
		deps.restartTakesStandalonePath,
		func() error {
			t.Fatal("a standalone-served daemon must not be booted out of launchd")
			return nil
		},
		func(path string) error {
			events = append(events, "ensure:"+path)
			return nil
		},
		func() (int, error) {
			events = append(events, "terminate-standalone")
			return 1, nil
		},
		func() bool {
			events = append(events, "process-exited")
			return true
		},
		func(path string) bool {
			events = append(events, "wait:"+path)
			return true
		},
	)
	if err != nil {
		t.Fatalf("restartReachableDaemonForSettingsReloadWith returned error: %v", err)
	}
	want := []string{"terminate-standalone", "wait:/tmp/boss.sock", "process-exited", "ensure:/tmp/boss.sock"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

// TestRestartDaemonAfterUpgradeKeepsAStandaloneServedDaemonStandalone is the
// other half of R4. The upgrade path already refused to change a user's
// service mode, but it keyed on Status.Installed, which is true here.
func TestRestartDaemonAfterUpgradeKeepsAStandaloneServedDaemonStandalone(t *testing.T) {
	deps := newTestGroup()

	deps.restoreDaemonCommandStubs(t)
	_, socketPath := daemonRestartProfileFixture(t)
	deps.daemonStandaloneServed = func(*daemon.Status) bool { return true }

	var events []string
	deps.defaultSocketPath = func() (string, error) { return socketPath, nil }
	deps.daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: true}, nil
	}
	deps.daemonStop = func() error {
		t.Fatal("upgrade must not boot a standalone-served daemon out of launchd")
		return nil
	}
	deps.restartDaemon = func() error {
		t.Fatal("upgrade must not bootstrap a LaunchAgent for a standalone-served daemon")
		return nil
	}
	deps.terminateCurrentProfileBossd = func() (int, error) {
		events = append(events, "terminate-standalone")
		return 1, nil
	}
	deps.waitForDaemonSocketGone = func(string) bool {
		events = append(events, "socket-gone")
		return true
	}
	deps.waitForStandaloneBossdExit = func(string) bool { return true }
	deps.daemonEnsureRunning = func(string) error {
		events = append(events, "start-standalone")
		return nil
	}
	deps.daemonSocketReachable = func(string) bool { return true }
	deps.daemonRestartReadyTimeout = 20 * time.Millisecond
	deps.daemonRestartPollInterval = time.Millisecond

	if err := deps.restartDaemonAfterUpgrade(); err != nil {
		t.Fatalf("restartDaemonAfterUpgrade: %v", err)
	}
	if want := []string{"terminate-standalone", "socket-gone", "start-standalone"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestRestartReachableDaemonForSettingsReloadReapsStandaloneWhenInstalledServiceInactive(t *testing.T) {
	deps := newTestGroup()

	var events []string

	err := restartReachableDaemonForSettingsReloadWith(
		"/tmp/boss.sock",
		func() (*daemon.Status, error) {
			events = append(events, "status")
			return &daemon.Status{Installed: true, Running: false}, nil
		},
		deps.restartTakesStandalonePath,
		func() error {
			t.Fatal("daemon.Stop should not be called for an inactive installed service")
			return nil
		},
		func(path string) error {
			events = append(events, "ensure:"+path)
			return nil
		},
		func() (int, error) {
			events = append(events, "terminate-standalone")
			return 1, nil
		},
		nil,
		func(path string) bool {
			events = append(events, "wait:"+path)
			return true
		},
	)
	if err != nil {
		t.Fatalf("restartReachableDaemonForSettingsReloadWith returned error: %v", err)
	}

	want := []string{"status", "terminate-standalone", "wait:/tmp/boss.sock", "ensure:/tmp/boss.sock"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

type fakeProcess struct {
	err error
}

func (deps *Group) restoreDaemonCommandStubs(t *testing.T) {
	t.Helper()

	oldDefaultSocketPath := deps.defaultSocketPath
	oldDaemonSocketReachable := deps.daemonSocketReachable
	oldDaemonGetStatus := deps.daemonGetStatus
	oldDaemonEnsureRunning := deps.daemonEnsureRunning
	oldDaemonEnsureRunningWithPolicy := deps.daemonEnsureRunningWithPolicy
	oldDaemonStop := deps.daemonStop
	oldRestartDaemon := deps.restartDaemon
	oldTerminateStandaloneCurrentProfile := deps.terminateStandaloneCurrentProfile
	oldTerminateAllBossdProcesses := deps.terminateAllBossdProcesses
	oldTerminateAllPluginProcesses := deps.terminateAllPluginProcesses
	oldFindBossdPluginPIDs := deps.daemonProcesses.FindPluginPIDs
	oldWaitForDaemonSocketGone := deps.waitForDaemonSocketGone
	oldWaitForStandaloneBossdExit := deps.waitForStandaloneBossdExit
	oldTerminateCurrentProfileBossd := deps.terminateCurrentProfileBossd
	oldDaemonStandaloneServed := deps.daemonStandaloneServed
	oldBossdProcessCommandLine := deps.daemonProcesses.CommandLine
	oldDaemonRestartReadyTimeout := deps.daemonRestartReadyTimeout
	oldDaemonRestartPollInterval := deps.daemonRestartPollInterval
	t.Cleanup(func() {
		deps.defaultSocketPath = oldDefaultSocketPath
		deps.daemonSocketReachable = oldDaemonSocketReachable
		deps.daemonGetStatus = oldDaemonGetStatus
		deps.daemonEnsureRunning = oldDaemonEnsureRunning
		deps.daemonEnsureRunningWithPolicy = oldDaemonEnsureRunningWithPolicy
		deps.daemonStop = oldDaemonStop
		deps.restartDaemon = oldRestartDaemon
		deps.terminateStandaloneCurrentProfile = oldTerminateStandaloneCurrentProfile
		deps.terminateAllBossdProcesses = oldTerminateAllBossdProcesses
		deps.terminateAllPluginProcesses = oldTerminateAllPluginProcesses
		deps.daemonProcesses.FindPluginPIDs = oldFindBossdPluginPIDs
		deps.waitForDaemonSocketGone = oldWaitForDaemonSocketGone
		deps.waitForStandaloneBossdExit = oldWaitForStandaloneBossdExit
		deps.terminateCurrentProfileBossd = oldTerminateCurrentProfileBossd
		deps.daemonStandaloneServed = oldDaemonStandaloneServed
		deps.daemonProcesses.CommandLine = oldBossdProcessCommandLine
		deps.daemonRestartReadyTimeout = oldDaemonRestartReadyTimeout
		deps.daemonRestartPollInterval = oldDaemonRestartPollInterval
	})
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	oldStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = writer
	var buf bytes.Buffer
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(&buf, reader)
		done <- err
	}()
	defer func() {
		os.Stdout = oldStdout
		_ = reader.Close()
	}()

	fn()

	if err := writer.Close(); err != nil {
		t.Fatalf("close stdout writer: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("copy stdout: %v", err)
	}
	return buf.String()
}

// TestObservedServingFactsRefusesUnverifiableAndForeignRecords pins the
// criterion-4 downgrade risk: the mirror image of BOS-1181. A daemon-state
// record must not produce a standalone verdict — and boot a correctly
// supervised daemon out of the service manager — unless it is both
// attributable to this profile's socket and liveness-verifiable.
func TestObservedServingFactsRefusesUnverifiableAndForeignRecords(t *testing.T) {
	deps := newTestGroup()

	const supervisedPID = 4242
	const recordedPID = 27923

	tests := []struct {
		name     string
		metadata func(socketPath string) daemonstate.Metadata
		wantPID  int
		wantMode daemon.ServingMode
	}{
		{
			// metadataMatchesRunningProcess short-circuits to true with no
			// probe when no executable is recorded. Trusting that here would
			// declare standalone on a stale PID nothing verified.
			name: "record naming no executable is not liveness-verifiable",
			metadata: func(socketPath string) daemonstate.Metadata {
				return daemonstate.Metadata{PID: recordedPID, SocketPath: socketPath}
			},
			wantPID:  recordedPID,
			wantMode: daemon.ServingModeSupervised,
		},
		{
			// Same app data dir, different socket: the record says nothing
			// about what is serving this profile.
			name: "record bound to a different socket is not this profile's",
			metadata: func(string) daemonstate.Metadata {
				return daemonstate.Metadata{
					PID:            recordedPID,
					ExecutablePath: "/opt/homebrew/bin/bossd",
					SocketPath:     "/tmp/some-other-profile/bossd.sock",
				}
			},
			wantPID:  0,
			wantMode: daemon.ServingModeSupervised,
		},
		{
			// The control: a complete, matching, live record still classifies
			// standalone, so the guards above are not simply disabling BOS-1181.
			name: "complete matching live record still reads standalone",
			metadata: func(socketPath string) daemonstate.Metadata {
				return daemonstate.Metadata{
					PID:            recordedPID,
					ExecutablePath: "/opt/homebrew/bin/bossd",
					SocketPath:     socketPath,
				}
			},
			wantPID:  recordedPID,
			wantMode: daemon.ServingModeStandalone,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			deps.restoreDaemonCommandStubs(t)
			appDataDir, socketPath := daemonRestartProfileFixture(t)
			deps.defaultSocketPath = func() (string, error) { return socketPath, nil }
			deps.daemonProcesses.CommandLine = func(int) (string, error) {
				return "/opt/homebrew/bin/bossd", nil
			}
			if err := daemonstate.Write(appDataDir, tc.metadata(socketPath)); err != nil {
				t.Fatalf("write daemon state: %v", err)
			}

			profile, err := deps.currentDaemonProfile()
			if err != nil {
				t.Fatalf("currentDaemonProfile: %v", err)
			}
			st := &daemon.Status{Installed: true, Running: true, PID: supervisedPID}
			facts := deps.observedServingFacts(st, profile)
			if facts.StandalonePID != tc.wantPID {
				t.Fatalf("StandalonePID = %d, want %d", facts.StandalonePID, tc.wantPID)
			}
			wantMode := tc.wantMode
			if !daemon.StandaloneServingSupported() {
				// Off macOS the standalone verdict is compiled out entirely.
				wantMode = daemon.ServingModeSupervised
			}
			if got := daemon.ClassifyServingMode(facts); got != wantMode {
				t.Fatalf("ClassifyServingMode = %q, want %q", got, wantMode)
			}
		})
	}
}

// TestRunDaemonRestartRefusesARejectedSupervisionModeWithoutStoppingTheDaemon
// pins the BOS-1184 regression fix.
//
// platformRestart's own fail-closed check is not enough on this path, and on
// its own it reproduced the exact failure BOS-1181 exists to prevent: the
// launchd branch stops the running daemon BEFORE calling restartDaemon(), whose
// error return exits immediately — the daemonEnsureRunning fallback is reachable
// only from waitForDaemonRestartReady, further down. A refused supervision mode
// therefore stopped a healthy supervised daemon and then declined to replace it.
//
// The refusal must happen before anything is stopped, so the host keeps the
// daemon it had. Asserting "no events" is the whole point: an implementation
// that refuses only after the stop still returns an error and would pass a test
// that checked the error alone.
func TestRunDaemonRestartRefusesARejectedSupervisionModeWithoutStoppingTheDaemon(t *testing.T) {
	deps := newTestGroup()

	deps.restoreDaemonCommandStubs(t)
	_, socketPath := daemonRestartProfileFixture(t)
	writeSupervisionModeSettings(t, "definitely-not-a-mode")

	var events []string
	deps.daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: true, PID: 4242}, nil
	}
	deps.defaultSocketPath = func() (string, error) { return socketPath, nil }
	deps.daemonSocketReachable = func(string) bool { return true }
	deps.daemonStop = func() error {
		events = append(events, "launchd-stop")
		return nil
	}
	deps.restartDaemon = func() error {
		events = append(events, "launchd-restart")
		return nil
	}
	deps.waitForDaemonSocketGone = func(string) bool {
		events = append(events, "socket-gone")
		return true
	}
	deps.terminateCurrentProfileBossd = func() (int, error) {
		events = append(events, "terminate-standalone")
		return 0, nil
	}
	deps.daemonEnsureRunning = func(string) error {
		events = append(events, "ensure-running")
		return nil
	}

	var err error
	captureStdout(t, func() { err = deps.runDaemonRestart(&cobra.Command{}) })

	if err == nil {
		t.Fatal("a rejected supervision mode must refuse the restart, not report success")
	}
	if !strings.Contains(err.Error(), "daemon left running") {
		t.Fatalf("error %q must tell the operator the daemon was left running", err)
	}
	if !strings.Contains(err.Error(), "definitely-not-a-mode") {
		t.Fatalf("error %q must name the rejected value", err)
	}
	if len(events) != 0 {
		t.Fatalf("nothing may be stopped or started when the mode is refused; got %v", events)
	}
}

// TestRunDaemonStopRoutesToWhatIsServing is the BOS-1217 finding-1 regression
// and BOS-1218 R3/R4.
//
// The reported host had a LaunchAgent launchd had registered and never spawned,
// and a detached bossd holding the profile socket. `boss daemon stop` read
// st.Running — true, because `launchctl list` exits 0 for a registration — took
// the launchd branch, booted out a job that owned no process, signalled
// nothing, and then polled the detached daemon's socket for the full
// LifecycleShutdownTimeout before failing. The second invocation succeeded.
//
// The verdicts here are the real daemon.ClassifyServingMode probe driven
// through the daemon-state record and the status the service manager reports,
// not a stubbed classifier, so what is under test is the routing rather than a
// restatement of it. The whole event slice is compared rather than asserting
// absences one at a time: that pins ordering AND that no other branch ran,
// which is what "bootout was not called" has to mean here.
//
// There is no wall-clock assertion. The 60s symptom is a consequence of
// entering waitForDaemonSocketGone after signalling nothing, so the events
// pin the branch and the absence of that wait directly.
func TestRunDaemonStopRoutesToWhatIsServing(t *testing.T) {
	deps := newTestGroup()

	if !daemon.StandaloneServingSupported() {
		t.Skip("standalone serving is a macOS mode; runDaemonStop's launchd branch is unchanged without it")
	}
	const standalonePID = 27923
	const launchdPID = 4242

	for _, tc := range []struct {
		name string
		// status is what the service manager reports. PIDKnown is stated on
		// every row: after BOS-1218 a Status that omits it claims its PID was
		// never established, which is a different host.
		status     daemon.Status
		record     func(t *testing.T, appDataDir, socketPath string)
		terminated int
		wantEvents []string
		wantOutput string
	}{
		{
			// The incident. Running is false on the FIRST invocation now,
			// because launchd reported no PID for the job it registered.
			name:   "a registered job that owns no process signals the standalone daemon",
			status: daemon.Status{Installed: true, Running: false, PID: 0, PIDKnown: true},
			record: func(t *testing.T, appDataDir, socketPath string) {
				deps.recordLiveStandaloneDaemon(t, appDataDir, socketPath, standalonePID)
			},
			terminated: 1,
			wantEvents: []string{"terminate-standalone", "socket-gone"},
			wantOutput: "Stopped standalone bossd for current profile.",
		},
		{
			// The mirror of the BOS-1181 restart case, and why `!st.Running`
			// alone was not enough: launchd owns a live PID, so the old
			// question answers "the service manager is running" while the
			// socket is held by someone else entirely.
			name:   "a live standalone daemon beside a spawned job is the one signalled",
			status: daemon.Status{Installed: true, Running: true, PID: launchdPID, PIDKnown: true},
			record: func(t *testing.T, appDataDir, socketPath string) {
				deps.recordLiveStandaloneDaemon(t, appDataDir, socketPath, standalonePID)
			},
			terminated: 1,
			wantEvents: []string{"terminate-standalone", "socket-gone"},
			wantOutput: "Stopped standalone bossd for current profile.",
		},
		{
			name:       "launchd owning the serving process still takes the bootout path",
			status:     daemon.Status{Installed: true, Running: true, PID: launchdPID, PIDKnown: true},
			record:     func(*testing.T, string, string) {},
			wantEvents: []string{"launchd-stop", "socket-gone"},
			wantOutput: "Daemon stopped.",
		},
		{
			// The daemon-state record bossd writes on EVERY startup, so a
			// launchd-spawned daemon records its own PID. Reading the record
			// alone would boot the supervised daemon out through the wrong
			// door; the PID identity is what separates them.
			name:   "a record naming the launchd PID is not a standalone daemon",
			status: daemon.Status{Installed: true, Running: true, PID: launchdPID, PIDKnown: true},
			record: func(t *testing.T, appDataDir, socketPath string) {
				deps.recordLiveStandaloneDaemon(t, appDataDir, socketPath, launchdPID)
			},
			wantEvents: []string{"launchd-stop", "socket-gone"},
			wantOutput: "Daemon stopped.",
		},
		{
			// observedServingFacts fails closed on a record that names no
			// executable: its liveness cannot be verified, and declaring
			// standalone on it would boot a correctly supervised daemon out of
			// the service manager.
			name:   "a record naming no executable fails closed onto the launchd path",
			status: daemon.Status{Installed: true, Running: true, PID: launchdPID, PIDKnown: true},
			record: func(t *testing.T, appDataDir, socketPath string) {
				if err := daemonstate.Write(appDataDir, daemonstate.Metadata{
					PID:        standalonePID,
					SocketPath: socketPath,
				}); err != nil {
					t.Fatalf("write daemon state: %v", err)
				}
			},
			wantEvents: []string{"launchd-stop", "socket-gone"},
			wantOutput: "Daemon stopped.",
		},
		{
			name:       "an installed service with nothing serving is already stopped",
			status:     daemon.Status{Installed: true, Running: false, PID: 0, PIDKnown: true},
			record:     func(*testing.T, string, string) {},
			wantEvents: []string{"terminate-standalone"},
			wantOutput: "Daemon is already stopped.",
		},
		{
			name:       "no service file and nothing standalone signals nothing",
			status:     daemon.Status{Installed: false},
			record:     func(*testing.T, string, string) {},
			wantEvents: []string{"terminate-standalone"},
			wantOutput: "Daemon is not installed and no standalone bossd is running.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps.restoreDaemonCommandStubs(t)
			appDataDir, socketPath := daemonRestartProfileFixture(t)
			tc.record(t, appDataDir, socketPath)

			var events []string
			status := tc.status
			deps.daemonGetStatus = func() (*daemon.Status, error) { return &status, nil }
			deps.defaultSocketPath = func() (string, error) { return socketPath, nil }
			deps.daemonStop = func() error {
				events = append(events, "launchd-stop")
				return nil
			}
			deps.terminateStandaloneCurrentProfile = func(profile daemonProfile) (int, error) {
				if profile.AppDataDir != appDataDir {
					t.Fatalf("profile app data dir = %q, want %q", profile.AppDataDir, appDataDir)
				}
				events = append(events, "terminate-standalone")
				return tc.terminated, nil
			}
			deps.waitForDaemonSocketGone = func(path string) bool {
				if path != socketPath {
					t.Fatalf("socket wait path = %q, want %q", path, socketPath)
				}
				events = append(events, "socket-gone")
				return true
			}

			out := captureStdout(t, func() {
				if err := deps.runDaemonStop(&cobra.Command{}); err != nil {
					t.Fatalf("runDaemonStop: %v", err)
				}
			})
			if !reflect.DeepEqual(events, tc.wantEvents) {
				t.Fatalf("events = %v, want %v", events, tc.wantEvents)
			}
			if !strings.Contains(out, tc.wantOutput) {
				t.Fatalf("output = %q, want %q", out, tc.wantOutput)
			}
		})
	}
}

// TestStopAndRestartAgreeOnTheServingMode is BOS-1218 U3: stop, restart and the
// reporting surfaces decide from one classifier, so they cannot drift into
// disagreeing about one host.
//
// The table is ServingFacts rather than Status because that is the shape
// daemon.ClassifyServingMode adjudicates and the shape
// daemon_supervision_test.go already pins status and doctor against. The
// claim asserted is the one that matters operationally: for a host with a
// service file, stop goes to the service manager exactly when the service
// manager is what is serving.
//
// Installed is excluded from that claim rather than asserted over, and
// deliberately: it is not a discriminator for ClassifyServingMode, while both
// stop and restart short-circuit on it because a profile with no service file
// has never had a service-manager strategy available.
//
// The `Running = true, ServiceManagerPID = 0` rows are the ones this table
// used to be silent about, and silence there was the wrong kind: it is a shape
// SYSTEMD produces today (`systemctl --user is-active` says active while the
// separate MainPID read fails), and it is the single shape on which the stop
// predicate and the classifier deliberately part company. Omitting it made the
// agreement claim look total when it is not. Each such row therefore states
// its divergence in `stopDivergesBecause` rather than being left out, so the
// exception is pinned and a future change to it fails here.
func TestStopAndRestartAgreeOnTheServingMode(t *testing.T) {
	deps := newTestGroup()

	const servicePID = 4242
	const standalonePID = 27923

	for _, tc := range []struct {
		name  string
		facts daemon.ServingFacts
		// stopDivergesBecause, when non-empty, records that stop routes this
		// row to the SERVICE MANAGER even though ClassifyServingMode does not
		// call it supervised. Empty means the two must agree.
		stopDivergesBecause string
		// needsStandaloneSupport marks a row whose expected verdict only holds
		// where a standalone serving mode exists at all. Off macOS
		// ClassifyServingMode cannot return Standalone, so such a row would be
		// asserting a different host than the one it names.
		needsStandaloneSupport bool
	}{
		{
			// Named for the finding so a future regression names itself.
			name: "BOS-1217 finding 1: registered job, no service PID, live standalone daemon",
			facts: daemon.ServingFacts{
				Installed: true, Running: false, ServiceManagerPID: 0,
				StandalonePID: standalonePID, StandaloneAlive: true,
			},
		},
		{
			name:  "installed, nothing running, nothing recorded",
			facts: daemon.ServingFacts{Installed: true},
		},
		{
			name:  "installed, nothing running, a dead standalone record",
			facts: daemon.ServingFacts{Installed: true, StandalonePID: standalonePID},
		},
		{
			name: "the service manager owns the serving process",
			facts: daemon.ServingFacts{
				Installed: true, Running: true, ServiceManagerPID: servicePID,
			},
		},
		{
			name: "the service manager owns the process that wrote the record",
			facts: daemon.ServingFacts{
				Installed: true, Running: true, ServiceManagerPID: servicePID,
				StandalonePID: servicePID, StandaloneAlive: true,
			},
		},
		{
			name: "the service manager owns a PID while a different live daemon is recorded",
			facts: daemon.ServingFacts{
				Installed: true, Running: true, ServiceManagerPID: servicePID,
				StandalonePID: standalonePID, StandaloneAlive: true,
			},
		},
		{
			name: "the service manager owns a PID and the record is stale",
			facts: daemon.ServingFacts{
				Installed: true, Running: true, ServiceManagerPID: servicePID,
				StandalonePID: standalonePID,
			},
		},
		{
			// systemd: `is-active` said active, the MainPID read failed. The
			// classifier's PID term makes this Unserved, but there IS a live
			// unit here and `systemctl --user stop` is the only thing that can
			// stop it — routing this to the standalone path would signal a
			// process nobody recorded and leave the active unit running. The
			// divergence is the classifier being conservative for RESTART's
			// question ("who is serving, and must be preserved") where stop
			// asks a different one ("what is there to stop").
			name: "systemd: the unit is active but its MainPID could not be read",
			facts: daemon.ServingFacts{
				Installed: true, Running: true, ServiceManagerPID: 0,
			},
			stopDivergesBecause: "there is a live service-manager job to stop even though no PID was reported",
		},
		{
			// Same systemd shape, but something standalone is recorded and
			// alive. Here the two agree again, because the standalone verdict
			// does not depend on the unreadable MainPID at all.
			name: "systemd: the unit is active with an unreadable MainPID while a live standalone daemon is recorded",
			facts: daemon.ServingFacts{
				Installed: true, Running: true, ServiceManagerPID: 0,
				StandalonePID: standalonePID, StandaloneAlive: true,
			},
			needsStandaloneSupport: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.needsStandaloneSupport && !daemon.StandaloneServingSupported() {
				t.Skipf("no standalone serving mode on this platform, so %q describes a host that cannot exist here", tc.name)
			}
			deps.restoreDaemonCommandStubs(t)
			facts := tc.facts
			facts.StandaloneSupported = daemon.StandaloneServingSupported()
			mode := daemon.ClassifyServingMode(facts)
			deps.daemonStandaloneServed = func(*daemon.Status) bool {
				return mode == daemon.ServingModeStandalone
			}

			st := &daemon.Status{
				Installed: facts.Installed,
				Running:   facts.Running,
				PID:       facts.ServiceManagerPID,
				// Derived rather than hardcoded so each row is a Status some
				// substrate can actually produce: launchd never reports
				// Running without a settled PID, and the systemd rows below
				// are Running with the PID read UNsettled.
				PIDKnown: !facts.Running || facts.ServiceManagerPID > 0,
			}
			gotStandalone := deps.stopTakesStandalonePath(st)
			wantStandalone := mode != daemon.ServingModeSupervised
			if tc.stopDivergesBecause != "" {
				if wantStandalone == false {
					t.Fatalf("row declares a divergence from %q but the classifier already routes it to the service manager; delete stopDivergesBecause", mode)
				}
				wantStandalone = false
			}
			if gotStandalone != wantStandalone {
				t.Fatalf("stopTakesStandalonePath = %v for %q, want %v; stop and the serving probe disagree about this host (declared divergence: %q)",
					gotStandalone, mode, wantStandalone, tc.stopDivergesBecause)
			}
			// Restart lacks the `!st.Running` term on purpose: an installed
			// service the manager has not started still has to be STARTED, so
			// restart routes it through the service manager where stop has
			// nothing to bootout. Everywhere else the two must agree.
			if st.Running && deps.restartTakesStandalonePath(st) != gotStandalone {
				t.Fatalf("stop takes standalone = %v but restart takes standalone = %v for %q",
					gotStandalone, deps.restartTakesStandalonePath(st), mode)
			}
		})
	}
}

// TestDaemonRestartEnvelope pins the `boss daemon restart --json` schema and
// its outcome vocabulary. Driven through emitDaemonRestart rather than end to
// end, because the surrounding handler stops and starts real daemons on the
// host; the parts under test here are the envelope and the code selection.
func TestDaemonRestartEnvelope(t *testing.T) {
	run := func(asJSON bool, outcome string, supervised bool, message string) (string, error) {
		cmd := &cobra.Command{Use: "restart"}
		var out bytes.Buffer
		cmd.SetOut(&out)
		err := emitDaemonRestart(cmd, asJSON, outcome, supervised, "/tmp/bossd.sock", message)
		return out.String(), err
	}

	stdout, err := run(true, cliutil.OutcomeDaemonRestartedUnsupervised, false, "Daemon restarted by starting bossd directly: …")
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	var env struct {
		Daemon struct {
			Outcome    string `json:"outcome"`
			Supervised bool   `json:"supervised"`
			SocketPath string `json:"socket_path"`
			Message    string `json:"message"`
		} `json:"daemon"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("stdout is not one JSON object: %v (stdout=%q)", err, stdout)
	}
	if env.Daemon.Outcome != "RESTARTED_UNSUPERVISED" {
		t.Errorf("outcome = %q, want RESTARTED_UNSUPERVISED", env.Daemon.Outcome)
	}
	if env.Daemon.Supervised {
		t.Error("supervised = true on the degraded fallback")
	}
	if env.Daemon.SocketPath != "/tmp/bossd.sock" {
		t.Errorf("socket_path = %q", env.Daemon.SocketPath)
	}
	if env.Daemon.Message == "" {
		t.Error("message should carry the same sentence the human path prints")
	}

	// Without --json the output is byte-identical to what it printed before the
	// flag existed — the human line goes to stdout via fmt.Println, so the
	// command's own buffer stays empty.
	humanStdout, err := run(false, cliutil.OutcomeDaemonRestarted, true, "Daemon restarted.")
	if err != nil {
		t.Fatalf("emit human: %v", err)
	}
	if humanStdout != "" {
		t.Errorf("the human path must not write the envelope, got %q", humanStdout)
	}
}

// TestRestartFallbackOutcomeIsProbedNotAssumed pins that the announced fallback
// does not automatically mean supervision was lost: the follow-up start can
// land under the service manager after all, and reporting that as a loss would
// be as wrong as reporting a real loss as an ordinary restart.
func TestRestartFallbackOutcomeIsProbedNotAssumed(t *testing.T) {
	if got := restartFallbackOutcome(false); got != cliutil.OutcomeDaemonRestartedUnsupervised {
		t.Errorf("unsupervised fallback = %q, want %q", got, cliutil.OutcomeDaemonRestartedUnsupervised)
	}
	if got := restartFallbackOutcome(true); got != cliutil.OutcomeDaemonRestartedAfterFallback {
		t.Errorf("supervised fallback = %q, want %q", got, cliutil.OutcomeDaemonRestartedAfterFallback)
	}
	// The four success outcomes are distinct, or a driver cannot branch on them.
	seen := map[string]bool{}
	for _, o := range []string{
		cliutil.OutcomeDaemonRestarted, cliutil.OutcomeDaemonRestartedStandalone,
		cliutil.OutcomeDaemonStartedStandalone, cliutil.OutcomeDaemonRestartedUnsupervised,
		cliutil.OutcomeDaemonRestartedAfterFallback,
	} {
		if seen[o] {
			t.Errorf("duplicate restart outcome code %q", o)
		}
		seen[o] = true
	}
}

// TestDaemonRestartRegistersJSONFlag proves the flag reached the real command
// tree, so the envelope is reachable rather than merely implemented.
func TestDaemonRestartRegistersJSONFlag(t *testing.T) {
	deps := newTestGroup()
	root := deps.warningRoot()
	restart, _, err := root.Find([]string{"daemon", "restart"})
	if err != nil {
		t.Fatalf("find daemon restart: %v", err)
	}
	if restart.Name() != "restart" {
		t.Fatalf("resolved %q, not the restart command", restart.Name())
	}
	if restart.Flags().Lookup(cliutil.JSONFlagName) == nil {
		t.Error("boss daemon restart registers no --json flag")
	}
}

func (p fakeProcess) Signal(os.Signal) error { return p.err }
