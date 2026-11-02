package newsession

// Conflict-aware handling of a refused create (BOS-2143). CreateSession refuses
// with AlreadyExists for several causes, each bypassed by a different request
// flag (or by none). The daemon names the cause in a CreateSessionConflict
// detail; this file picks the prompt and the bypass flag from it, and shows the
// server's text instead of a prompt whenever retrying could not help, so a
// conflict prompt never reappears after its own confirmation.

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"

	"github.com/recurser/boss/api/tui/ui"
	"github.com/recurser/boss/internal/domain/rpcerr"
)

// applyCreateFailure records a failed create. An AlreadyExists refusal whose
// bypass is not yet armed opens its prompt; every other failure — a bypass
// already sent and refused again, a session still starting, or a refusal with
// no recognised detail — shows the server's text on the form phase.
func (m Model) applyCreateFailure(err error) Model {
	m.phase = newSessionPhaseForm
	if conflict, ok := m.promptableConflict(err); ok {
		m.conflict = conflict
		m.err = nil
		return m
	}
	m.conflict = nil
	m.err = err
	return m
}

// promptableConflict returns the conflict detail on err when it names a cause
// the user can still bypass with a flag this wizard has not yet sent.
func (m Model) promptableConflict(err error) (*pb.CreateSessionConflict, bool) {
	if rpcerr.KindOf(err) != rpcerr.AlreadyExists {
		return nil, false
	}
	conflict, ok := rpcerr.Detail[*pb.CreateSessionConflict](err)
	if !ok || conflict == nil {
		return nil, false
	}
	switch conflict.GetReason() {
	case pb.CreateSessionConflictReason_CREATE_SESSION_CONFLICT_REASON_ALREADY_SHIPPED,
		pb.CreateSessionConflictReason_CREATE_SESSION_CONFLICT_REASON_DUPLICATE_ACTIVE_SESSION:
		return conflict, !m.force
	case pb.CreateSessionConflictReason_CREATE_SESSION_CONFLICT_REASON_BRANCH_EXISTS:
		return conflict, !m.forceBranch
	case pb.CreateSessionConflictReason_CREATE_SESSION_CONFLICT_REASON_UNSPECIFIED,
		pb.CreateSessionConflictReason_CREATE_SESSION_CONFLICT_REASON_SESSION_STARTING:
		// Nothing in the request bypasses these, so a prompt could only loop.
	}
	return nil, false
}

// clearCreateOverrides drops every bypass flag a conflict prompt armed.
func (m *Model) clearCreateOverrides() {
	m.force = false
	m.forceBranch = false
}

// updateConflict routes a key press on the open conflict prompt.
func (m Model) updateConflict(msg tea.KeyMsg) (Model, tea.Cmd) {
	key := msg.String()
	switch key {
	case "n", "N", "esc":
		return m.cancelConflict()
	}
	switch m.conflict.GetReason() {
	case pb.CreateSessionConflictReason_CREATE_SESSION_CONFLICT_REASON_DUPLICATE_ACTIVE_SESSION:
		switch key {
		case "a", "A", "enter":
			m.conflict, m.createdSess, m.done = nil, &pb.Session{Id: m.conflict.GetExistingSessionId()}, true
			return m, nil
		case "f", "F":
			m.conflict, m.force = nil, true
			return m, m.startCreating()
		}
	case pb.CreateSessionConflictReason_CREATE_SESSION_CONFLICT_REASON_ALREADY_SHIPPED:
		if key == "y" || key == "Y" || key == "enter" {
			m.conflict, m.force = nil, true
			return m, m.startCreating()
		}
	case pb.CreateSessionConflictReason_CREATE_SESSION_CONFLICT_REASON_BRANCH_EXISTS:
		if key == "y" || key == "Y" || key == "enter" {
			m.conflict, m.forceBranch = nil, true
			return m, m.startCreating()
		}
	case pb.CreateSessionConflictReason_CREATE_SESSION_CONFLICT_REASON_UNSPECIFIED,
		pb.CreateSessionConflictReason_CREATE_SESSION_CONFLICT_REASON_SESSION_STARTING:
		// applyCreateFailure never opens a prompt for these.
	}
	return m, nil
}

// cancelConflict closes the prompt and returns to the phase the create was
// started from, with every bypass flag cleared.
func (m Model) cancelConflict() (Model, tea.Cmd) {
	m.conflict = nil
	m.clearCreateOverrides()
	m.err = nil
	switch m.selectedType {
	case sessionTypeLinearTicket, sessionTypeSentryIssue:
		m.phase = newSessionPhaseIssueSelect
		return m, nil
	case sessionTypeExistingPR:
		m.phase = newSessionPhasePRSelect
		return m, nil
	default:
		m.phase = newSessionPhaseForm
		m.buildForm()
		return m, m.form.Init()
	}
}

// renderConflict renders the prompt for the open conflict.
func (m Model) renderConflict() string {
	warning, body, actions := conflictPromptCopy(m.conflict)
	var b strings.Builder
	b.WriteString(m.headerView())
	b.WriteString("\n")
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(ui.ColorWarning).Render(warning))
	b.WriteString("\n")
	if body != "" {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(body))
		b.WriteString("\n")
	}
	b.WriteString(ui.StyleActionBar.Render(actions))
	return b.String()
}

// conflictPromptCopy returns the warning line, body line and action bar for a
// conflict prompt.
func conflictPromptCopy(conflict *pb.CreateSessionConflict) (warning, body, actions string) {
	switch conflict.GetReason() {
	case pb.CreateSessionConflictReason_CREATE_SESSION_CONFLICT_REASON_ALREADY_SHIPPED:
		return fmt.Sprintf("%s already shipped via PR #%d (%s).", conflict.GetTrackerId(), conflict.GetPrNumber(), conflict.GetPrState()),
			"Create another session anyway?",
			"[y/enter] create anyway  [n/esc] cancel"
	case pb.CreateSessionConflictReason_CREATE_SESSION_CONFLICT_REASON_DUPLICATE_ACTIVE_SESSION:
		return fmt.Sprintf("An active session (%s) already exists for this %s.", conflict.GetExistingSessionId(), conflict.GetMatchedTarget()),
			"",
			"[a/enter] attach  [f] create another  [n/esc] cancel"
	case pb.CreateSessionConflictReason_CREATE_SESSION_CONFLICT_REASON_UNSPECIFIED,
		pb.CreateSessionConflictReason_CREATE_SESSION_CONFLICT_REASON_SESSION_STARTING,
		pb.CreateSessionConflictReason_CREATE_SESSION_CONFLICT_REASON_BRANCH_EXISTS:
	}
	return "A branch with this name already exists.",
		"Remove the old branch and create a new session?",
		"[y/enter] confirm  [n/esc] cancel"
}
