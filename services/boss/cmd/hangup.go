package main

import (
	"os"
	"sync/atomic"

	"github.com/recurser/boss/internal/termreset"
)

// installHangupRescueFn is the seam tests substitute for the real SIGHUP
// rescue installation, which would otherwise register a process-wide handler
// for the whole test binary.
var installHangupRescueFn = termreset.InstallHangupRescue

// writeStdoutReset clears every input-reporting mode a boss process can strand
// on the outer terminal — but only when stdout is a terminal, so redirected
// output is never corrupted with control bytes. See BOS-650.
func writeStdoutReset() {
	termreset.WriteAbnormalExitResetIfTerminal(os.Stdout)
}

// writeStderrReset is writeStdoutReset for the paths that talk to the user on
// stderr — the startup skill prompt, which blocks on stdin and so must not be
// typed into by a terminal that is still emitting mouse reports.
func writeStderrReset() {
	termreset.WriteAbnormalExitResetIfTerminal(os.Stderr)
}

// hangupCleanup is the best-effort work a SIGHUP runs before boss dies: clear
// the outer terminal's input-reporting modes (the mouse modes a proxied tmux or
// Claude pane may have enabled, plus the focus/paste/keyboard modes boss's own
// renderer sets), then undo the tmux session options boss turned on — once
// there are any. Both are what the normal-exit defers would have done, and Go's
// default SIGHUP disposition skips every one of them.
//
// The tmux half is handed over through a pointer rather than captured, because
// the rescue must be armed well before it is known: preflight and the daemon
// wait are full Bubble Tea programs that already enable the modes, and
// views.RunDaemonWait waits indefinitely. Disarming and re-arming mid-run would
// be the obvious alternative and is wrong — InstallHangupRescue's handler is
// single-shot, so a stop() racing an already-buffered SIGHUP can select the
// done channel instead and swallow the signal, leaving boss alive on a hangup
// it should have died from. Loading atomically keeps one handler armed
// throughout; the signal goroutine and the launch path both touch this.
func hangupCleanup(tmuxRestore *atomic.Pointer[func()]) func() {
	return func() {
		writeStdoutReset()
		if fn := tmuxRestore.Load(); fn != nil {
			(*fn)()
		}
	}
}
