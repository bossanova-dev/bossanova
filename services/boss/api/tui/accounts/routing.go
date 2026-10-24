package accounts

import (
	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/api/tui/ui"
)

func (m *AgentAccountsListModel) SetSize(w, h int)                { m.width, m.height = w, h }
func (m AgentAccountsListModel) Width() int                       { return m.width }
func (m AgentAccountsListModel) Height() int                      { return m.height }
func (m *AgentAccountsListModel) SetHeight(h int)                 { m.height = h }
func (m *AgentAccountsListModel) SetReturnView(v ui.View)         { m.returnView = v }
func (m AgentAccountsListModel) ReturnView() ui.View              { return m.returnView }
func (m AgentAccountsListModel) HasTelemetry() bool               { return m.telemetry != nil }
func (m *AgentAccountEditModel) SetSize(w, h int)                 { m.width, m.height = w, h }
func (m AgentAccountEditModel) Width() int                        { return m.width }
func (m AgentAccountEditModel) Height() int                       { return m.height }
func (m *AgentAccountEditModel) SetHeight(h int)                  { m.height = h }
func (m *AgentAccountEditModel) SetReturnView(v ui.View)          { m.returnView = v }
func (m AgentAccountEditModel) ReturnView() ui.View               { return m.returnView }
func (m AgentAccountEditModel) HasTelemetry() bool                { return m.telemetry != nil }
func (m *AgentAccountRegisterModel) SetSize(w, h int)             { m.width, m.height = w, h }
func (m AgentAccountRegisterModel) Width() int                    { return m.width }
func (m AgentAccountRegisterModel) Height() int                   { return m.height }
func (m *AgentAccountRegisterModel) SetHeight(h int)              { m.height = h }
func (m *AgentAccountRegisterModel) SetReturnView(v ui.View)      { m.returnView = v }
func (m AgentAccountRegisterModel) ReturnView() ui.View           { return m.returnView }
func (m AgentAccountRegisterModel) HasTelemetry() bool            { return m.telemetry != nil }
func (m *AgentAccountsListModel) Table() *table.Model             { return &m.table }
func (m AgentAccountsListModel) TableHeight() int                 { return m.tableHeight() }
func (m *AgentAccountsListModel) SetStatus(s string, failed bool) { m.setStatus(s, failed) }
func (m AgentAccountsListModel) Status() string                   { return m.status }
func (m AgentAccountsListModel) StatusError() bool                { return m.statusErr }
func (m AgentAccountsListModel) IsRefreshing() bool               { return m.refreshing }
func (m AgentAccountsListModel) IsDisabling(id string) bool       { return m.disabling[id] }
func (m AgentAccountsListModel) IsRemoving(id string) bool        { return m.removing[id] }
func (m AgentAccountEditModel) Error() error                      { return m.err }
func (m AgentAccountEditModel) TextEntryActive() bool             { return m.textEntryActive() }
func (m AgentAccountRegisterModel) TextEntryActive() bool         { return m.textEntryActive() }
func (m AgentAccountRegisterModel) FlowRunning() bool             { return m.flowRunning() }
func (m AgentAccountRegisterModel) ReauthAgentAccountID() string  { return m.reauthAccountID }
func (m *AgentAccountRegisterModel) ClearReauthAgentAccountID()   { m.reauthAccountID = "" }
func (m AgentAccountRegisterModel) FlowVerdictLine() string       { return m.flowVerdictLine() }
func (m AgentAccountRegisterModel) BeginReauth(id string) (AgentAccountRegisterModel, tea.Cmd) {
	return m.beginReauth(id)
}

func (m AgentAccountsListModel) ConfirmActive() bool                   { return m.confirm.Active }
func (m *AgentAccountRegisterModel) SetReauthAgentAccountID(id string) { m.reauthAccountID = id }
func (m *AgentAccountRegisterModel) SetPrompter(p *TUIPrompter)        { m.prompter = p }
