package newsession

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"

	"github.com/recurser/boss/internal/adapter/rpcerror"
)

const branchExistsText = "A branch with this name already exists."

func branchExistsConflict() *pb.CreateSessionConflict {
	return &pb.CreateSessionConflict{
		Reason:     pb.CreateSessionConflictReason_CREATE_SESSION_CONFLICT_REASON_BRANCH_EXISTS,
		BranchName: "won-123-test",
	}
}

func shippedConflict() *pb.CreateSessionConflict {
	return &pb.CreateSessionConflict{
		Reason:    pb.CreateSessionConflictReason_CREATE_SESSION_CONFLICT_REASON_ALREADY_SHIPPED,
		TrackerId: "WON-123",
		PrNumber:  1146,
		PrState:   "merged",
	}
}

func duplicateConflict() *pb.CreateSessionConflict {
	return &pb.CreateSessionConflict{
		Reason:            pb.CreateSessionConflictReason_CREATE_SESSION_CONFLICT_REASON_DUPLICATE_ACTIVE_SESSION,
		ExistingSessionId: "sess-existing",
		MatchedTarget:     "tracker issue",
	}
}

// alreadyExistsErr builds the error the create stream hands the wizard: a
// Connect AlreadyExists refusal, with the conflict attached as a real detail
// when non-nil, wrapped by the same adapter the daemon client uses.
func alreadyExistsErr(t *testing.T, text string, conflict *pb.CreateSessionConflict) error {
	t.Helper()
	failure := connect.NewError(connect.CodeAlreadyExists, errors.New(text))
	if conflict != nil {
		detail, err := connect.NewErrorDetail(conflict)
		if err != nil {
			t.Fatalf("NewErrorDetail: %v", err)
		}
		failure.AddDetail(detail)
	}
	return rpcerror.WrapError(failure)
}

// conflictWizard returns a wizard ready to create a session for a Linear ticket
// whose CreateSession calls fail with createErr.
func conflictWizard(t *testing.T, createErr error) (Model, *stubClient) {
	t.Helper()
	sc := &stubClient{repos: oneRepo(), createErr: createErr}
	m := NewModel(sc, context.Background())
	m = sendMsg(t, m, ReposMsg{Repos: sc.repos})
	m.selectedType = sessionTypeLinearTicket
	m.selectedIssue = &pb.TrackerIssue{ExternalId: "WON-123", Title: "Test", BranchName: "won-123-test"}
	return m, sc
}

// startCreate submits the create and feeds its failure back into the wizard.
func startCreate(t *testing.T, m Model) Model {
	t.Helper()
	cmd := m.startCreating()
	if cmd == nil {
		t.Fatal("startCreating returned nil cmd")
	}
	return sendMsg(t, m, cmd())
}

// pressAndRun presses a key on the wizard and, when it issues a create, feeds
// that create's result back in.
func pressAndRun(t *testing.T, m Model, key tea.KeyPressMsg) Model {
	t.Helper()
	updated, cmd := m.Update(key)
	m = assertValueType(t, updated)
	if cmd != nil {
		m = sendMsg(t, m, cmd())
	}
	return m
}

func requireView(t *testing.T, m Model, want []string, notWant []string) {
	t.Helper()
	view := m.View().Content
	for _, w := range want {
		if !strings.Contains(view, w) {
			t.Errorf("view missing %q; view:\n%s", w, view)
		}
	}
	for _, w := range notWant {
		if strings.Contains(view, w) {
			t.Errorf("view contains %q; view:\n%s", w, view)
		}
	}
}

// Regression for BOS-2143: an already-shipped refusal prompts with its own
// cause, confirming sends Force (not ForceBranch), and a repeat refusal after
// that confirm shows the server text instead of prompting again.
func TestConflict_AlreadyShippedConfirmSendsForceAndNeverLoops_Sessions_AC214301(t *testing.T) {
	serverText := "ticket WON-123 already shipped via PR #1146 (merged); pass force to create another"
	m, sc := conflictWizard(t, alreadyExistsErr(t, serverText, shippedConflict()))

	m = startCreate(t, m)
	requireView(t, m,
		[]string{"WON-123 already shipped via PR #1146 (merged).", "Create another session anyway?", "[y/enter] create anyway"},
		[]string{branchExistsText})

	m = pressAndRun(t, m, keyPress('y'))
	if sc.createCalls != 2 || !sc.createReq.GetForce() || sc.createReq.GetForceBranch() {
		t.Fatalf("calls=%d Force=%v ForceBranch=%v, want 2 / true / false", sc.createCalls, sc.createReq.GetForce(), sc.createReq.GetForceBranch())
	}
	if m.conflict != nil || m.err == nil {
		t.Fatalf("after a refused confirm: conflict=%v err=%v, want no prompt and the error", m.conflict, m.err)
	}
	requireView(t, m, []string{serverText}, []string{"Create another session anyway?", branchExistsText})
}

// The stream's own Err() path classifies a refusal the same way as the open.
func TestConflict_StreamErrorPathPrompts(t *testing.T) {
	m, _ := conflictWizard(t, nil)
	m.streamID = &streamIdentity{}
	m = sendMsg(t, m, streamErrorMsg{streamID: m.streamID, err: alreadyExistsErr(t, "shipped", shippedConflict())})
	requireView(t, m, []string{"WON-123 already shipped via PR #1146 (merged)."}, []string{branchExistsText})
}

