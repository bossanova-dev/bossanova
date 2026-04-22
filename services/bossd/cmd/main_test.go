package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/recurser/bossalib/config"
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
