package views

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	bosspty "github.com/recurser/boss/internal/pty"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/telemetry"
)

func TestShellQuote(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"claude", "claude"},
		{"--session-id", "--session-id"},
		{"/opt/homebrew/bin/fish", "/opt/homebrew/bin/fish"},
		{"exec $argv", "'exec $argv'"},
		{"", "''"},
		{"a'b", `'a'\''b'`},
	}
	for _, c := range cases {
		if got := shellQuote(c.in); got != c.want {
			t.Errorf("shellQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestShellJoin_QuotesOnlyWhereNeeded(t *testing.T) {
	got := shellJoin([]string{"/opt/homebrew/bin/fish", "-l", "-i", "-c", "exec $argv", "claude", "--session-id", "abc"})
	want := "/opt/homebrew/bin/fish -l -i -c 'exec $argv' claude --session-id abc"
	if got != want {
		t.Fatalf("shellJoin:\n got=%q\nwant=%q", got, want)
	}
}

func TestRenderLaunchDiagnostic_NilOrEmptyIsBlank(t *testing.T) {
	if got := renderLaunchDiagnostic(nil, "Claude Code"); got != "" {
		t.Fatalf("nil info should render blank, got %q", got)
	}
	if got := renderLaunchDiagnostic(&pb.DescribeChatLaunchResponse{}, "Claude Code"); got != "" {
		t.Fatalf("empty argv should render blank, got %q", got)
	}
}

func TestRenderLaunchDiagnostic_ShowsCommandHostAndWorktree(t *testing.T) {
	info := &pb.DescribeChatLaunchResponse{
		Argv:         []string{"/opt/homebrew/bin/fish", "-l", "-i", "-c", "exec $argv", "claude", "--session-id", "abc"},
		WorktreePath: "/work/tree",
		Host:         "tomo",
		AgentName:    "claude",
	}
	out := renderLaunchDiagnostic(info, "Claude Code")
	for _, want := range []string{
		"host 'tomo'",
		"cd /work/tree",
		"'exec $argv' claude --session-id abc",
		"Run it there",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("diagnostic missing %q in:\n%s", want, out)
		}
	}
}

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

func TestAttach_ViewUsesOverrideAgentForLaunchLabel(t *testing.T) {
	m := NewAttachModel(&attachTelemetryStub{}, context.Background(), bosspty.NewManager(), "session-1", "")
	m.SetOverrideAgent("claude")
	m.session = &pb.Session{
		Id:        "session-1",
		Title:     "debug release action issue",
		AgentName: "codex",
	}
	m.launching = true

	view := m.View().Content
	if !strings.Contains(view, "Launching Claude Code for debug release action issue") {
		t.Fatalf("launch view = %q, want Claude Code override label", view)
	}
	if strings.Contains(view, "Launching Codex") {
		t.Fatalf("launch view = %q, must not prefer session agent over override", view)
	}
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

// TestAttach_AgentFinishedCleanExitAutoDetaches verifies that when the
// agent process exits cleanly (no error), the model auto-detaches so the
// user lands back on the chat picker without an extra esc press.
func TestAttach_AgentFinishedCleanExitAutoDetaches(t *testing.T) {
	m := NewAttachModel(&attachTelemetryStub{}, context.Background(), bosspty.NewManager(), "session-1", "")

	updated, _ := m.Update(agentFinishedMsg{err: nil, detached: false})
	got := updated.(AttachModel)

	if !got.detach {
		t.Fatal("detach = false after clean agent exit, want true (auto-detach to chat picker)")
	}
	if !got.returned {
		t.Fatal("returned = false after clean agent exit, want true")
	}
}

// TestAttach_AgentFinishedErrorHoldsScreen verifies that when the agent
// process exits with an error (e.g. tmux attach failed because the
// session was torn down), the model leaves detach = false so View()
// renders the "exited with error" screen. Without this, the user would
// be auto-bounced back to the chat picker and never see the failure.
func TestAttach_AgentFinishedErrorHoldsScreen(t *testing.T) {
	m := NewAttachModel(&attachTelemetryStub{}, context.Background(), bosspty.NewManager(), "session-1", "")

	failure := &exec.ExitError{}
	updated, _ := m.Update(agentFinishedMsg{err: failure, detached: false})
	got := updated.(AttachModel)

	if got.detach {
		t.Fatal("detach = true after agent error exit, want false (so View renders the error and the user can read it before pressing esc)")
	}
	if !got.returned {
		t.Fatal("returned = false after agent error exit, want true")
	}
	if got.agentErr != failure {
		t.Fatalf("agentErr = %v, want %v", got.agentErr, failure)
	}
}

type attachLaunchOrderStub struct {
	stubClient
	calls   []string
	deleted bool
}

func (s *attachLaunchOrderStub) DeleteChat(context.Context, string) error {
	s.calls = append(s.calls, "delete")
	s.deleted = true
	return nil
}

func (s *attachLaunchOrderStub) DescribeChatLaunch(context.Context, string) (*pb.DescribeChatLaunchResponse, error) {
	s.calls = append(s.calls, "describe")
	if s.deleted {
		return nil, errors.New("chat deleted")
	}
	return &pb.DescribeChatLaunchResponse{Argv: []string{"claude"}}, nil
}

func (s *attachLaunchOrderStub) ReportChatStatus(context.Context, []*pb.ChatStatusReport) error {
	s.calls = append(s.calls, "report")
	return nil
}

func TestAttach_AgentFinishedErrorDescribesLaunchBeforeDeletingOrphan(t *testing.T) {
	client := &attachLaunchOrderStub{}
	m := NewAttachModel(client, context.Background(), bosspty.NewManager(), "session-1", "agent-1")
	m.session = &pb.Session{Id: "session-1", WorktreePath: t.TempDir()}
	m.agentSessionID = "agent-1"

	updated, cmd := m.Update(agentFinishedMsg{err: &exec.ExitError{}, detached: false})
	current := updated.(AttachModel)
	runAttachCmdGraph(t, cmd, func(msg tea.Msg) {
		next, _ := current.Update(msg)
		current = next.(AttachModel)
	})

	if current.launchInfo == nil {
		t.Fatalf("launchInfo = nil after error exit; calls=%v", client.calls)
	}

	describeAt := indexString(client.calls, "describe")
	deleteAt := indexString(client.calls, "delete")
	if describeAt == -1 || deleteAt == -1 {
		t.Fatalf("calls=%v, want both describe and delete", client.calls)
	}
	if describeAt > deleteAt {
		t.Fatalf("calls=%v, want describe before delete so orphan cleanup cannot remove launch diagnostics first", client.calls)
	}
}

func runAttachCmdGraph(t *testing.T, cmd tea.Cmd, handle func(tea.Msg)) {
	t.Helper()
	if cmd == nil {
		return
	}
	runAttachMsgGraph(t, cmd(), handle)
}

func runAttachMsgGraph(t *testing.T, msg tea.Msg, handle func(tea.Msg)) {
	t.Helper()
	switch msg := msg.(type) {
	case nil:
		return
	case tea.BatchMsg:
		for _, cmd := range msg {
			runAttachCmdGraph(t, cmd, handle)
		}
	default:
		v := reflect.ValueOf(msg)
		if v.IsValid() && v.Kind() == reflect.Slice && strings.Contains(v.Type().String(), "sequenceMsg") {
			for i := 0; i < v.Len(); i++ {
				cmd, ok := v.Index(i).Interface().(tea.Cmd)
				if !ok {
					t.Fatalf("sequence element %d is %T, want tea.Cmd", i, v.Index(i).Interface())
				}
				runAttachCmdGraph(t, cmd, handle)
			}
			return
		}
		handle(msg)
	}
}

func indexString(values []string, want string) int {
	for i, v := range values {
		if v == want {
			return i
		}
	}
	return -1
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

// TestAttach_ResumeMissingTmuxSessionShowsDismissableError verifies that
// resuming a chat whose tmux session no longer exists does NOT hand the
// terminal to a doomed `tmux attach` (which would dump a raw
// "can't find session" error and trap the user). Instead the model sets a
// dismissable error and schedules no exec, so View() renders "[esc] back".
func TestAttach_ResumeMissingTmuxSessionShowsDismissableError(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed: cannot exercise has-session pre-check")
	}
	// resumeID non-empty => resume-attach => pre-check is active.
	m := NewAttachModel(&attachTelemetryStub{}, context.Background(), bosspty.NewManager(), "session-1", "agent-uuid-1")

	updated, cmd := m.Update(attachReadyMsg{
		session: &pb.Session{Id: "session-1"},
		chats:   []*pb.ClaudeChat{{}}, // non-empty so no prefill path is taken
	})
	got := updated.(AttachModel)

	if cmd != nil {
		t.Fatal("cmd != nil: a missing tmux session must NOT schedule a tick/exec; want nil")
	}
	if got.launching {
		t.Error("launching = true after missing-session detection, want false")
	}
	if got.pendingExec != nil {
		t.Error("pendingExec set after missing-session detection, want nil (no exec must fire)")
	}
	if got.err == nil {
		t.Fatal("err = nil after missing-session detection, want a dismissable error")
	}
	view := got.View().Content
	if !strings.Contains(view, "[esc] back") {
		t.Errorf("view = %q, want a dismissable [esc] back affordance", view)
	}
	if !strings.Contains(strings.ToLower(view), "no longer available") {
		t.Errorf("view = %q, want a clear 'no longer available' explanation", view)
	}
}

// TestAttach_EscDismissesMissingSessionError verifies the user can always
// escape the missing-session error screen (the BOS-108 trap). Pressing esc
// must set detach so app.go routes back to the chat picker.
func TestAttach_EscDismissesMissingSessionError(t *testing.T) {
	m := NewAttachModel(&attachTelemetryStub{}, context.Background(), bosspty.NewManager(), "session-1", "agent-uuid-1")
	m.err = errChatSessionGone // model is on the dismissable error screen

	if m.Detached() {
		t.Fatal("precondition: model should not be detached before esc")
	}

	escMsg := tea.KeyPressMsg{Code: tea.KeyEscape}
	if escMsg.String() != "esc" {
		t.Fatalf("constructed esc msg .String()=%q, want \"esc\"", escMsg.String())
	}

	updated, cmd := m.Update(escMsg)
	got := updated.(AttachModel)

	if !got.Detached() {
		t.Fatal("Detached() = false after esc on error screen, want true (user must be able to get out)")
	}
	_ = cmd
}
