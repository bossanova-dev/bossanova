package views

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// seedAccountsListWithSessions returns a loaded AccountsListModel that has also
// received the one-shot session snapshot (as Init batches it), so bound-session
// counts are computable in the disable/remove prompts (BOS-268).
func seedAccountsListWithSessions(t *testing.T, stub *accountsStub) AccountsListModel {
	t.Helper()
	m := seedAccountsList(t, stub)
	updated, _ := m.Update(sessionsLoadedMsg{sessions: stub.sessions, err: stub.sessionsErr})
	return updated.(AccountsListModel)
}

// flattenPrompt strips ANSI styling and collapses all whitespace (including the
// newlines lipgloss inserts when the confirm footer width-wraps long copy) so
// substring assertions on rendered prompt content are robust to wrapping.
func flattenPrompt(s string) string { return strings.Join(strings.Fields(stripANSI(s)), " ") }

// boundSession builds a *pb.Session bound to accountID (AccountId is an
// optional/pointer proto field).
func boundSession(id, accountID string) *pb.Session {
	a := accountID
	return &pb.Session{Id: id, AccountId: &a}
}

// cursorDown moves the highlight down one row (fixture[1] = the disabled codex
// account) by forwarding a 'j' key, which the list forwards to the table.
func cursorDown(t *testing.T, m AccountsListModel) AccountsListModel {
	t.Helper()
	updated, _ := m.Update(keyPress('j'))
	return updated.(AccountsListModel)
}

// TestAccountActions_DisableUnbound_ImmediateUpdate: `x` on an active, known-
// unbound account disables it immediately (no confirm) via a single
// UpdateAccount{Status:"disabled"}.
func TestAccountActions_DisableUnbound_ImmediateUpdate(t *testing.T) {
	stub := &accountsStub{accounts: accountsListFixture()} // no sessions
	m := seedAccountsListWithSessions(t, stub)             // sessionsKnown=true, count=0

	updated, cmd := m.Update(keyPress('x'))
	am := updated.(AccountsListModel)
	if am.confirm.active {
		t.Fatal("x on a known-unbound active account must NOT open a confirm")
	}
	if !am.disabling["acc-claude"] {
		t.Fatal("x must mark the row as disabling")
	}
	if runCmd(cmd) == nil {
		t.Fatal("x must emit an UpdateAccount command")
	}
	if len(stub.updateReqs) != 1 {
		t.Fatalf("UpdateAccount calls = %d, want 1", len(stub.updateReqs))
	}
	if got := stub.updateReqs[0]; got.GetId() != "acc-claude" || got.GetStatus() != "disabled" {
		t.Fatalf("UpdateAccount req = {id:%q status:%q}, want {acc-claude disabled}", got.GetId(), got.GetStatus())
	}
}

// TestAccountActions_DisableBound_ConfirmThenUpdate: `x` on a bound active
// account opens a confirm stating the bound count; confirming issues the
// disable.
func TestAccountActions_DisableBound_ConfirmThenUpdate(t *testing.T) {
	stub := &accountsStub{
		accounts: accountsListFixture(),
		sessions: []*pb.Session{
			boundSession("s1", "acc-claude"),
			boundSession("s2", "acc-claude"),
		},
	}
	m := seedAccountsListWithSessions(t, stub)

	updated, cmd := m.Update(keyPress('x'))
	am := updated.(AccountsListModel)
	if !am.confirm.active {
		t.Fatal("x on a bound active account must open a confirm")
	}
	if cmd != nil {
		t.Fatal("opening the disable confirm must not fire an RPC yet")
	}
	content := flattenPrompt(am.View().Content)
	if !strings.Contains(content, "2 running chats") {
		t.Fatalf("disable confirm must state the bound count\n%s", content)
	}
	if !strings.Contains(content, "may be interrupted") {
		t.Fatalf("disable confirm must warn about interruption\n%s", content)
	}
	if len(stub.updateReqs) != 0 {
		t.Fatalf("no UpdateAccount before confirm, got %d", len(stub.updateReqs))
	}

	updated, cmd = am.Update(keyPress('y'))
	am = updated.(AccountsListModel)
	if !am.disabling["acc-claude"] {
		t.Fatal("confirming disable must mark the row as disabling")
	}
	if runCmd(cmd) == nil {
		t.Fatal("confirming disable must run the UpdateAccount command")
	}
	if len(stub.updateReqs) != 1 || stub.updateReqs[0].GetStatus() != "disabled" {
		t.Fatalf("confirm must issue exactly one disable UpdateAccount, got %+v", stub.updateReqs)
	}
}

