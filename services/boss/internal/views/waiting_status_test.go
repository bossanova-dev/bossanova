package views

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// An actionable waiting reason that remains visible beneath the status badge.
// ready_for_review is neither routine (#2901) nor standby (BOS-1531), so Home
// keeps it.
const testWaitingReason = "awaiting ready_for_review on acme/widget#123"

// Home hides routine PR waits (#2901); the chat picker shows them, because in
// the chat view that wait is all the parked chat is doing (BOS-1517).
func TestRoutinePRWaitingHintsHidden(t *testing.T) {
	for _, trigger := range []string{"checks_passed", "checks_passed_ready", "merged"} {
		t.Run(trigger, func(t *testing.T) {
			reason := displaystatus.CallbackWaitingReason(trigger, "acme", "widget", 123)
			if reason == "" {
				t.Fatal("callback must provide a waiting reason")
			}
			for _, demoted := range []bool{false, true} {
				if got := waitingHintLine(reason, demoted); got != "" {
					t.Fatalf("routine hint = %q, want empty", got)
				}
			}
			h := NewHomeModel(nil, context.Background(), nil)
			updated, _ := h.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
			h = updated.(HomeModel)
			sess := &pb.Session{Id: "s1", Title: "parked session", DisplayLabel: displaystatus.WaitingLabel, DisplaySpinner: true, DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_INFO}
			updated, _ = h.Update(sessionListMsg{sessions: []*pb.Session{sess}, daemonStatuses: map[string]string{"s1": statusWaiting}, daemonWaitingReasons: map[string]string{"s1": reason}})
			h = updated.(HomeModel)
			if got := sessionSubRowCount(sess, reason, 60); got != 0 {
				t.Fatalf("routine wait reserves %d sub-rows", got)
			}
			rendered := stripANSI(h.View().Content)
			if strings.Contains(rendered, reason) || !strings.Contains(rendered, stripANSI(h.spinner.View())+displaystatus.WaitingLabel) {
				t.Fatalf("expected waiting spinner without hint: %s", rendered)
			}
			m := ChatPickerModel{session: sess, chats: []*pb.ClaudeChat{{AgentSessionId: "a", Title: "verify"}}, daemonStatuses: map[string]string{"a": statusWaiting}, daemonWaitingReasons: map[string]string{"a": reason}}
			if got, want := m.waitingReasonLine(), "verify · "+reason; got != want {
				t.Fatalf("chat picker routine wait = %q, want %q", got, want)
			}
			if got := m.waitingLineHeight(); got != 2 {
				t.Fatalf("chat picker routine wait reserves %d rows, want 2", got)
			}
		})
	}
}

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

// Other waiting reasons retain their daemon-supplied wording.
func TestWaitingHintLine(t *testing.T) {
	got := waitingHintLine(testWaitingReason, false)
	want := testWaitingReason
	if got != want {
		t.Fatalf("waitingHintLine = %q, want %q", got, want)
	}
	if !strings.Contains(got, "ready_for_review") || !strings.Contains(got, "#123") {
		t.Fatalf("waiting hint must carry the trigger and PR number: %q", got)
	}
	if got := waitingHintLine("", false); got != "" {
		t.Fatalf("waitingHintLine(\"\") = %q, want empty so callers skip the row", got)
	}
	if got := waitingHintLine("", true); got != "" {
		t.Fatalf("waitingHintLine(\"\", demoted) = %q, want empty — the prefix must not fabricate a line", got)
	}
}

// Visible hints regain their waiting prefix when the badge is superseded.
func TestWaitingHintLine_DemotedRowRegainsItsAntecedent(t *testing.T) {
	got := waitingHintLine(testWaitingReason, true)
	want := "waiting: " + testWaitingReason
	if got != want {
		t.Fatalf("waitingHintLine(demoted) = %q, want %q", got, want)
	}
	if !strings.Contains(got, "ready_for_review") || !strings.Contains(got, "#123") {
		t.Fatalf("the demoted hint must still carry the trigger and PR number: %q", got)
	}
}

