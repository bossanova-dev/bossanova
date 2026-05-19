package views

import (
	"context"
	"os/exec"
	"testing"

	bosspty "github.com/recurser/boss/internal/pty"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/telemetry"
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

type attachTelemetryStub struct {
	stubClient
}

func (s *attachTelemetryStub) RecordChat(context.Context, string, string, string, string, bool) (*pb.ClaudeChat, error) {
	return &pb.ClaudeChat{TmuxSessionName: "boss-test-chat"}, nil
}

func TestAttach_CapturesChatCreatedAndAttachedTelemetry(t *testing.T) {
	enableViewTelemetryForTest(t)
	rec := &fakeTelemetry{}
	m := NewAttachModel(&attachTelemetryStub{}, context.Background(), bosspty.NewManager(), "session-1", "")
	m.SetTelemetry(rec)

	updated, cmd := m.Update(attachReadyMsg{
		session: &pb.Session{Id: "session-1"},
		chats:   nil,
	})
	_ = updated.(AttachModel)
	if cmd == nil {
		t.Fatal("expected a tick cmd from attachReadyMsg")
	}

	if len(rec.events) != 2 {
		t.Fatalf("events = %d, want 2", len(rec.events))
	}
	if rec.events[0] != telemetry.EventChatCreated {
		t.Fatalf("event[0] = %q, want %q", rec.events[0], telemetry.EventChatCreated)
	}
	if rec.events[1] != telemetry.EventChatAttached {
		t.Fatalf("event[1] = %q, want %q", rec.events[1], telemetry.EventChatAttached)
	}
	for _, props := range rec.props {
		assertNoSensitiveTelemetryProps(t, props)
	}
}

// TestAttach_AttachReadyDefersExec verifies that the launching message stays
// rendered after attachReadyMsg arrives: m.launching must remain true and
// m.pendingExec must be primed for the follow-up tick. Without this, the
// "Launching... Press Ctrl+X to detach" line flashes for only the RPC time.
func TestAttach_AttachReadyDefersExec(t *testing.T) {
	m := NewAttachModel(&attachTelemetryStub{}, context.Background(), bosspty.NewManager(), "session-1", "")

	updated, cmd := m.Update(attachReadyMsg{
		session: &pb.Session{Id: "session-1"},
		chats:   nil,
	})
	got := updated.(AttachModel)

	if !got.launching {
		t.Fatal("launching = false after attachReadyMsg, want true (the message must keep rendering)")
	}
	if got.pendingExec == nil {
		t.Fatal("pendingExec = nil after attachReadyMsg, want a non-nil stash")
	}
	if got.pendingExec.tmuxName != "boss-test-chat" {
		t.Errorf("pendingExec.tmuxName = %q, want %q", got.pendingExec.tmuxName, "boss-test-chat")
	}
	if cmd == nil {
		t.Fatal("cmd = nil, want a tick cmd that will fire startExecMsg")
	}
}

// TestAttach_StartExecCompletesLaunch verifies that once startExecMsg
// fires, the launching flag is dropped, the pendingExec is consumed, and
// a non-nil cmd (the actual tea.Exec) is returned.
func TestAttach_StartExecCompletesLaunch(t *testing.T) {
	m := NewAttachModel(&attachTelemetryStub{}, context.Background(), bosspty.NewManager(), "session-1", "")

	updated, _ := m.Update(attachReadyMsg{
		session: &pb.Session{Id: "session-1"},
		chats:   nil,
	})
	primed := updated.(AttachModel)
	if primed.pendingExec == nil {
		t.Fatal("precondition: pendingExec not set by attachReadyMsg")
	}

	updated2, cmd := primed.Update(startExecMsg{})
	got := updated2.(AttachModel)

	if got.launching {
		t.Error("launching = true after startExecMsg, want false")
	}
	if got.pendingExec != nil {
		t.Error("pendingExec still set after startExecMsg, want nil")
	}
	if cmd == nil {
		t.Fatal("cmd = nil after startExecMsg, want the tea.Exec cmd")
	}
}

// TestAttach_StartExecAfterDetachIsNoop verifies that if the user has
// already pressed esc (m.detach = true) while we were waiting on the
// launching-display tick, the eventual startExecMsg does not relaunch
// the exec. Without the guard, the user would be re-attached against
// their will after detaching.
func TestAttach_StartExecAfterDetachIsNoop(t *testing.T) {
	m := NewAttachModel(&attachTelemetryStub{}, context.Background(), bosspty.NewManager(), "session-1", "")

	updated, _ := m.Update(attachReadyMsg{
		session: &pb.Session{Id: "session-1"},
		chats:   nil,
	})
	primed := updated.(AttachModel)
	primed.detach = true

	updated2, cmd := primed.Update(startExecMsg{})
	got := updated2.(AttachModel)

	if cmd != nil {
		t.Error("cmd != nil after detach + startExecMsg, want nil (no exec should fire)")
	}
	if got.pendingExec != nil {
		t.Error("pendingExec still set after detach + startExecMsg, want cleared")
	}
}
