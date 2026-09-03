package views

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

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

func TestAgentDisplayName(t *testing.T) {
	if got := agentDisplayName("opencode"); got != "OpenCode" {
		t.Errorf("agentDisplayName(opencode) = %q, want OpenCode", got)
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

func TestRenderLaunchDiagnostic_DoesNotAssertSingleCause(t *testing.T) {
	info := &pb.DescribeChatLaunchResponse{
		Argv:      []string{"claude", "--session-id", "abc"},
		AgentName: "claude",
	}
	out := renderLaunchDiagnostic(info, "Claude Code")
	// Regression: the old prose asserted the CLI "could not be launched" — a
	// PATH/login-shell verdict that misdiagnosed an agent which DID launch and
	// then exited on its own (e.g. "Session ID already in use"). The diagnostic
	// must name the agent-refused-to-start possibility and not claim one cause.
	if strings.Contains(out, "could not be launched") {
		t.Errorf("diagnostic still asserts the CLI could not be launched:\n%s", out)
	}
	if !strings.Contains(out, "refusing to start") {
		t.Errorf("diagnostic should mention the agent may refuse to start:\n%s", out)
	}
}

// TestRenderLaunchDiagnostic_ShowsCapturedOutput verifies that when bossd
// captured the agent's own final output (BOS-477), the diagnostic leads with it
// verbatim under a "reported:" heading, and still shows the reproduction command.
func TestRenderLaunchDiagnostic_ShowsCapturedOutput(t *testing.T) {
	info := &pb.DescribeChatLaunchResponse{
		Argv:           []string{"claude", "--session-id", "abc"},
		WorktreePath:   "/work/tree",
		Host:           "tomo",
		AgentName:      "claude",
		CapturedOutput: "Error: Session ID abc is already in use",
	}
	out := renderLaunchDiagnostic(info, "Claude Code")
	for _, want := range []string{
		"Claude Code reported:",
		"Session ID abc is already in use",
		"bossd ran this on host 'tomo':",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("diagnostic missing %q in:\n%s", want, out)
		}
	}
	// The neutral PATH/login-shell fallback prose must NOT appear when we have
	// the agent's real error.
	if strings.Contains(out, "refusing to start") {
		t.Fatalf("captured-output diagnostic should not show the fallback prose:\n%s", out)
	}
}

// TestRenderLaunchDiagnostic_FallsBackWhenCapturedEmpty verifies the empty-capture
// case still renders the neutral fallback prose (acceptance criterion 3).
func TestRenderLaunchDiagnostic_FallsBackWhenCapturedEmpty(t *testing.T) {
	info := &pb.DescribeChatLaunchResponse{
		Argv:      []string{"claude", "--session-id", "abc"},
		AgentName: "claude",
		// CapturedOutput deliberately empty.
	}
	out := renderLaunchDiagnostic(info, "Claude Code")
	if !strings.Contains(out, "refusing to start") {
		t.Fatalf("empty-capture diagnostic should fall back to neutral prose:\n%s", out)
	}
	if strings.Contains(out, "reported:") {
		t.Fatalf("empty-capture diagnostic should not show a reported: block:\n%s", out)
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

// blockingRecordChatStub stands in for a wedged daemon: RecordChat parks until
// the test releases it. Any implementation that calls it from inside Update
// parks the whole Bubble Tea loop with it.
type blockingRecordChatStub struct {
	stubClient
	release chan struct{}
}

// newBlockingRecordChatStub returns a stub whose RecordChat blocks until the
// test ends, releasing it from t.Cleanup so no goroutine is left parked.
func newBlockingRecordChatStub(t *testing.T) *blockingRecordChatStub {
	t.Helper()
	s := &blockingRecordChatStub{release: make(chan struct{})}
	t.Cleanup(func() { close(s.release) })
	return s
}

func (s *blockingRecordChatStub) RecordChat(context.Context, string, string, string, string, bool) (*pb.ClaudeChat, error) {
	<-s.release
	return &pb.ClaudeChat{TmuxSessionName: "boss-test-chat"}, nil
}

// updateWithin runs Update on a goroutine and fails the test if it has not
// returned within d. Bounding it this way means a regression to a synchronous
// RecordChat FAILS these tests instead of hanging the package.
func updateWithin(t *testing.T, m AttachModel, msg tea.Msg, d time.Duration) (AttachModel, tea.Cmd) {
	t.Helper()
	type result struct {
		model tea.Model
		cmd   tea.Cmd
	}
	done := make(chan result, 1)
	go func() {
		updated, cmd := m.Update(msg)
		done <- result{model: updated, cmd: cmd}
	}()
	select {
	case r := <-done:
		return r.model.(AttachModel), r.cmd
	case <-time.After(d):
		t.Fatalf("Update(%T) did not return within %s — the RPC is still running on the update loop", msg, d)
		return AttachModel{}, nil
	}
}

// driveAttachReady runs the two-step launch: Update(attachReadyMsg) now only
// stages RecordChat as a tea.Cmd, so everything downstream of the RPC
// (pendingExec, telemetry, the resume guard) only materialises once that cmd's
// chatRecordedMsg is fed back in. Returns the model and cmd from the second step.
func driveAttachReady(t *testing.T, m AttachModel, msg attachReadyMsg) (AttachModel, tea.Cmd) {
	t.Helper()
	updated, cmd := m.Update(msg)
	staged := updated.(AttachModel)
	if cmd == nil {
		t.Fatal("attachReadyMsg returned a nil cmd, want the off-loop RecordChat cmd")
	}
	produced := cmd()
	recorded, ok := produced.(chatRecordedMsg)
	if !ok {
		t.Fatalf("attachReadyMsg cmd produced %T, want chatRecordedMsg", produced)
	}
	next, nextCmd := staged.Update(recorded)
	return next.(AttachModel), nextCmd
}

// TestAttach_AttachReadyDoesNotBlockOnRecordChat is the core BOS-723 guard: the
// RecordChat RPC must run inside a tea.Cmd, not inside Update. Against a daemon
// that never answers, Update must still return promptly (with the cmd that will
// do the RPC), because a blocked Update reads no keys and repaints nothing.
func TestAttach_AttachReadyDoesNotBlockOnRecordChat(t *testing.T) {
	m := NewAttachModel(newBlockingRecordChatStub(t), context.Background(), bosspty.NewManager(), "session-1", "")

	_, cmd := updateWithin(t, m, attachReadyMsg{
		session: &pb.Session{Id: "session-1"},
		chats:   nil,
	}, 2*time.Second)

	if cmd == nil {
		t.Fatal("cmd = nil, want the tea.Cmd that performs RecordChat off the update loop")
	}
}

// TestAttach_EscDetachesWhileRecordChatIsInFlight is the user-visible half: with
// the RPC parked, the model must still process keys, so esc gets the user out
// instead of leaving Ctrl+C as the only escape.
func TestAttach_EscDetachesWhileRecordChatIsInFlight(t *testing.T) {
	m := NewAttachModel(newBlockingRecordChatStub(t), context.Background(), bosspty.NewManager(), "session-1", "")

	staged, _ := updateWithin(t, m, attachReadyMsg{
		session: &pb.Session{Id: "session-1"},
		chats:   nil,
	}, 2*time.Second)
	if !staged.launching {
		t.Fatal("launching = false while RecordChat is in flight, want true (the launching line must keep rendering)")
	}

	updated, _ := updateWithin(t, staged, tea.KeyPressMsg{Code: tea.KeyEscape}, 2*time.Second)
	if !updated.Detached() {
		t.Fatal("Detached() = false after esc during an in-flight RecordChat, want true")
	}
}

// TestAttach_ChatRecordedErrorRendersDismissableError verifies a failed (e.g.
// deadline-exceeded) RecordChat lands on the dismissable error frame rather
// than leaving the launching line up forever.
func TestAttach_ChatRecordedErrorRendersDismissableError(t *testing.T) {
	m := NewAttachModel(&attachTelemetryStub{}, context.Background(), bosspty.NewManager(), "session-1", "")

	updated, cmd := m.Update(chatRecordedMsg{
		session: &pb.Session{Id: "session-1"},
		err:     errors.New("context deadline exceeded"),
	})
	got := updated.(AttachModel)

	if cmd != nil {
		t.Error("cmd != nil after a failed RecordChat, want nil (nothing more to schedule)")
	}
	if got.err == nil {
		t.Fatal("err = nil after a failed RecordChat, want the wrapped error")
	}
	view := got.View().Content
	if !strings.Contains(view, "record chat") {
		t.Errorf("view = %q, want the wrapped 'record chat' error", view)
	}
	if !strings.Contains(view, "[esc] back") {
		t.Errorf("view = %q, want a dismissable [esc] back affordance", view)
	}
	if strings.Contains(view, "Launching") {
		t.Errorf("view = %q, must not still render the launching line", view)
	}
}

// TestAttach_StaleChatRecordedMsgIsDropped guards the hazard the off-loop move
// introduced: RecordChat can now outlive the model that issued it.
//
// esc → chat picker → open a different chat replaces a.attach wholesale
// (app_routing.go, app_delegate.go) while the abandoned RPC is still in flight.
// Without the identity check, that late message drives the NEW model with the
// OLD chat's tmux session name — registering it under the new model's agent
// session id and scheduling an exec the new model's detach guard does not stop.
func TestAttach_StaleChatRecordedMsgIsDropped(t *testing.T) {
	m := NewAttachModel(&attachTelemetryStub{}, context.Background(), bosspty.NewManager(), "session-1", "")
	m.agentSessionID = "the-chat-now-open"
	m.launchID = 7

	updated, cmd := m.Update(chatRecordedMsg{
		session:  &pb.Session{Id: "session-1"},
		chat:     &pb.ClaudeChat{TmuxSessionName: "tmux-for-the-abandoned-chat"},
		launchID: 6,
	})
	got := updated.(AttachModel)

	if cmd != nil {
		t.Error("cmd != nil for a stale chatRecordedMsg, want nil (nothing must be scheduled for a chat the user left)")
	}
	if got.pendingExec != nil {
		t.Errorf("pendingExec = %+v for a stale chatRecordedMsg, want nil", got.pendingExec)
	}
	if got.tmuxName != "" {
		t.Errorf("tmuxName = %q, want empty: the abandoned chat's tmux session must not be adopted", got.tmuxName)
	}
	if got.err != nil {
		t.Errorf("err = %v, want nil: a stale result is dropped silently, not surfaced to the user", got.err)
	}
}

// TestAttach_ChatRecordedMsgIsDroppedAfterDetach covers the simpler half: the
// user pressed esc while the RPC was parked, so nothing from it may be acted on.
// TestAttach_UnstagedModelDropsAnyChatRecordedMsg turns attachLaunchSeq's
// documented invariant into a gate. The guard is safe only because Add returns
// the POST-increment value, so a real chatRecordedMsg always carries >= 1 and a
// model that never staged a launch (launchID at its zero value) matches
// nothing. Every other launchID assertion hand-sets a non-zero ticket first, so
// all of them would still pass if the mint changed to a pre-increment scheme or
// a per-model counter — and a freshly built AttachModel would then start
// accepting an abandoned attempt's result, which is the whole bug the nonce
// exists to prevent.
func TestAttach_UnstagedModelDropsAnyChatRecordedMsg(t *testing.T) {
	// Rewind the process-wide counter so the FIRST issue is what gets asserted.
	// A pre-increment mint — the regression this half exists to catch — returns
	// 0 only on the first Add of the process, and by the time this test runs in
	// a whole-package run several attach tests have already minted, so without
	// the rewind the assertion would pass against the very scheme it targets.
	// Safe to mutate: no test in this package that touches attach runs parallel.
	prevSeq := attachLaunchSeq.Swap(0)
	t.Cleanup(func() { attachLaunchSeq.Store(prevSeq) })
	if got := attachLaunchSeq.Add(1); got == 0 {
		t.Fatalf("attachLaunchSeq.Add(1) = 0 on the first issue: the mint must never hand out the zero value an unstaged model carries")
	}

	m := NewAttachModel(&attachTelemetryStub{}, context.Background(), bosspty.NewManager(), "session-1", "")
	if m.launchID != 0 {
		t.Fatalf("a fresh AttachModel has launchID %d, want 0 — this test's premise is that an unstaged model carries the zero value", m.launchID)
	}

	updated, cmd := m.Update(chatRecordedMsg{
		session:  &pb.Session{Id: "session-1"},
		chat:     &pb.ClaudeChat{TmuxSessionName: "tmux-from-some-other-attach"},
		launchID: 1,
	})
	got := updated.(AttachModel)

	if cmd != nil {
		t.Error("cmd != nil: a model that never staged a launch must not act on any result")
	}
	if got.pendingExec != nil {
		t.Errorf("pendingExec = %+v: a model that never staged a launch must not act on any result", got.pendingExec)
	}
	if got.tmuxName != "" {
		t.Errorf("tmuxName = %q, want empty", got.tmuxName)
	}
}

func TestAttach_ChatRecordedMsgIsDroppedAfterDetach(t *testing.T) {
	m := NewAttachModel(&attachTelemetryStub{}, context.Background(), bosspty.NewManager(), "session-1", "")
	m.agentSessionID = "chat-1"
	m.launchID = 3
	m.detach = true

	updated, cmd := m.Update(chatRecordedMsg{
		session:  &pb.Session{Id: "session-1"},
		chat:     &pb.ClaudeChat{TmuxSessionName: "tmux-1"},
		launchID: 3,
	})
	got := updated.(AttachModel)

	if cmd != nil {
		t.Error("cmd != nil after detach, want nil")
	}
	if got.pendingExec != nil {
		t.Errorf("pendingExec = %+v after detach, want nil", got.pendingExec)
	}
}

// TestAttach_AbandonedResultCannotReprimeAConsumedLaunch is why the guard keys
// on a launch nonce rather than on the chat: resuming the chat the user just
// backed out of gives both attempts the SAME agentSessionID, so a chat-keyed
// guard would let the abandoned attempt's result through.
//
// It would land on a model that has already launched and had its stash
// consumed by startExecMsg. Which harm lands first depends on what
// probeTmuxSession answers, so all three assertions below are load-bearing in
// some environment and the test is red in every one:
//   - probe says gone (the usual answer where no tmux session exists, e.g. CI):
//     the resume guard sets errChatSessionGone, overwriting the live model's
//     state — and since View() checks m.err before m.returned, that replaces an
//     agent-exited diagnostic screen with a bogus "start a new chat".
//   - probe says alive or cannot tell: the handler runs on and, because it sets
//     m.pendingExec unconditionally, re-primes the stash and schedules a second
//     exec that startExecMsg's pendingExec == nil guard can no longer absorb —
//     re-running `tmux attach` over whatever the user is looking at.
func TestAttach_AbandonedResultCannotReprimeAConsumedLaunch(t *testing.T) {
	// resumeID is empty so the launch reaches pendingExec: the resume path's
	// probeTmuxSession guard would short-circuit it in a test environment.
	// The abandoned duplicate below is what carries resume, as it would when
	// the user re-opens the chat they backed out of.
	m := NewAttachModel(&attachTelemetryStub{}, context.Background(), bosspty.NewManager(), "session-1", "")

	launched, _ := driveAttachReady(t, m, attachReadyMsg{
		session: &pb.Session{Id: "session-1"},
		chats:   []*pb.ClaudeChat{{}}, // non-empty: no prefill path
	})
	if launched.pendingExec == nil {
		t.Fatal("precondition: the launch did not prime pendingExec, so the assertions below would hold vacuously")
	}
	// startExecMsg consumes the stash, exactly as the real launch does.
	consumed, _ := launched.Update(startExecMsg{})
	afterExec := consumed.(AttachModel)
	if afterExec.pendingExec != nil {
		t.Fatal("precondition: startExecMsg did not consume pendingExec")
	}

	// The abandoned attempt's result finally arrives. Same chat, same session,
	// different launch.
	updated, cmd := afterExec.Update(chatRecordedMsg{
		session:  &pb.Session{Id: "session-1"},
		chat:     &pb.ClaudeChat{TmuxSessionName: "boss-test-chat"},
		launchID: afterExec.launchID - 1,
		resume:   true,
	})
	got := updated.(AttachModel)

	if got.pendingExec != nil {
		t.Errorf("pendingExec = %+v: an abandoned result re-primed a launch that had already been consumed", got.pendingExec)
	}
	if cmd != nil {
		t.Error("cmd != nil: an abandoned result scheduled a second exec")
	}
	if got.err != nil {
		t.Errorf("err = %v: an abandoned result overwrote the model's state", got.err)
	}
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

	_, cmd := driveAttachReady(t, m, attachReadyMsg{
		session: &pb.Session{Id: "session-1"},
		chats:   nil,
	})
	if cmd == nil {
		t.Fatal("expected a tick cmd from chatRecordedMsg")
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
// rendered across both halves of the launch — the attachReadyMsg that stages
// RecordChat and the chatRecordedMsg that lands it: m.launching must remain
// true and m.pendingExec must be primed for the follow-up tick. Without this,
// the "Launching… Press Ctrl+X to detach" line flashes for only the RPC time.
func TestAttach_AttachReadyDefersExec(t *testing.T) {
	m := NewAttachModel(&attachTelemetryStub{}, context.Background(), bosspty.NewManager(), "session-1", "")

	staged, _ := m.Update(attachReadyMsg{
		session: &pb.Session{Id: "session-1"},
		chats:   nil,
	})
	if !staged.(AttachModel).launching {
		t.Fatal("launching = false while RecordChat is still in flight, want true")
	}

	got, cmd := driveAttachReady(t, m, attachReadyMsg{
		session: &pb.Session{Id: "session-1"},
		chats:   nil,
	})

	if !got.launching {
		t.Fatal("launching = false after chatRecordedMsg, want true (the message must keep rendering)")
	}
	if got.pendingExec == nil {
		t.Fatal("pendingExec = nil after chatRecordedMsg, want a non-nil stash")
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

	primed, _ := driveAttachReady(t, m, attachReadyMsg{
		session: &pb.Session{Id: "session-1"},
		chats:   nil,
	})
	if primed.pendingExec == nil {
		t.Fatal("precondition: pendingExec not set by chatRecordedMsg")
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

func (s *attachLaunchOrderStub) DescribeChatMCP(context.Context, string) (*pb.DescribeChatMCPResponse, error) {
	return nil, nil
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

	primed, _ := driveAttachReady(t, m, attachReadyMsg{
		session: &pb.Session{Id: "session-1"},
		chats:   nil,
	})
	if primed.pendingExec == nil {
		t.Fatal("precondition: pendingExec not set, so the noop assertions below would hold vacuously")
	}
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

	got, cmd := driveAttachReady(t, m, attachReadyMsg{
		session: &pb.Session{Id: "session-1"},
		chats:   []*pb.ClaudeChat{{}}, // non-empty so no prefill path is taken
	})

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

// TestAttachTmuxEnvNormalizesTerm verifies attachTmuxEnv delegates to
// termnorm to produce a resolvable TERM for the `tmux attach` child, and
// that the returned effective TERM matches what's actually in the env.
func TestAttachTmuxEnvNormalizesTerm(t *testing.T) {
	base := []string{"PATH=/bin", "TERM=xterm-ghostty"}
	// force fallback by pointing probe through termnorm's seam is out of scope
	// here; assert the helper delegates to termnorm by checking TERM is present
	// and non-empty (integration correctness is covered by termnorm's own tests).
	env, eff := attachTmuxEnv(base, "")
	if eff == "" {
		t.Fatal("effective TERM empty")
	}
	got := ""
	for _, e := range env {
		if strings.HasPrefix(e, "TERM=") {
			got = strings.TrimPrefix(e, "TERM=")
		}
	}
	if got != eff {
		t.Fatalf("env TERM %q != effective %q", got, eff)
	}
}

// TestRenderTmuxAttachDiagnostic verifies the diagnostic includes a
// copy-pasteable `TERM=<eff> tmux -u attach -t <name>` reproduction command
// plus the captured tmux startup output (e.g. a missing-terminfo error).
//
// The `-u` is asserted verbatim because the whole point of this line is that it
// reproduces the attach boss actually ran; a repro missing a flag the real argv
// carries is the drift this test exists to catch.
func TestRenderTmuxAttachDiagnostic(t *testing.T) {
	out := renderTmuxAttachDiagnostic(
		"",
		"boss-abc-123",
		"xterm-256color",
		"missing or unsuitable terminal: xterm-ghostty\n",
	)
	for _, want := range []string{
		"tmux -u attach -t boss-abc-123",
		"TERM=xterm-256color",
		"missing or unsuitable terminal: xterm-ghostty",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("diagnostic missing %q; got:\n%s", want, out)
		}
	}
}

// TestRenderTmuxAttachDiagnosticEmptyWhenNoSignal verifies the diagnostic
// renders blank when there's no tmux session name to reproduce with.
func TestRenderTmuxAttachDiagnosticEmptyWhenNoSignal(t *testing.T) {
	if got := renderTmuxAttachDiagnostic("", "", "", ""); got != "" {
		t.Fatalf("want empty diagnostic, got %q", got)
	}
}

// TestSanitizeTmuxTail verifies that the captured PTY tail is stripped of ANSI
// escape sequences and stray C0 control bytes (so it can't scramble the error
// frame) while plain text, newlines, and tabs survive.
func TestSanitizeTmuxTail(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain text passes through", "missing or unsuitable terminal: xterm-ghostty", "missing or unsuitable terminal: xterm-ghostty"},
		{"newlines and tabs kept", "line1\n\tline2", "line1\n\tline2"},
		{"CSI color sequence stripped", "\x1b[31mred error\x1b[0m", "red error"},
		{"cursor-move sequence stripped", "\x1b[2J\x1b[Hcleared", "cleared"},
		{"stray control bytes removed", "err\x07\x08\rmsg", "errmsg"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeTmuxTail(tc.in); got != tc.want {
				t.Fatalf("sanitizeTmuxTail(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRenderTmuxAttachDiagnosticSanitizesTail verifies escape sequences in the
// captured tail don't reach the rendered diagnostic while the human-readable
// error text still does.
func TestRenderTmuxAttachDiagnosticSanitizesTail(t *testing.T) {
	out := renderTmuxAttachDiagnostic(
		"",
		"boss-abc-123",
		"xterm-256color",
		"\x1b[2J\x1b[31mmissing or unsuitable terminal: xterm-ghostty\x1b[0m\n",
	)
	if !strings.Contains(out, "missing or unsuitable terminal: xterm-ghostty") {
		t.Fatalf("diagnostic dropped the error text; got:\n%s", out)
	}
	if strings.Contains(out, "\x1b[2J") || strings.Contains(out, "\x1b[31m") {
		t.Fatalf("diagnostic leaked raw escape sequences; got:\n%q", out)
	}
}
