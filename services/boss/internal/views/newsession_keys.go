package views

// Key routing for the new-session wizard (BOS-528). handleKey is the router
// NewSessionModel.Update hands every tea.KeyMsg to: the confirm-overwrite
// overlay first, then one handler per phase, then keyGlobal for the phases
// with no block of their own. That order is behaviour; do not reorder it. The
// PR and issue phases delegate their filter-mode keys to keyPRFilter /
// keyIssueFilter, and the two update*FilterInput pumps forward keystrokes to
// the textinput models. handleKey and the key* methods are new declarations
// introduced by this PR's Update decomposition; the bodies they carry are the
// original if-chain's branches verbatim.

import (
	tea "charm.land/bubbletea/v2"
)

// handleKey dispatches a key press on the wizard phase. The
// confirmingOverwrite check stays first because the overwrite prompt
// preceded every per-phase block in the original if-chain, and that
// precedence is behaviour.
//
// Every newSessionPhase constant is listed with no default arm so the
// exhaustive linter flags a future phase here. The loading/creating/done
// phases deliberately have no per-phase key block: they fall through to
// keyGlobal exactly as they fell out of the if-chain before.
func (m NewSessionModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.confirmingOverwrite {
		return m.updateConfirmOverwrite(msg)
	}

	switch m.phase {
	case newSessionPhaseRepoSelect:
		return m.keyRepoSelect(msg)
	case newSessionPhaseAgentSelect:
		return m.keyAgentSelect(msg)
	case newSessionPhaseTypeSelect:
		return m.keyTypeSelect(msg)
	case newSessionPhasePRSelect:
		return m.keyPRSelect(msg)
	case newSessionPhaseIssueSelect:
		return m.keyIssueSelect(msg)
	case newSessionPhaseForm:
		return m.keyForm(msg)
	case newSessionPhaseLoading, newSessionPhaseCreating, newSessionPhaseDone:
		// No per-phase key block: these fall through to the global handling
		// below, exactly as they fell out of the per-phase `if` chain before.
	}

	return m.keyGlobal(msg)
}

// keyRepoSelect handles keys in the repo picker.
func (m NewSessionModel) keyRepoSelect(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.cancel = true
		return m, nil
	case "enter":
		idx := m.repoTable.Cursor()
		if idx >= 0 && idx < len(m.repos) {
			m.selectedRepoID = m.repos[idx].Id
			return m.advanceFromRepo(), nil
		}
		return m, nil
	}

	var cmd tea.Cmd
	m.repoTable, cmd = m.repoTable.Update(msg)
	updateCursorColumn(&m.repoTable)
	return m, cmd
}

// keyAgentSelect handles keys in the agent picker.
func (m NewSessionModel) keyAgentSelect(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		if len(m.repos) > 1 {
			m.phase = newSessionPhaseRepoSelect
			return m, nil
		}
		m.cancel = true
		return m, nil
	case "enter", " ", "space":
		idx := m.agentTable.Cursor()
		if idx >= 0 && idx < len(m.agents) {
			agentName := m.agents[idx].Name
			m.initialAgent = agentName
			m.preferredAgent = agentName
			if m.onAgentSelected != nil {
				if err := m.onAgentSelected(agentName); err != nil {
					m.err = err
				}
			}
			return m.advanceToType(), nil
		}
		return m, nil
	}

	var cmd tea.Cmd
	m.agentTable, cmd = m.agentTable.Update(msg)
	updateCursorColumn(&m.agentTable)
	return m, cmd
}

// keyTypeSelect handles keys in the session-type picker.
func (m NewSessionModel) keyTypeSelect(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		// Go back to the most-recent picker: agent select, then repo
		// select; else cancel.
		if len(m.agents) > 1 {
			m.phase = newSessionPhaseAgentSelect
			m.buildAgentTable()
			return m, nil
		}
		if len(m.repos) > 1 {
			m.phase = newSessionPhaseRepoSelect
			return m, nil
		}
		m.cancel = true
		return m, nil
	case "enter":
		idx := m.typeTable.Cursor()
		opts := m.buildSessionTypeOptions()
		m.selectedType = opts[idx].typ
		return m.advanceFromTypeSelect()
	}

	var cmd tea.Cmd
	m.typeTable, cmd = m.typeTable.Update(msg)
	updateCursorColumn(&m.typeTable)
	return m, cmd
}

// keyPRSelect handles keys in the PR picker.
func (m NewSessionModel) keyPRSelect(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// While the filter input is focused, route keys through it (with
	// special handling for commit/clear/navigation).
	if m.prFilter.Active() {
		return m.keyPRFilter(msg)
	}

	switch msg.String() {
	case "/":
		cmd := m.prFilter.Activate()
		// Activate transitions the filter line from hidden (Height=0)
		// to visible (Height=1); rebuild the table so its height is
		// recomputed before the next render.
		m.buildPRTable()
		return m, cmd
	case "esc":
		if m.prFilter.Applied() {
			m.prFilter.Reset()
			m.prTable.SetCursor(0)
			m.buildPRTable()
			if len(m.prTable.Rows()) > 0 {
				m.prTable.GotoTop()
			}
			updateCursorColumn(&m.prTable)
			return m, nil
		}
		m.phase = newSessionPhaseTypeSelect
		m.forceBranch = false
		return m, nil
	case "enter":
		idx := m.prTable.Cursor()
		if idx >= 0 && idx < len(m.prsFiltered) {
			cmd := m.startCreating()
			return m, cmd
		}
		return m, nil
	}

	var cmd tea.Cmd
	m.prTable, cmd = m.prTable.Update(msg)
	updateCursorColumn(&m.prTable)
	return m, cmd
}

