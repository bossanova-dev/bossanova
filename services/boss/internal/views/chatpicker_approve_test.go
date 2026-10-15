package views

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// seedChatPickerParked returns a picker whose session carries the verify-park
// attention on PR #42, the state [m]erge approves the park for. The park only
// asks for a UI sign-off.
func seedChatPickerParked(stub *chatPickerStub) ChatPickerModel {
	return seedChatPickerParkedWith(stub, "needs human: human-review:ui")
}

// seedChatPickerParkedWith is seedChatPickerParked with the park's attention
// summary, in the daemon's "needs human: <codes>" shape.
func seedChatPickerParkedWith(stub *chatPickerStub, summary string) ChatPickerModel {
	prNum := int32(42)
	m := seedChatPicker(stub, statusWorking)
	m.session = &pb.Session{
		Id:              "session-1",
		PrNumber:        &prNum,
		DisplayStatus:   pb.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN,
		AttentionStatus: &pb.AttentionStatus{Reason: pb.AttentionReason_ATTENTION_REASON_AWAITING_HUMAN_INPUT, Summary: summary},
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 30})
	return updated.(ChatPickerModel)
}

// approveThroughConfirm presses m then y and runs the approve command,
// returning the model with the result landed.
func approveThroughConfirm(t *testing.T, m ChatPickerModel) ChatPickerModel {
	t.Helper()
	updated, _ := m.Update(keyPress('m'))
	m = updated.(ChatPickerModel)
	if m.confirm != confirmApprove {
		t.Fatalf("m on a parked PR raised confirm %v, want confirmApprove", m.confirm)
	}
	updated, cmd := m.Update(keyPress('y'))
	m = updated.(ChatPickerModel)
	if !m.approving || cmd == nil {
		t.Fatalf("confirming did not start the approval (approving=%v, cmd nil=%v)", m.approving, cmd == nil)
	}
	msg := cmd()
	if _, ok := msg.(approveResultMsg); !ok {
		t.Fatalf("approve command produced %T, want approveResultMsg", msg)
	}
	updated, _ = m.Update(msg)
	return updated.(ChatPickerModel)
}

// assertNoApproveWording fails when the rendered picker names an approval: the
// parked-PR [m]erge runs one, but the TUI presents it only as a merge.
func assertNoApproveWording(t *testing.T, m ChatPickerModel) {
	t.Helper()
	if got := m.View().Content; strings.Contains(strings.ToLower(got), "approv") {
		t.Fatalf("the picker still says approve:\n%s", got)
	}
}

func TestChatPicker_ParkedPROffersMergeOnTheMergeKey(t *testing.T) {
	m := seedChatPickerParked(&chatPickerStub{})
	if !m.canApprove() || m.canMerge() {
		t.Fatalf("canApprove=%v canMerge=%v, want approve only on a parked PR", m.canApprove(), m.canMerge())
	}
	if rendered := m.View().Content; !strings.Contains(rendered, "[m]erge") {
		t.Fatalf("action bar on a parked PR should offer [m]erge:\n%s", rendered)
	}
	assertNoApproveWording(t, m)
}

func TestChatPicker_ApproveNeedsTheVerifyPark(t *testing.T) {
	passing := seedChatPickerWithPassingPR()
	if passing.canApprove() {
		t.Fatal("canApprove() = true on a passing PR; [m] must stay merge there")
	}
	orphaned := seedChatPickerParked(&chatPickerStub{})
	orphaned.session.State = pb.SessionState_SESSION_STATE_ORPHANED
	if orphaned.canApprove() {
		t.Fatal("canApprove() = true on an orphaned session; the park overlay cannot have produced it")
	}
	noPR := seedChatPickerParked(&chatPickerStub{})
	noPR.session.PrNumber = nil
	if noPR.canApprove() {
		t.Fatal("canApprove() = true without a PR")
	}
}

func TestChatPicker_ApproveConfirmNamesWhatHappensAndCancels(t *testing.T) {
	stub := &chatPickerStub{}
	m := seedChatPickerParked(stub)
	updated, _ := m.Update(keyPress('m'))
	m = updated.(ChatPickerModel)
	if got := stripANSI(m.View().Content); !strings.Contains(got, "Merge PR #42? Verify waives its park on the current head, re-checks it, then merges:") ||
		!strings.Contains(got, "human-review:ui: a UI change; watch the evidence before it merges") ||
		!strings.Contains(got, "[y/enter] confirm  [n/esc] cancel") || strings.Contains(got, "overrides") {
		t.Fatalf("approve confirm not rendered:\n%s", got)
	}
	assertNoApproveWording(t, m)
	updated, cmd := m.Update(keyPress('n'))
	m = updated.(ChatPickerModel)
	if cmd != nil || m.approving || m.confirm != confirmNone || len(stub.approveCalls) != 0 {
		t.Fatalf("declining approved anyway: cmd=%v approving=%v calls=%v", cmd != nil, m.approving, stub.approveCalls)
	}
}

