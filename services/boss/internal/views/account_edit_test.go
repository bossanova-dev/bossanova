package views

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// stubAccountEditClient implements client.BossClient for AccountEditModel
// tests. It embeds stubSessionSettingsClient (which stubs the full interface)
// and overrides UpdateAccount to capture the request. Every other method still
// panics via the embedded stub so an unexpected call fails loudly.
type stubAccountEditClient struct {
	stubSessionSettingsClient
	updatedAccount *pb.Account
	updateAcctErr  error
	updateAcctReq  *pb.UpdateAccountRequest // captures the last UpdateAccount request

	testResp  *pb.TestAccountResponse
	testErr   error
	testCalls int
	testedID  string
}

var _ client.BossClient = (*stubAccountEditClient)(nil)

func (s *stubAccountEditClient) UpdateAccount(_ context.Context, req *pb.UpdateAccountRequest) (*pb.Account, error) {
	s.updateAcctReq = req
	if s.updateAcctErr != nil {
		return nil, s.updateAcctErr
	}
	return s.updatedAccount, nil
}

func (s *stubAccountEditClient) TestAccount(_ context.Context, id string) (*pb.TestAccountResponse, error) {
	s.testCalls++
	s.testedID = id
	if s.testErr != nil {
		return nil, s.testErr
	}
	return s.testResp, nil
}

func newAccountEditModel(stub *stubAccountEditClient, acct *pb.Account) AccountEditModel {
	return NewAccountEditModel(stub, context.Background(), acct)
}

func TestAccountEditSeedsInputs(t *testing.T) {
	m := newAccountEditModel(&stubAccountEditClient{}, &pb.Account{
		Id:       "acct-1",
		Label:    "prod-key",
		Priority: 7,
		Status:   "active",
	})
	if m.labelInput.Value() != "prod-key" {
		t.Fatalf("label input = %q, want %q", m.labelInput.Value(), "prod-key")
	}
	if m.priorityInput.Value() != "7" {
		t.Fatalf("priority input = %q, want %q", m.priorityInput.Value(), "7")
	}
	if m.editingField != -1 {
		t.Fatalf("editingField = %d, want -1", m.editingField)
	}
}

func TestAccountEditLabelEditSavesOnlyLabel(t *testing.T) {
	stub := &stubAccountEditClient{updatedAccount: &pb.Account{Id: "acct-1", Label: "renamed"}}
	m := newAccountEditModel(stub, &pb.Account{Id: "acct-1", Label: "old", Priority: 3, Status: "active"})

	// Enter editing on the label row (cursor starts at label row 0).
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(AccountEditModel)
	if m.editingField != accountEditRowLabel {
		t.Fatalf("editingField = %d, want label row", m.editingField)
	}
	m.labelInput.SetValue("  renamed  ")
	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := updated.(AccountEditModel)
	if got.editingField != -1 {
		t.Fatalf("editingField = %d, want -1 after commit", got.editingField)
	}
	if cmd == nil {
		t.Fatal("expected a save command")
	}
	saved, ok := cmd().(accountEditSavedMsg)
	if !ok {
		t.Fatal("save command did not return accountEditSavedMsg")
	}
	if saved.err != nil {
		t.Fatalf("save returned err: %v", saved.err)
	}
	req := stub.updateAcctReq
	if req == nil {
		t.Fatal("UpdateAccount was not called")
	}
	if req.Id != "acct-1" {
		t.Fatalf("req.Id = %q, want acct-1", req.Id)
	}
	if req.Label == nil || *req.Label != "renamed" {
		t.Fatalf("req.Label = %v, want \"renamed\" (trimmed)", req.Label)
	}
	if req.Priority != nil {
		t.Fatalf("req.Priority = %v, want nil (unchanged field)", req.Priority)
	}
	if req.Status != nil {
		t.Fatalf("req.Status = %v, want nil (unchanged field)", req.Status)
	}
	if len(req.AllowedModels) != 0 {
		t.Fatalf("req.AllowedModels = %v, want empty (never sent)", req.AllowedModels)
	}
}

