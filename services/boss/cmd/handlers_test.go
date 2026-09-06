package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/client"
	"github.com/recurser/boss/internal/daemon"
	"github.com/recurser/boss/internal/preflight"
	"github.com/recurser/boss/internal/views"
	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/daemonbin"
	"github.com/recurser/bossalib/daemonstate"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestBossdPgrepArgsRestrictsToEffectiveUser(t *testing.T) {
	got := bossdPgrepArgs()
	want := []string{"-u", strconv.Itoa(os.Geteuid()), "-x", "bossd"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bossdPgrepArgs() = %v, want %v", got, want)
	}
}

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
	restoreDaemonCommandStubs(t)
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
	defaultSocketPath = func() (string, error) { return socketPath, nil }

	profile, err := currentDaemonProfile()
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
	restoreDaemonCommandStubs(t)
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
	daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: true, PID: 99, ServicePath: "/tmp/service"}, nil
	}
	defaultSocketPath = func() (string, error) { return socketPath, nil }
	daemonSocketReachable = func(path string) bool {
		if path != socketPath {
			t.Fatalf("socket reachability path = %q, want %q", path, socketPath)
		}
		return true
	}

	out := captureStdout(t, func() {
		if err := runDaemonStatus(&cobra.Command{}); err != nil {
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
func writeDaemonStatusProfile(t *testing.T) string {
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
	defaultSocketPath = func() (string, error) { return socketPath, nil }
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
	restoreDaemonCommandStubs(t)
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
	socketPath := writeDaemonStatusProfile(t)
	daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: true, PID: daemonStatusRecordedPID, ServicePath: "/tmp/service"}, nil
	}
	probes := 0
	daemonSocketReachable = func(path string) bool {
		if path != socketPath {
			t.Fatalf("socket reachability path = %q, want %q", path, socketPath)
		}
		probes++
		return false
	}

	out := captureStdout(t, func() {
		if err := runDaemonStatus(&cobra.Command{}); err != nil {
			t.Fatalf("runDaemonStatus: %v", err)
		}
	})
	if strings.Contains(out, "Daemon is running.") {
		t.Fatalf("output = %q, must not claim the daemon is running while its socket is unreachable", out)
	}
	for _, want := range []string{
		"registered but not serving",
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
	restoreDaemonCommandStubs(t)
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
	writeDaemonStatusProfile(t)
	stubDaemonDoctorProcess(t, nil)
	daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: false, ServicePath: "/tmp/service"}, nil
	}
	daemonSocketReachable = func(string) bool { return true }

	out := captureStdout(t, func() {
		if err := runDaemonStatus(&cobra.Command{}); err != nil {
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
	restoreDaemonCommandStubs(t)
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
	writeDaemonStatusProfile(t)
	stubDaemonDoctorProcess(t, nil)
	daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: true, PID: daemonStatusRecordedPID, ServicePath: "/tmp/service"}, nil
	}
	daemonSocketReachable = func(string) bool { return true }

	out := captureStdout(t, func() {
		if err := runDaemonStatus(&cobra.Command{}); err != nil {
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
// reportDaemonSupervision's st.PID == 0 rung (services/boss/cmd/daemon_doctor.go):
// launchctl output that will not parse, or a systemd MainPID read that fails,
// leaves ownership unproven. Certifying supervision there would emit a false
// healthy verdict from the one line added to detect an ownership mismatch.
func TestRunDaemonStatusReportsUnknownSupervisionWithoutServicePID(t *testing.T) {
	restoreDaemonCommandStubs(t)
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
	writeDaemonStatusProfile(t)
	stubDaemonDoctorProcess(t, nil)
	daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: true, PID: 0, ServicePath: "/tmp/service"}, nil
	}
	daemonSocketReachable = func(string) bool { return true }

	out := captureStdout(t, func() {
		if err := runDaemonStatus(&cobra.Command{}); err != nil {
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
	restoreDaemonCommandStubs(t)
	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
	writeDaemonStatusProfile(t)
	stubDaemonDoctorProcess(t, syscall.ESRCH)
	daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: true, PID: daemonStatusRecordedPID + 1, ServicePath: "/tmp/service"}, nil
	}
	daemonSocketReachable = func(string) bool { return true }

	out := captureStdout(t, func() {
		if err := runDaemonStatus(&cobra.Command{}); err != nil {
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
	restoreDaemonCommandStubs(t)
	socketPath := filepath.Join(t.TempDir(), "bossd.sock")
	defaultSocketPath = func() (string, error) { return socketPath, nil }
	daemonSocketReachable = func(path string) bool {
		if path != socketPath {
			t.Fatalf("socket reachability path = %q, want %q", path, socketPath)
		}
		return true
	}
	daemonEnsureRunning = func(string) error {
		t.Fatal("daemonEnsureRunning called for reachable socket")
		return nil
	}
	daemonEnsureRunningWithMode = func(string) (daemon.StartMode, error) {
		t.Fatal("daemonEnsureRunningWithMode called for reachable socket")
		return daemon.StartModeUnknown, nil
	}

	out := captureStdout(t, func() {
		if err := runDaemonStart(&cobra.Command{}); err != nil {
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
	restoreDaemonCommandStubs(t)
	socketPath := filepath.Join(t.TempDir(), "bossd.sock")
	defaultSocketPath = func() (string, error) { return socketPath, nil }
	daemonSocketReachable = func(string) bool { return false }
	daemonEnsureRunningWithMode = func(path string) (daemon.StartMode, error) {
		if path != socketPath {
			t.Fatalf("start socket path = %q, want %q", path, socketPath)
		}
		return daemon.StartModeDetached, nil
	}

	out := captureStdout(t, func() {
		if err := runDaemonStart(&cobra.Command{}); err != nil {
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
		"boss daemon doctor",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want %q", out, want)
		}
	}
}

// TestDaemonUnsupervisedConsequencesAreOneSentenceEverywhere pins what the
// shared constant buys: three surfaces reported three different consequence
// sets for one fact, and the operator learned the least from the surface that
// tells them first. Asserting the constant is USED, rather than re-asserting
// the wording, is what keeps this from being a copy of the string.
func TestDaemonUnsupervisedConsequencesAreOneSentenceEverywhere(t *testing.T) {
	for _, want := range []string{"keychain", "unauthenticated", "will not survive reboot"} {
		if !strings.Contains(daemonUnsupervisedConsequences, want) {
			t.Fatalf("shared consequence sentence = %q, want it to name %q", daemonUnsupervisedConsequences, want)
		}
	}

	t.Setenv("BOSS_DAEMON_SKIP_LAUNCHCTL", "")
	stubDaemonDoctorProcess(t, nil)
	const recordedPID = 5150
	st := daemon.Status{Installed: true, Running: false}

	statusLine := daemonSupervisionLine(&st, recordedPID)
	if !strings.Contains(statusLine, daemonUnsupervisedConsequences) {
		t.Fatalf("status line = %q, want the shared consequence sentence", statusLine)
	}

	previous := daemonGetStatus
	daemonGetStatus = func() (*daemon.Status, error) { return &st, nil }
	t.Cleanup(func() { daemonGetStatus = previous })
	var doctorOut bytes.Buffer
	reportDaemonSupervision(&doctorOut, daemonstate.Metadata{PID: recordedPID}, nil)
	if !strings.Contains(doctorOut.String(), daemonUnsupervisedConsequences) {
		t.Fatalf("doctor output = %q, want the shared consequence sentence", doctorOut.String())
	}
}

// TestRunDaemonStartOmitsFallbackNoticeWhenSupervised is the other half: a
// warning printed on the healthy path is a warning operators stop reading.
func TestRunDaemonStartOmitsFallbackNoticeWhenSupervised(t *testing.T) {
	restoreDaemonCommandStubs(t)
	socketPath := filepath.Join(t.TempDir(), "bossd.sock")
	defaultSocketPath = func() (string, error) { return socketPath, nil }
	daemonSocketReachable = func(string) bool { return false }
	daemonEnsureRunningWithMode = func(string) (daemon.StartMode, error) {
		return daemon.StartModeServiceManager, nil
	}

	out := captureStdout(t, func() {
		if err := runDaemonStart(&cobra.Command{}); err != nil {
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

// TestDaemonStopAllStandaloneStillSweepsPlugins pins the one CLI path that is
// still allowed to signal plugin processes. `boss daemon stop --all-standalone`
// is the explicit cross-profile crash-cleanup escape hatch, so it must keep
// sweeping bossd-plugin-* processes (BOS-349). The plugin sweep is stubbed here
// so the test never invokes the real pgrep/SIGTERM path.
func TestDaemonStopAllStandaloneStillSweepsPlugins(t *testing.T) {
	restoreDaemonCommandStubs(t)
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
	daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: false, Running: false}, nil
	}
	defaultSocketPath = func() (string, error) { return socketPath, nil }
	terminateCalled := false
	terminateAllBossdProcesses = func() (int, error) {
		terminateCalled = true
		return 2, nil
	}
	pluginSweepCalled := false
	terminateAllPluginProcesses = func(daemonProfile) (int, error) {
		pluginSweepCalled = true
		return 3, nil
	}
	waitForDaemonSocketGone = func(path string) bool {
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
		if err := runDaemonStop(cmd); err != nil {
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
// --all-standalone may sweep. findBossdPluginPIDs is the discovery seam every
// sweep funnels through, so if a normal path never reaches it, it never sweeps.
func TestDaemonStopNeverSweepsPluginsOnNormalPaths(t *testing.T) {
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
			restoreDaemonCommandStubs(t)
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
			daemonGetStatus = func() (*daemon.Status, error) { return tc.status, nil }
			defaultSocketPath = func() (string, error) { return socketPath, nil }
			daemonStop = func() error { return nil }
			waitForDaemonSocketGone = func(string) bool { return true }

			swept := false
			findBossdPluginPIDs = func() ([]int, error) {
				swept = true
				return nil, nil
			}

			captureStdout(t, func() {
				if err := runDaemonStop(&cobra.Command{}); err != nil {
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
			restoreDaemonCommandStubs(t)
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
			daemonGetStatus = func() (*daemon.Status, error) { return tc.status, nil }
			defaultSocketPath = func() (string, error) { return socketPath, nil }
			daemonSocketReachable = func(path string) bool { return path == socketPath }
			waitForDaemonSocketGone = func(string) bool { return true }
			daemonRestartReadyTimeout = 200 * time.Millisecond
			daemonRestartPollInterval = time.Millisecond

			var events []string
			daemonStop = func() error {
				events = append(events, "stop")
				return nil
			}
			restartDaemon = func() error {
				events = append(events, "restart")
				return nil
			}
			terminateCurrentProfileBossd = func() (int, error) {
				events = append(events, "terminate-current-profile")
				return 1, nil
			}
			daemonEnsureRunning = func(path string) error {
				if path != socketPath {
					t.Fatalf("ensure path = %q, want %q", path, socketPath)
				}
				events = append(events, "start")
				return nil
			}

			swept := false
			findBossdPluginPIDs = func() ([]int, error) {
				swept = true
				return nil, nil
			}

			captureStdout(t, func() {
				if err := runDaemonRestart(&cobra.Command{}); err != nil {
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
	restoreDaemonCommandStubs(t)
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
	daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: true}, nil
	}
	defaultSocketPath = func() (string, error) { return socketPath, nil }
	// The new daemon never starts accepting connections, so the readiness loop
	// must exhaust its deadline and return an error.
	daemonSocketReachable = func(string) bool { return false }
	waitForDaemonSocketGone = func(string) bool { return true }
	daemonStop = func() error { return nil }
	findBossdPluginPIDs = func() ([]int, error) { return nil, nil }
	restartDaemon = func() error { return nil }
	daemonEnsureRunning = func(string) error { return nil }
	// Keep the readiness loop fast so the failure path doesn't wait 30s.
	daemonRestartReadyTimeout = 20 * time.Millisecond
	daemonRestartPollInterval = time.Millisecond

	err := runDaemonRestart(&cobra.Command{})
	if err == nil {
		t.Fatal("runDaemonRestart returned nil, want a not-reachable error")
	}
	if !strings.Contains(err.Error(), "did not become reachable") {
		t.Fatalf("error = %v, want it to mention socket not reachable", err)
	}
}

func TestRunDaemonRestartWaitsForStandaloneExitBeforeStartingReplacement(t *testing.T) {
	restoreDaemonCommandStubs(t)
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
	daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: false}, nil
	}
	defaultSocketPath = func() (string, error) { return socketPath, nil }
	terminateStandaloneCurrentProfile = func(profile daemonProfile) (int, error) {
		if profile.AppDataDir != appDataDir {
			t.Fatalf("profile app data dir = %q, want %q", profile.AppDataDir, appDataDir)
		}
		events = append(events, "terminate")
		return 1, nil
	}
	waitForDaemonSocketGone = func(path string) bool {
		if path != socketPath {
			t.Fatalf("socket path = %q, want %q", path, socketPath)
		}
		events = append(events, "socket-gone")
		return true
	}
	waitForStandaloneBossdExit = func(dir string) bool {
		if dir != appDataDir {
			t.Fatalf("state dir = %q, want %q", dir, appDataDir)
		}
		events = append(events, "process-exited")
		return true
	}
	daemonEnsureRunning = func(path string) error {
		if path != socketPath {
			t.Fatalf("ensure path = %q, want %q", path, socketPath)
		}
		events = append(events, "start")
		return nil
	}

	if err := runDaemonRestart(&cobra.Command{}); err != nil {
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
func recordLiveStandaloneDaemon(t *testing.T, appDataDir, socketPath string, pid int) {
	t.Helper()
	const executable = "/opt/homebrew/bin/bossd"
	if err := daemonstate.Write(appDataDir, daemonstate.Metadata{
		PID:            pid,
		ExecutablePath: executable,
		SocketPath:     socketPath,
	}); err != nil {
		t.Fatalf("write daemon state: %v", err)
	}
	bossdProcessCommandLine = func(queried int) (string, error) {
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
	if !daemon.StandaloneServingSupported() {
		t.Skip("standalone serving is a macOS mode; the sibling test pins the unchanged path elsewhere")
	}
	restoreDaemonCommandStubs(t)
	appDataDir, socketPath := daemonRestartProfileFixture(t)
	recordLiveStandaloneDaemon(t, appDataDir, socketPath, 27923)

	var events []string
	serving := true
	// Installed AND Running are both true here, and neither is a statement
	// about what is serving: the plist exists and launchctl accepted the job,
	// but launchd never spawned it, so there is no PID.
	daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: true}, nil
	}
	defaultSocketPath = func() (string, error) { return socketPath, nil }
	daemonSocketReachable = func(string) bool { return serving }
	daemonStop = func() error {
		events = append(events, "launchd-stop")
		serving = false
		return nil
	}
	restartDaemon = func() error {
		// launchd accepts the bootstrap and still spawns nothing — the half of
		// the incident that made the outcome worse than doing nothing.
		events = append(events, "launchd-restart")
		return nil
	}
	terminateStandaloneCurrentProfile = func(profile daemonProfile) (int, error) {
		events = append(events, "terminate-standalone")
		serving = false
		return 1, nil
	}
	waitForDaemonSocketGone = func(string) bool {
		events = append(events, "socket-gone")
		return true
	}
	waitForStandaloneBossdExit = func(string) bool {
		events = append(events, "process-exited")
		return true
	}
	daemonEnsureRunning = func(string) error {
		events = append(events, "start-standalone")
		serving = true
		return nil
	}
	daemonRestartReadyTimeout = 20 * time.Millisecond
	daemonRestartPollInterval = time.Millisecond

	captureStdout(t, func() {
		if err := runDaemonRestart(&cobra.Command{}); err != nil {
			t.Fatalf("runDaemonRestart: %v", err)
		}
	})

	want := []string{"terminate-standalone", "socket-gone", "process-exited", "start-standalone"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v (a standalone-served daemon must restart in kind, never through launchd)", events, want)
	}
	if !daemonSocketReachable(socketPath) {
		t.Fatal("restart ended with no reachable socket; it began with one")
	}
}

// TestRunDaemonRestartLeavesServiceManagerPathUnchangedWithoutStandaloneMode
// is the other half of the platform seam: where there is no standalone serving
// mode (Linux), the very same inputs must still restart through the service
// manager. BOS-1181 deliberately does not change Linux behaviour.
func TestRunDaemonRestartLeavesServiceManagerPathUnchangedWithoutStandaloneMode(t *testing.T) {
	if daemon.StandaloneServingSupported() {
		t.Skip("this platform has a standalone serving mode; the sibling test covers it")
	}
	restoreDaemonCommandStubs(t)
	appDataDir, socketPath := daemonRestartProfileFixture(t)
	recordLiveStandaloneDaemon(t, appDataDir, socketPath, 27923)

	var events []string
	daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: true}, nil
	}
	defaultSocketPath = func() (string, error) { return socketPath, nil }
	daemonSocketReachable = func(string) bool { return true }
	daemonStop = func() error {
		events = append(events, "service-stop")
		return nil
	}
	restartDaemon = func() error {
		events = append(events, "service-restart")
		return nil
	}
	terminateStandaloneCurrentProfile = func(daemonProfile) (int, error) {
		t.Fatal("the standalone path must not be reachable on a platform without a standalone serving mode")
		return 0, nil
	}
	waitForDaemonSocketGone = func(string) bool {
		events = append(events, "socket-gone")
		return true
	}
	daemonEnsureRunning = func(string) error { return nil }
	daemonRestartReadyTimeout = 20 * time.Millisecond
	daemonRestartPollInterval = time.Millisecond

	captureStdout(t, func() {
		if err := runDaemonRestart(&cobra.Command{}); err != nil {
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
	restoreDaemonCommandStubs(t)
	appDataDir, socketPath := daemonRestartProfileFixture(t)
	const launchdPID = 4242
	recordLiveStandaloneDaemon(t, appDataDir, socketPath, launchdPID)

	var events []string
	daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: true, PID: launchdPID}, nil
	}
	defaultSocketPath = func() (string, error) { return socketPath, nil }
	daemonSocketReachable = func(string) bool { return true }
	daemonStop = func() error {
		events = append(events, "launchd-stop")
		return nil
	}
	restartDaemon = func() error {
		events = append(events, "launchd-restart")
		return nil
	}
	waitForDaemonSocketGone = func(string) bool {
		events = append(events, "socket-gone")
		return true
	}
	terminateStandaloneCurrentProfile = func(daemonProfile) (int, error) {
		t.Fatal("a launchd-served daemon must not be restarted as a standalone one")
		return 0, nil
	}
	daemonEnsureRunning = func(string) error {
		t.Fatal("a launchd-served daemon must not be started standalone while launchd can serve it")
		return nil
	}
	daemonRestartReadyTimeout = 20 * time.Millisecond
	daemonRestartPollInterval = time.Millisecond

	captureStdout(t, func() {
		if err := runDaemonRestart(&cobra.Command{}); err != nil {
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
	restoreDaemonCommandStubs(t)
	appDataDir, socketPath := daemonRestartProfileFixture(t)

	var events []string
	serving := true
	daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: true, PID: 4242}, nil
	}
	defaultSocketPath = func() (string, error) { return socketPath, nil }
	daemonSocketReachable = func(string) bool { return serving }
	daemonStop = func() error {
		events = append(events, "launchd-stop")
		serving = false
		return nil
	}
	waitForDaemonSocketGone = func(string) bool { return true }
	restartDaemon = func() error {
		events = append(events, "launchd-restart")
		return nil
	}
	daemonEnsureRunning = func(string) error {
		events = append(events, "restore")
		// The fallback really did leave a directly-spawned bossd serving, so
		// the record it writes is what the announcement must be read from.
		// Off macOS that record is compiled out of the verdict, which is the
		// whole point of the platform-split assertion below.
		recordLiveStandaloneDaemon(t, appDataDir, socketPath, 27923)
		serving = true
		return nil
	}
	daemonRestartReadyTimeout = 20 * time.Millisecond
	daemonRestartPollInterval = time.Millisecond

	out := captureStdout(t, func() {
		if err := runDaemonRestart(&cobra.Command{}); err != nil {
			t.Fatalf("runDaemonRestart: %v", err)
		}
	})

	if want := []string{"launchd-stop", "launchd-restart", "restore"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	if !daemonSocketReachable(socketPath) {
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
	restoreDaemonCommandStubs(t)
	_, socketPath := daemonRestartProfileFixture(t)

	daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: true, PID: 4242}, nil
	}
	defaultSocketPath = func() (string, error) { return socketPath, nil }
	daemonSocketReachable = func(string) bool { return false }
	daemonStop = func() error { return nil }
	waitForDaemonSocketGone = func(string) bool { return true }
	restartDaemon = func() error { return nil }
	daemonEnsureRunning = func(string) error { return errors.New("no bossd binary") }
	daemonRestartReadyTimeout = 20 * time.Millisecond
	daemonRestartPollInterval = time.Millisecond

	err := runDaemonRestart(&cobra.Command{})
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
	restoreDaemonCommandStubs(t)
	daemonStandaloneServed = func(*daemon.Status) bool { return true }

	var events []string
	err := restartReachableDaemonForSettingsReloadWith(
		"/tmp/boss.sock",
		func() (*daemon.Status, error) {
			return &daemon.Status{Installed: true, Running: true, PID: 0}, nil
		},
		restartTakesStandalonePath,
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
	restoreDaemonCommandStubs(t)
	_, socketPath := daemonRestartProfileFixture(t)
	daemonStandaloneServed = func(*daemon.Status) bool { return true }

	var events []string
	defaultSocketPath = func() (string, error) { return socketPath, nil }
	daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: true}, nil
	}
	daemonStop = func() error {
		t.Fatal("upgrade must not boot a standalone-served daemon out of launchd")
		return nil
	}
	restartDaemon = func() error {
		t.Fatal("upgrade must not bootstrap a LaunchAgent for a standalone-served daemon")
		return nil
	}
	terminateCurrentProfileBossd = func() (int, error) {
		events = append(events, "terminate-standalone")
		return 1, nil
	}
	waitForDaemonSocketGone = func(string) bool {
		events = append(events, "socket-gone")
		return true
	}
	waitForStandaloneBossdExit = func(string) bool { return true }
	daemonEnsureRunning = func(string) error {
		events = append(events, "start-standalone")
		return nil
	}
	daemonSocketReachable = func(string) bool { return true }
	daemonRestartReadyTimeout = 20 * time.Millisecond
	daemonRestartPollInterval = time.Millisecond

	if err := restartDaemonAfterUpgrade(); err != nil {
		t.Fatalf("restartDaemonAfterUpgrade: %v", err)
	}
	if want := []string{"terminate-standalone", "socket-gone", "start-standalone"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestRunLocalProviderStartupBeforeClientRestartsReachableDaemonAfterLoginShellChange(t *testing.T) {
	oldRunProviderStartupIfNeeded := runProviderStartupIfNeeded
	oldRestartDaemonAfterLoginShellCapture := restartDaemonAfterLoginShellCapture
	defer func() {
		runProviderStartupIfNeeded = oldRunProviderStartupIfNeeded
		restartDaemonAfterLoginShellCapture = oldRestartDaemonAfterLoginShellCapture
	}()

	t.Setenv("BOSS_SOCKET", "")
	var events []string
	runProviderStartupIfNeeded = func() (views.ProviderStartupResult, error) {
		events = append(events, "provider-startup")
		return views.ProviderStartupResult{LoginShellChanged: true}, nil
	}
	restartDaemonAfterLoginShellCapture = func() error {
		events = append(events, "restart")
		return nil
	}

	if err := runLocalProviderStartupBeforeClient(); err != nil {
		t.Fatalf("runLocalProviderStartupBeforeClient: %v", err)
	}
	events = append(events, "new-client")

	want := []string{"provider-startup", "restart", "new-client"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestRunLocalProviderStartupBeforeClientRestartsReachableDaemonAfterSettingsChange(t *testing.T) {
	oldRunProviderStartupIfNeeded := runProviderStartupIfNeeded
	oldRestartDaemonAfterLoginShellCapture := restartDaemonAfterLoginShellCapture
	defer func() {
		runProviderStartupIfNeeded = oldRunProviderStartupIfNeeded
		restartDaemonAfterLoginShellCapture = oldRestartDaemonAfterLoginShellCapture
	}()

	var events []string
	runProviderStartupIfNeeded = func() (views.ProviderStartupResult, error) {
		events = append(events, "provider-startup")
		return views.ProviderStartupResult{SettingsChanged: true}, nil
	}
	restartDaemonAfterLoginShellCapture = func() error {
		events = append(events, "restart")
		return nil
	}

	if err := runLocalProviderStartupBeforeClient(); err != nil {
		t.Fatalf("runLocalProviderStartupBeforeClient: %v", err)
	}

	want := []string{"provider-startup", "restart"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestRunLocalProviderStartupBeforeClientDoesNotRestartWhenDaemonNotReachable(t *testing.T) {
	oldRunProviderStartupIfNeeded := runProviderStartupIfNeeded
	oldDefaultSocketPath := defaultSocketPath
	oldDaemonSocketReachable := daemonSocketReachable
	defer func() {
		runProviderStartupIfNeeded = oldRunProviderStartupIfNeeded
		defaultSocketPath = oldDefaultSocketPath
		daemonSocketReachable = oldDaemonSocketReachable
	}()

	t.Setenv("BOSS_SOCKET", "")
	runProviderStartupIfNeeded = func() (views.ProviderStartupResult, error) {
		return views.ProviderStartupResult{LoginShellChanged: true}, nil
	}
	defaultSocketPath = func() (string, error) {
		return "/tmp/boss.sock", nil
	}
	daemonSocketReachable = func(string) bool {
		return false
	}

	if err := restartDaemonAfterLoginShellCapture(); err != nil {
		t.Fatalf("restartDaemonAfterLoginShellCapture: %v", err)
	}
}

func TestRunLocalProviderStartupBeforeClientRestartsBeforeReturningProviderError(t *testing.T) {
	oldRunProviderStartupIfNeeded := runProviderStartupIfNeeded
	oldRestartDaemonAfterLoginShellCapture := restartDaemonAfterLoginShellCapture
	defer func() {
		runProviderStartupIfNeeded = oldRunProviderStartupIfNeeded
		restartDaemonAfterLoginShellCapture = oldRestartDaemonAfterLoginShellCapture
	}()

	providerErr := errors.New("provider startup cancelled after login shell capture")
	var events []string
	runProviderStartupIfNeeded = func() (views.ProviderStartupResult, error) {
		events = append(events, "provider-startup")
		return views.ProviderStartupResult{LoginShellChanged: true}, providerErr
	}
	restartDaemonAfterLoginShellCapture = func() error {
		events = append(events, "restart")
		return nil
	}

	err := runLocalProviderStartupBeforeClient()
	if !errors.Is(err, providerErr) {
		t.Fatalf("error = %v, want %v", err, providerErr)
	}
	want := []string{"provider-startup", "restart"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestNeedsLocalDaemonStartup(t *testing.T) {
	t.Run("local default", func(t *testing.T) {
		t.Setenv("BOSS_SOCKET", "")

		if !needsLocalDaemonStartup(&cobra.Command{Use: "boss"}) {
			t.Fatal("expected local daemon startup for default local command")
		}
	})

	t.Run("remote", func(t *testing.T) {
		t.Setenv("BOSS_SOCKET", "")

		if needsLocalDaemonStartup(remoteTestCommand(t)) {
			t.Fatal("remote command should not run local daemon startup")
		}
	})

	t.Run("host", func(t *testing.T) {
		t.Setenv("BOSS_SOCKET", "")

		if needsLocalDaemonStartup(hostTestCommand(t)) {
			t.Fatal("--host command should not run local daemon startup")
		}
	})

	t.Run("explicit socket", func(t *testing.T) {
		t.Setenv("BOSS_SOCKET", "/tmp/boss.sock")

		if needsLocalDaemonStartup(&cobra.Command{Use: "boss"}) {
			t.Fatal("explicit BOSS_SOCKET should not run local daemon startup")
		}
	})
}

// TestNeedsLocalTmux pins which targets the missing-tmux install screen may
// block. It is deliberately NOT the same predicate as needsLocalDaemonStartup:
// three of the four cases below agree with that one and the --remote case does
// not, so folding the two together would silently stop requiring tmux for a
// cloud session that attaches through the local tmux server.
func TestNeedsLocalTmux(t *testing.T) {
	t.Run("local default needs it", func(t *testing.T) {
		if !needsLocalTmux(&cobra.Command{Use: "boss"}) {
			t.Fatal("a local session attaches through the local tmux and must still require it")
		}
	})

	t.Run("host does not", func(t *testing.T) {
		if needsLocalTmux(hostTestCommand(t)) {
			t.Fatal("--host attaches over ssh to the REMOTE tmux; requiring a local one blocks a valid client machine behind the install screen")
		}
	})

	t.Run("remote still does", func(t *testing.T) {
		if !needsLocalTmux(remoteTestCommand(t)) {
			t.Fatal("--remote has no ssh destination, so buildAttachCommand returns the LOCAL tmux form: the check must stay")
		}
	})

	t.Run("explicit socket still does", func(t *testing.T) {
		t.Setenv("BOSS_SOCKET", "/tmp/boss.sock")

		if !needsLocalTmux(&cobra.Command{Use: "boss"}) {
			t.Fatal("BOSS_SOCKET names a LOCAL daemon, whose tmux server is local like any other")
		}
	})
}

func TestRestartReachableDaemonForSettingsReloadReapsStandaloneWhenInstalledServiceInactive(t *testing.T) {
	var events []string

	err := restartReachableDaemonForSettingsReloadWith(
		"/tmp/boss.sock",
		func() (*daemon.Status, error) {
			events = append(events, "status")
			return &daemon.Status{Installed: true, Running: false}, nil
		},
		restartTakesStandalonePath,
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

func TestLaunchSettingsDoesNotSaveInstalledAtWhenLoadFails(t *testing.T) {
	oldLoadSettings := loadSettings
	oldSaveSettings := saveSettings
	defer func() {
		loadSettings = oldLoadSettings
		saveSettings = oldSaveSettings
	}()

	settings := config.DefaultSettings()
	settings.BossCloudGuestOfferHidden = true
	loadSettings = func() (config.Settings, error) {
		return settings, errors.New("corrupt settings")
	}
	saveSettings = func(config.Settings) error {
		t.Fatal("saveSettings called after load error")
		return nil
	}

	got := launchSettings(time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC))

	if !got.BossCloudGuestOfferHidden {
		t.Fatal("launchSettings did not return loaded runtime settings")
	}
}

func TestLaunchSettingsSavesWhenInstalledAtMissing(t *testing.T) {
	oldLoadSettings := loadSettings
	oldSaveSettings := saveSettings
	defer func() {
		loadSettings = oldLoadSettings
		saveSettings = oldSaveSettings
	}()

	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.FixedZone("JST", 9*60*60))
	settings := config.DefaultSettings()
	loadSettings = func() (config.Settings, error) {
		return settings, nil
	}
	var saved config.Settings
	saveCalls := 0
	saveSettings = func(s config.Settings) error {
		saveCalls++
		saved = s
		return nil
	}

	got := launchSettings(now)

	if saveCalls != 1 {
		t.Fatalf("saveSettings calls = %d, want 1", saveCalls)
	}
	if !saved.InstalledAt.Equal(now.UTC()) {
		t.Fatalf("saved InstalledAt = %s, want %s", saved.InstalledAt, now.UTC())
	}
	if !got.InstalledAt.Equal(now.UTC()) {
		t.Fatalf("returned InstalledAt = %s, want %s", got.InstalledAt, now.UTC())
	}
}

func TestEnabledAgentProvidersUsesLoadedAgentMetadata(t *testing.T) {
	settings := config.Settings{
		Plugins: []config.PluginConfig{
			{Name: "opencode", Enabled: true},
			{Name: "repair", Enabled: true},
			{Name: "codex", Enabled: true},
			{Name: "claude", Enabled: false},
		},
	}
	agents := []client.AgentInfo{
		{Name: "opencode"},
		{Name: "codex"},
		{Name: "claude"},
	}

	got := enabledAgentProviders(settings, agents, "")

	if !reflect.DeepEqual(got, []string{"codex"}) {
		t.Fatalf("enabledAgentProviders = %v, want [codex]", got)
	}
}

func TestEnabledAgentProvidersFiltersSelectedSessionAgent(t *testing.T) {
	settings := config.Settings{
		Plugins: []config.PluginConfig{
			{Name: "claude", Enabled: true},
			{Name: "codex", Enabled: true},
			{Name: "codex", Enabled: true},
		},
	}
	agents := []client.AgentInfo{
		{Name: "claude"},
		{Name: "codex"},
	}

	got := enabledAgentProviders(settings, agents, "codex")

	if !reflect.DeepEqual(got, []string{"codex"}) {
		t.Fatalf("enabledAgentProviders = %v, want [codex]", got)
	}
}

func TestEnabledAgentProvidersRequiresEnabledLoadedCLIBackedProvider(t *testing.T) {
	settings := config.Settings{
		Plugins: []config.PluginConfig{
			{Name: "claude", Enabled: false},
			{Name: "codex", Enabled: true},
			{Name: "opencode", Enabled: true},
			{Name: "repair", Enabled: true},
		},
	}
	agents := []client.AgentInfo{
		{Name: "claude"},
		{Name: "opencode"},
		{Name: "repair"},
	}

	got := enabledAgentProviders(settings, agents, "")

	if len(got) != 0 {
		t.Fatalf("enabledAgentProviders = %v, want empty", got)
	}
}

func TestRunAgentPreflightsUsesSelectedSessionWorktree(t *testing.T) {
	oldCheck := checkAgentsResolvableForPreflight
	defer func() { checkAgentsResolvableForPreflight = oldCheck }()

	stub := &agentPreflightStub{
		agents: []client.AgentInfo{{Name: "codex"}},
		session: &pb.Session{
			Id:           "target-session",
			WorktreePath: "/tmp/target-worktree",
			AgentName:    "codex",
		},
		resolveResp: &pb.ResolveContextResponse{
			Session: &pb.Session{
				Id:           "cwd-session",
				WorktreePath: "/tmp/cwd-worktree",
				AgentName:    "codex",
			},
		},
	}
	settings := config.Settings{
		LoginShell: "/bin/sh",
		Plugins: []config.PluginConfig{
			{Name: "claude", Enabled: true},
			{Name: "codex", Enabled: true},
		},
	}

	var checkedAgents []string
	var checkedWorktree string
	checkAgentsResolvableForPreflight = func(_ string, agents []string, worktree string) *preflight.Issue {
		checkedAgents = agents
		checkedWorktree = worktree
		return nil
	}

	if err := runAgentPreflights(context.Background(), nil, stub, settings, "target-session"); err != nil {
		t.Fatalf("runAgentPreflights returned error: %v", err)
	}
	if stub.getSessionID != "target-session" {
		t.Fatalf("GetSession called with %q, want target-session", stub.getSessionID)
	}
	if stub.resolveCalled {
		t.Fatal("ResolveContext should not be called when attach session is selected")
	}
	if !slices.Equal(checkedAgents, []string{"codex"}) {
		t.Fatalf("checked agents = %v, want [codex]", checkedAgents)
	}
	if checkedWorktree != "/tmp/target-worktree" {
		t.Fatalf("checked worktree = %q, want /tmp/target-worktree", checkedWorktree)
	}
}

// TestRunAgentPreflightsSkipsRemoteTransports: the agent probe shells out on
// *this* machine, so under --host (and --remote) it would look for a CLI that
// lives on the other end and block the TUI over it. The daemon must not be
// asked anything either — a local cwd means nothing to a remote ResolveContext.
func TestRunAgentPreflightsSkipsRemoteTransports(t *testing.T) {
	oldCheck := checkAgentsResolvableForPreflight
	defer func() { checkAgentsResolvableForPreflight = oldCheck }()

	settings := config.Settings{
		LoginShell: "/bin/sh",
		Plugins:    []config.PluginConfig{{Name: "claude", Enabled: true}},
	}

	cases := []struct {
		name string
		cmd  *cobra.Command
	}{
		{"host", hostTestCommand(t)},
		{"remote", remoteTestCommand(t)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &agentPreflightStub{
				agents:  []client.AgentInfo{{Name: "claude"}},
				session: &pb.Session{Id: "s", WorktreePath: "/remote/worktree", AgentName: "claude"},
			}
			checkAgentsResolvableForPreflight = func(_ string, agents []string, worktree string) *preflight.Issue {
				t.Fatalf("the local agent probe must not run: agents=%v worktree=%q", agents, worktree)
				return nil
			}

			if err := runAgentPreflights(context.Background(), tc.cmd, stub, settings, "s"); err != nil {
				t.Fatalf("runAgentPreflights: %v", err)
			}
			if stub.listCalled {
				t.Fatal("ListAgents must not be called for a non-local transport")
			}
			if stub.getSessionID != "" || stub.resolveCalled {
				t.Fatal("no session lookup should happen for a non-local transport")
			}
		})
	}
}

func TestRunAgentPreflightsSkipsNonCLIBackedPlugins(t *testing.T) {
	oldCheck := checkAgentsResolvableForPreflight
	defer func() { checkAgentsResolvableForPreflight = oldCheck }()

	stub := &agentPreflightStub{
		agents: []client.AgentInfo{{Name: "opencode"}},
		session: &pb.Session{
			Id:           "target-session",
			WorktreePath: "/tmp/target-worktree",
			AgentName:    "opencode",
		},
	}
	settings := config.Settings{
		LoginShell: "/bin/sh",
		Plugins: []config.PluginConfig{
			{Name: "opencode", Enabled: true},
		},
	}

	probed := false
	checkAgentsResolvableForPreflight = func(_ string, agents []string, worktree string) *preflight.Issue {
		if len(agents) > 0 {
			probed = true
			t.Fatalf("non-CLI plugin should not be probed: agents=%v worktree=%q", agents, worktree)
		}
		return nil
	}

	if err := runAgentPreflights(context.Background(), nil, stub, settings, "target-session"); err != nil {
		t.Fatalf("runAgentPreflights returned error: %v", err)
	}
	if probed {
		t.Fatal("non-CLI plugin was probed")
	}
}

type agentPreflightStub struct {
	agents        []client.AgentInfo
	session       *pb.Session
	resolveResp   *pb.ResolveContextResponse
	getSessionID  string
	resolveCalled bool
	listCalled    bool
}

func (s *agentPreflightStub) ListAgents(context.Context) ([]client.AgentInfo, error) {
	s.listCalled = true
	return s.agents, nil
}

func (s *agentPreflightStub) GetSession(_ context.Context, id string, _ client.SessionReadOptions) (*pb.Session, error) {
	s.getSessionID = id
	return s.session, nil
}

func (s *agentPreflightStub) ResolveContext(context.Context, string) (*pb.ResolveContextResponse, error) {
	s.resolveCalled = true
	return s.resolveResp, nil
}

func TestParsePgrepOutput(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []int
	}{
		{
			name: "single PID",
			in:   "12345\n",
			want: []int{12345},
		},
		{
			name: "multiple PIDs",
			in:   "100\n200\n300\n",
			want: []int{100, 200, 300},
		},
		{
			name: "empty pgrep output",
			in:   "",
			want: nil,
		},
		{
			name: "blank trailing lines tolerated",
			in:   "\n\n42\n\n",
			want: []int{42},
		},
		{
			name: "non-numeric lines skipped",
			in:   "42\nnot a pid\n99\n",
			want: []int{42, 99},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parsePgrepOutput(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parsePgrepOutput(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

type fakeProcess struct {
	err error
}

func (p fakeProcess) Signal(os.Signal) error {
	return p.err
}

func restoreDaemonCommandStubs(t *testing.T) {
	t.Helper()

	oldDefaultSocketPath := defaultSocketPath
	oldDaemonSocketReachable := daemonSocketReachable
	oldDaemonGetStatus := daemonGetStatus
	oldDaemonEnsureRunning := daemonEnsureRunning
	oldDaemonEnsureRunningWithMode := daemonEnsureRunningWithMode
	oldDaemonStop := daemonStop
	oldRestartDaemon := restartDaemon
	oldTerminateStandaloneCurrentProfile := terminateStandaloneCurrentProfile
	oldTerminateAllBossdProcesses := terminateAllBossdProcesses
	oldTerminateAllPluginProcesses := terminateAllPluginProcesses
	oldFindBossdPluginPIDs := findBossdPluginPIDs
	oldWaitForDaemonSocketGone := waitForDaemonSocketGone
	oldWaitForStandaloneBossdExit := waitForStandaloneBossdExit
	oldTerminateCurrentProfileBossd := terminateCurrentProfileBossd
	oldDaemonStandaloneServed := daemonStandaloneServed
	oldBossdProcessCommandLine := bossdProcessCommandLine
	oldDaemonRestartReadyTimeout := daemonRestartReadyTimeout
	oldDaemonRestartPollInterval := daemonRestartPollInterval
	t.Cleanup(func() {
		defaultSocketPath = oldDefaultSocketPath
		daemonSocketReachable = oldDaemonSocketReachable
		daemonGetStatus = oldDaemonGetStatus
		daemonEnsureRunning = oldDaemonEnsureRunning
		daemonEnsureRunningWithMode = oldDaemonEnsureRunningWithMode
		daemonStop = oldDaemonStop
		restartDaemon = oldRestartDaemon
		terminateStandaloneCurrentProfile = oldTerminateStandaloneCurrentProfile
		terminateAllBossdProcesses = oldTerminateAllBossdProcesses
		terminateAllPluginProcesses = oldTerminateAllPluginProcesses
		findBossdPluginPIDs = oldFindBossdPluginPIDs
		waitForDaemonSocketGone = oldWaitForDaemonSocketGone
		waitForStandaloneBossdExit = oldWaitForStandaloneBossdExit
		terminateCurrentProfileBossd = oldTerminateCurrentProfileBossd
		daemonStandaloneServed = oldDaemonStandaloneServed
		bossdProcessCommandLine = oldBossdProcessCommandLine
		daemonRestartReadyTimeout = oldDaemonRestartReadyTimeout
		daemonRestartPollInterval = oldDaemonRestartPollInterval
	})
}

func TestRefreshSessionPRForwardsSelectors(t *testing.T) {
	cases := []struct {
		name      string
		sessionID string
		prNumber  int32
		wantID    string
		wantPR    int32
	}{
		{name: "session id only", sessionID: "sess-1", wantID: "sess-1"},
		{name: "PR only", prNumber: 42, wantPR: 42},
		{name: "both", sessionID: "sess-1", prNumber: 42, wantID: "sess-1", wantPR: 42},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sessionPR := int32(42)
			stub := &refreshPRStub{session: &pb.Session{
				Id:            "sess-1",
				PrNumber:      &sessionPR,
				DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING,
			}}
			cmd := &cobra.Command{}
			var out bytes.Buffer
			cmd.SetOut(&out)
			if err := refreshSessionPR(cmd, stub, tc.sessionID, tc.prNumber); err != nil {
				t.Fatalf("refreshSessionPR: %v", err)
			}
			if got := stub.req.GetId(); got != tc.wantID {
				t.Fatalf("id = %q, want %q", got, tc.wantID)
			}
			if got := stub.req.GetPrNumber(); got != tc.wantPR {
				t.Fatalf("pr_number = %d, want %d", got, tc.wantPR)
			}
			if !strings.Contains(out.String(), "refreshed PR") || !strings.Contains(out.String(), "passing") {
				t.Fatalf("output = %q, want refreshed PR passing line", out.String())
			}
		})
	}
}

func TestRefreshSessionPRRequiresSelector(t *testing.T) {
	err := refreshSessionPR(&cobra.Command{}, &refreshPRStub{}, "", 0)
	if err == nil || !strings.Contains(err.Error(), "session id or --pr is required") {
		t.Fatalf("error = %v, want missing selector error", err)
	}
}

type refreshPRStub struct {
	req     *pb.RefreshSessionPRRequest
	session *pb.Session
}

func (s *refreshPRStub) RefreshSessionPR(_ context.Context, req *pb.RefreshSessionPRRequest) (*pb.Session, error) {
	s.req = req
	return s.session, nil
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

// TestListRenderersEmitFullIDs is the regression guard for the ID-truncation
// bug: CLI list/detail renderers must print the full, untruncated resource ID
// so the displayed value round-trips back into the exact-match commands
// (repo/cron/account removal accept only the full 16-char id). printChatsTable
// exercises the shared table path (MaxColWidth("ID", ids, 0) + table.New) used
// by the session/repo/trash/cron list tables; printSessionShowHeader covers the
// `boss show` printf path.
func TestListRenderersEmitFullIDs(t *testing.T) {
	const fullSessionID = "ae347e386b61682c"                   // 16-hex sqlutil.NewID form
	const fullAgentID = "550e8400-e29b-41d4-a716-446655440000" // full agent-session UUID

	t.Run("chats table shows full agent-session id", func(t *testing.T) {
		cmd := &cobra.Command{}
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		printChatsTable(cmd, []*pb.ClaudeChat{{AgentSessionId: fullAgentID}}, nil, true)
		if !strings.Contains(buf.String(), fullAgentID) {
			t.Fatalf("chats table truncated the id; got:\n%s", buf.String())
		}
	})

	t.Run("show header shows full session id", func(t *testing.T) {
		out := captureStdout(t, func() {
			printSessionShowHeader(&pb.Session{Id: fullSessionID})
		})
		if !strings.Contains(out, fullSessionID) {
			t.Fatalf("show header truncated the id; got:\n%s", out)
		}
	})
}

type recordingProcess struct {
	pid     int
	signals *[]int
	err     error
}

func (p recordingProcess) Signal(os.Signal) error {
	*p.signals = append(*p.signals, p.pid)
	return p.err
}

func TestTerminateProfileBossdProcessSignalsMetadataPIDOnly(t *testing.T) {
	dir := t.TempDir()
	if err := daemonstate.Write(dir, daemonstate.Metadata{PID: 200, ExecutablePath: "/tmp/bossd"}); err != nil {
		t.Fatalf("write daemon metadata: %v", err)
	}
	oldBossdProcessCommandLine := bossdProcessCommandLine
	bossdProcessCommandLine = func(pid int) (string, error) {
		if pid != 200 {
			t.Fatalf("process command line pid = %d, want metadata pid 200", pid)
		}
		return "/tmp/bossd", nil
	}
	t.Cleanup(func() { bossdProcessCommandLine = oldBossdProcessCommandLine })

	var signalled []int
	got, err := terminateProfileBossdProcess(dir, func(pid int) (processSignaler, error) {
		if pid != 200 {
			t.Fatalf("findProcess pid = %d, want metadata pid 200", pid)
		}
		return recordingProcess{pid: pid, signals: &signalled}, nil
	})

	if err != nil {
		t.Fatalf("terminateProfileBossdProcess() returned error: %v", err)
	}
	if got != 1 {
		t.Fatalf("terminateProfileBossdProcess() signalled %d processes, want 1", got)
	}
	if !reflect.DeepEqual(signalled, []int{200}) {
		t.Fatalf("signalled PIDs = %v, want [200]", signalled)
	}
}

func TestTerminateProfileBossdProcessMatchesExecutablePathWithSpaces(t *testing.T) {
	dir := t.TempDir()
	executable := "/Users/dev/Library/Application Support/bossanova/bin/bossd"
	if err := daemonstate.Write(dir, daemonstate.Metadata{PID: 200, ExecutablePath: executable}); err != nil {
		t.Fatalf("write daemon metadata: %v", err)
	}
	oldBossdProcessCommandLine := bossdProcessCommandLine
	bossdProcessCommandLine = func(pid int) (string, error) {
		if pid != 200 {
			t.Fatalf("process command line pid = %d, want metadata pid 200", pid)
		}
		return executable + " --daemon", nil
	}
	t.Cleanup(func() { bossdProcessCommandLine = oldBossdProcessCommandLine })

	var signalled []int
	got, err := terminateProfileBossdProcess(dir, func(pid int) (processSignaler, error) {
		return recordingProcess{pid: pid, signals: &signalled}, nil
	})

	if err != nil {
		t.Fatalf("terminateProfileBossdProcess() returned error: %v", err)
	}
	if got != 1 {
		t.Fatalf("terminateProfileBossdProcess() signalled %d processes, want 1", got)
	}
	if !reflect.DeepEqual(signalled, []int{200}) {
		t.Fatalf("signalled PIDs = %v, want [200]", signalled)
	}
}

func TestTerminateProfileBossdProcessIgnoresExecutableMismatch(t *testing.T) {
	dir := t.TempDir()
	if err := daemonstate.Write(dir, daemonstate.Metadata{PID: 200, ExecutablePath: "/tmp/bossd"}); err != nil {
		t.Fatalf("write daemon metadata: %v", err)
	}
	oldBossdProcessCommandLine := bossdProcessCommandLine
	bossdProcessCommandLine = func(int) (string, error) {
		return "/usr/bin/yes", nil
	}
	t.Cleanup(func() { bossdProcessCommandLine = oldBossdProcessCommandLine })

	got, err := terminateProfileBossdProcess(dir, func(pid int) (processSignaler, error) {
		t.Fatalf("findProcess called for mismatched pid %d", pid)
		return fakeProcess{}, nil
	})

	if err != nil {
		t.Fatalf("terminateProfileBossdProcess() returned error: %v", err)
	}
	if got != 0 {
		t.Fatalf("terminateProfileBossdProcess() signalled %d processes, want 0", got)
	}
}

func TestTerminateProfileBossdProcessIgnoresMissingMetadata(t *testing.T) {
	got, err := terminateProfileBossdProcess(filepath.Join(t.TempDir(), "missing"), func(pid int) (processSignaler, error) {
		t.Fatalf("findProcess called for pid %d", pid)
		return fakeProcess{}, nil
	})

	if err != nil {
		t.Fatalf("terminateProfileBossdProcess() returned error: %v", err)
	}
	if got != 0 {
		t.Fatalf("terminateProfileBossdProcess() signalled %d processes, want 0", got)
	}
}

func TestTerminateProfileBossdProcessTreatsExitedProcessAsStopped(t *testing.T) {
	dir := t.TempDir()
	if err := daemonstate.Write(dir, daemonstate.Metadata{PID: 200}); err != nil {
		t.Fatalf("write daemon metadata: %v", err)
	}

	got, err := terminateProfileBossdProcess(dir, func(int) (processSignaler, error) {
		return fakeProcess{err: syscall.ESRCH}, nil
	})

	if err != nil {
		t.Fatalf("terminateProfileBossdProcess() returned error: %v", err)
	}
	if got != 0 {
		t.Fatalf("terminateProfileBossdProcess() signalled %d processes, want 0", got)
	}
}

func TestSignalBossdProcessesCountsOnlySuccessfulSignals(t *testing.T) {
	got, err := signalBossdProcesses([]int{100, 200, 300}, func(pid int) (processSignaler, error) {
		switch pid {
		case 100:
			return fakeProcess{}, nil
		case 200:
			return fakeProcess{err: syscall.ESRCH}, nil
		case 300:
			return fakeProcess{err: syscall.EPERM}, nil
		default:
			t.Fatalf("unexpected pid %d", pid)
			return fakeProcess{}, nil
		}
	})

	if got != 1 {
		t.Fatalf("signalBossdProcesses signalled %d processes, want 1", got)
	}
	if !errors.Is(err, syscall.EPERM) {
		t.Fatalf("signalBossdProcesses error = %v, want EPERM", err)
	}
}

func TestSignalBossdProcessesSurfacesFindFailures(t *testing.T) {
	findErr := errors.New("missing process")
	got, err := signalBossdProcesses([]int{100}, func(int) (processSignaler, error) {
		return nil, findErr
	})

	if got != 0 {
		t.Fatalf("signalBossdProcesses signalled %d processes, want 0", got)
	}
	if !errors.Is(err, findErr) {
		t.Fatalf("signalBossdProcesses error = %v, want %v", err, findErr)
	}
}

// TestBossdPluginProcessMatcherSweepsAllProfilePlugins covers the broad matcher
// that backs `boss daemon stop --all-standalone` (BOS-349). It matches
// configured plugin paths and, because the escape hatch cleans crash orphans
// from any profile, any bossd-plugin-* executable — while still rejecting
// ordinary programs that merely mention a plugin path as an argument.
func TestBossdPluginProcessMatcherSweepsAllProfilePlugins(t *testing.T) {
	matches := bossdPluginProcessMatcher([]config.PluginConfig{{
		Name: "custom",
		Path: "/tmp/custom/bossd-plugin-custom",
	}, {
		Name: "claude",
		Path: "/Users/dev/Library/Application Support/bossanova/plugins/bossd-plugin-claude",
	}})

	for _, commandLine := range []string{
		// Configured plugin paths, including one containing spaces.
		"/tmp/custom/bossd-plugin-custom --flag",
		"/Users/dev/Library/Application Support/bossanova/plugins/bossd-plugin-claude --stdio",
		// Cross-profile / Homebrew plugin executables the broad sweep also reaps.
		"/opt/homebrew/Cellar/bossanova/2.1.197/libexec/plugins/bossd-plugin-claude",
		"/tmp/profile-b/bossd-plugin-claude",
		"/Users/dev/Library/Application Support/bossanova-profile-b/plugins/bossd-plugin-codex --stdio",
	} {
		if !matches(commandLine) {
			t.Fatalf("broad matcher rejected plugin executable %q", commandLine)
		}
	}

	for _, commandLine := range []string{
		"/bin/sh -c /tmp/profile-b/bossd-plugin-claude",
		"/bin/sh -c /Users/dev/Library/Application Support/bossanova-profile-b/plugins/bossd-plugin-codex",
		"/bin/echo /Users/dev/Library/Application Support/bossanova-profile-b/plugins/bossd-plugin-codex",
		"/usr/bin/vim /tmp/profile-b/bossd-plugin-claude",
		"/usr/local/bin/not-a-plugin",
		"sh -c /tmp/profile-b/bossd-plugin-claude",
		"",
	} {
		if matches(commandLine) {
			t.Fatalf("broad matcher accepted non-plugin executable %q", commandLine)
		}
	}
}

// TestHomebrewPrefixesForBinDirDelegatesToDaemonbin pins the BOS-864
// delegation: the Cellar shape now has one definition in daemonbin, and this
// helper must keep returning the same two prefixes its plugin-directory and
// brew-executable callers rely on.
func TestHomebrewPrefixesForBinDirDelegatesToDaemonbin(t *testing.T) {
	for _, binDir := range []string{
		"/opt/homebrew/Cellar/bossanova/2.1.197/bin",
		"/usr/local/Cellar/bossanova/v1.2.3/bin",
		"/usr/local/bin",
		"/opt/homebrew/Cellar/other/1.0.0/bin",
	} {
		gotFormula, gotBrew, gotOK := homebrewPrefixesForBinDir(binDir)
		wantFormula, wantBrew, wantOK := daemonbin.HomebrewCellarPrefixes(binDir)
		if gotFormula != wantFormula || gotBrew != wantBrew || gotOK != wantOK {
			t.Errorf("homebrewPrefixesForBinDir(%q) = (%q, %q, %t), want (%q, %q, %t)",
				binDir, gotFormula, gotBrew, gotOK, wantFormula, wantBrew, wantOK)
		}
	}

	formula, brew, ok := homebrewPrefixesForBinDir("/opt/homebrew/Cellar/bossanova/2.1.197/bin")
	if !ok || formula != "/opt/homebrew/Cellar/bossanova/2.1.197" || brew != "/opt/homebrew" {
		t.Fatalf("homebrewPrefixesForBinDir() = (%q, %q, %t), want the formula and brew prefixes", formula, brew, ok)
	}
}

func TestSignalBossdPluginProcessesFiltersBeforeSignaling(t *testing.T) {
	oldBossdProcessCommandLine := bossdProcessCommandLine
	bossdProcessCommandLine = func(pid int) (string, error) {
		switch pid {
		case 100:
			return "/tmp/custom/bossd-plugin-custom", nil
		case 200:
			return "/usr/bin/vim /tmp/custom/bossd-plugin-other", nil
		case 300:
			return "/opt/homebrew/Cellar/bossanova/2.1.197/libexec/plugins/bossd-plugin-claude", nil
		default:
			t.Fatalf("unexpected pid %d", pid)
			return "", nil
		}
	}
	t.Cleanup(func() { bossdProcessCommandLine = oldBossdProcessCommandLine })

	var signalled []int
	matches := bossdPluginProcessMatcher([]config.PluginConfig{{
		Name: "custom",
		Path: "/tmp/custom/bossd-plugin-custom",
	}})
	got, err := signalBossdPluginProcesses([]int{100, 200, 300}, matches, func(pid int) (processSignaler, error) {
		return recordingProcess{pid: pid, signals: &signalled}, nil
	})

	if err != nil {
		t.Fatalf("signalBossdPluginProcesses() returned error: %v", err)
	}
	if got != 2 {
		t.Fatalf("signalBossdPluginProcesses signalled %d processes, want 2", got)
	}
	if !reflect.DeepEqual(signalled, []int{100, 300}) {
		t.Fatalf("signalled PIDs = %v, want [100 300]", signalled)
	}
}

func (s *agentPreflightStub) SwitchSessionAccount(context.Context, *pb.SwitchSessionAccountRequest) (*pb.SwitchSessionAccountResponse, error) {
	panic("unused")
}

// TestPrintSessionShowHeaderPrintsTheFullBlockedReason is the 2026-09-03 fix.
//
// The reason was reachable from no CLI surface at all — not `boss show`, not
// `--json`, not `boss ls` — while the TUI truncated it to 48 runes and the
// daemon, started detached, was writing its logs to /dev/null. Diagnosis ended
// in a direct sqlite3 query against bossd.db. This line is the CLI's answer, and
// it must never acquire a truncation: that is the whole point of it.
func TestPrintSessionShowHeaderPrintsTheFullBlockedReason(t *testing.T) {
	// Deliberately longer than the TUI's 48-rune hint cap, and multi-line, which
	// is the shape a real `gh pr create` failure has.
	const reason = "draft PR creation failed: create PR: HTTP 401: Requires authentication (https://api.github.com/graphql)\nTry authenticating with:  gh auth login"

	out := captureStdout(t, func() {
		printSessionShowHeader(&pb.Session{Id: "sess-1234abcd", BlockedReason: ptrTo(reason)})
	})

	if !strings.Contains(out, "Blocked:") {
		t.Fatalf("output has no Blocked line:\n%s", out)
	}
	// Every load-bearing fragment survives, including the part a 48-rune cap
	// would have cut and the part after the newline.
	for _, fragment := range []string{
		"HTTP 401: Requires authentication",
		"https://api.github.com/graphql",
		"gh auth login",
	} {
		if !strings.Contains(out, fragment) {
			t.Fatalf("output dropped %q — it must print the reason in full:\n%s", fragment, out)
		}
	}
	if strings.Contains(out, "…") {
		t.Fatalf("output was truncated; this surface exists precisely because the TUI truncates:\n%s", out)
	}
}

// TestPrintSessionShowHeaderOmitsAnEmptyBlockedReason keeps the common case
// clean: an unblocked session must not grow a dangling label.
func TestPrintSessionShowHeaderOmitsAnEmptyBlockedReason(t *testing.T) {
	out := captureStdout(t, func() {
		printSessionShowHeader(&pb.Session{Id: "sess-1234abcd"})
	})
	if strings.Contains(out, "Blocked:") {
		t.Fatalf("unblocked session printed a Blocked line:\n%s", out)
	}
}

func ptrTo[T any](v T) *T { return &v }

// TestObservedServingFactsRefusesUnverifiableAndForeignRecords pins the
// criterion-4 downgrade risk: the mirror image of BOS-1181. A daemon-state
// record must not produce a standalone verdict — and boot a correctly
// supervised daemon out of the service manager — unless it is both
// attributable to this profile's socket and liveness-verifiable.
func TestObservedServingFactsRefusesUnverifiableAndForeignRecords(t *testing.T) {
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
			restoreDaemonCommandStubs(t)
			appDataDir, socketPath := daemonRestartProfileFixture(t)
			defaultSocketPath = func() (string, error) { return socketPath, nil }
			bossdProcessCommandLine = func(int) (string, error) {
				return "/opt/homebrew/bin/bossd", nil
			}
			if err := daemonstate.Write(appDataDir, tc.metadata(socketPath)); err != nil {
				t.Fatalf("write daemon state: %v", err)
			}

			profile, err := currentDaemonProfile()
			if err != nil {
				t.Fatalf("currentDaemonProfile: %v", err)
			}
			st := &daemon.Status{Installed: true, Running: true, PID: supervisedPID}
			facts := observedServingFacts(st, profile)
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
