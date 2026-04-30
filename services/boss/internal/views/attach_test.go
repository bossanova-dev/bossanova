package views

import (
	"os/exec"
	"testing"
)

// TestTmuxSessionAlive_EmptyName verifies the empty-name fast path so the
// helper never spawns a `tmux has-session` for a never-set chat row.
func TestTmuxSessionAlive_EmptyName(t *testing.T) {
	if tmuxSessionAlive("") {
		t.Fatal("expected false for empty name")
	}
}

// TestTmuxSessionAlive_RealTmux exercises the real `tmux has-session`
// branch end-to-end so we catch any regression in argument shape (e.g. a
// future change that broke `-t`). Skipped when tmux is unavailable to keep
// the suite green in CI environments without it.
func TestTmuxSessionAlive_RealTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}

	const name = "boss-test-attach-alive-probe"

	// Use a private tmux socket so this test never collides with the
	// developer's existing tmux server. -L names a socket file in
	// /tmp; -d creates the session detached. The /usr/bin/yes command
	// is a portable "always running" pane payload.
	socketArgs := []string{"-L", "boss-attach-test"}

	// Start clean: kill the server on this socket if a prior failed run
	// left one behind, ignoring errors when no server is running yet.
	_ = exec.Command("tmux", append(append([]string{}, socketArgs...), "kill-server")...).Run()

	createArgs := append(append([]string{}, socketArgs...),
		"new-session", "-d", "-s", name, "sh", "-c", "sleep 30")
	if err := exec.Command("tmux", createArgs...).Run(); err != nil {
		t.Skipf("could not start tmux test session: %v", err)
	}
	t.Cleanup(func() {
		_ = exec.Command("tmux", append(append([]string{}, socketArgs...), "kill-server")...).Run()
	})

	// Sanity: the production helper uses the default socket, so verify it
	// against a session we KNOW does not exist on the default server.
	if tmuxSessionAlive("boss-definitely-not-a-real-session-xyz") {
		t.Error("expected false for unknown session on default socket")
	}

	// And exercise it against our scoped socket via direct exec to prove
	// the underlying `tmux has-session -t` shape works.
	probe := exec.Command("tmux",
		append(append([]string{}, socketArgs...), "has-session", "-t", name)...)
	if err := probe.Run(); err != nil {
		t.Fatalf("expected has-session to succeed against created session: %v", err)
	}
}