// TestAccountActions_Enable_NoWarning: `x` on a disabled account re-enables it
// immediately with no bound-session warning (active-ward direction).
func TestAccountActions_Enable_NoWarning(t *testing.T) {
	stub := &accountsStub{
		accounts: accountsListFixture(),
		// Even a bound disabled account enables without warning.
		sessions: []*pb.Session{boundSession("s1", "acc-codex")},
	}
	m := seedAccountsListWithSessions(t, stub)
	m = cursorDown(t, m) // highlight acc-codex (disabled)

	updated, cmd := m.Update(keyPress('x'))
	am := updated.(AccountsListModel)
	if am.confirm.active {
		t.Fatal("enabling a disabled account must not open a confirm")
	}
	if runCmd(cmd) == nil {
		t.Fatal("x must emit an UpdateAccount command to enable")
	}
	if len(stub.updateReqs) != 1 {
		t.Fatalf("UpdateAccount calls = %d, want 1", len(stub.updateReqs))
	}
	if got := stub.updateReqs[0]; got.GetId() != "acc-codex" || got.GetStatus() != "active" {
		t.Fatalf("enable req = {id:%q status:%q}, want {acc-codex active}", got.GetId(), got.GetStatus())
	}
}

// TestAccountActions_RemoveConfirm_CallsRemoveOnce: `d` opens a remove confirm;
// y/enter issues exactly one RemoveAccount for the highlighted id.
func TestAccountActions_RemoveConfirm_CallsRemoveOnce(t *testing.T) {
	for _, confirmKey := range []tea.KeyPressMsg{keyPress('y'), {Code: tea.KeyEnter}} {
		stub := &accountsStub{accounts: accountsListFixture()}
		m := seedAccountsListWithSessions(t, stub)

		updated, cmd := m.Update(keyPress('d'))
		am := updated.(AccountsListModel)
		if !am.confirm.active {
			t.Fatal("d must open a remove confirm")
		}
		if cmd != nil {
			t.Fatal("opening the remove confirm must not fire an RPC yet")
		}
		content := flattenPrompt(am.View().Content)
		if !strings.Contains(content, "Remove account") || !strings.Contains(content, "Claude Prod") {
			t.Fatalf("remove confirm must name the account\n%s", content)
		}

		updated, cmd = am.Update(confirmKey)
		am = updated.(AccountsListModel)
		if !am.removing["acc-claude"] {
			t.Fatal("confirming remove must mark the row as removing")
		}
		if runCmd(cmd) == nil {
			t.Fatal("confirming remove must run the RemoveAccount command")
		}
		if len(stub.removeIDs) != 1 || stub.removeIDs[0] != "acc-claude" {
			t.Fatalf("RemoveAccount ids = %v, want exactly [acc-claude]", stub.removeIDs)
		}
		if len(stub.updateReqs) != 0 {
			t.Fatalf("remove must not call UpdateAccount, got %d", len(stub.updateReqs))
		}
	}
}