func TestAccountEditEmptyLabelRejectedLocally(t *testing.T) {
	stub := &stubAccountEditClient{}
	m := newAccountEditModel(stub, &pb.Account{Id: "acct-1", Label: "old", Status: "active"})

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(AccountEditModel)
	m.labelInput.SetValue("   ")
	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := updated.(AccountEditModel)

	if got.err == nil || !strings.Contains(got.err.Error(), "label cannot be empty") {
		t.Fatalf("err = %v, want empty-label error", got.err)
	}
	if cmd != nil {
		t.Fatal("expected no save command for empty label")
	}
	if got.editingField != accountEditRowLabel {
		t.Fatalf("should remain in editing on error, editingField = %d", got.editingField)
	}
	if got.labelInput.Value() != "   " {
		t.Fatalf("input should be preserved on reject, got %q", got.labelInput.Value())
	}
	if stub.updateAcctReq != nil {
		t.Fatalf("UpdateAccount must not be called for empty label: %+v", stub.updateAcctReq)
	}
}

func TestAccountEditStatusToggleSavesImmediately(t *testing.T) {
	t.Run("active toggles to disabled", func(t *testing.T) {
		stub := &stubAccountEditClient{updatedAccount: &pb.Account{Id: "acct-1", Status: "disabled"}}
		m := newAccountEditModel(stub, &pb.Account{Id: "acct-1", Label: "l", Status: "active"})
		// Move cursor to the status row.
		updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		m = updated.(AccountEditModel)
		if m.cursor != accountEditRowStatus {
			t.Fatalf("cursor = %d, want status row", m.cursor)
		}
		updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		got := updated.(AccountEditModel)
		if got.editingField != -1 {
			t.Fatalf("status toggle must not enter text editing, editingField = %d", got.editingField)
		}
		if cmd == nil {
			t.Fatal("expected a save command from status toggle")
		}
		if _, ok := cmd().(accountEditSavedMsg); !ok {
			t.Fatal("status toggle command did not return accountEditSavedMsg")
		}
		req := stub.updateAcctReq
		if req == nil || req.Status == nil {
			t.Fatalf("status not sent: %+v", req)
		}
		if *req.Status != "disabled" {
			t.Fatalf("status = %q, want disabled", *req.Status)
		}
		if req.Label != nil || req.Priority != nil {
			t.Fatalf("only status must be sent, got %+v", req)
		}
	})

	t.Run("disabled toggles to active", func(t *testing.T) {
		stub := &stubAccountEditClient{updatedAccount: &pb.Account{Id: "acct-1", Status: "active"}}
		m := newAccountEditModel(stub, &pb.Account{Id: "acct-1", Label: "l", Status: "disabled"})
		updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		m = updated.(AccountEditModel)
		_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		if cmd == nil {
			t.Fatal("expected a save command")
		}
		cmd()
		if stub.updateAcctReq.Status == nil || *stub.updateAcctReq.Status != "active" {
			t.Fatalf("status = %v, want active", stub.updateAcctReq.Status)
		}
	})
}

func TestAccountEditPriorityEdit(t *testing.T) {
	t.Run("valid integer saves only priority", func(t *testing.T) {
		stub := &stubAccountEditClient{updatedAccount: &pb.Account{Id: "acct-1"}}
		m := newAccountEditModel(stub, &pb.Account{Id: "acct-1", Label: "l", Priority: 1, Status: "active"})
		// Move cursor to the priority row (label -> status -> priority).
		updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		m = updated.(AccountEditModel)
		updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		m = updated.(AccountEditModel)
		if m.cursor != accountEditRowPriority {
			t.Fatalf("cursor = %d, want priority row", m.cursor)
		}
		updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		m = updated.(AccountEditModel)
		m.priorityInput.SetValue("5")
		updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		got := updated.(AccountEditModel)
		if got.err != nil {
			t.Fatalf("unexpected err: %v", got.err)
		}
		if cmd == nil {
			t.Fatal("expected a save command")
		}
		cmd()
		req := stub.updateAcctReq
		if req == nil || req.Priority == nil || *req.Priority != 5 {
			t.Fatalf("priority not sent as 5: %+v", req)
		}
		if req.Label != nil || req.Status != nil {
			t.Fatalf("only priority must be sent, got %+v", req)
		}
	})

	t.Run("non-integer rejected locally without saving", func(t *testing.T) {
		stub := &stubAccountEditClient{}
		m := newAccountEditModel(stub, &pb.Account{Id: "acct-1", Label: "l", Priority: 1, Status: "active"})
		updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		m = updated.(AccountEditModel)
		updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		m = updated.(AccountEditModel)
		updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		m = updated.(AccountEditModel)
		m.priorityInput.SetValue("abc")
		updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		got := updated.(AccountEditModel)
		if got.err == nil {
			t.Fatal("expected a parse error for non-integer priority")
		}
		if cmd != nil {
			t.Fatal("expected no save command on parse error")
		}
		if got.priorityInput.Value() != "abc" {
			t.Fatalf("input should be preserved, got %q", got.priorityInput.Value())
		}
		if stub.updateAcctReq != nil {
			t.Fatalf("UpdateAccount must not be called: %+v", stub.updateAcctReq)
		}
	})
}

