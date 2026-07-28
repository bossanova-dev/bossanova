//go:build linux

package server

import (
	"os"
	"testing"
)

// TestPlatformEndpointNamespaceGate_Linux exercises the real Linux security
// control directly (not the injected EndpointNamespaceGate seam): it must
// hydrate only for an exact, non-empty match of the daemon's own
// /proc/self/ns/net identity, and fail closed on an empty or mismatched
// client identity.
func TestPlatformEndpointNamespaceGate_Linux(t *testing.T) {
	own, err := os.Readlink("/proc/self/ns/net")
	if err != nil || own == "" {
		t.Skipf("cannot read /proc/self/ns/net (%v); namespace gate not exercisable here", err)
	}

	if !platformEndpointNamespaceGate(own) {
		t.Errorf("gate(own=%q) = false, want true for an exact namespace match", own)
	}
	if platformEndpointNamespaceGate("") {
		t.Error(`gate("") = true, want false: an empty client identity must fail closed`)
	}
	if platformEndpointNamespaceGate(own + "-mismatch") {
		t.Error("gate(mismatch) = true, want false: a non-matching identity must fail closed")
	}
}
