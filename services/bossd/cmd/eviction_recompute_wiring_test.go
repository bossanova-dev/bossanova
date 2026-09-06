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

// TestEvictionRecomputeSeamsWired pins the BOS-1096 fix to real startup wiring
// rather than to a helper call in a unit test.
//
// Every other test for this fix — the tracker's eviction hook, the batch
// recompute helper, the end-to-end compose — passes just as happily on a daemon
// that never calls SetOnEntriesEvicted, because each one installs the hook
// itself. Dropping that single line in main.go would therefore ship green with
// the reported symptom fully intact: sessions whose chats stop heartbeating go
// on displaying whatever the last edge wrote until the daemon restarts.
// Asserting `live` here is what makes that regression a red test.
//
// Deliberately mirrors TestTransientResumeSeamsWired's shape (temp HOME, no
// plugins, stopSig/ready channels) so the seam tests fail the same way and
// share the same startup cost profile.
func TestEvictionRecomputeSeamsWired(t *testing.T) {
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

	var hookFired atomic.Int32
	var seamsLive atomic.Int32
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
			onEvictionRecomputeSeamsWired: func(live bool) {
				hookFired.Store(1)
				if live {
					seamsLive.Store(1)
				}
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

	if hookFired.Load() != 1 {
		t.Fatal("onEvictionRecomputeSeamsWired never fired")
	}
	if seamsLive.Load() != 1 {
		t.Fatal("eviction recompute hook was not installed on the tracker after startup wiring")
	}

	stopSig <- syscall.SIGTERM
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return within 15s of SIGTERM")
	}
}
