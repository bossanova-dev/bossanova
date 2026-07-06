package views

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// drivePickCAccount presses "c" on the seeded chat and resolves the async
// account-load command so the returned model is showing the account picker.
func drivePickCAccount(t *testing.T, m ChatPickerModel) ChatPickerModel {
	t.Helper()
	updated, cmd := m.Update(keyPress('c'))
	m = updated.(ChatPickerModel)
	if cmd == nil {
		t.Fatal("pressing 'c' on a selected chat must return an account-load cmd")
	}
	updated, _ = m.Update(cmd()) // switchAccountsLoadedMsg
	m = updated.(ChatPickerModel)
	if !m.pickingAccount {
		t.Fatal("expected pickingAccount=true after the account-load resolves")
	}
	return m
}

// TestChatPicker_SwitchAccount_WorkingChat_ConfirmForcesSwitch drives the full
// switch flow for a mid-turn (WORKING) chat: [c] opens the account picker, a
// non-default account is chosen, the confirm dialog appears, and confirming
// dispatches SwitchSessionAccount with Force=true. Asserts the account picker,
// the confirm dialog, and the resumed notice all render in View().
func TestChatPicker_SwitchAccount_WorkingChat_ConfirmForcesSwitch(t *testing.T) {
	stub := &chatPickerStub{
		accounts: []*pb.Account{
			{Id: "acct-1", Label: "Work", Provider: "claude", Status: "active"},
		},
		switchResp: &pb.SwitchSessionAccountResponse{
			Resumed:     true,
			TargetLabel: "Work",
			NoticeText:  "switched to Work — resumed",
		},
	}
	m := seedChatPicker(stub, statusWorking)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = updated.(ChatPickerModel)

	m = drivePickCAccount(t, m)

	// The account picker must render the System default row and the account.
	picker := stripANSI(m.View().Content)
	if !strings.Contains(picker, "System default") {
		t.Errorf("account picker missing System default row:\n%s", picker)
	}
	if !strings.Contains(picker, "Work") {
		t.Errorf("account picker missing the 'Work' account:\n%s", picker)
	}

	// Move the cursor to the account row (row 1) and confirm the selection.
	updated, _ = m.Update(keyPress('j'))
	m = updated.(ChatPickerModel)
	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(ChatPickerModel)
	if cmd != nil {
		t.Fatalf("selecting an account on a WORKING chat must not fire the RPC yet, got cmd %T", cmd)
	}
	if m.confirm != confirmSwitch {
		t.Fatalf("expected confirmSwitch dialog for a mid-turn chat, got confirm=%d", m.confirm)
	}

	// The confirm dialog must render.
	dialog := stripANSI(m.View().Content)
	if !strings.Contains(dialog, "mid-turn") {
		t.Errorf("confirm dialog missing mid-turn wording:\n%s", dialog)
	}
	if !strings.Contains(dialog, "[y/enter] confirm") {
		t.Errorf("confirm dialog missing confirm action bar:\n%s", dialog)
	}

	// Confirm — this must fire the RPC with Force=true.
	updated, cmd = m.Update(keyPress('y'))
	m = updated.(ChatPickerModel)
	if cmd == nil {
		t.Fatal("confirming the switch must return the SwitchSessionAccount cmd")
	}
	if !m.switching {
		t.Fatal("expected switching=true while the RPC is in flight")
	}
	result := cmd() // executes SwitchSessionAccount, returns switchAccountResultMsg

	calls := stub.switchCallsSnapshot()
	if len(calls) != 1 {
		t.Fatalf("SwitchSessionAccount called %d times, want 1", len(calls))
	}
	got := calls[0]
	if !got.GetForce() {
		t.Error("expected Force=true after confirming a mid-turn switch")
	}
	if got.GetAccountId() != "acct-1" {
		t.Errorf("AccountId = %q, want acct-1", got.GetAccountId())
	}
	if got.GetAgentSessionId() != "agent-1" {
		t.Errorf("AgentSessionId = %q, want agent-1", got.GetAgentSessionId())
	}
	if got.GetSessionId() != "session-1" {
		t.Errorf("SessionId = %q, want session-1", got.GetSessionId())
	}

	// Feed the result; the resumed notice must render as a passive system line.
	updated, _ = m.Update(result)
	m = updated.(ChatPickerModel)
	if m.switching {
		t.Error("switching must be cleared once the result arrives")
	}
	rendered := stripANSI(m.View().Content)
	if !strings.Contains(rendered, "switched to Work — resumed") {
		t.Errorf("view missing resumed notice line:\n%s", rendered)
	}
}

