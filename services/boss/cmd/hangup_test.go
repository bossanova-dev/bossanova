package main

import (
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/recurser/boss/internal/termreset"
)

// TestHangupCleanupPicksUpTmuxRestoreWhenItArrives is the BOS-650 wiring guard:
// the rescue is armed before the tmux options are known, so the cleanup must be
// a no-op on the tmux half until then and must pick the restore up afterwards
// without the handler being disarmed. The terminal-reset half is deliberately
// not faked — os.Stdout is never a terminal under `go test`, so the real write
// is a silent no-op, and asserting on an injected stub would only prove that a
// closure calls the func it closed over.
func TestHangupCleanupPicksUpTmuxRestoreWhenItArrives(t *testing.T) {
	var tmuxRestore atomic.Pointer[func()]
	cleanup := hangupCleanup(&tmuxRestore)

	// Armed before the tmux options exist: must not panic, must not restore.
	cleanup()

	restores := 0
	restore := func() { restores++ }
	tmuxRestore.Store(&restore)

	cleanup()

	if restores != 1 {
		t.Errorf("restoreTmux called %d times after handover, want 1", restores)
	}
}

// TestInstallHangupRescueFnDefaultsToTermreset pins the production seam to the
// real installer, so substituting it in tests cannot silently disable the
// rescue in the shipped binary.
func TestInstallHangupRescueFnDefaultsToTermreset(t *testing.T) {
	if reflect.ValueOf(installHangupRescueFn).Pointer() != reflect.ValueOf(termreset.InstallHangupRescue).Pointer() {
		t.Error("installHangupRescueFn does not default to termreset.InstallHangupRescue")
	}
}
