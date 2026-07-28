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

// TestTransientResumeSeamsWired pins the BOS-518 auto-resume lane to real
// startup wiring rather than to a constructor call in a unit test. The resumer
// is only reachable through status.Tracker's transient-API-error transition
// hook, so a daemon that constructs it but forgets the hook (or installs the
// hook but leaves the resumer nil) is indistinguishable from one that has no
// auto-resume at all — silently, and only in production. Asserting `live` here
// is what makes that regression a red test.
//
// Deliberately mirrors TestRotationSeamsWired's shape (temp HOME, no plugins,
// stopSig/ready channels) so the two seam tests fail the same way and share the
// same startup cost profile.
func TestTransientResumeSeamsWired(t *testing.T) {
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
			onTransientResumeSeamsWired: func(live bool) {
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
		t.Fatal("onTransientResumeSeamsWired never fired")
	}
	if seamsLive.Load() != 1 {
		t.Fatal("transient auto-resume seams were not live after startup wiring")
	}

	stopSig <- syscall.SIGTERM
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return within 15s of SIGTERM")
	}
}
