package views

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/fixtures"
	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// BOS-1531: after a build hands off to verify, the build chat may still hold a
// standby (contingency) watch while the verify chat blocks the PR. These tests
// pin how the TUI tells the two apart.

// standbyCallback is a live callback for chatID on acme/my-app#2960.
func standbyCallback(id, chatID, trigger string) *pb.GithubCallback {
	cb := armedTestCallback(id, trigger)
	cb.TargetChatId = chatID
	return cb
}

// standbyPicker lists a build chat (agent-aaa, first in table order, holding
// buildReason) and a verify chat (agent-zzz, holding verifyReason; "" leaves it
// working) into a picker whose callbacks are known.
func standbyPicker(t *testing.T, sess *pb.Session, buildReason, verifyReason string) ChatPickerModel {
	t.Helper()
	stub := &chatPickerStub{callbacks: []*pb.GithubCallback{
		standbyCallback("cb-build", "agent-aaa", displaystatus.CallbackWaitingTrigger(buildReason)),
		standbyCallback("cb-verify", "agent-zzz", displaystatus.CallbackWaitingTrigger(verifyReason)),
	}}
	statuses := map[string]string{"agent-aaa": statusWaiting, "agent-zzz": statusWorking}
	reasons := map[string]string{"agent-aaa": buildReason}
	if verifyReason != "" {
		statuses["agent-zzz"] = statusWaiting
		reasons["agent-zzz"] = verifyReason
	}
	return armedPicker(t, stub, sess,
		[]*pb.ClaudeChat{verifyChat("agent-aaa", "build"), verifyChat("agent-zzz", "verify")},
		statuses, reasons)
}

func statusCellsByTitle(m ChatPickerModel) map[string]string {
	rows := m.table.Rows()
	out := map[string]string{}
	for _, r := range rows {
		out[stripANSI(r[1])] = r[len(r)-1]
	}
	return out
}

// A standby chat's STATUS cell is a muted "standby" with no spinner; the
// blocking chat keeps "<spinner><trigger>" in INFO style. Standby is decided by
// the daemon's reason, so it holds before the callback fetch answers too.
func TestChatPicker_StandbyStatusCell(t *testing.T) {
	for _, trigger := range []string{"checks_failed", "closed"} {
		t.Run(trigger, func(t *testing.T) {
			m := standbyPicker(t, &pb.Session{Id: "session-1"}, armedTestReason(trigger), armedTestReason("checks_passed_ready"))
			cells := statusCellsByTitle(m)
			if got, want := cells["build"], styleStatusMuted.Render(displaystatus.StandbyLabel); got != want {
				t.Fatalf("standby STATUS = %q, want %q", stripANSI(got), stripANSI(want))
			}
			if spin := strings.TrimSpace(stripANSI(m.spinner.View())); spin != "" && strings.Contains(stripANSI(cells["build"]), spin) {
				t.Fatalf("standby STATUS must carry no spinner: %q", stripANSI(cells["build"]))
			}
			if got, want := cells["verify"], styleStatusInfo.Render(m.spinner.View()+"checks_passed_ready"); got != want {
				t.Fatalf("blocking STATUS = %q, want %q", stripANSI(got), stripANSI(want))
			}
		})
	}

	// No callback known yet: still standby, because the reason decides it.
	m := ChatPickerModel{
		chats:                []*pb.ClaudeChat{{AgentSessionId: "a", Title: "build"}},
		daemonStatuses:       map[string]string{"a": statusWaiting},
		daemonWaitingReasons: map[string]string{"a": armedTestReason("checks_failed")},
	}
	m.buildTableRows()
	if got, want := statusCellsByTitle(m)["build"], styleStatusMuted.Render(displaystatus.StandbyLabel); got != want {
		t.Fatalf("standby STATUS without a known callback = %q, want %q", stripANSI(got), stripANSI(want))
	}
}

// The waiting line describes the blocking chat even when the standby chat
// comes first in table order and holds the cursor.
func TestChatPicker_WaitingLinePrefersBlockingChat(t *testing.T) {
	blocking := armedTestReason("checks_passed_ready")
	m := standbyPicker(t, &pb.Session{Id: "session-1"}, armedTestReason("checks_failed"), blocking)
	for _, cursor := range []int{0, 1} {
		m.table.SetCursor(cursor)
		got := m.waitingReasonLine()
		if want := "verify · " + blocking + " · expires in 6d"; got != want {
			t.Fatalf("cursor %d: waitingReasonLine = %q, want %q", cursor, got, want)
		}
	}
}