// TestAccountActions_RemoveDismiss_NoRPC: dismissing the remove confirm (n/esc)
// makes zero RPC calls.
func TestAccountActions_RemoveDismiss_NoRPC(t *testing.T) {
	for _, dismissKey := range []tea.KeyPressMsg{keyPress('n'), {Code: tea.KeyEsc}} {
		stub := &accountsStub{accounts: accountsListFixture()}
		m := seedAccountsListWithSessions(t, stub)

		updated, _ := m.Update(keyPress('d'))
		am := updated.(AccountsListModel)
		if !am.confirm.active {
			t.Fatal("d must open a remove confirm")
		}

		updated, cmd := am.Update(dismissKey)
		am = updated.(AccountsListModel)
		if am.confirm.active {
			t.Fatal("dismiss must close the confirm")
		}
		if got := runCmd(cmd); got != nil {
			t.Fatalf("dismiss must not run an action command, got %T", got)
		}
		if len(stub.removeIDs) != 0 || len(stub.updateReqs) != 0 {
			t.Fatalf("dismiss must make zero RPC calls: removes=%v updates=%d", stub.removeIDs, len(stub.updateReqs))
		}
		if am.removing["acc-claude"] {
			t.Fatal("dismiss must not mark the row as removing")
		}
	}
}

// TestAccountActions_BoundWarningCopy_GenericFallback: when the session list is
// unavailable (errored), both prompts degrade to a generic warning rather than
// claiming zero, and the remove prompt still recommends disable.
func TestAccountActions_BoundWarningCopy_GenericFallback(t *testing.T) {
	stub := &accountsStub{
		accounts:    accountsListFixture(),
		sessionsErr: errors.New("sessions unavailable"),
	}
	m := seedAccountsListWithSessions(t, stub)
	if m.sessionsKnown {
		t.Fatal("a session-list error must leave sessionsKnown false")
	}

	// Disable of an active account with an unknown count opens a confirm with
	// the generic warning (not a fabricated "0 running chats").
	updated, _ := m.Update(keyPress('x'))
	dm := updated.(AccountsListModel)
	if !dm.confirm.active {
		t.Fatal("disable with unknown bound count must open a confirm (fail toward warning)")
	}
	// Flatten so the assertion is robust to the width-wrapping the confirm
	// footer applies to long copy (lipgloss inserts newlines + per-line ANSI).
	disableCopy := flattenPrompt(dm.View().Content)
	if !strings.Contains(disableCopy, "Running chats bound to this account may be interrupted") {
		t.Fatalf("disable generic fallback copy missing\n%s", disableCopy)
	}
	if strings.Contains(disableCopy, "0 running") {
		t.Fatalf("unknown count must not claim zero\n%s", disableCopy)
	}

	// Remove prompt: generic warning + disable recommendation.
	updated, _ = m.Update(keyPress('d'))
	rm := updated.(AccountsListModel)
	removeCopy := flattenPrompt(rm.View().Content)
	if !strings.Contains(removeCopy, "consider disable instead of remove") {
		t.Fatalf("remove prompt must recommend disable\n%s", removeCopy)
	}
	if !strings.Contains(removeCopy, "may continue or fail") {
		t.Fatalf("remove prompt must explain spawn-state outcome\n%s", removeCopy)
	}
}

// TestAccountActions_RemovePromptText_StatesBoundCount is the direct evidence
// that the remove prompt copy folds in the bound count and disable recommend.
func TestAccountActions_RemovePromptText_StatesBoundCount(t *testing.T) {
	acct := &pb.Account{Id: "acc-1", Label: "Prod", Status: "active"}

	if got := accountRemovePromptText(acct, 3, true); !strings.Contains(got, "3 running chats") ||
		!strings.Contains(got, "consider disable instead of remove") {
		t.Fatalf("bound remove copy = %q", got)
	}
	if got := accountRemovePromptText(acct, 0, true); strings.Contains(got, "consider disable") {
		t.Fatalf("unbound remove copy must not recommend disable: %q", got)
	}
	if got := accountRemovePromptText(acct, 0, false); !strings.Contains(got, "consider disable instead of remove") {
		t.Fatalf("unknown-count remove copy must recommend disable: %q", got)
	}
}

