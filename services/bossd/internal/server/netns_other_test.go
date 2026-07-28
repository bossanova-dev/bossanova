//go:build !linux

package server

import "testing"

// TestPlatformEndpointNamespaceGate_Other pins the off-Linux behavior: there
// are no per-process network namespaces, so a request that arrived over the
// daemon's local Unix socket is already proven local and the gate is always
// satisfied — regardless of any client-supplied identity, including empty.
func TestPlatformEndpointNamespaceGate_Other(t *testing.T) {
	for _, ns := range []string{"", "net:[4026531840]", "anything"} {
		if !platformEndpointNamespaceGate(ns) {
			t.Errorf("gate(%q) = false, want true off Linux (Unix-socket locality is sufficient)", ns)
		}
	}
}
