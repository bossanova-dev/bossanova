package views

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// seedChatPickerParked returns a picker whose session carries the verify-park
// attention on PR #42, the state [m]approve exists for.
func seedChatPickerParked(stub *chatPickerStub) ChatPickerModel {
	prNum := int32(42)
	m := seedChatPicker(stub, statusWorking)
	m.session = &pb.Session{
		Id:              "session-1",
		PrNumber:        &prNum,
		DisplayStatus:   pb.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN,
		AttentionStatus: &pb.AttentionStatus{Reason: pb.AttentionReason_ATTENTION_REASON_AWAITING_HUMAN_INPUT},
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

func TestChatPicker_ParkedPROffersApproveOnTheMergeKey(t *testing.T) {
	m := seedChatPickerParked(&chatPickerStub{})
	if !m.canApprove() || m.canMerge() {
		t.Fatalf("canApprove=%v canMerge=%v, want approve only on a parked PR", m.canApprove(), m.canMerge())
	}
	rendered := m.View().Content
	if !strings.Contains(rendered, "[m]approve") || strings.Contains(rendered, "[m]erge") {
		t.Fatalf("action bar on a parked PR should offer [m]approve, not [m]erge:\n%s", rendered)
	}
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
	if got := m.View().Content; !strings.Contains(got, "Approve PR #42? Verify waives its needs-human park on the current head, then re-checks and merges it.") ||
		!strings.Contains(got, "[y/enter] confirm  [n/esc] cancel") {
		t.Fatalf("approve confirm not rendered:\n%s", got)
	}
	updated, cmd := m.Update(keyPress('n'))
	m = updated.(ChatPickerModel)
	if cmd != nil || m.approving || m.confirm != confirmNone || len(stub.approveCalls) != 0 {
		t.Fatalf("declining approved anyway: cmd=%v approving=%v calls=%v", cmd != nil, m.approving, stub.approveCalls)
	}
}

func TestChatPicker_ApproveInFlightShowsSpinnerAndSwallowsKeys(t *testing.T) {
	m := seedChatPickerParked(&chatPickerStub{})
	updated, _ := m.Update(keyPress('m'))
	updated, _ = updated.(ChatPickerModel).Update(keyPress('y'))
	m = updated.(ChatPickerModel)
	if got := m.View().Content; !strings.Contains(got, "Approving PR #42…") {
		t.Fatalf("in-flight approval not shown:\n%s", got)
	}
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
	const want = "Approved #42: waived ledger-open on head abcdef01 (record: https://x/9). Verify will re-check and merge."
	if m.statusNote != want {
		t.Fatalf("statusNote = %q, want %q", m.statusNote, want)
	}
	if m.canApprove() || strings.Contains(m.View().Content, "[m]approve") {
		t.Fatal("[m]approve still offered after the approval succeeded")
	}
}

func TestChatPicker_ApproveRefusalAndErrorLandAsTheStatusLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		stub *chatPickerStub
		want string
	}{
		{"a refusal names the reason", &chatPickerStub{approveResult: &pb.VerifyApproveResult{Reason: "no-standing-park"}},
			"PR #42 not approved: no `needs human:` park stands on the PR's current head [no-standing-park]"},
		{"a daemon error says it could not approve", &chatPickerStub{approveErr: connect.NewError(connect.CodeFailedPrecondition, errors.New("boss-verify toolbox not found"))},
			"Couldn't approve: "},
		{"a timeout says the record may already be posted", &chatPickerStub{approveErr: connect.NewError(connect.CodeDeadlineExceeded, errors.New("verify-gate timed out"))},
			"an approval record may already be posted, so approve again to confirm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := approveThroughConfirm(t, seedChatPickerParked(tc.stub))
			if !strings.Contains(m.statusMsg, tc.want) || m.statusNote != "" {
				t.Fatalf("statusMsg = %q statusNote = %q, want %q", m.statusMsg, m.statusNote, tc.want)
			}
			if !m.canApprove() {
				t.Fatal("a failed approval must leave [m]approve available to retry")
			}
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