// TestAccountActions_BoundSessionCount verifies the client-side count + the
// known/unknown degradation.
func TestAccountActions_BoundSessionCount(t *testing.T) {
	sessions := []*pb.Session{
		boundSession("s1", "a"),
		boundSession("s2", "a"),
		boundSession("s3", "b"),
	}
	if n, known := boundSessionCount(sessions, true, "a"); n != 2 || !known {
		t.Fatalf("count(a) = (%d,%v), want (2,true)", n, known)
	}
	if n, known := boundSessionCount(sessions, true, "z"); n != 0 || !known {
		t.Fatalf("count(z) = (%d,%v), want (0,true)", n, known)
	}
	if n, known := boundSessionCount(sessions, false, "a"); n != 0 || known {
		t.Fatalf("count with unknown sessions = (%d,%v), want (0,false)", n, known)
	}
}

// TestAccountActions_PendingRender_Removing: an in-flight remove renders a
// "removing" STATUS cell (mirrors destructive_row_pending_test.go).
func TestAccountActions_PendingRender_Removing(t *testing.T) {
	m := AccountsListModel{
		spinner:   newStatusSpinner(),
		table:     newBossTable(nil, nil, 0),
		testing:   map[string]bool{},
		disabling: map[string]bool{},
		removing:  map[string]bool{"acc-claude": true},
		accounts:  accountsListFixture(),
	}
	m.rebuildTable()

	rows := m.table.Rows()
	if len(rows) != 2 {
		t.Fatalf("table rows = %d, want 2", len(rows))
	}
	// STATUS is column index 3: {indicator, label, provider, status, ...}.
	if got := rows[0][3]; !strings.Contains(got, "removing") {
		t.Fatalf("removing account STATUS = %q, want removing", got)
	}
	if got := rows[1][3]; strings.Contains(got, "removing") {
		t.Fatalf("non-removing account STATUS = %q, want normal status", got)
	}
}

// TestAccountActions_ConfirmPrompt_CredentialSafety: the remove/disable confirm
// surfaces never render secret-shaped tokens.
func TestAccountActions_ConfirmPrompt_CredentialSafety(t *testing.T) {
	stub := &accountsStub{
		accounts: accountsListFixture(),
		sessions: []*pb.Session{boundSession("s1", "acc-claude")},
	}
	m := seedAccountsListWithSessions(t, stub)

	for _, key := range []tea.KeyPressMsg{keyPress('x'), keyPress('d')} {
		updated, _ := m.Update(key)
		content := strings.ToLower(updated.(AccountsListModel).View().Content)
		for _, forbidden := range []string{"password", "sk-ant", "secret", "bearer "} {
			if strings.Contains(content, forbidden) {
				t.Fatalf("confirm prompt leaked secret-shaped token %q\n%s", forbidden, content)
			}
		}
	}
}

// TestAccountEdit_Remove_ConfirmCallsRemoveAndCancels: `d` on the edit form
// (not editing a field) opens the shared remove confirm; confirming issues one
// RemoveAccount and pops back to the list (Cancelled()==true).
func TestAccountEdit_Remove_ConfirmCallsRemoveAndCancels(t *testing.T) {
	stub := &accountsStub{}
	acct := &pb.Account{Id: "acc-1", Provider: "claude", Label: "Edit Me", Status: "active"}
	m := NewAccountEditModel(stub, context.Background(), acct)

	updated, _ := m.Update(keyPress('d'))
	em := updated.(AccountEditModel)
	if !em.confirm.active {
		t.Fatal("d must open the remove confirm on the edit form")
	}
	if !strings.Contains(em.View().Content, "Remove account") {
		t.Fatalf("edit confirm missing remove copy\n%s", em.View().Content)
	}

	updated, cmd := em.Update(keyPress('y'))
	em = updated.(AccountEditModel)
	msg := runCmd(cmd)
	if len(stub.removeIDs) != 1 || stub.removeIDs[0] != "acc-1" {
		t.Fatalf("RemoveAccount ids = %v, want [acc-1]", stub.removeIDs)
	}
	removed, ok := msg.(accountRemovedMsg)
	if !ok {
		t.Fatalf("confirm produced %T, want accountRemovedMsg", msg)
	}

	updated, _ = em.Update(removed)
	em = updated.(AccountEditModel)
	if !em.Cancelled() {
		t.Fatal("a successful remove must pop back to the list (Cancelled()==true)")
	}
}

