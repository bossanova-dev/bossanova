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

	out := captureStdout(t, func() {
		if err := runDaemonStart(&cobra.Command{}); err != nil {
			t.Fatalf("runDaemonStart: %v", err)
		}
	})
	if !strings.Contains(out, "Daemon is already running.") {
		t.Fatalf("output = %q, want already running message", out)
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
	oldDaemonStop := daemonStop
	oldRestartDaemon := restartDaemon
	oldTerminateStandaloneCurrentProfile := terminateStandaloneCurrentProfile
	oldTerminateAllBossdProcesses := terminateAllBossdProcesses
	oldTerminateAllPluginProcesses := terminateAllPluginProcesses
	oldFindBossdPluginPIDs := findBossdPluginPIDs
	oldWaitForDaemonSocketGone := waitForDaemonSocketGone
	oldWaitForStandaloneBossdExit := waitForStandaloneBossdExit
	oldDaemonRestartReadyTimeout := daemonRestartReadyTimeout
	oldDaemonRestartPollInterval := daemonRestartPollInterval
	t.Cleanup(func() {
		defaultSocketPath = oldDefaultSocketPath
		daemonSocketReachable = oldDaemonSocketReachable
		daemonGetStatus = oldDaemonGetStatus
		daemonEnsureRunning = oldDaemonEnsureRunning
		daemonStop = oldDaemonStop
		restartDaemon = oldRestartDaemon
		terminateStandaloneCurrentProfile = oldTerminateStandaloneCurrentProfile
		terminateAllBossdProcesses = oldTerminateAllBossdProcesses
		terminateAllPluginProcesses = oldTerminateAllPluginProcesses
		findBossdPluginPIDs = oldFindBossdPluginPIDs
		waitForDaemonSocketGone = oldWaitForDaemonSocketGone
		waitForStandaloneBossdExit = oldWaitForStandaloneBossdExit
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
