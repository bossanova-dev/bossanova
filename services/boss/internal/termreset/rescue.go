package termreset

import (
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// restoreDefaultDisposition hands sig back to the OS default handling by
// dropping every Go handler registered for it. The rescue calls this *before*
// running cleanup, not after: while signal.Notify is still registered, a second
// SIGHUP is diverted into this package's channel and dropped, so a cleanup that
// wedges would leave boss un-hangup-able (a nohup'd boss surviving a logout it
// should not). Resetting up front means a second hangup always kills for real.
//
// It is a var so tests can neutralise it — a real reset inside the test binary
// would make the next self-delivered SIGHUP terminate the test process.
var restoreDefaultDisposition = func(sig syscall.Signal) {
	signal.Reset(sig)
}

// raise re-raises sig so boss still dies with the right status once the rescue
// cleanup has run. restoreDefaultDisposition has already dropped the Go handler,
// so this terminates rather than re-entering the handler. Deliberately NOT
// os.Exit: that would report the wrong termination reason to whatever
// supervises boss.
//
// It is a var so tests can substitute it via swapSignalSeams and observe the
// ordering without killing the test binary.
var raise = func(sig syscall.Signal) {
	_ = syscall.Kill(syscall.Getpid(), sig)
}

// swapSignalSeams replaces both signal seams and returns a func that restores
// the previous pair. Test-only — a test that swaps raise MUST also neutralise
// restoreDefaultDisposition, or the real signal.Reset would make the next
// self-delivered SIGHUP kill the test binary.
func swapSignalSeams(restoreDefault, raiseFn func(syscall.Signal)) func() {
	prevRestore, prevRaise := restoreDefaultDisposition, raise
	restoreDefaultDisposition, raise = restoreDefault, raiseFn
	return func() { restoreDefaultDisposition, raise = prevRestore, prevRaise }
}

// InstallHangupRescue registers a process-wide SIGHUP handler that restores
// SIGHUP's default disposition, runs cleanup exactly once, and then re-raises
// SIGHUP so boss dies with the right status. It returns an idempotent stop func
// that deregisters the handler.
//
// Go's default disposition for SIGHUP is to terminate the process outright, so
// none of boss's teardown defers run when an SSH connection drops — leaving the
// outer terminal stranded in whatever mouse-reporting mode the proxied pane
// enabled. This is best-effort by nature: when the socket is already dead
// ("connection reset by peer") no write reaches the user's terminal at all. It
// does help the adjacent cases where the tty is still writable: `kill -HUP`, an
// outer `tmux kill-session`, a locally closed tab, a systemd/launchd stop. See
// BOS-650.
func InstallHangupRescue(cleanup func()) (stop func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)

	done := make(chan struct{})
	// Belt and braces: the select below has no loop, so the handler is
	// single-shot by construction and this Once can only ever fire once. It is
	// kept so turning the select into a loop cannot silently double-run cleanup.
	var cleanupOnce sync.Once
	go func() {
		select {
		case <-ch:
			// Hand SIGHUP back to the default disposition first. cleanup shells
			// out to tmux with no timeout (views.tmuxCommand), and the very
			// scenarios this handler exists for are ones where the tmux server
			// may itself be mid-teardown — so cleanup can block. Resetting
			// before running it keeps a second hangup lethal.
			restoreDefaultDisposition(syscall.SIGHUP)
			if cleanup != nil {
				cleanupOnce.Do(func() {
					// cleanup is caller-supplied and in production shells out to
					// tmux. A panic here must not skip the re-raise below, or
					// boss dies with the wrong status and dumps a goroutine trace
					// into the very terminal the rescue exists to clean up.
					defer func() { _ = recover() }()
					cleanup()
				})
			}
			raise(syscall.SIGHUP)
		case <-done:
		}
	}()

	var stopOnce sync.Once
	return func() {
		stopOnce.Do(func() {
			signal.Stop(ch)
			close(done)
		})
	}
}