// keyPRFilter handles keys while the PR filter input is focused.
func (m NewSessionModel) keyPRFilter(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		if !m.prFilter.Commit() {
			m.prFilter.Dismiss()
			m.buildPRTable()
		}
		return m, nil
	case "esc":
		m.prFilter.Reset()
		m.prTable.SetCursor(0)
		m.buildPRTable()
		if len(m.prTable.Rows()) > 0 {
			m.prTable.GotoTop()
		}
		updateCursorColumn(&m.prTable)
		return m, nil
	case "up", "down", "ctrl+p", "ctrl+n", "ctrl+d", "ctrl+u":
		var cmd tea.Cmd
		m.prTable, cmd = m.prTable.Update(msg)
		updateCursorColumn(&m.prTable)
		return m, cmd
	}
	cmd := m.updatePRFilterInput(msg)
	return m, cmd
}

// keyIssueSelect handles keys in the tracker-issue picker.
func (m NewSessionModel) keyIssueSelect(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.issueFilter.Active() {
		return m.keyIssueFilter(msg)
	}

	switch msg.String() {
	case "/":
		cmd := m.issueFilter.Activate()
		// See the matching comment on the PR filter branch above.
		m.buildIssueTable()
		return m, cmd
	case "esc":
		// Bumping seq here invalidates any in-flight fetch whose
		// response would otherwise snap the user back to issue select.
		m.issueSearchSeq++
		m.phase = newSessionPhaseTypeSelect
		m.forceBranch = false
		return m, nil
	case "enter":
		idx := m.issueTable.Cursor()
		if idx >= 0 && idx < len(m.issuesFiltered) {
			m.selectedIssue = m.trackerIssues[m.issuesFiltered[idx]]
			cmd := m.startCreating()
			return m, cmd
		}
		return m, nil
	}

	var cmd tea.Cmd
	m.issueTable, cmd = m.issueTable.Update(msg)
	updateCursorColumn(&m.issueTable)
	return m, cmd
}

// keyIssueFilter handles keys while the issue filter input is focused.
func (m NewSessionModel) keyIssueFilter(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		// Live debounced search means there is nothing to "commit"
		// — pressing Enter selects the highlighted row, mirroring
		// how Enter behaves once the filter input is blurred.
		idx := m.issueTable.Cursor()
		if idx >= 0 && idx < len(m.issuesFiltered) {
			m.selectedIssue = m.trackerIssues[m.issuesFiltered[idx]]
			cmd := m.startCreating()
			return m, cmd
		}
		return m, nil
	case "esc":
		// Clear the filter and refetch the unfiltered list. The
		// seq bump invalidates any in-flight tick or fetch.
		m.issueFilter.Reset()
		m.err = nil
		m.issueSearchSeq++
		m.issueSearchQuery = ""
		m.issuesFetching = true
		m.buildIssueTable()
		return m, fetchIssues(m.client, m.ctx, m.selectedRepoID, "", m.trackerSource(), m.issueSearchSeq)
	case "up", "down", "ctrl+p", "ctrl+n", "ctrl+d", "ctrl+u":
		var cmd tea.Cmd
		m.issueTable, cmd = m.issueTable.Update(msg)
		updateCursorColumn(&m.issueTable)
		return m, cmd
	}
	cmd := m.updateIssueFilterInput(msg)
	return m, cmd
}

// keyForm handles keys in the form phase. esc tears the form down and returns
// to the type chooser; everything else is delegated to the global handling,
// which routes it into the huh form.
func (m NewSessionModel) keyForm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "esc" {
		m.phase = newSessionPhaseTypeSelect
		m.form = nil
		m.err = nil
		m.fd = nil
		m.forceBranch = false
		return m, nil
	}
	return m.keyGlobal(msg)
}

// keyGlobal is the fall-through for phases with no per-phase key block
// (loading, creating, done) and for non-esc keys in the form phase.
func (m NewSessionModel) keyGlobal(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "esc" {
		m.cancel = true
		return m, nil
	}
	if model, cmd, ok := m.updateForm(msg); ok {
		return model, cmd
	}
	return m, nil
}

func (m NewSessionModel) updateConfirmOverwrite(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y", "enter":
		m.confirmingOverwrite = false
		m.forceBranch = true
		cmd := m.startCreating()
		return m, cmd
	case "n", "N", "esc":
		m.confirmingOverwrite = false
		m.forceBranch = false
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
	return m, nil
}

func (m *NewSessionModel) updatePRFilterInput(msg tea.Msg) tea.Cmd {
	cmd, changed := m.prFilter.Update(msg)
	if changed {
		m.prTable.SetCursor(0)
		m.buildPRTable()
		if len(m.prTable.Rows()) > 0 {
			m.prTable.GotoTop()
		}
		updateCursorColumn(&m.prTable)
	}
	return cmd
}

func (m *NewSessionModel) updateIssueFilterInput(msg tea.Msg) tea.Cmd {
	cmd, changed := m.issueFilter.Update(msg)
	if changed {
		m.issueTable.SetCursor(0)
		m.buildIssueTable()
		if len(m.issueTable.Rows()) > 0 {
			m.issueTable.GotoTop()
		}
		updateCursorColumn(&m.issueTable)
		m.issueSearchSeq++
		m.issuesFetching = true
		return tea.Batch(cmd, scheduleIssueSearch(m.issueSearchSeq, m.issueFilter.Query()))
	}
	return cmd
}
