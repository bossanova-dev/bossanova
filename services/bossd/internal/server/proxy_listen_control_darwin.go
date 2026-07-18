//go:build darwin

package server

import "syscall"

// reusableSocketControl sets SO_REUSEADDR and SO_REUSEPORT on the listening
// socket before bind (BOS-409, Darwin). Both are required to re-bind the fixed
// failover-proxy port immediately after a daemon restart: on macOS/BSD,
// SO_REUSEADDR ALONE is not sufficient to re-bind a just-closed listening port
// (an immediate re-bind still fails with EADDRINUSE), so SO_REUSEPORT is
// load-bearing here, not merely a softener — without it a restart falls back to
// an ephemeral port and re-wedges the very panes this ticket fixes
// (TestProxyListen_ImmediateRebindSamePort guards exactly this on Darwin).
//
// The tradeoff: SO_REUSEPORT does let two sockets that BOTH set it bind the same
// 127.0.0.1:<port> and have the kernel load-balance across them, so during a
// restart-overlap window a new daemon can coexist with an old one instead of
// getting EADDRINUSE. That is acceptable because bossd runs one daemon per host
// (the socketauth/singleton guards prevent a second live daemon), and it is the
// only way to get a reliable same-port restart-rebind on macOS. Returned as
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
