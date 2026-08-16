package views

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The canonical reason string the daemon stamps for an armed PR callback
// (displaystatus.CallbackWaitingReason). Spelled out literally here so a
// silent change to the composer's shape shows up as a TUI test failure and
// not just a bossalib one — the operator-visible wording is the contract.
const testWaitingReason = "awaiting checks_passed_ready on acme/widget#123"

// TestChatStatusString_Waiting pins the WAITING enum to the canonical label
// rather than falling through to "stopped" — a parked chat that renders as
// stopped would also be offered [w]ake, which does nothing for it.
func TestChatStatusString_Waiting(t *testing.T) {
	got := chatStatusString(pb.ChatStatus_CHAT_STATUS_WAITING)
	if got != displaystatus.WaitingLabel {
		t.Fatalf("chatStatusString(WAITING) = %q, want %q", got, displaystatus.WaitingLabel)
	}
	if got == statusStopped {
		t.Fatalf("WAITING must not collapse onto the stopped label")
	}
	if !displaystatus.IsWaitingLabel(got) {
		t.Fatalf("displaystatus.IsWaitingLabel(%q) = false", got)
	}
}

// TestRenderClaudeStatus_Waiting checks the per-chat badge: the canonical
// label, informational styling, and spinner — a chat parked on an external
// event is active while it awaits the external event.
func TestRenderClaudeStatus_Waiting(t *testing.T) {
	sp := newStatusSpinner()
	got := renderClaudeStatus(statusWaiting, sp)
	plain := stripANSI(got)
	want := sp.View() + displaystatus.WaitingLabel
	if plain != want {
		t.Fatalf("renderClaudeStatus(waiting) = %q, want %q", plain, want)
	}
	if !strings.Contains(plain, strings.TrimSpace(sp.View())) && strings.TrimSpace(sp.View()) != "" {
		t.Fatalf("waiting badge must animate a spinner: %q", plain)
	}
	if got != styleStatusInfo.Render(want) {
		t.Fatalf("waiting badge must use the info style (DISPLAY_INTENT_INFO parity)")
	}
}

// TestWaitingHintLine pins the shared wording both surfaces render: the
// daemon-supplied reason alone, with no label prefix (BOS-863). The web UI
// composes the identical string in services/web/src/sessionStatus.ts; if this
// changes, that must change with it.
func TestWaitingHintLine(t *testing.T) {
	got := waitingHintLine(testWaitingReason)
	want := testWaitingReason
	if got != want {
		t.Fatalf("waitingHintLine = %q, want %q", got, want)
	}
	if !strings.Contains(got, "checks_passed_ready") || !strings.Contains(got, "#123") {
		t.Fatalf("waiting hint must carry the trigger and PR number: %q", got)
	}
	if got := waitingHintLine(""); got != "" {
		t.Fatalf("waitingHintLine(\"\") = %q, want empty so callers skip the row", got)
	}
}

// TestSessionSubRowCount_WaitingReason confirms the waiting reason gets its own
// auxiliary row in the home list, and that an absent reason costs nothing — the
// count is the single source of truth for cursor arithmetic (BOS-474).
func TestSessionSubRowCount_WaitingReason(t *testing.T) {
	sess := &pb.Session{Id: "s1"}
	if got := sessionSubRowCount(sess, ""); got != 0 {
		t.Fatalf("sessionSubRowCount with no reason = %d, want 0", got)
	}
	if got := sessionSubRowCount(sess, testWaitingReason); got != 1 {
		t.Fatalf("sessionSubRowCount with a waiting reason = %d, want 1", got)
	}
}