func TestAccountEditSaveErrorRetainsEdits(t *testing.T) {
	stub := &stubAccountEditClient{}
	m := newAccountEditModel(stub, &pb.Account{Id: "acct-1", Label: "old", Status: "active"})
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(AccountEditModel)
	m.labelInput.SetValue("edited")
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(AccountEditModel)

	// Simulate the daemon rejecting the save.
	wantErr := errors.New("label \"x\" is reserved")
	updated, _ = m.Update(accountEditSavedMsg{err: wantErr})
	got := updated.(AccountEditModel)
	if !errors.Is(got.err, wantErr) {
		t.Fatalf("err = %v, want %v", got.err, wantErr)
	}
	if got.labelInput.Value() != "edited" {
		t.Fatalf("edit lost on save error, input = %q", got.labelInput.Value())
	}
	if got.account.GetLabel() != "old" {
		t.Fatalf("account should be unchanged on error, got %q", got.account.GetLabel())
	}
}

func TestAccountEditSaveSuccessRefreshes(t *testing.T) {
	m := newAccountEditModel(&stubAccountEditClient{}, &pb.Account{Id: "acct-1", Label: "old", Priority: 1, Status: "active"})
	m.err = errors.New("stale")
	updated, _ := m.Update(accountEditSavedMsg{account: &pb.Account{Id: "acct-1", Label: "fresh", Priority: 9, Status: "disabled"}})
	got := updated.(AccountEditModel)
	if got.account.GetLabel() != "fresh" {
		t.Fatalf("account not replaced: %+v", got.account)
	}
	if got.err != nil {
		t.Fatalf("err not cleared: %v", got.err)
	}
	if got.labelInput.Value() != "fresh" {
		t.Fatalf("label input not re-seeded: %q", got.labelInput.Value())
	}
	if got.priorityInput.Value() != "9" {
		t.Fatalf("priority input not re-seeded: %q", got.priorityInput.Value())
	}
}

// A save command runs asynchronously, so its result can arrive after the
// operator has opened another field's editor. The result must still be applied
// (a failed save must surface its error) rather than being swallowed by the
// editing message-routing branch — and it must not clobber the in-progress edit.
func TestAccountEditSaveResultAppliedWhileEditingAnotherField(t *testing.T) {
	t.Run("error surfaces and edit is preserved", func(t *testing.T) {
		m := newAccountEditModel(&stubAccountEditClient{}, &pb.Account{Id: "acct-1", Label: "old", Status: "active"})
		// Open the label editor and simulate an in-progress edit.
		updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		m = updated.(AccountEditModel)
		m.labelInput.SetValue("work-in-progress")
		if m.editingField != accountEditRowLabel {
			t.Fatalf("precondition: not editing label, editingField=%d", m.editingField)
		}

		wantErr := errors.New("status update rejected")
		updated, _ = m.Update(accountEditSavedMsg{err: wantErr})
		got := updated.(AccountEditModel)
		if !errors.Is(got.err, wantErr) {
			t.Fatalf("save error dropped while editing: err=%v", got.err)
		}
		if got.editingField != accountEditRowLabel {
			t.Fatalf("should stay in edit mode, editingField=%d", got.editingField)
		}
		if got.labelInput.Value() != "work-in-progress" {
			t.Fatalf("in-progress edit clobbered: %q", got.labelInput.Value())
		}
	})

	t.Run("success updates account without re-seeding the edited input", func(t *testing.T) {
		m := newAccountEditModel(&stubAccountEditClient{}, &pb.Account{Id: "acct-1", Label: "old", Status: "active"})
		updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		m = updated.(AccountEditModel)
		m.labelInput.SetValue("work-in-progress")

		updated, _ = m.Update(accountEditSavedMsg{account: &pb.Account{Id: "acct-1", Label: "fresh", Status: "disabled"}})
		got := updated.(AccountEditModel)
		if got.account.GetLabel() != "fresh" {
			t.Fatalf("account not updated from save result: %q", got.account.GetLabel())
		}
		if got.labelInput.Value() != "work-in-progress" {
			t.Fatalf("editing input must not be re-seeded mid-edit: %q", got.labelInput.Value())
		}
	})
}

