package chatpicker

import (
	"strings"
	"testing"

	"github.com/recurser/boss/api/tui/ui"

	"github.com/recurser/boss/internal/fixtures"
	"github.com/recurser/bossalib/displaystatus"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// BOS-1531: after a build hands off to verify, the build chat may still hold a
// standby (contingency) watch while the verify chat blocks the PR. These tests
// pin how the TUI tells the two apart.

// standbyCallback is a live callback for chatID on acme/my-app#2960.
func standbyCallback(id, chatID, trigger string) *pb.GitHubCallback {
	cb := armedTestCallback(id, trigger)
	cb.TargetChatId = chatID
	return cb
}

// standbyPicker lists a build chat (agent-aaa, first in table order, holding
// buildReason) and a verify chat (agent-zzz, holding verifyReason; "" leaves it
// working) into a picker whose callbacks are known.
func standbyPicker(t *testing.T, sess *pb.Session, buildReason, verifyReason string) Model {
	t.Helper()
	stub := &chatPickerStub{callbacks: []*pb.GitHubCallback{
		standbyCallback("cb-build", "agent-aaa", displaystatus.CallbackWaitingTrigger(buildReason)),
		standbyCallback("cb-verify", "agent-zzz", displaystatus.CallbackWaitingTrigger(verifyReason)),
	}}
	statuses := map[string]string{"agent-aaa": ui.StatusWaiting, "agent-zzz": ui.StatusWorking}
	reasons := map[string]string{"agent-aaa": buildReason}
	if verifyReason != "" {
		statuses["agent-zzz"] = ui.StatusWaiting
		reasons["agent-zzz"] = verifyReason
	}
	return armedPicker(t, stub, sess,
		[]*pb.AgentChat{verifyChat("agent-aaa", "build"), verifyChat("agent-zzz", "verify")},
		statuses, reasons)
}

func statusCellsByTitle(m Model) map[string]string {
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
//
// closed is standby only on a PR other than the session's own: on its own PR
// it is a done wait (TestChatPicker_ArmedStatusCellShowsTrigger), so its
// session here owns another PR and the blocking chat's wait names #2960.
func TestChatPicker_StandbyStatusCell(t *testing.T) {
	for _, tc := range []struct {
		trigger      string
		sessionPR    int32
		blockingWant string
	}{
		{"checks_failed", 2960, "awaiting green"},
		{"closed", 3154, "awaiting green #2960"},
	} {
		trigger := tc.trigger
		t.Run(trigger, func(t *testing.T) {
			m := standbyPicker(t, &pb.Session{Id: "session-1", PrNumber: new(tc.sessionPR)}, armedTestReason(trigger), armedTestReason("checks_passed_ready"))
			cells := statusCellsByTitle(m)
			if got, want := cells["build"], ui.StyleStatusMuted.Render(displaystatus.StandbyLabel); got != want {
				t.Fatalf("standby STATUS = %q, want %q", stripANSI(got), stripANSI(want))
			}
			if spin := strings.TrimSpace(stripANSI(m.spinner.View())); spin != "" && strings.Contains(stripANSI(cells["build"]), spin) {
				t.Fatalf("standby STATUS must carry no spinner: %q", stripANSI(cells["build"]))
			}
			if got, want := cells["verify"], ui.StyleStatusInfo.Render(m.spinner.View()+tc.blockingWant); got != want {
				t.Fatalf("blocking STATUS = %q, want %q", stripANSI(got), stripANSI(want))
			}
		})
	}

	// No callback known yet: still standby, because the reason decides it.
	m := Model{
		chats:                []*pb.AgentChat{{AgentSessionId: "a", Title: "build"}},
		daemonStatuses:       map[string]string{"a": ui.StatusWaiting},
		daemonWaitingReasons: map[string]string{"a": armedTestReason("checks_failed")},
	}
	m.buildTableRows()
	if got, want := statusCellsByTitle(m)["build"], ui.StyleStatusMuted.Render(displaystatus.StandbyLabel); got != want {
		t.Fatalf("standby STATUS without a known callback = %q, want %q", stripANSI(got), stripANSI(want))
	}
}

// A standby chat's armed line is prefixed "standby: ", never "waiting: ", even
// when Ready supersedes the waiting badge.
func TestChatPicker_StandbyOnlyArmedLine(t *testing.T) {
	standby := armedTestReason("checks_failed")
	for _, sess := range []*pb.Session{
		{Id: "session-1"},
		{Id: "session-1", DisplayLabel: displaystatus.ReadyLabel},
		{Id: "session-1", IsWaitingDemoted: true},
	} {
		m := standbyPicker(t, sess, standby, "")
		want := "build · standby: " + standby + " · expires in 6d"
		if got := firstArmedLine(m); got != want {
			t.Fatalf("session %+v: armedLine = %q, want %q", sess, got, want)
		}
	}
}

// Home never presents a standby watch as the session's waiting hint, demoted
// or not, and reserves no sub-row for it.
func TestWaitingHintLine_HidesStandby(t *testing.T) {
	for _, trigger := range []string{"checks_failed", "closed"} {
		reason := displaystatus.CallbackWaitingReason(trigger, "acme", "widget", 123)
		for _, demoted := range []bool{false, true} {
			if got := ui.WaitingHintLine(reason, demoted); got != "" {
				t.Fatalf("waitingHintLine(%q, %v) = %q, want empty", reason, demoted, got)
			}
		}
		if got := ui.SessionSubRowCount(&pb.Session{Id: "s1"}, reason, true); got != 0 {
			t.Fatalf("sessionSubRowCount(%q, true) = %d, want 0", reason, got)
		}
		if got := ui.SessionSubRowCount(&pb.Session{Id: "s1", IsWaitingDemoted: true}, reason, true); got != 0 {
			t.Fatalf("demoted sessionSubRowCount(%q, true) = %d, want 0", reason, got)
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
		statuses[e.GetAgentSessionId()] = ui.ChatStatusString(e.GetStatus())
		if r := e.GetWaitingReason(); r != "" {
			reasons[e.GetAgentSessionId()] = r
		}
	}
	m := armedPicker(t, stub, sess, w.Chats, statuses, reasons)
	cells := statusCellsByTitle(m)
	if got := stripANSI(cells[fixtures.BuildHandoffBuildChatTitle]); got != displaystatus.StandbyLabel {
		t.Fatalf("build STATUS = %q, want %q", got, displaystatus.StandbyLabel)
	}
	if got := stripANSI(cells["verify"]); !strings.HasSuffix(got, "awaiting green") {
		t.Fatalf("verify STATUS = %q, want the blocking trigger phrased as a wait", got)
	}
	if line := firstArmedLine(m); !strings.HasPrefix(line, "verify · "+fixtures.BuildHandoffVerifyReason) {
		t.Fatalf("armedLine = %q, want it to describe verify", line)
	}

}
