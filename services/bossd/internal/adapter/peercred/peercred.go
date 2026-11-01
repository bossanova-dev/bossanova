// Package peercred reads kernel process credentials for local socket callers.
package peercred

import (
	"fmt"
	"net"
)

// PeerPID returns the kernel-attested PID of the process connecting conn.
func PeerPID(conn net.Conn) (int, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("peer credentials require a unix connection")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("unix connection descriptor: %w", err)
	}
	var pid int
	var credentialErr error
	if err := raw.Control(func(fd uintptr) { pid, credentialErr = peerPID(fd) }); err != nil {
		return 0, fmt.Errorf("access unix connection descriptor: %w", err)
	}
	if credentialErr != nil {
		return 0, fmt.Errorf("read peer PID: %w", credentialErr)
	}
	if pid <= 0 {
		return 0, fmt.Errorf("invalid peer PID %d", pid)
	}
	return pid, nil
}
