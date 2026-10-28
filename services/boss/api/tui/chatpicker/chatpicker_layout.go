package chatpicker

import (
	"charm.land/bubbles/v2/spinner"
	"github.com/recurser/boss/api/tui/ui"
)

func (m *Model) SetSize(w, h int)                             { m.width, m.height = w, h }
func (m Model) Width() int                                    { return m.width }
func (m Model) Height() int                                   { return m.height }
func (m *Model) SetHeight(h int)                              { m.height = h }
func (m Model) Spinner() spinner.Model                        { return m.spinner }
func (m Model) Archiving() bool                               { return m.archiving }
func (m *Model) SetArchiving(v bool)                          { m.archiving = v }
func (m *Model) SetAttentionAction(a ui.AttentionActionState) { m.attentionAction = a }
func (m Model) AttentionAction() ui.AttentionActionState      { return m.attentionAction }
func (m Model) TextEntryActive() bool                         { return m.textEntryActive() }
func (m Model) SessionID() string                             { return m.sessionID }
func (m Model) HighlightID() string                           { return m.highlightID }
func (m Model) SelectedChatID() string                        { return m.selectedChat().GetAgentSessionId() }
func (m *Model) SyncTableHeight()                             { m.syncTableHeight() }
func (m *Model) Table() *ui.Table                             { return &m.table }
func setReservedTableHeight(t *ui.Table, h int) {
	t.SetHeight(h)
}
