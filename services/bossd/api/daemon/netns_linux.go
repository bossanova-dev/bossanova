//go:build linux

package server

import "os"

// platformEndpointNamespaceGate reports whether an opted-in local read may have
// its machine-local endpoints hydrated, given the client-supplied network-
// namespace identity. On Linux the daemon fails closed: it hydrates only when
// the client identity is non-empty and byte-for-byte equal to the daemon's own
// /proc/self/ns/net target. This prevents a Unix socket bridged across network
// namespaces from resolving a localhost link to a different service, and any
// read failure (unreadable /proc/self/ns/net) denies hydration.
func platformEndpointNamespaceGate(clientNS string) bool {
	own, err := os.Readlink("/proc/self/ns/net")
	if err != nil || own == "" {
		return false
	}
	return clientNS != "" && clientNS == own
}
