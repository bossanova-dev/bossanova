package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/daemonstate"
	"github.com/recurser/bossalib/telemetry"
	"github.com/recurser/bossd/internal/server"
)

// TestRun_GracefulShutdown_NoGoroutineLeak boots the full daemon with an
// isolated DB + socket, sends a synthetic SIGTERM, and asserts run() returns
// within 10s without leaking goroutines. This is the end-to-end guard for
// the shutdownWG discipline added in the Sprint 2 FL1 work: if any daemon
// goroutine is spawned without being tracked (or respects a ctx that doesn't
// fire during shutdown), goleak will catch it here.
//
// The hung-plugin Ping scenario is covered at the plugin-host level by
// plugin.TestStopNoGoroutineLeak — Kill() fires SIGTERM/SIGKILL regardless
// of Ping state, and the pingAll loop now snapshots under the lock and
// times out each ping outside it.
func TestRun_GracefulShutdown_NoGoroutineLeak(t *testing.T) {
	// lumberjack's mill goroutine (log rotation worker) has no public stop hook;
	// Close() shuts the file but not the goroutine. It's a known upstream quirk
	// (natefinch/lumberjack#56) and benign — the goroutine dies when the process
	// exits. Ignore it so this test still catches leaks we own.
	defer goleak.VerifyNone(t,
		goleak.IgnoreCurrent(),
		goleak.IgnoreAnyFunction("gopkg.in/natefinch/lumberjack%2ev2.(*Logger).millRun"),
	)

	// Use a short tempdir under /tmp so the unix socket path stays under
	// the 104-byte sun_path limit on macOS. t.TempDir() on darwin expands
	// into /private/var/folders/... which can push us past the limit once
	// we append the Library/Application Support/... suffix.
	baseDir, err := os.MkdirTemp("/tmp", "bossdtest-")
	if err != nil {
		t.Fatalf("mkdir base: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(baseDir) })

	// Isolate $HOME so config.Load, skilldata, and other HOME-relative
	// lookups don't touch the developer's real bossd state.
	t.Setenv("HOME", baseDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(baseDir, ".config"))
	t.Setenv("BOSS_SETTINGS_PATH", filepath.Join(baseDir, "settings.json"))
	// Opt out of the cloud orchestrator: avoids real network I/O during
	// the test (which would otherwise leak an http2 readLoop goroutine
	// and hit the real keychain, popping the "allow access to Bossanova
	// keychain" prompt on every developer run). Local-only mode is a
	// first-class production path, so this still exercises the same
	// shutdown code — just without the upstream Manager.
	t.Setenv("BOSSD_ORCHESTRATOR_URL", "")

	dbPath := filepath.Join(baseDir, "bossd.db")
	socketPath := filepath.Join(baseDir, "bossd.sock")

	stopSig := make(chan os.Signal, 1)
	ready := make(chan struct{})

	done := make(chan error, 1)
	go func() {
		done <- run(runOpts{
			stopSig:    stopSig,
			dbPath:     dbPath,
			socketPath: socketPath,
			plugins:    []config.PluginConfig{}, // disable discovery
			onReady:    func() { close(ready) },
		})
	}()

	// Wait for the daemon's startup to reach the ready point (all
	// goroutines launched, server listening).
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("run exited before ready: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("daemon did not reach ready state within 15s")
	}

	// Give the server goroutine a moment to actually bind the socket
	// before we shut down, so Shutdown has something to stop.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	stopSig <- syscall.SIGTERM

	// Shutdown must complete within the daemon's own 10s hard upper
	// bound, plus a small slack for the select-race and any last
	// defer unwinding.
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return within 15s of SIGTERM")
	}
}

func TestDaemonDistinctIDUsesHyphenatedSharedHelper(t *testing.T) {
	got := daemonDistinctIDFromHostname("host-value")
	want := telemetry.DaemonDistinctID("host-value")
	if got != want {
		t.Fatalf("daemonDistinctIDFromHostname() = %q, want %q", got, want)
	}
	if strings.Contains(got, ":") {
		t.Fatalf("daemon distinct ID %q contains colon", got)
	}
}

func TestDaemonDistinctIDFallbackIsHyphenated(t *testing.T) {
	if got := daemonDistinctIDFromHostname(""); got != "daemon-unknown" {
		t.Fatalf("daemonDistinctIDFromHostname empty = %q, want daemon-unknown", got)
	}
}

func TestRunUsesSettingsPathProfileForDBAndSocket(t *testing.T) {
	baseDir, err := os.MkdirTemp("/tmp", "bossd-profile-")
	if err != nil {
		t.Fatalf("mkdir base: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(baseDir) })

	settingsPath := filepath.Join(baseDir, "settings.json")
	appDataDir := filepath.Join(baseDir, "data")
	socketPath := filepath.Join(baseDir, "profile.sock")
	t.Setenv("BOSS_SETTINGS_PATH", settingsPath)
	t.Setenv("BOSSD_ORCHESTRATOR_URL", "")

	settings := config.DefaultSettings()
	settings.AppDataDir = appDataDir
	settings.SocketPath = socketPath
	if err := config.SaveTo(settingsPath, settings); err != nil {
		t.Fatalf("SaveTo() returned error: %v", err)
	}

	stopSig := make(chan os.Signal, 1)
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- run(runOpts{
			stopSig: stopSig,
			plugins: []config.PluginConfig{},
			onReady: func() { close(ready) },
		})
	}()

	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("run exited before ready: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("daemon did not reach ready state within 15s")
	}

	if _, err := os.Stat(filepath.Join(appDataDir, "bossd.db")); err != nil {
		t.Fatalf("settings DB path was not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(appDataDir, server.LockFileName)); err != nil {
		t.Fatalf("settings lock file was not created: %v", err)
	}
	metadata, err := daemonstate.Read(appDataDir)
	if err != nil {
		t.Fatalf("daemon metadata was not written: %v", err)
	}
	if metadata.PID != os.Getpid() {
		t.Fatalf("metadata PID = %d, want %d", metadata.PID, os.Getpid())
	}
	if metadata.SettingsPath != settingsPath {
		t.Fatalf("metadata settings path = %q, want %q", metadata.SettingsPath, settingsPath)
	}
	if metadata.SocketPath != socketPath {
		t.Fatalf("metadata socket path = %q, want %q", metadata.SocketPath, socketPath)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("settings socket path was not created at %s", socketPath)
		}
		time.Sleep(25 * time.Millisecond)
	}

	stopSig <- syscall.SIGTERM
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return within 15s of SIGTERM")
	}
	if _, err := daemonstate.Read(appDataDir); !os.IsNotExist(err) {
		t.Fatalf("daemon metadata after shutdown error = %v, want not exist", err)
	}
}