// With only standby waits, the line still describes one — prefixed "standby: ",
// never "waiting: ", even when Ready supersedes the waiting badge.
func TestChatPicker_StandbyOnlyWaitingLine(t *testing.T) {
	standby := armedTestReason("checks_failed")
	for _, sess := range []*pb.Session{
		{Id: "session-1"},
		{Id: "session-1", DisplayLabel: displaystatus.ReadyLabel},
		{Id: "session-1", IsWaitingDemoted: true},
	} {
		m := standbyPicker(t, sess, standby, "")
		want := "build · standby: " + standby + " · expires in 6d"
		if got := m.waitingReasonLine(); got != want {
			t.Fatalf("session %+v: waitingReasonLine = %q, want %q", sess, got, want)
		}
		if h := m.waitingLineHeight(); h != 2 {
			t.Fatalf("waitingLineHeight = %d, want 2", h)
		}
	}
}

// Of two standby chats, the cursor one is described, else the first.
func TestChatPicker_StandbyLineFollowsCursorAmongStandby(t *testing.T) {
	m := ChatPickerModel{
		chats:                []*pb.ClaudeChat{{AgentSessionId: "a", Title: "one"}, {AgentSessionId: "b", Title: "two"}},
		daemonStatuses:       map[string]string{"a": statusWaiting, "b": statusWaiting},
		daemonWaitingReasons: map[string]string{"a": armedTestReason("checks_failed"), "b": armedTestReason("closed")},
	}
	m.buildTableRows()
	m.table.SetCursor(1)
	if got, want := m.waitingReasonLine(), "two · standby: "+armedTestReason("closed"); got != want {
		t.Fatalf("waitingReasonLine = %q, want %q", got, want)
	}
}

// Home never presents a standby watch as the session's waiting hint, demoted
// or not, and reserves no sub-row for it.
func TestWaitingHintLine_HidesStandby(t *testing.T) {
	for _, trigger := range []string{"checks_failed", "closed"} {
		reason := displaystatus.CallbackWaitingReason(trigger, "acme", "widget", 123)
		for _, demoted := range []bool{false, true} {
			if got := waitingHintLine(reason, demoted); got != "" {
				t.Fatalf("waitingHintLine(%q, %v) = %q, want empty", reason, demoted, got)
			}
		}
		if got := sessionSubRowCount(&pb.Session{Id: "s1"}, reason); got != 0 {
			t.Fatalf("sessionSubRowCount(%q) = %d, want 0", reason, got)
		}
		if got := sessionSubRowCount(&pb.Session{Id: "s1", IsWaitingDemoted: true}, reason); got != 0 {
			t.Fatalf("demoted sessionSubRowCount(%q) = %d, want 0", reason, got)
		}
	}
}

// The build-handoff-standby fixture through the real picker and Home: the
// proof's screens in test form.
func TestChatPicker_BuildHandoffStandbyFixture(t *testing.T) {
	w := fixtures.BuildHandoffStandbyWorld()
	sess := w.Sessions[0]
	stub := &chatPickerStub{callbacks: w.GithubCallbacks}
	statuses := map[string]string{}
	reasons := map[string]string{}
	for _, e := range w.ChatStatuses {
		statuses[e.GetAgentSessionId()] = chatStatusString(e.GetStatus())
		if r := e.GetWaitingReason(); r != "" {
			reasons[e.GetAgentSessionId()] = r
		}
	}
	m := armedPicker(t, stub, sess, w.Chats, statuses, reasons)
	cells := statusCellsByTitle(m)
	if got := stripANSI(cells[fixtures.BuildHandoffBuildChatTitle]); got != displaystatus.StandbyLabel {
		t.Fatalf("build STATUS = %q, want %q", got, displaystatus.StandbyLabel)
	}
	if got := stripANSI(cells["verify"]); !strings.HasSuffix(got, "checks_passed_ready") {
		t.Fatalf("verify STATUS = %q, want the blocking trigger", got)
	}
	if line := m.waitingReasonLine(); !strings.HasPrefix(line, "verify · "+fixtures.BuildHandoffVerifyReason) {
		t.Fatalf("waitingReasonLine = %q, want it to describe verify", line)
	}

	h := NewHomeModel(nil, context.Background(), nil)
	updated, _ := h.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	h = updated.(HomeModel)
	for _, sessionReason := range []string{fixtures.BuildHandoffVerifyReason, fixtures.BuildHandoffStandbyReason} {
		updated, _ = h.Update(sessionListMsg{
			sessions:             w.Sessions,
			daemonStatuses:       map[string]string{sess.GetId(): statusWaiting},
			daemonWaitingReasons: map[string]string{sess.GetId(): sessionReason},
		})
		h = updated.(HomeModel)
		home := stripANSI(h.View().Content)
		if strings.Contains(home, "awaiting checks_failed") {
			t.Fatalf("Home shows the standby wait as the session hint:\n%s", home)
		}
	}
}