// TestAccountEdit_Remove_NotWhileEditing: `d` while typing in a text field must
// reach the input, not open the remove confirm.
func TestAccountEdit_Remove_NotWhileEditing(t *testing.T) {
	stub := &accountsStub{}
	acct := &pb.Account{Id: "acc-1", Provider: "claude", Label: "Edit Me", Status: "active"}
	m := NewAccountEditModel(stub, context.Background(), acct)

	// Enter the Label editor (cursor starts on Label), then press 'd'.
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	em := updated.(AccountEditModel)
	updated, _ = em.Update(keyPress('d'))
	em = updated.(AccountEditModel)
	if em.confirm.active {
		t.Fatal("d while editing a field must not open the remove confirm")
	}
	if len(stub.removeIDs) != 0 {
		t.Fatalf("editing must not remove: %v", stub.removeIDs)
	}
}

// TestAccountActions_StatusUpdateError_ClearsPendingNoRefetch: a failed
// UpdateAccount clears the row's disabling pending, surfaces an error status,
// and does NOT issue a follow-up account refetch.
func TestAccountActions_StatusUpdateError_ClearsPendingNoRefetch(t *testing.T) {
	stub := &accountsStub{
		accounts:  accountsListFixture(), // acc-claude active, known-unbound
		updateErr: errors.New("daemon rejected status"),
	}
	m := seedAccountsListWithSessions(t, stub)

	updated, cmd := m.Update(keyPress('x')) // immediate disable
	am := updated.(AccountsListModel)
	if !am.disabling["acc-claude"] {
		t.Fatal("x must mark the row disabling before the RPC resolves")
	}
	msg := runCmd(cmd)
	res, ok := msg.(accountStatusUpdatedMsg)
	if !ok || res.err == nil {
		t.Fatalf("expected a failed accountStatusUpdatedMsg, got %T (%v)", msg, msg)
	}

	updated, follow := am.Update(res)
	am = updated.(AccountsListModel)
	if am.disabling["acc-claude"] {
		t.Fatal("a failed status update must clear the disabling pending")
	}
	if !am.statusErr {
		t.Fatal("a failed status update must set an error status")
	}
	if follow != nil {
		t.Fatal("a failed status update must NOT trigger a follow-up refetch")
	}
	if stub.listCalls != 0 {
		t.Fatalf("no ListAccounts refetch expected on error, got %d", stub.listCalls)
	}
}

// TestAccountActions_RemoveError_ClearsPendingNoRefetch: a failed RemoveAccount
// clears the removing pending, surfaces an error status, and issues no refetch.
func TestAccountActions_RemoveError_ClearsPendingNoRefetch(t *testing.T) {
	stub := &accountsStub{
		accounts:  accountsListFixture(),
		removeErr: errors.New("daemon rejected remove"),
	}
	m := seedAccountsListWithSessions(t, stub)

	updated, _ := m.Update(keyPress('d'))
	am := updated.(AccountsListModel)
	updated, cmd := am.Update(keyPress('y'))
	am = updated.(AccountsListModel)
	if !am.removing["acc-claude"] {
		t.Fatal("confirming remove must mark the row removing")
	}
	msg := runCmd(cmd)
	res, ok := msg.(accountRemovedMsg)
	if !ok || res.err == nil {
		t.Fatalf("expected a failed accountRemovedMsg, got %T (%v)", msg, msg)
	}

	updated, follow := am.Update(res)
	am = updated.(AccountsListModel)
	if am.removing["acc-claude"] {
		t.Fatal("a failed remove must clear the removing pending")
	}
	if !am.statusErr {
		t.Fatal("a failed remove must set an error status")
	}
	if follow != nil {
		t.Fatal("a failed remove must NOT trigger a follow-up refetch")
	}
}

