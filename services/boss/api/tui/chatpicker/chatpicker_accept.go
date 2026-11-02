package chatpicker

import (
	tea "charm.land/bubbletea/v2"

	"github.com/recurser/boss/api/tui/ui"
)

// The acc[e]pt blocker flow (BOS-2134): the picker hosts ui.AcceptBlockerFlow,
// which owns the keyboard while it is open and renders the whole body.

// canAcceptBlocker reports whether acc[e]pt blocker is offered. The key and the
// action bar both read it, so the key never does what the bar does not show.
func (m Model) canAcceptBlocker() bool {
	if m.loading || m.confirm != confirmNone || m.renaming || m.merging || m.archiving || m.switching {
		return false
	}
	if m.attentionAction.Active() || m.attentionAction.MergeUnresolved() || m.acceptBlocker.Active() {
		return false
	}
	return ui.CanAcceptBlocker(m.session)
}

// keyAcceptBlocker opens the flow on the session's PR. When the action is
// hidden the key is swallowed: a hidden shortcut must do nothing.
func (m Model) keyAcceptBlocker() (Model, tea.Cmd) {
	if !m.canAcceptBlocker() {
		return m, nil
	}
	target := ui.AcceptBlockerTarget{SessionID: m.sessionID, PR: int(m.session.GetPrNumber())}
	var cmd tea.Cmd
	m.acceptBlocker, cmd = m.acceptBlocker.Start(m.ctx, m.client, target)
	return m, cmd
}

// handleAcceptBlockerKey gives the open flow its keys. While the accept runs,
// Esc leaves the picker: the daemon finishes the accept regardless.
func (m Model) handleAcceptBlockerKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	if m.acceptBlocker.Busy() && msg.String() == "esc" {
		m.cancel = true
		return m, nil
	}
	return m.handleAcceptBlockerMsg(msg)
}

func (m Model) handleAcceptBlockerMsg(msg tea.Msg) (Model, tea.Cmd) {
	var cmd tea.Cmd
	m.acceptBlocker, cmd = m.acceptBlocker.Update(msg)
	return m, cmd
}

// handlePaste gives a bracketed paste to whichever text field is open: the
// accept-blocker reason or the inline rename prompt.
func (m Model) handlePaste(msg tea.PasteMsg) (Model, tea.Cmd) {
	if m.acceptBlocker.TextEntryActive() {
		return m.handleAcceptBlockerMsg(msg)
	}
	if !m.renaming {
		return m, nil
	}
	var cmd tea.Cmd
	m.renameInput, cmd = m.renameInput.Update(msg)
	return m, cmd
}

// handleAcceptBlockerResult closes the flow and shows how it ended.
func (m Model) handleAcceptBlockerResult(msg ui.AcceptBlockerResultMsg) Model {
	if msg.SessionID != m.sessionID {
		return m
	}
	m.acceptBlocker, _ = m.acceptBlocker.Update(msg)
	m.statusMsg, m.statusNote = "", msg.Text
	if msg.Err != nil {
		m.statusMsg, m.statusNote = ui.RPCStatusDetail(msg.Err), ""
	}
	m.table.SetHeight(m.tableHeight())
	return m
}
