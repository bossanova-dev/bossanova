package views

import "testing"

// TestHostContextAccessorsFollowTheDestination pins the one switch every
// local-machine call site in the TUI consults. A regression here does not fail
// loudly — it makes attach shell out to the wrong machine's tmux and makes the
// transcript readers answer about a directory that does not exist — so the
// accessors are asserted directly rather than only through their callers.
func TestHostContextAccessorsFollowTheDestination(t *testing.T) {
	withHostDestination(t, "")
	if isRemoteHost() {
		t.Fatal("isRemoteHost() = true with no --host destination, want false")
	}
	if got := remoteHostDestination(); got != "" {
		t.Fatalf("remoteHostDestination() = %q with no --host destination, want empty", got)
	}

	withHostDestination(t, "deploy@bastion.example.com")
	if !isRemoteHost() {
		t.Fatal("isRemoteHost() = false after SetHostDestination, want true")
	}
	if got := remoteHostDestination(); got != "deploy@bastion.example.com" {
		t.Fatalf("remoteHostDestination() = %q, want the destination verbatim", got)
	}
}
