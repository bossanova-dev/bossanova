package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/recurser/boss/api/tui/ui"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/bossalib/displaystatus"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// An actionable waiting reason that remains visible beneath the status badge.
// ready_for_review is neither routine (#2901) nor standby (BOS-1531), so Home
// keeps it.
const testWaitingReason = "awaiting ready_for_review on acme/widget#123"

// Home hides routine PR waits (#2901); opening the chat still carries them to
// the launch screen, because there that wait is all the parked chat is doing
// (BOS-1517).
func TestRoutinePRWaitingHintsHidden(t *testing.T) {
	for _, trigger := range []string{"checks_passed", "checks_passed_ready", "merged"} {
		t.Run(trigger, func(t *testing.T) {
			reason := displaystatus.CallbackWaitingReason(trigger, "acme", "widget", 123)
			if reason == "" {
				t.Fatal("callback must provide a waiting reason")
			}
			for _, demoted := range []bool{false, true} {
				if got := ui.WaitingHintLine(reason, demoted); got != "" {
					t.Fatalf("routine hint = %q, want empty", got)
				}
			}
			h := NewHomeModel(nil, context.Background(), nil)
			updated, _ := h.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
			h = updated.(HomeModel)
			sess := &pb.Session{Id: "s1", Title: "parked session", DisplayLabel: displaystatus.WaitingLabel, DisplaySpinner: true, DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_INFO}
			updated, _ = h.Update(sessionListMsg{Sessions: []*pb.Session{sess}, HeartbeatStatuses: map[string]string{"s1": ui.StatusWaiting}, DaemonWaitingReasons: map[string]string{"s1": reason}})
			h = updated.(HomeModel)
			if got := ui.SessionSubRowCount(sess, reason, true); got != 0 {
				t.Fatalf("routine wait reserves %d sub-rows", got)
			}
			rendered := stripANSI(h.View().Content)
			if strings.Contains(rendered, reason) || !strings.Contains(rendered, stripANSI(h.Spinner.View())+displaystatus.WaitingLabel) {
				t.Fatalf("expected waiting spinner without hint: %s", rendered)
			}

		})
	}
}