func TestAccountEditViewShowsReadOnlyDetails(t *testing.T) {
	// BOS-269: the edit screen IS the detail screen. Above the editable rows it
	// renders a read-only Details section with the full safe operational
	// breakdown. (BOS-266 previously omitted this; BOS-269 adds it.)
	m := newAccountEditModel(&stubAccountEditClient{}, &pb.Account{
		Id:        "acct-1",
		Label:     "prod",
		Provider:  "claude",
		Status:    "active",
		Health:    "ok",
		Tier:      "max",
		CreatedAt: timestamppb.New(time.Now().Add(-48 * time.Hour)),
		UpdatedAt: timestamppb.New(time.Now().Add(-1 * time.Hour)),
	})
	got := m.View().Content
	for _, want := range []string{"Details", "Health:", "Priority:", "Tier:", "Cooldown:", "Last used:", "Last tested:", "Created:", "Updated:"} {
		if !strings.Contains(got, want) {
			t.Fatalf("detail section missing read-only row %q:\n%s", want, got)
		}
	}
}

func TestAccountEditDetailUsageRows(t *testing.T) {
	t.Run("populated account renders usage rows", func(t *testing.T) {
		m := newAccountEditModel(&stubAccountEditClient{}, &pb.Account{
			Id:       "acct-1",
			Label:    "prod",
			Provider: "claude",
			Status:   "active",
			Health:   "ok",
			Tier:     "max",
			Usage: &pb.UsageSnapshot{
				Util_5H:   0.42,
				Util_7D:   0.93,
				Reset_5H:  timestamppb.New(time.Now().Add(3 * time.Hour)),
				Reset_7D:  timestamppb.New(time.Now().Add(5 * 24 * time.Hour)),
				Status:    "active",
				PlanTier:  "pro",
				FetchedAt: timestamppb.New(time.Now().Add(-4 * time.Minute)),
			},
		})
		got := m.View().Content
		for _, want := range []string{"Usage 5h:", "Usage 7d:", "Plan tier:", "Usage age:", "42%", "93%", "resets in", "pro"} {
			if !strings.Contains(got, want) {
				t.Fatalf("detail usage rows missing %q:\n%s", want, got)
			}
		}
		// The usage-age row is a "fetched <rel> ago" freshness line.
		if !strings.Contains(got, "fetched") || !strings.Contains(got, "ago") {
			t.Fatalf("detail usage age row missing 'fetched <rel> ago':\n%s", got)
		}
	})

	t.Run("nil usage renders em dashes", func(t *testing.T) {
		m := newAccountEditModel(&stubAccountEditClient{}, &pb.Account{
			Id:       "acct-1",
			Label:    "prod",
			Provider: "claude",
			Status:   "active",
			Health:   "ok",
		})
		got := m.View().Content
		// The row labels are always present; their values are em dashes when the
		// snapshot is nil (never probed).
		for _, want := range []string{"Usage 5h:", "Usage 7d:", "Plan tier:", "Usage age:", "—"} {
			if !strings.Contains(got, want) {
				t.Fatalf("nil-usage detail missing %q:\n%s", want, got)
			}
		}
		// No fabricated utilization percentage for a never-probed account.
		if strings.Contains(got, "0%") {
			t.Fatalf("nil-usage detail must not render a fabricated 0%%:\n%s", got)
		}
	})
}

