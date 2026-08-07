package views

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestChatPicker_TerminalActionIsHiddenUnderRemoteHost pins the gate at its
// single source: newTabSupported already hides both the [t]erminal key and the
// action-bar entry, so folding the remote context into it is the whole fix.
// [t]erminal opens the session's worktree in a LOCAL terminal tab, and under
// --host that path belongs to the other machine.
func TestChatPicker_TerminalActionIsHiddenUnderRemoteHost(t *testing.T) {
	withSupportedNewTabTerminal(t)
	withHostDestination(t, "deploy@bastion.example.com")

	m := NewChatPickerModel(&chatPickerHostStub{}, context.Background(), "session-1", "")
	if m.newTabSupported {
		t.Fatal("newTabSupported = true under a --host context, want false: the worktree path is on the remote machine")
	}
}

// TestChatPicker_TerminalActionFollowsTheTerminalLocally is the falsification
// half: with no --host destination the flag must go back to answering the one
// question it always answered — does this terminal support new tabs. Asserted
// against a *true* answer, so a gate that fired unconditionally cannot pass.
func TestChatPicker_TerminalActionFollowsTheTerminalLocally(t *testing.T) {
	withSupportedNewTabTerminal(t)
	withHostDestination(t, "")

	m := NewChatPickerModel(&chatPickerHostStub{}, context.Background(), "session-1", "")
	if !m.newTabSupported {
		t.Fatal("newTabSupported = false locally in a terminal that supports new tabs, want true (local behaviour unchanged)")
	}
}

// withSupportedNewTabTerminal forces hasNewTabSupport() to answer true for one
// test. Without it the two assertions above are vacuous everywhere the test
// actually runs unattended: hasNewTabSupport() reads $TMUX / $TERM_PROGRAM /
// $ITERM_SESSION_ID / $GHOSTTY_RESOURCES_DIR, none of which are set on a CI box,
// so `hasNewTabSupport() && !isRemoteHost()` is already false from the left
// operand and deleting the --host gate entirely would still pass.
func withSupportedNewTabTerminal(t *testing.T) {
	t.Helper()
	// $TMUX is the first branch newTabCmd takes and the only one that is
	// platform-independent, so it makes the answer true on every runner.
	t.Setenv("TMUX", "/private/tmp/tmux-501/default,1,0")
	if !hasNewTabSupport() {
		t.Fatal("precondition: hasNewTabSupport() = false with $TMUX set; the assertions below would be vacuous")
	}
}

// TestChatPicker_ActionBarTracksNewTabSupport pins that the flag is what the
// action bar reads, so the gate above actually removes the button — not just a
// field nothing renders.
func TestChatPicker_ActionBarTracksNewTabSupport(t *testing.T) {
	withHostDestination(t, "")

	m := NewChatPickerModel(&chatPickerHostStub{}, context.Background(), "session-1", "")

	m.newTabSupported = true
	_, middle, _ := m.chatListActionGroups()
	if !containsAction(middle, "[t]erminal") {
		t.Fatalf("action bar = %q, want [t]erminal when new tabs are supported", middle)
	}

	m.newTabSupported = false
	_, middle, _ = m.chatListActionGroups()
	if containsAction(middle, "[t]erminal") {
		t.Fatalf("action bar = %q, want no [t]erminal when new tabs are unsupported", middle)
	}
	// The rest of the bar must survive the removal — hiding one action is not
	// permission to render an empty bar.
	if !containsAction(middle, "[n]ew chat") {
		t.Fatalf("action bar = %q, want the remaining actions intact", middle)
	}
}

// TestChatPicker_KeyNewTabIsInertUnderRemoteHost covers the keystroke as well as
// the button: a hidden action whose key still fires would open a remote path in
// a local terminal, which is the failure the button was hidden to prevent.
func TestChatPicker_KeyNewTabIsInertUnderRemoteHost(t *testing.T) {
	withSupportedNewTabTerminal(t)
	withHostDestination(t, "deploy@bastion.example.com")

	m := NewChatPickerModel(&chatPickerHostStub{}, context.Background(), "session-1", "")
	m.session = &pb.Session{Id: "session-1", WorktreePath: "/home/deploy/worktrees/feature"}

	if _, cmd := m.keyNewTab(); cmd != nil {
		t.Fatal("keyNewTab returned a cmd under a --host context, want nil")
	}
}