// TestChatPicker_ApproveOverAFailureNamesItAndDefaultsToCancel is the failure
// half of the confirm: each code in plain words, which failure merging
// overrides, the send-back alternative, and enter cancels where y merges.
func TestChatPicker_ApproveOverAFailureNamesItAndDefaultsToCancel(t *testing.T) {
	stub := &chatPickerStub{}
	m := seedChatPickerParkedWith(stub, "needs human: human-review:ui,ledger-open")
	// More chats than the terminal holds, so the table height is bound by the
	// footer reservation rather than by the row count.
	chats := make([]*pb.ClaudeChat, 40)
	for i := range chats {
		chats[i] = &pb.ClaudeChat{SessionId: "session-1", AgentSessionId: fmt.Sprintf("agent-%d", i), Title: fmt.Sprintf("chat %d", i)}
	}
	updated, _ := m.Update(chatsListedMsg{chats: chats})
	m = updated.(ChatPickerModel)
	heightBefore := m.table.Height()
	updated, _ = m.Update(keyPress('m'))
	m = updated.(ChatPickerModel)
	got := stripANSI(m.View().Content)
	if lines := strings.Count(strings.TrimRight(got, "\n"), "\n") + 1; lines > 30 {
		t.Fatalf("the confirm pushed the view to %d lines on a 30-line terminal:\n%s", lines, got)
	}
	for _, want := range []string{
		"Merge PR #42 anyway? Verify waives its park on the current head",
		"ledger-open: review left must-fix findings open",
		"human-review:ui: a UI change; watch the evidence before it merges",
		"Merging overrides ledger-open (review left must-fix findings open).",
		`To send it back instead: boss pr send-back 42 "<note>"`,
		"[y] merge anyway  [n/enter/esc] cancel",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("failure confirm missing %q:\n%s", want, got)
		}
	}
	assertNoApproveWording(t, m)
	if m.table.Height() >= heightBefore {
		t.Fatalf("table height %d with the confirm open, want it shrunk below %d for the code lines", m.table.Height(), heightBefore)
	}

	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(ChatPickerModel)
	if cmd != nil || m.approving || m.confirm != confirmNone || len(stub.approveCalls) != 0 {
		t.Fatalf("enter merged over a failure: cmd=%v approving=%v calls=%v", cmd != nil, m.approving, stub.approveCalls)
	}
	if m.table.Height() != heightBefore {
		t.Fatalf("table height %d after the confirm closed, want %d restored", m.table.Height(), heightBefore)
	}

	updated, _ = m.Update(keyPress('m'))
	updated, cmd = updated.(ChatPickerModel).Update(keyPress('y'))
	m = updated.(ChatPickerModel)
	if !m.approving || cmd == nil {
		t.Fatalf("y did not merge over the failure (approving=%v, cmd nil=%v)", m.approving, cmd == nil)
	}
}

func TestChatPicker_ApproveInFlightShowsSpinnerAndSwallowsKeys(t *testing.T) {
	m := seedChatPickerParked(&chatPickerStub{})
	updated, _ := m.Update(keyPress('m'))
	updated, _ = updated.(ChatPickerModel).Update(keyPress('y'))
	m = updated.(ChatPickerModel)
	if got := m.View().Content; !strings.Contains(got, "Requesting merge of PR #42…") {
		t.Fatalf("in-flight approval not shown:\n%s", got)
	}
	assertNoApproveWording(t, m)
	updated, _ = m.Update(keyPress('m'))
	if updated.(ChatPickerModel).confirm != confirmNone {
		t.Fatal("m during an in-flight approval raised another confirm")
	}
}

func TestChatPicker_ApprovedLandsTheSharedVerdictAndHidesApprove(t *testing.T) {
	stub := &chatPickerStub{approveResult: &pb.VerifyApproveResult{
		IsApproved: true, Waives: "ledger-open", HeadSha: "abcdef0123456789", ApprovalUrl: "https://x/9", TrackerWrites: "ok",
	}}
	m := approveThroughConfirm(t, seedChatPickerParked(stub))
	if len(stub.approveCalls) != 1 || stub.approveCalls[0] != "session-1" {
		t.Fatalf("approveCalls = %v, want [session-1]", stub.approveCalls)
	}
	if m.approving || m.statusMsg != "" {
		t.Fatalf("approving=%v statusMsg=%q after a successful approval", m.approving, m.statusMsg)
	}
	const want = "Merge requested for #42: verify waived ledger-open on head abcdef01 and will re-check and merge (record: https://x/9)."
	if m.statusNote != want {
		t.Fatalf("statusNote = %q, want %q", m.statusNote, want)
	}
	if m.canApprove() || strings.Contains(m.View().Content, "[m]erge") {
		t.Fatal("[m]erge still offered after the merge request succeeded")
	}
	assertNoApproveWording(t, m)
}

func TestChatPicker_ApproveRefusalAndErrorLandAsTheStatusLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		stub *chatPickerStub
		want string
	}{
		{"a refusal names the reason", &chatPickerStub{approveResult: &pb.VerifyApproveResult{Reason: "no-standing-park"}},
			"PR #42 not merged: no `needs human:` park stands on the PR's current head [no-standing-park]"},
		{"a daemon error says it could not request the merge", &chatPickerStub{approveErr: connect.NewError(connect.CodeFailedPrecondition, errors.New("boss-verify toolbox not found"))},
			"Couldn't request the merge: "},
		{"a timeout says the record may already be posted", &chatPickerStub{approveErr: connect.NewError(connect.CodeDeadlineExceeded, errors.New("verify-gate timed out"))},
			"verify may already have recorded it, so merge again to confirm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := approveThroughConfirm(t, seedChatPickerParked(tc.stub))
			if !strings.Contains(m.statusMsg, tc.want) || m.statusNote != "" {
				t.Fatalf("statusMsg = %q statusNote = %q, want %q", m.statusMsg, m.statusNote, tc.want)
			}
			if !m.canApprove() {
				t.Fatal("a failed merge request must leave [m]erge available to retry")
			}
			assertNoApproveWording(t, m)
		})
	}
}

func TestChatPicker_ApproveResultForAnotherSessionIsIgnored(t *testing.T) {
	m := seedChatPickerParked(&chatPickerStub{})
	m.approving = true
	updated, _ := m.Update(approveResultMsg{sessionID: "other", pr: 42, result: &pb.VerifyApproveResult{IsApproved: true}})
	if got := updated.(ChatPickerModel); !got.approving || got.approved {
		t.Fatal("an orphan approval result changed this session's state")
	}
}