// TestChatStatusString_Waiting pins the WAITING enum to the canonical label
// rather than falling through to "stopped" — a parked chat that renders as
// stopped would also be offered [w]ake, which does nothing for it.
func TestChatStatusString_Waiting(t *testing.T) {
	got := ui.ChatStatusString(pb.ChatStatus_CHAT_STATUS_WAITING)
	if got != displaystatus.WaitingLabel {
		t.Fatalf("chatStatusString(WAITING) = %q, want %q", got, displaystatus.WaitingLabel)
	}
	if got == ui.StatusStopped {
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
	sp := ui.NewStatusSpinner()
	got := ui.RenderClaudeStatus(ui.StatusWaiting, sp)
	plain := stripANSI(got)
	want := sp.View() + displaystatus.WaitingLabel
	if plain != want {
		t.Fatalf("renderClaudeStatus(waiting) = %q, want %q", plain, want)
	}
	if !strings.Contains(plain, strings.TrimSpace(sp.View())) && strings.TrimSpace(sp.View()) != "" {
		t.Fatalf("waiting badge must animate a spinner: %q", plain)
	}
	if got != ui.StyleStatusInfo.Render(want) {
		t.Fatalf("waiting badge must use the info style (DISPLAY_INTENT_INFO parity)")
	}
}

// Other waiting reasons retain their daemon-supplied wording.
func TestWaitingHintLine(t *testing.T) {
	got := ui.WaitingHintLine(testWaitingReason, false)
	want := testWaitingReason
	if got != want {
		t.Fatalf("waitingHintLine = %q, want %q", got, want)
	}
	if !strings.Contains(got, "ready_for_review") || !strings.Contains(got, "#123") {
		t.Fatalf("waiting hint must carry the trigger and PR number: %q", got)
	}
	if got := ui.WaitingHintLine("", false); got != "" {
		t.Fatalf("waitingHintLine(\"\") = %q, want empty so callers skip the row", got)
	}
	if got := ui.WaitingHintLine("", true); got != "" {
		t.Fatalf("waitingHintLine(\"\", demoted) = %q, want empty — the prefix must not fabricate a line", got)
	}
}

// Visible hints regain their waiting prefix when the badge is superseded.
func TestWaitingHintLine_DemotedRowRegainsItsAntecedent(t *testing.T) {
	got := ui.WaitingHintLine(testWaitingReason, true)
	want := "waiting: " + testWaitingReason
	if got != want {
		t.Fatalf("waitingHintLine(demoted) = %q, want %q", got, want)
	}
	if !strings.Contains(got, "ready_for_review") || !strings.Contains(got, "#123") {
		t.Fatalf("the demoted hint must still carry the trigger and PR number: %q", got)
	}
}

// TestSessionSubRowCount_WaitingReason confirms the waiting reason gets its own
// auxiliary row in the home list, and that an absent reason costs nothing — the
// count is the single source of truth for cursor arithmetic (BOS-474).
func TestSessionSubRowCount_WaitingReason(t *testing.T) {
	sess := &pb.Session{Id: "s1"}
	if got := ui.SessionSubRowCount(sess, "", true); got != 0 {
		t.Fatalf("sessionSubRowCount with no reason = %d, want 0", got)
	}
	if got := ui.SessionSubRowCount(sess, testWaitingReason, true); got != 1 {
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
		Sessions: []*pb.Session{{
			Id:              "s1",
			Title:           "parked session",
			RepoDisplayName: "widget",
			BranchName:      "b1",
			DisplayLabel:    displaystatus.WaitingLabel,
			DisplayIntent:   pb.DisplayIntent_DISPLAY_INTENT_INFO,
		}},
		HeartbeatStatuses:    map[string]string{"s1": ui.StatusWaiting},
		DaemonWaitingReasons: map[string]string{"s1": testWaitingReason},
	})
	h = updated.(HomeModel)

	rendered := stripANSI(h.View().Content)
	if !strings.Contains(rendered, displaystatus.WaitingLabel) {
		t.Fatalf("home list missing the waiting status in:\n%s", rendered)
	}
	if !strings.Contains(rendered, "ready_for_review") {
		t.Fatalf("home list missing the waiting trigger in:\n%s", rendered)
	}
	if !strings.Contains(rendered, "#123") {
		t.Fatalf("home list missing the awaited PR number in:\n%s", rendered)
	}
}

// TestChatPicker_ArmedLineCarriesReasonVerbatim: only a waiting chat has an
// armed line, and it carries the chat title (BOS-1517) and the daemon-supplied
// reason verbatim, with no "waiting" label prefix (BOS-863).

// TestChatPicker_ArmedLine_EmptyWhenNoneWaiting: a chat that is not parked
// carries no armed line.

// TestChatPicker_WaitingRendersBadgeNotReason: one waiting chat alongside one
// genuinely working chat. Each row carries its own badge, and the daemon's
// reason is not repeated as a line under the table.

func TestReadyWaitingHints(t *testing.T) {
	sess := &pb.Session{DisplayLabel: displaystatus.ReadyLabel, DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_SUCCESS}
	if got := ui.RenderDisplayStatus(sess, ui.NewStatusSpinner()); got != ui.StyleStatusSuccess.Render(displaystatus.ReadyLabel) {
		t.Fatalf("Ready style = %q", got)
	}
	if !ui.WaitingBadgeSuperseded(sess) {
		t.Fatal("Ready must restore the waiting antecedent")
	}

	h := NewHomeModel(nil, context.Background(), nil)
	updated, _ := h.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	h = updated.(HomeModel)
	sess.Id, sess.Title = "s1", "Hand off the importer"
	updated, _ = h.Update(sessionListMsg{Sessions: []*pb.Session{sess}, HeartbeatStatuses: map[string]string{"s1": ui.StatusWaiting}, DaemonWaitingReasons: map[string]string{"s1": testWaitingReason}})
	h = updated.(HomeModel)
	rendered := stripANSI(h.View().Content)
	for _, token := range []string{displaystatus.ReadyLabel, "waiting: " + testWaitingReason} {
		if !strings.Contains(rendered, token) {
			t.Fatalf("missing %q in %s", token, rendered)
		}
	}
	if ui.WaitingBadgeSuperseded(nil) || ui.WaitingBadgeSuperseded(&pb.Session{DisplayLabel: displaystatus.WaitingLabel}) {
		t.Fatal("ordinary waiting rows retain their bare reason")
	}
}