// TestChatPicker_BackfillTitlesSkipsLocalReadsUnderRemoteHost proves the loop is
// skipped, not merely harmless: the spy counts the reads, because reading a
// worktree that lives on another machine returns "" without erroring and the
// titles would silently stay "New chat" forever.
func TestChatPicker_BackfillTitlesSkipsLocalReadsUnderRemoteHost(t *testing.T) {
	withHostDestination(t, "deploy@bastion.example.com")
	spy := withTranscriptSpy(t, "a parsed title", false)

	m := chatPickerAwaitingBackfill()
	if cmd := m.backfillTitles(); cmd != nil {
		t.Error("backfillTitles returned a cmd under a --host context, want nil")
		runAttachCmdGraph(t, cmd, func(tea.Msg) {})
	}
	if spy.titleCalls != 0 {
		t.Fatalf("agent.ChatTitle called %d time(s) under --host, want 0", spy.titleCalls)
	}
}

// TestChatPicker_BackfillTitlesStillRunsLocally is the falsification half — a
// gate that fired unconditionally would pass the test above.
func TestChatPicker_BackfillTitlesStillRunsLocally(t *testing.T) {
	withHostDestination(t, "")
	spy := withTranscriptSpy(t, "a parsed title", false)

	m := chatPickerAwaitingBackfill()
	cmd := m.backfillTitles()
	if cmd == nil {
		t.Fatal("backfillTitles returned nil locally, want the backfill cmd")
	}
	runAttachCmdGraph(t, cmd, func(tea.Msg) {})

	if spy.titleCalls != 1 {
		t.Fatalf("agent.ChatTitle called %d time(s) locally, want 1 (unchanged local backfill)", spy.titleCalls)
	}
}

// chatPickerAwaitingBackfill builds a picker holding one untitled chat, which is
// the only state backfillTitles does any work in.
func chatPickerAwaitingBackfill() ChatPickerModel {
	m := NewChatPickerModel(&chatPickerHostStub{}, context.Background(), "session-1", "")
	m.session = &pb.Session{Id: "session-1", WorktreePath: "/home/deploy/worktrees/feature"}
	m.chats = []*pb.ClaudeChat{{AgentSessionId: "agent-1", Title: "New chat"}}
	return m
}

func containsAction(actions []string, want string) bool {
	for _, a := range actions {
		if strings.Contains(a, want) {
			return true
		}
	}
	return false
}

// chatPickerHostStub is a client that answers nothing: these tests drive model
// construction and pure view/command builders, never an RPC.
type chatPickerHostStub struct {
	stubClient
}

func (s *chatPickerHostStub) UpdateChatTitle(context.Context, string, string) error { return nil }

// TestChatPicker_ErrStateNeverShowsTheForwardedSocketDial is the session/chat
// half of acceptance criterion 2 — the gap the ticket names. The home screen
// already knew about the --host context; this view did not, so a tunnel that
// dropped while a session was open rendered
// "Error: unavailable: dial unix /var/folders/…/bossd.sock: …" and the session
// looked frozen. The absence assertion is the regression guard.
func TestChatPicker_ErrStateNeverShowsTheForwardedSocketDial(t *testing.T) {
	withHostDestination(t, "deploy@bastion.example.com")
	withHostTunnelReason(t, "remotehost: ssh tunnel to deploy@bastion.example.com exited: signal: terminated")

	m := ChatPickerModel{err: errForwardedSocketDial, width: 100, sessionID: "session-1"}
	rendered := normalizedRender(m.renderErrState())

	assertNoForwardedSocketDial(t, "chat picker error state", rendered)
	if !strings.Contains(rendered, "Reconnecting to deploy@bastion.example.com") {
		t.Fatalf("chat picker error state %q must show the reconnecting affordance naming the destination", rendered)
	}
	if !strings.Contains(rendered, "signal: terminated") {
		t.Fatalf("chat picker error state %q must report why the tunnel went down", rendered)
	}
	// The view's own affordances must survive the substitution.
	if !strings.Contains(rendered, "[esc] back") {
		t.Fatalf("chat picker error state %q must keep its action bar", rendered)
	}
}

// TestChatPicker_ErrStateKeepsLocalErrorsVerbatim is acceptance criterion 6 for
// this view: nothing changes when boss is driving the local daemon, and an
// ordinary RPC failure still reads as itself.
func TestChatPicker_ErrStateKeepsLocalErrorsVerbatim(t *testing.T) {
	withHostDestination(t, "")
	withHostTunnelReason(t, "")

	m := ChatPickerModel{err: errForwardedSocketDial, width: 100, sessionID: "session-1"}
	rendered := normalizedRender(m.renderErrState())
	if !strings.Contains(rendered, "dial unix") {
		t.Fatalf("local chat picker error state %q must keep showing the real error", rendered)
	}
	if strings.Contains(rendered, "Reconnecting to") {
		t.Fatalf("local chat picker error state %q must not claim an ssh tunnel is being redialled", rendered)
	}
}