// TestHome_WaitingReasonSubRow renders the home session list for a waiting
// session and asserts the reason is on screen beneath it — the operator must be
// able to read WHY the session is parked without opening it.
func TestHome_WaitingReasonSubRow(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	updated, _ := h.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	h = updated.(HomeModel)
	updated, _ = h.Update(sessionListMsg{
		sessions: []*pb.Session{{
			Id:              "s1",
			Title:           "parked session",
			RepoDisplayName: "widget",
			BranchName:      "b1",
			DisplayLabel:    displaystatus.WaitingLabel,
			DisplayIntent:   pb.DisplayIntent_DISPLAY_INTENT_INFO,
		}},
		daemonStatuses:       map[string]string{"s1": statusWaiting},
		daemonWaitingReasons: map[string]string{"s1": testWaitingReason},
	})
	h = updated.(HomeModel)

	rendered := stripANSI(h.View().Content)
	if !strings.Contains(rendered, displaystatus.WaitingLabel) {
		t.Fatalf("home list missing the waiting status in:\n%s", rendered)
	}
	if !strings.Contains(rendered, "checks_passed_ready") {
		t.Fatalf("home list missing the waiting trigger in:\n%s", rendered)
	}
	if !strings.Contains(rendered, "#123") {
		t.Fatalf("home list missing the awaited PR number in:\n%s", rendered)
	}
}

// TestChatPicker_WaitingReasonLine unit-tests the session-detail hint: only a
// waiting chat contributes, and the line carries the daemon-supplied reason
// alone — no label prefix (BOS-863).
func TestChatPicker_WaitingReasonLine(t *testing.T) {
	m := ChatPickerModel{
		chats: []*pb.ClaudeChat{
			{AgentSessionId: "a", AgentName: "claude"},
			{AgentSessionId: "b", AgentName: "claude"},
		},
		daemonStatuses:       map[string]string{"a": statusWorking, "b": statusWaiting},
		daemonWaitingReasons: map[string]string{"b": testWaitingReason},
	}
	got := m.waitingReasonLine()
	want := testWaitingReason
	if got != want {
		t.Fatalf("waitingReasonLine = %q, want %q", got, want)
	}
}

// TestChatPicker_WaitingReasonLine_EmptyWhenNoneWaiting keeps the line silent
// (and the layout unshifted) when nothing in the session is parked.
func TestChatPicker_WaitingReasonLine_EmptyWhenNoneWaiting(t *testing.T) {
	m := ChatPickerModel{
		chats:          []*pb.ClaudeChat{{AgentSessionId: "a", AgentName: "claude"}},
		daemonStatuses: map[string]string{"a": statusWorking},
	}
	if got := m.waitingReasonLine(); got != "" {
		t.Fatalf("waitingReasonLine = %q, want empty", got)
	}
	if got := m.waitingLineHeight(); got != 0 {
		t.Fatalf("waitingLineHeight = %d, want 0 so the table keeps its rows", got)
	}
}

// TestChatPicker_WaitingRendersReasonAndBadge is the session-detail proof in
// test form: one waiting chat alongside one genuinely working chat, with the
// waiting badge, the trigger and the PR number all on screen at once.
func TestChatPicker_WaitingRendersReasonAndBadge(t *testing.T) {
	stub := &chatPickerStub{}
	m := NewChatPickerModel(stub, context.Background(), "session-1", "")
	updated, _ := m.Update(chatsListedMsg{
		chats: []*pb.ClaudeChat{
			{SessionId: "session-1", AgentSessionId: "agent-working", Title: "implementing", AgentName: "claude", CreatedAt: timestamppb.Now()},
			{SessionId: "session-1", AgentSessionId: "agent-waiting", Title: "awaiting CI", AgentName: "claude", CreatedAt: timestamppb.Now()},
		},
		daemonStatuses: map[string]string{
			"agent-working": statusWorking,
			"agent-waiting": statusWaiting,
		},
		daemonWaitingReasons: map[string]string{"agent-waiting": testWaitingReason},
	})
	m = updated.(ChatPickerModel)
	updated, _ = m.Update(tea.WindowSizeMsg{Width: 140, Height: 34})
	m = updated.(ChatPickerModel)
	m.session = &pb.Session{Id: "session-1"}

	rendered := stripANSI(m.View().Content)
	for _, token := range []string{displaystatus.WaitingLabel, "working", "checks_passed_ready", "#123"} {
		if !strings.Contains(rendered, token) {
			t.Fatalf("session detail missing %q in:\n%s", token, rendered)
		}
	}
}
