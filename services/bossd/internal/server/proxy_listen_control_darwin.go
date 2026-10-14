//go:build darwin

package server

import "syscall"

// darwinReusePortAllowsCrossUserCoBind records whether macOS lets a socket
// owned by a DIFFERENT user co-bind the fixed proxy port that
// reusableSocketControl binds with SO_REUSEPORT (BOS-1587). If it did, a second
// local user could take a share of live panes' API traffic while bossd is up,
// and the decision rule is to stop setting SO_REUSEPORT on the fixed port.
//
// Status: UNMEASURED. The probe needs root to run its two halves as two
// unprivileged uids, and the build host had no non-interactive root, so the
// listener is unchanged and this value is the expectation the probe checks —
// macOS refusing a cross-user SO_REUSEPORT co-bind. Measure it before relying
// on it, from services/bossd:
//
//	sudo BOSS_MEASURE_REUSEPORT=1 go test -run TestDarwinReusePortCrossUserCoBind ./internal/server/
//
// The probe fails when the measurement contradicts this value. Refused: keep
// SO_REUSEPORT and replace this paragraph with the macOS version and date.
// Allowed: set this true, stop setting SO_REUSEPORT on the fixed port, and
// retry the bind for up to 2s before the ephemeral-port fallback.
const darwinReusePortAllowsCrossUserCoBind = false

// reusableSocketControl sets SO_REUSEADDR and SO_REUSEPORT on the listening
// socket before bind (BOS-409, Darwin). Both are required to re-bind the fixed
// failover-proxy port immediately after a daemon restart: on macOS/BSD,
// SO_REUSEADDR ALONE is not sufficient to re-bind a just-closed listening port
// (an immediate re-bind still fails with EADDRINUSE), so SO_REUSEPORT is
// load-bearing here, not merely a softener — without it a restart falls back to
// an ephemeral port and re-wedges the very panes this ticket fixes
// (TestProxyListen_ImmediateRebindSamePort guards exactly this on Darwin).
//
// The tradeoff: SO_REUSEPORT lets two sockets that BOTH set it bind the same
// 127.0.0.1:<port>. Darwin does not load-balance them: measured, every new
// connection goes to the most recently bound socket, so the later binder takes
// all of the earlier one's traffic.
// Nothing makes bossd a per-host singleton — its flock is per data dir, so a
// verify stack or a dev daemon with its own data dir is a second, fully live
// bossd (BOS-1530). The invariant that keeps this safe is that no second
// LISTENER is ever co-bound to the fixed port: ProxyServer.Listen probes the
// port first (fixedPortHeldByLiveListener) and, when a live listener answers,
// falls back to an ephemeral port instead of binding. A same-data-dir restart
// still re-binds immediately, because the replacement only reaches Listen after
// taking the flock the old daemon releases at exit. Returned as
// net.ListenConfig.Control.
func reusableSocketControl(_, _ string, c syscall.RawConn) error {
	var sockErr error
	if err := c.Control(func(fd uintptr) {
		if err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
			sockErr = err
			return
		}
		sockErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEPORT, 1)
	}); err != nil {
		return err
	}
	return sockErr
}