// TestHintDemoted_DemotedRowRendersTheAttentionHintBright asserts KTD-7 as a
// DECISION rather than leaving it as an emergent consequence. hintDemoted fades
// a demotable hint when the row's own composite is a live-activity label;
// "✓ passing" is not one, so on a demoted row the attention hint goes from faded
// to bright. That is the documented fail-loud direction for this predicate, and
// the row genuinely is no longer live, so it is accepted rather than repaired.
// If it ever reads wrong in use, the remedy is the recorded follow-up (teaching
// IsLiveActivityLabel about a demoted row), not a quiet widening here.
func TestHintDemoted_DemotedRowRendersTheAttentionHintBright(t *testing.T) {
	demotable := sessionHint{Text: "stalled", Demotable: true}

	waiting := &pb.Session{
		State:        pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
		DisplayLabel: displaystatus.WaitingLabel,
	}
	if !hintDemoted(waiting, demotable) {
		t.Fatal("hintDemoted(waiting row) = false, want true — waiting IS a live-activity label")
	}

	demoted := &pb.Session{
		State:            pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
		DisplayStatus:    pb.DisplayStatus_DISPLAY_STATUS_PASSING,
		DisplayLabel:     "✓ passing",
		IsWaitingDemoted: true,
	}
	if hintDemoted(demoted, demotable) {
		t.Fatal("hintDemoted(demoted row) = true, want false — the hint renders bright (KTD-7)")
	}

	// The repair hint is NOT part of the flip: it is already suppressed for a
	// passing PR at the source, so it never reaches the style gate here.
	//
	// Asserting that on the bare `demoted` session above would prove nothing —
	// repairFailureHint returns early on LastRepairAttemptCount == 0, so the
	// empty result would come from "this session never had a repair attempt",
	// not from repairFailureResolved. Give it a real repair-failure history so
	// the count and error guards both pass and repairFailureResolved is the
	// only thing left that can empty the hint.
	withFailures := func(status pb.DisplayStatus) *pb.Session {
		return &pb.Session{
			State:                  pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
			DisplayStatus:          status,
			DisplayLabel:           "✓ passing",
			IsWaitingDemoted:       true,
			LastRepairAttemptCount: 3,
			LastRepairRunnerError:  "runner exited 1",
			LastRepairStartedAt:    timestamppb.New(time.Now().Add(-time.Hour)),
		}
	}
	if got := repairHint(withFailures(pb.DisplayStatus_DISPLAY_STATUS_PASSING)); got != "" {
		t.Fatalf("repairHint on a passing PR = %q, want empty — it is suppressed at the source, not by the fade", got)
	}
	// The negative control: identical session, a status repairFailureResolved
	// does NOT resolve. A non-empty hint here is what proves the emptiness above
	// came from repairFailureResolved rather than from an earlier guard.
	if got := repairHint(withFailures(pb.DisplayStatus_DISPLAY_STATUS_FAILING)); got == "" {
		t.Fatal("repairHint on a FAILING PR is empty — the passing-PR assertion above is vacuous")
	}
}

// TestSessionSubRowCount_WaitingReason confirms the waiting reason gets its own
// auxiliary row in the home list, and that an absent reason costs nothing — the
// count is the single source of truth for cursor arithmetic (BOS-474).
func TestSessionSubRowCount_WaitingReason(t *testing.T) {
	sess := &pb.Session{Id: "s1"}
	if got := sessionSubRowCount(sess, "", 60); got != 0 {
		t.Fatalf("sessionSubRowCount with no reason = %d, want 0", got)
	}
	if got := sessionSubRowCount(sess, testWaitingReason, 60); got != 1 {
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
	if !strings.Contains(rendered, "ready_for_review") {
		t.Fatalf("home list missing the waiting trigger in:\n%s", rendered)
	}
	if !strings.Contains(rendered, "#123") {
		t.Fatalf("home list missing the awaited PR number in:\n%s", rendered)
	}
}

// TestChatPicker_WaitingReasonLine unit-tests the session-detail hint: only a
// waiting chat contributes, and the line carries the chat title (BOS-1517) and
// the daemon-supplied reason verbatim — no "waiting" label prefix (BOS-863).
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
	want := "New chat · " + testWaitingReason
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
	for _, token := range []string{displaystatus.WaitingLabel, "working", "ready_for_review", "#123"} {
		if !strings.Contains(rendered, token) {
			t.Fatalf("session detail missing %q in:\n%s", token, rendered)
		}
	}
}

func TestReadyWaitingHints(t *testing.T) {
	sess := &pb.Session{DisplayLabel: displaystatus.ReadyLabel, DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_SUCCESS}
	if got := renderDisplayStatus(sess, newStatusSpinner()); got != styleStatusSuccess.Render(displaystatus.ReadyLabel) {
		t.Fatalf("Ready style = %q", got)
	}
	if !waitingBadgeSuperseded(sess) {
		t.Fatal("Ready must restore the waiting antecedent")
	}
	m := ChatPickerModel{session: sess, chats: []*pb.ClaudeChat{{AgentSessionId: "a"}}, daemonStatuses: map[string]string{"a": statusWaiting}, daemonWaitingReasons: map[string]string{"a": testWaitingReason}}
	if got := m.waitingReasonLine(); got != "New chat · waiting: "+testWaitingReason {
		t.Fatalf("Ready chat picker hint = %q", got)
	}
	h := NewHomeModel(nil, context.Background(), nil)
	updated, _ := h.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	h = updated.(HomeModel)
	sess.Id, sess.Title = "s1", "Hand off the importer"
	updated, _ = h.Update(sessionListMsg{sessions: []*pb.Session{sess}, daemonStatuses: map[string]string{"s1": statusWaiting}, daemonWaitingReasons: map[string]string{"s1": testWaitingReason}})
	h = updated.(HomeModel)
	rendered := stripANSI(h.View().Content)
	for _, token := range []string{displaystatus.ReadyLabel, "waiting: " + testWaitingReason} {
		if !strings.Contains(rendered, token) {
			t.Fatalf("missing %q in %s", token, rendered)
		}
	}
	if waitingBadgeSuperseded(nil) || waitingBadgeSuperseded(&pb.Session{DisplayLabel: displaystatus.WaitingLabel}) {
		t.Fatal("ordinary waiting rows retain their bare reason")
	}
}
