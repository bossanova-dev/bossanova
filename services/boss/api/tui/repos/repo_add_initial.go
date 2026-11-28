package repos

import (
	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/api/tui/ui"
)

// SetInitialPath seeds the add-repository wizard with a local checkout path
// so Init validates it immediately and advances straight to the details phase
// when valid, or lands on the editable Path field on failure or Esc.
func (m *RepoAddModel) SetInitialPath(path string) {
	if path == "" {
		return
	}
	m.sourceMode = sourceModeOpen
	m.fd.localPath = path
	m.phase = repoAddPhaseInput
	m.form = nil
	m.formFields = ui.FormFields{}
	m.validating = true
	m.err = nil
	m.pendingValidation = true
}

func (m RepoAddModel) initCmd() tea.Cmd {
	if m.pendingValidation {
		m.pendingValidation = false
		return m.validatePathCmd()
	}
	if m.form != nil {
		return m.form.Init()
	}
	return nil
}

func (m *RepoAddModel) startPathValidation() tea.Cmd {
	m.validating = true
	m.err = nil
	return m.validatePathCmd()
}

func (m RepoAddModel) validatePathCmd() tea.Cmd {
	return validateAndDetectCmd(m.ctx, m.client, m.fd.localPath)
}
