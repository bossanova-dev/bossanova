//go:build !linux

package server

// platformEndpointNamespaceGate is always satisfied off Linux. macOS has no
// per-process network namespaces and no /proc/self/ns/net, so a request that
// arrived over the daemon's local Unix socket is already proven local; there is
// nothing further to match. The client-supplied identity is ignored.
func platformEndpointNamespaceGate(_ string) bool {
	return true
}
