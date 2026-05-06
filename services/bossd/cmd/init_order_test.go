package main

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/recurser/bossalib/config"
)

// TestInitOrder_BootstrapBeforeServe pins the invariant that the chat-status
// poller's Bootstrap call MUST run before the gRPC server starts accepting
// connections. Without this ordering, the first wave of GetChatStatuses
// requests after a daemon restart would return is_running=false for every
// chat with a live tmux until the polling loop catches up — bad UX, and a
// regression we have already paid for once.
//
// The test boots the full daemon via run() with onBootstrapComplete and
// onServeStart hooks, then asserts onServeStart never fires before
// onBootstrapComplete has fired.
func TestInitOrder_BootstrapBeforeServe(t *testing.T) {
	baseDir, err := os.MkdirTemp("/tmp", "bossdtest-")
	if err != nil {
		t.Fatalf("mkdir base: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(baseDir) })

	t.Setenv("HOME", baseDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(baseDir, ".config"))
	t.Setenv("BOSSD_ORCHESTRATOR_URL", "")

	dbPath := filepath.Join(baseDir, "bossd.db")
	socketPath := filepath.Join(baseDir, "bossd.sock")

	var bootstrapDone atomic.Int32
	var serveStarted atomic.Int32
	var serveBeforeBootstrap atomic.Int32

	stopSig := make(chan os.Signal, 1)
	ready := make(chan struct{})

	done := make(chan error, 1)
	go func() {
		done <- run(runOpts{
			stopSig:    stopSig,
			dbPath:     dbPath,
			socketPath: socketPath,
			plugins:    []config.PluginConfig{},
			onReady:    func() { close(ready) },
			onBootstrapComplete: func() {
				bootstrapDone.Store(1)
			},
			onServeStart: func() {
				if bootstrapDone.Load() != 1 {
					serveBeforeBootstrap.Store(1)
				}
				serveStarted.Store(1)
			},
		})
	}()

	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("run exited before ready: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("daemon did not reach ready state within 15s")
	}

	// onServeStart fires inside a goroutine spawned just before onReady,
	// so it may race with the ready signal. Wait briefly for the socket
	// to appear (a stronger proxy for "Serve goroutine has run") before
	// asserting on the atomics.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if serveStarted.Load() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if serveBeforeBootstrap.Load() != 0 {
		t.Errorf("server.Serve started before TmuxStatusPoller.Bootstrap completed")
	}
	if bootstrapDone.Load() != 1 {
		t.Errorf("onBootstrapComplete never fired")
	}
	if serveStarted.Load() != 1 {
		t.Errorf("onServeStart never fired")
	}

	stopSig <- syscall.SIGTERM
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return within 15s of SIGTERM")
	}
}