func TestConflict_DuplicateSessionAttachOrCreateAnother_Sessions_AC214302(t *testing.T) {
	for _, key := range []tea.KeyPressMsg{keyPress('a'), specialKeyPress(tea.KeyEnter)} {
		m, sc := conflictWizard(t, alreadyExistsErr(t, "active session sess-existing already exists", duplicateConflict()))
		m = startCreate(t, m)
		requireView(t, m,
			[]string{"An active session (sess-existing) already exists for this tracker issue.", "[a/enter] attach", "[f] create another"},
			[]string{branchExistsText})

		m = pressAndRun(t, m, key)
		if !m.Done() || m.CreatedSession().GetId() != "sess-existing" {
			t.Fatalf("%s: Done=%v CreatedSession=%v, want attach to sess-existing", key, m.Done(), m.CreatedSession())
		}
		if sc.createCalls != 1 {
			t.Fatalf("%s: CreateSession calls = %d, want 1 (attach makes no new create)", key, sc.createCalls)
		}
	}

	m, sc := conflictWizard(t, alreadyExistsErr(t, "active session sess-existing already exists", duplicateConflict()))
	m = startCreate(t, m)
	m = pressAndRun(t, m, keyPress('f'))
	if sc.createCalls != 2 || !sc.createReq.GetForce() {
		t.Fatalf("calls=%d Force=%v, want 2 / true", sc.createCalls, sc.createReq.GetForce())
	}
	if m.conflict != nil || m.Done() {
		t.Fatalf("after refused create-another: conflict=%v done=%v, want error state", m.conflict, m.Done())
	}
}

func TestConflict_BranchExistsConfirmSendsForceBranchAndNeverLoops_Sessions_AC214303(t *testing.T) {
	serverText := "branch already exists for this session title"
	m, sc := conflictWizard(t, alreadyExistsErr(t, serverText, branchExistsConflict()))

	m = startCreate(t, m)
	requireView(t, m, []string{branchExistsText, "Remove the old branch and create a new session?"}, nil)

	m = pressAndRun(t, m, keyPress('y'))
	if !sc.createReq.GetForceBranch() || sc.createReq.GetForce() {
		t.Fatalf("ForceBranch=%v Force=%v, want true / false", sc.createReq.GetForceBranch(), sc.createReq.GetForce())
	}
	if m.conflict != nil {
		t.Fatal("branch prompt reopened after its own confirm")
	}
	requireView(t, m, []string{serverText}, []string{branchExistsText})
}

// A shipped confirm followed by a real branch collision prompts once more, and
// that confirm sends both flags.
func TestConflict_ShippedThenBranchExistsSendsBothFlags(t *testing.T) {
	m, sc := conflictWizard(t, alreadyExistsErr(t, "shipped", shippedConflict()))
	m = startCreate(t, m)

	sc.createErr = alreadyExistsErr(t, "branch already exists for this session title", branchExistsConflict())
	m = pressAndRun(t, m, keyPress('y'))
	requireView(t, m, []string{branchExistsText}, nil)

	m = pressAndRun(t, m, keyPress('y'))
	if sc.createCalls != 3 || !sc.createReq.GetForce() || !sc.createReq.GetForceBranch() {
		t.Fatalf("calls=%d Force=%v ForceBranch=%v, want 3 / true / true", sc.createCalls, sc.createReq.GetForce(), sc.createReq.GetForceBranch())
	}
	if m.conflict != nil {
		t.Fatal("a prompt reopened after both bypasses were sent")
	}
}

func TestConflict_UnbypassableRefusalsShowServerText_Sessions_AC214303(t *testing.T) {
	for name, err := range map[string]error{
		"session starting": alreadyExistsErr(t, `session already starting for branch "won-123-test"`, &pb.CreateSessionConflict{
			Reason:            pb.CreateSessionConflictReason_CREATE_SESSION_CONFLICT_REASON_SESSION_STARTING,
			ExistingSessionId: "sess-starting",
			BranchName:        "won-123-test",
		}),
		"no detail": alreadyExistsErr(t, `session already starting for branch "won-123-test"`, nil),
	} {
		m, _ := conflictWizard(t, err)
		m = startCreate(t, m)
		if m.conflict != nil || m.err == nil {
			t.Fatalf("%s: conflict=%v err=%v, want the error and no prompt", name, m.conflict, m.err)
		}
		requireView(t, m, []string{`session already starting for branch "won-123-test"`}, []string{branchExistsText, "[y/enter]", "[a/enter]"})
	}
}

func TestConflict_CancelClearsBothFlags(t *testing.T) {
	for name, conflict := range map[string]*pb.CreateSessionConflict{
		"already shipped": shippedConflict(),
		"duplicate":       duplicateConflict(),
		"branch exists":   branchExistsConflict(),
	} {
		for _, key := range []tea.KeyPressMsg{keyPress('n'), specialKeyPress(tea.KeyEscape)} {
			m, _ := conflictWizard(t, alreadyExistsErr(t, "refused", conflict))
			m = startCreate(t, m)
			m.force, m.forceBranch = true, true
			m.conflict = conflict
			m = pressAndRun(t, m, key)
			if m.conflict != nil || m.force || m.forceBranch {
				t.Fatalf("%s/%s: conflict=%v force=%v forceBranch=%v, want all cleared", name, key, m.conflict, m.force, m.forceBranch)
			}
			if m.phase != newSessionPhaseIssueSelect {
				t.Fatalf("%s/%s: phase = %d, want issue select", name, key, m.phase)
			}
		}
	}
}