func TestAccountEditDetailStates(t *testing.T) {
	t.Run("cooling account shows resets", func(t *testing.T) {
		m := newAccountEditModel(&stubAccountEditClient{}, &pb.Account{
			Id:            "acct-1",
			Label:         "personal",
			Provider:      "codex",
			Status:        "disabled",
			Health:        "failed",
			CooldownUntil: timestamppb.New(time.Now().Add(90 * time.Minute)),
		})
		got := m.View().Content
		if !strings.Contains(got, "failed") {
			t.Fatalf("detail should render failed health:\n%s", got)
		}
		if !strings.Contains(got, "resets") {
			t.Fatalf("cooling detail should render a cooldown reset time:\n%s", got)
		}
	})

	t.Run("never-tested account", func(t *testing.T) {
		m := newAccountEditModel(&stubAccountEditClient{}, &pb.Account{Id: "acct-1", Label: "l", Status: "active", Health: "ok"})
		if !strings.Contains(m.View().Content, "never") {
			t.Fatalf("untested account should render 'never' last-tested:\n%s", m.View().Content)
		}
	})
}

func TestAccountEditEscCancels(t *testing.T) {
	m := newAccountEditModel(&stubAccountEditClient{}, &pb.Account{Id: "acct-1", Label: "l", Status: "active"})
	if m.Cancelled() {
		t.Fatal("precondition: should not be cancelled")
	}
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if !updated.(AccountEditModel).Cancelled() {
		t.Fatal("esc should set cancelled")
	}
}

func TestAccountEditNavigationClampsToEditableRows(t *testing.T) {
	m := newAccountEditModel(&stubAccountEditClient{}, &pb.Account{Id: "acct-1", Label: "l", Status: "active"})
	// Press down many times; cursor must not exceed the last editable row.
	for i := 0; i < 10; i++ {
		updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		m = updated.(AccountEditModel)
	}
	if m.cursor != accountEditRowPriority {
		t.Fatalf("cursor = %d, want clamped to last editable row %d", m.cursor, accountEditRowPriority)
	}
	// Up past the top clamps to 0.
	for i := 0; i < 10; i++ {
		updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
		m = updated.(AccountEditModel)
	}
	if m.cursor != accountEditRowLabel {
		t.Fatalf("cursor = %d, want 0", m.cursor)
	}
}

func TestAccountEditWindowSize(t *testing.T) {
	m := newAccountEditModel(&stubAccountEditClient{}, &pb.Account{Id: "acct-1", Label: "l", Status: "active"})
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if got := updated.(AccountEditModel).width; got != 120 {
		t.Fatalf("width = %d, want 120", got)
	}
}

func TestAccountEditEditingForwardsKeysToInput(t *testing.T) {
	m := newAccountEditModel(&stubAccountEditClient{}, &pb.Account{Id: "acct-1", Label: "", Status: "active"})
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(AccountEditModel)
	if m.editingField != accountEditRowLabel {
		t.Fatalf("precondition: not editing label (editingField=%d)", m.editingField)
	}
	updated, _ = m.Update(keyPress('z'))
	if got := updated.(AccountEditModel).labelInput.Value(); got != "z" {
		t.Fatalf("label input = %q, want the typed %q", got, "z")
	}
}

func TestAccountEditViewMasksLastTestError(t *testing.T) {
	const secret = "sk-ant-api03-SUPERSECRETTOKENVALUE1234567890"
	m := newAccountEditModel(&stubAccountEditClient{}, &pb.Account{
		Id:            "acct-1",
		Label:         "prod",
		Provider:      "claude",
		Status:        "active",
		Priority:      2,
		Health:        "failed",
		LastTestError: "auth failed: api_key=" + secret,
	})
	got := m.View().Content
	// CREDENTIAL SAFETY: BOS-269 renders a last-tested line in the Details
	// section, but the error text ALWAYS passes through maskTestError — the raw
	// secret must never reach the screen, and the redaction sentinel proves it
	// was masked.
	if strings.Contains(got, secret) {
		t.Fatalf("edit form leaked the raw secret token:\n%s", got)
	}
	if !strings.Contains(got, "Last tested:") {
		t.Fatalf("detail should render a masked last-tested row:\n%s", got)
	}
	if !strings.Contains(got, redactionSentinel) {
		t.Fatalf("masked last-test error should show the redaction sentinel:\n%s", got)
	}
}

