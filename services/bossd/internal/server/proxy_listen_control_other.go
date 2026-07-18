//go:build !darwin

package server

import "syscall"

// reusableSocketControl sets SO_REUSEADDR on the listening socket before bind
// (BOS-409, non-Darwin). SO_REUSEADDR lets the listening socket re-bind a fixed
// port even while connections from a prior daemon linger in TIME_WAIT; it never
// permits two *live* listeners to serve the same port — the EADDRINUSE fallback
// in Listen covers that. (Go's net already sets SO_REUSEADDR by default on unix
// listeners; setting it explicitly makes the intent testable and portable.)
// SO_REUSEPORT is added only on Darwin (see proxy_listen_control_darwin.go).
// Returned as net.ListenConfig.Control.
func reusableSocketControl(_, _ string, c syscall.RawConn) error {
	var sockErr error
	if err := c.Control(func(fd uintptr) {
		sockErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
	}); err != nil {
		return err
	}
	return sockErr
}
