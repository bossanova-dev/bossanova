package main

import (
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/rs/zerolog"

	sharedplugin "github.com/recurser/bossalib/plugin"
)

// TestHandleSigtermDrainsThenExits pins the incident fix: after SIGTERM the
// handler must drain (bounded by shutdownTimeout) and then EXIT the process.
// The old handler drained but never exited, leaving an alive-but-CANCELLED
// zombie the host health loop considered healthy forever.
func TestHandleSigtermDrainsThenExits(t *testing.T) {
	sigCh := make(chan os.Signal, 1)
	var order []string
	var gotTimeout time.Duration
	var gotCode int
	done := make(chan struct{})

	shutdown := func(d time.Duration) {
		order = append(order, "shutdown")
		gotTimeout = d
	}
	exit := func(code int) {
		order = append(order, "exit")
		gotCode = code
		close(done)
	}

	go handleSigterm(sigCh, zerolog.Nop(), shutdown, exit)
	sigCh <- syscall.SIGTERM

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not exit within 2s of SIGTERM")
	}
	if len(order) != 2 || order[0] != "shutdown" || order[1] != "exit" {
		t.Fatalf("call order = %v, want [shutdown exit]", order)
	}
	if gotTimeout != shutdownTimeout {
		t.Fatalf("shutdown timeout = %v, want %v", gotTimeout, shutdownTimeout)
	}
	if gotCode != 0 {
		t.Fatalf("exit code = %d, want 0", gotCode)
	}
}

// TestParentWatchFailsOpenOnStartup is the plan's negative control, run
// against the real production entry point with the real poll interval: a main
// whose parent identity is absent, or present but not describing this process,
// must reach goplugin.Serve rather than exiting during startup. If the
// arm-time guard were wired backwards this test binary would be terminated by
// the watchdog's os.Exit rather than failing an assertion.
func TestParentWatchFailsOpenOnStartup(t *testing.T) {
	t.Run("identity absent", func(t *testing.T) {
		t.Setenv(sharedplugin.ParentPIDEnvVar, "")
		sharedplugin.StartParentWatch(zerolog.Nop())()
	})

	t.Run("identity does not describe this process", func(t *testing.T) {
		t.Setenv(sharedplugin.ParentPIDEnvVar, "999999")
		stop := sharedplugin.StartParentWatch(zerolog.Nop())
		defer stop()
		// Outlive one real poll interval: an armed watcher would fire here.
		time.Sleep(sharedplugin.DefaultParentPollInterval + 500*time.Millisecond)
	})
}