// TestAccountEdit_RemoveError_KeepsFormOpen: a failed RemoveAccount from the
// edit form surfaces the error and keeps the form open (not popped back).
func TestAccountEdit_RemoveError_KeepsFormOpen(t *testing.T) {
	stub := &accountsStub{removeErr: errors.New("daemon rejected remove")}
	acct := &pb.Account{Id: "acc-1", Provider: "claude", Label: "Edit Me", Status: "active"}
	m := NewAccountEditModel(stub, context.Background(), acct)

	updated, _ := m.Update(keyPress('d'))
	em := updated.(AccountEditModel)
	updated, cmd := em.Update(keyPress('y'))
	em = updated.(AccountEditModel)
	res, ok := runCmd(cmd).(accountRemovedMsg)
	if !ok || res.err == nil {
		t.Fatalf("expected a failed accountRemovedMsg")
	}

	updated, _ = em.Update(res)
	em = updated.(AccountEditModel)
	if em.Cancelled() {
		t.Fatal("a failed remove must keep the edit form open, not pop back")
	}
}

// TestAccountActions_InFlightGuard_NoDuplicateRPC: while a row's status flip or
// remove is in flight, a repeated x/d keypress is ignored so no duplicate RPC
// is dispatched.
func TestAccountActions_InFlightGuard_NoDuplicateRPC(t *testing.T) {
	// Immediate disable in flight -> second x is a no-op.
	stub := &accountsStub{accounts: accountsListFixture()}
	m := seedAccountsListWithSessions(t, stub)
	updated, firstCmd := m.Update(keyPress('x'))
	am := updated.(AccountsListModel)
	if runCmd(firstCmd) == nil {
		t.Fatal("first x must emit an UpdateAccount command")
	}
	if len(stub.updateReqs) != 1 {
		t.Fatalf("first x must issue one UpdateAccount, got %d", len(stub.updateReqs))
	}
	_, cmd := am.Update(keyPress('x'))
	if cmd != nil {
		t.Fatal("a second x while disabling is in flight must be a no-op")
	}
	if len(stub.updateReqs) != 1 {
		t.Fatalf("a second x must not issue a duplicate UpdateAccount, got %d", len(stub.updateReqs))
	}

	// Remove confirmed -> row removing -> second d does not open a new confirm.
	stub2 := &accountsStub{accounts: accountsListFixture()}
	m2 := seedAccountsListWithSessions(t, stub2)
	updated, _ = m2.Update(keyPress('d'))
	am2 := updated.(AccountsListModel)
	updated, cmd = am2.Update(keyPress('y'))
	am2 = updated.(AccountsListModel)
	if !am2.removing["acc-claude"] {
		t.Fatal("confirming remove must mark the row removing")
	}
	_ = runCmd(cmd) // fire the RemoveAccount
	if len(stub2.removeIDs) != 1 {
		t.Fatalf("one RemoveAccount expected, got %d", len(stub2.removeIDs))
	}
	updated, cmd = am2.Update(keyPress('d'))
	am2 = updated.(AccountsListModel)
	if am2.confirm.active {
		t.Fatal("a second d while removing is in flight must not open a new confirm")
	}
	if cmd != nil {
		t.Fatal("a second d while removing is in flight must be a no-op")
	}
	if len(stub2.removeIDs) != 1 {
		t.Fatalf("a second d must not issue a duplicate RemoveAccount, got %d", len(stub2.removeIDs))
	}
}

// TestAccountActions_FetchSessions_RecordsCall: the one-shot session fetch
// batched in Init calls ListSessions exactly once and yields a snapshot.
func TestAccountActions_FetchSessions_RecordsCall(t *testing.T) {
	stub := &accountsStub{
		accounts: accountsListFixture(),
		sessions: []*pb.Session{boundSession("s1", "acc-claude")},
	}
	m := seedAccountsList(t, stub)

	loaded, ok := runCmd(m.fetchSessions()).(sessionsLoadedMsg)
	if !ok {
		t.Fatal("fetchSessions must produce a sessionsLoadedMsg")
	}
	if stub.sessionsCalls != 1 {
		t.Fatalf("ListSessions calls = %d, want 1", stub.sessionsCalls)
	}
	if len(loaded.sessions) != 1 || loaded.err != nil {
		t.Fatalf("sessionsLoadedMsg = %+v, want 1 session no err", loaded)
	}
}