func TestAccountEditTestAction_SplicesRefreshedAccount(t *testing.T) {
	// A live TestAccount returns an updated Account (health flips ok→failed with
	// a masked error). Driving 't' must splice it into m.account and show the
	// transient Detail status. CREDENTIAL SAFETY: on a failed live smoke test the
	// daemon returns Detail byte-identical to LastTestError (recordAndRespond →
	// err.Error()), so the fixture mirrors that — the SAME secret-bearing string
	// in BOTH fields — to prove the transient status line masks it, not just the
	// Details row.
	const secret = "sk-ant-api03-FAKESECRET1234567890abcdef"
	const daemonErr = "auth failed: api_key=" + secret
	stub := &stubAccountEditClient{
		testResp: &pb.TestAccountResponse{
			Account: &pb.Account{
				Id:            "acct-1",
				Label:         "prod",
				Provider:      "claude",
				Status:        "active",
				Health:        "failed",
				LastTestError: daemonErr,
			},
			Detail: daemonErr,
		},
	}
	m := newAccountEditModel(stub, &pb.Account{Id: "acct-1", Label: "prod", Provider: "claude", Status: "active", Health: "ok"})

	// 't' marks the model as testing and fires the RPC.
	updated, cmd := m.Update(keyPress('t'))
	m = updated.(AccountEditModel)
	if !m.testing {
		t.Fatal("t must mark the model as testing")
	}
	if cmd == nil {
		t.Fatal("t must emit a test command")
	}
	msg := cmd()
	tested, ok := msg.(accountEditTestedMsg)
	if !ok {
		t.Fatalf("t command produced %T, want accountEditTestedMsg", msg)
	}
	if stub.testCalls != 1 || stub.testedID != "acct-1" {
		t.Fatalf("TestAccount not called live: calls=%d id=%q", stub.testCalls, stub.testedID)
	}

	updated, _ = m.Update(tested)
	got := updated.(AccountEditModel)
	if got.testing {
		t.Fatal("accountEditTestedMsg must clear the testing flag")
	}
	if got.account.GetHealth() != "failed" {
		t.Fatalf("refreshed account not spliced: health=%q", got.account.GetHealth())
	}
	content := got.View().Content
	// The transient status line must render, but MASKED: the daemon Detail
	// carried the secret, so the raw token must be absent everywhere on screen
	// and the redaction sentinel must prove the transient line was masked (not
	// just the Details row).
	if strings.Contains(content, secret) {
		t.Fatalf("spliced account / transient status leaked the raw secret:\n%s", content)
	}
	if !strings.Contains(content, redactionSentinel) {
		t.Fatalf("transient test-status line not masked (no redaction sentinel):\n%s", content)
	}
	if !strings.Contains(content, "failed") {
		t.Fatalf("spliced failed health not rendered:\n%s", content)
	}
}

func TestAccountEditTestAction_RPCErrorIsNonBlocking(t *testing.T) {
	stub := &stubAccountEditClient{testErr: errors.New("daemon unreachable")}
	orig := &pb.Account{Id: "acct-1", Label: "prod", Provider: "claude", Status: "active", Health: "ok"}
	m := newAccountEditModel(stub, orig)

	updated, cmd := m.Update(keyPress('t'))
	m = updated.(AccountEditModel)
	tested := cmd().(accountEditTestedMsg)
	if tested.err == nil {
		t.Fatal("expected the RPC error to propagate in the message")
	}

	updated, _ = m.Update(tested)
	got := updated.(AccountEditModel)
	if got.testing {
		t.Fatal("testing flag must clear on error")
	}
	if got.account.GetHealth() != "ok" {
		t.Fatalf("account must be unchanged on RPC error: %+v", got.account)
	}
	if got.Cancelled() {
		t.Fatal("an RPC error must not cancel the view")
	}
	if !strings.Contains(got.View().Content, "Test failed") {
		t.Fatalf("non-blocking error status not shown:\n%s", got.View().Content)
	}
}

func TestAccountEditViewShowsFields(t *testing.T) {
	m := newAccountEditModel(&stubAccountEditClient{}, &pb.Account{
		Id:       "acct-42",
		Label:    "prod-key",
		Provider: "claude",
		Status:   "active",
		Priority: 3,
		Health:   "ok",
	})
	got := m.View().Content
	// The title carries the provider ("Edit account · claude"); the editable
	// rows carry Label/Status/Priority. There is no read-only "Provider:" row.
	for _, want := range []string{"Edit account", "Label:", "Priority:", "Status:", "claude", "prod-key"} {
		if !strings.Contains(got, want) {
			t.Fatalf("View missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "[esc] back") {
		t.Fatalf("View missing back action:\n%s", got)
	}
}
