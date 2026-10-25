package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/recurser/bossalib/kit/config"
)

// Shutdown stops the lifecycle's background chat row rewrites. Without the
// call, a failover's rewrite still running at shutdown writes to a closed
// database.
func TestShutdownStopsBackgroundRelabels(t *testing.T) {
	baseDir, err := os.MkdirTemp("/tmp", "bossdtest-")
	if err != nil {
		t.Fatalf("mkdir base: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(baseDir) })

	t.Setenv("HOME", baseDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(baseDir, ".config"))
	t.Setenv("BOSSD_ORCHESTRATOR_URL", "")

	stopped := make(chan func() bool, 1)
	stopSig := make(chan os.Signal, 1)
	ready := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		done <- run(runOpts{
			stopSig:                stopSig,
			dbPath:                 filepath.Join(baseDir, "bossd.db"),
			socketPath:             filepath.Join(baseDir, "bossd.sock"),
			plugins:                []config.PluginConfig{},
			onReady:                func() { close(ready) },
			onRelabelShutdownProbe: func(probe func() bool) { stopped <- probe },
		})
	}()

	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("run exited before ready: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("daemon did not reach ready state within 15s")
	}

	var relabelsStopped func() bool
	select {
	case relabelsStopped = <-stopped:
	default:
		t.Fatal("onRelabelShutdownProbe never fired")
	}
	if relabelsStopped() {
		t.Fatal("background relabels were stopped before shutdown began")
	}

	stopSig <- syscall.SIGTERM
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return within 15s of SIGTERM")
	}
	if !relabelsStopped() {
		t.Fatal("run returned without stopping the background relabels")
	}
}
