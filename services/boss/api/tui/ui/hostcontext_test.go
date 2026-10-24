package ui

import (
	"testing"
)

// TestHostContextAccessorsFollowTheDestination pins the one switch every
// local-machine call site in the TUI consults. A regression here does not fail
// loudly — it makes attach shell out to the wrong machine's tmux and makes the
// transcript readers answer about a directory that does not exist — so the
// accessors are asserted directly rather than only through their callers.
func TestHostContextAccessorsFollowTheDestination(t *testing.T) {
	withHostDestination(t, "")
	if IsRemoteHost() {
		t.Fatal("isRemoteHost() = true with no --host destination, want false")
	}
	if got := RemoteHostDestination(); got != "" {
		t.Fatalf("remoteHostDestination() = %q with no --host destination, want empty", got)
	}

	withHostDestination(t, "deploy@bastion.example.com")
	if !IsRemoteHost() {
		t.Fatal("isRemoteHost() = false after SetHostDestination, want true")
	}
	if got := RemoteHostDestination(); got != "deploy@bastion.example.com" {
		t.Fatalf("remoteHostDestination() = %q, want the destination verbatim", got)
	}
}

func TestURLHostFromSSHDestination(t *testing.T) {
	cases := []struct{ destination, want string }{
		{"delta", "delta"}, {"  delta  ", "delta"}, {"deploy@delta", "delta"},
		{"deploy@delta.example.com", "delta.example.com"}, {"ssh://deploy@delta:2222", "delta"},
		{"ssh://[fe80::1]:22", "fe80::1"}, {"fe80::1", "fe80::1"}, {"[::1]", "::1"},
		{"a@b@delta", "delta"}, {"", ""}, {"  ", ""}, {"deploy@", ""},
		{"-oProxyCommand=x", ""}, {"del ta", ""}, {"delta/x", ""}, {"delta\x1b", ""},
		{"del\u00a0ta", ""}, {"delta\x7f", ""},
	}
	for _, tc := range cases {
		t.Run(tc.destination, func(t *testing.T) {
			if got := urlHostFromSSHDestination(tc.destination); got != tc.want {
				t.Errorf("host = %q, want %q", got, tc.want)
			}
		})
	}
}

func withHostDestination(t *testing.T, destination string) {
	t.Helper()
	previous := RemoteHostDestination()
	t.Cleanup(func() { SetHostDestination(previous) })
	SetHostDestination(destination)
}