// TestChatPicker_SwitchAccount_StoppedChat_SystemDefault_NoConfirm drives the
// switch flow for a non-mid-turn chat selecting the System default row: no
// confirm dialog, the RPC fires immediately with Force=false and an empty
// AccountId, and the "started fresh" notice renders.
func TestChatPicker_SwitchAccount_StoppedChat_SystemDefault_NoConfirm(t *testing.T) {
	stub := &chatPickerStub{
		accounts: []*pb.Account{
			{Id: "acct-1", Label: "Work", Provider: "claude", Status: "active"},
		},
		switchResp: &pb.SwitchSessionAccountResponse{
			Resumed:     false,
			TargetLabel: "System default",
			NoticeText:  "switched to System default — started fresh (no transcript)",
		},
	}
	m := seedChatPicker(stub, statusStopped)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = updated.(ChatPickerModel)

	m = drivePickCAccount(t, m)

	// Cursor defaults to row 0 (System default). Confirm immediately.
	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(ChatPickerModel)
	if m.confirm != confirmNone {
		t.Fatalf("a non-mid-turn chat must not show a confirm dialog, got confirm=%d", m.confirm)
	}
	if cmd == nil {
		t.Fatal("selecting an account on a stopped chat must fire the RPC directly")
	}
	if !m.switching {
		t.Fatal("expected switching=true while the RPC is in flight")
	}
	result := cmd()

	calls := stub.switchCallsSnapshot()
	if len(calls) != 1 {
		t.Fatalf("SwitchSessionAccount called %d times, want 1", len(calls))
	}
	got := calls[0]
	if got.GetForce() {
		t.Error("expected Force=false for a non-mid-turn switch (no confirm)")
	}
	if got.GetAccountId() != "" {
		t.Errorf("AccountId = %q, want empty (System default)", got.GetAccountId())
	}

	updated, _ = m.Update(result)
	m = updated.(ChatPickerModel)
	rendered := stripANSI(m.View().Content)
	if !strings.Contains(rendered, "switched to System default — started fresh") {
		t.Errorf("view missing started-fresh notice line:\n%s", rendered)
	}
}

// TestChatPicker_SwitchAccount_ErrorSurfaced verifies a daemon error is
// rendered on the same notice line as a human-readable message.
func TestChatPicker_SwitchAccount_ErrorSurfaced(t *testing.T) {
	stub := &chatPickerStub{
		accounts:  []*pb.Account{{Id: "acct-1", Label: "Work", Provider: "claude", Status: "cooling"}},
		switchErr: errors.New("account is cooling down; try again later"),
	}
	m := seedChatPicker(stub, statusStopped)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = updated.(ChatPickerModel)

	m = drivePickCAccount(t, m)

	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(ChatPickerModel)
	if cmd == nil {
		t.Fatal("expected the switch RPC cmd")
	}
	updated, _ = m.Update(cmd())
	m = updated.(ChatPickerModel)

	rendered := stripANSI(m.View().Content)
	if !strings.Contains(rendered, "account is cooling down") {
		t.Errorf("view missing the daemon error notice:\n%s", rendered)
	}
}

// TestChatPicker_SwitchAccount_ActionBarEntry verifies the action bar advertises
// the swit[c]h account action when a chat is selected.
func TestChatPicker_SwitchAccount_ActionBarEntry(t *testing.T) {
	stub := &chatPickerStub{}
	m := seedChatPicker(stub, statusWorking)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = updated.(ChatPickerModel)

	rendered := stripANSI(m.View().Content)
	if !strings.Contains(rendered, "swit[c]h account") {
		t.Errorf("action bar missing swit[c]h account entry:\n%s", rendered)
	}
}

// TestChatPicker_SwitchAccount_EscCancelsPicker verifies Esc in the account
// picker returns to the chat list without switching.
func TestChatPicker_SwitchAccount_EscCancelsPicker(t *testing.T) {
	stub := &chatPickerStub{
		accounts: []*pb.Account{{Id: "acct-1", Label: "Work", Provider: "claude", Status: "active"}},
	}
	m := seedChatPicker(stub, statusWorking)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = updated.(ChatPickerModel)

	m = drivePickCAccount(t, m)

	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = updated.(ChatPickerModel)
	if m.pickingAccount {
		t.Error("esc must dismiss the account picker")
	}
	if m.cancel {
		t.Error("esc inside the account picker must not cancel the whole chat picker")
	}
	if cmd != nil {
		t.Errorf("esc should not fire a cmd, got %T", cmd)
	}
	if len(stub.switchCallsSnapshot()) != 0 {
		t.Error("esc must not dispatch SwitchSessionAccount")
	}
}
