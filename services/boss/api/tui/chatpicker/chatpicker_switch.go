package chatpicker

import (
	"fmt"
	"time"

	"charm.land/bubbles/v2/table"
	"github.com/recurser/boss/api/tui/ui"
	"github.com/recurser/boss/internal/domain/display"

	// The chat picker's switch-account concern (BOS-171) — loading the rotation
	// accounts, building and driving the picker overlay, and the SwitchSessionAgentAccount
	// command plus its label helpers. Split out of chatpicker.go (BOS-527); the
	// declarations are unchanged.

	tea "charm.land/bubbletea/v2"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

// loadSwitchAccounts lists the registered rotation accounts for the given
// provider (the chat's agent name) off the update loop. Errors resolve to an
// empty list via switchAccountsLoadedMsg — the picker still opens with the
// unmanaged local credentials row.
func (m Model) loadSwitchAccounts(provider string) tea.Cmd {
	c := m.client
	ctx := m.ctx
	return func() tea.Msg {
		accounts, err := c.ListAgentAccounts(ctx, provider, false)
		return switchAccountsLoadedMsg{accounts: accounts, err: err}
	}
}

func shouldShowUnmanagedSwitchOption(accounts []*pb.AgentAccount, currentAccountID string) bool {
	return len(accounts) == 0 || currentAccountID == ""
}

func (m Model) currentSwitchAccountID() string {
	if m.session == nil {
		return ""
	}
	return m.session.GetAccountId()
}

// buildSwitchAccountTable populates m.switchAccountTable. Registered accounts
// are always shown; the unmanaged row is only shown when it is the fallback or
// current binding.
func (m *Model) buildSwitchAccountTable() {
	showUnmanaged := shouldShowUnmanagedSwitchOption(m.switchAccounts, m.currentSwitchAccountID())
	rowCount := len(m.switchAccounts)
	if showUnmanaged {
		rowCount++
	}
	labels := make([]string, rowCount)
	providers := make([]string, rowCount)
	statuses := make([]string, rowCount)
	healths := make([]string, rowCount)
	cooldowns := make([]string, rowCount)
	offset := 0
	if showUnmanaged {
		labels[0] = display.UnmanagedLocalCredentialsLabel
		statuses[0] = "system"
		offset = 1
	}
	now := time.Now()
	for i, a := range m.switchAccounts {
		row := i + offset
		labels[row] = ui.AgentAccountRowLabel(a)
		providers[row] = a.GetProvider()
		statuses[row] = a.GetStatus()
		healths[row] = ui.AgentAccountHealthLabel(a)
		cooldowns[row] = switchAccountCooldownLabel(a, now)
		if reason := switchAccountDisabledReason(a, now); reason != "" {
			if reason == "failed" {
				healths[row] = reason
			} else {
				statuses[row] = reason
			}
		}
	}

	rcols := []ui.ResponsiveColumn{
		{Col: ui.CursorColumn, Priority: 0, MinWidth: 1},
		{Col: table.Column{Title: "ACCOUNT", Width: display.MaxColWidth("ACCOUNT", labels, 30) + ui.TableColumnSep}, Priority: 0, MinWidth: 12},
		{Col: table.Column{Title: "PROVIDER", Width: display.MaxColWidth("PROVIDER", providers, 12) + ui.TableColumnSep}, Priority: 4, MinWidth: 4},
		{Col: table.Column{Title: "STATUS", Width: display.MaxColWidth("STATUS", statuses, 12) + ui.TableColumnSep}, Priority: 1, MinWidth: 4},
		{Col: table.Column{Title: "HEALTH", Width: display.MaxColWidth("HEALTH", healths, 10) + ui.TableColumnSep}, Priority: 2, MinWidth: 4},
		{Col: table.Column{Title: "COOLDOWN", Width: display.MaxColWidth("COOLDOWN", cooldowns, 14) + ui.TableColumnSep}, Priority: 3, MinWidth: 4},
	}
	fitted := ui.FitColumnsIndexed(rcols, m.chatPickerTableAvailWidth())
	cols := ui.FittedColumns(fitted)
	rows := make([]table.Row, len(labels))
	for i := range labels {
		indicator := ""
		if i == 0 {
			indicator = ui.CursorChevron
		}
		accountIndex := i - offset
		if accountIndex >= 0 && switchAccountDisabledReason(m.switchAccounts[accountIndex], now) != "" {
			rows[i] = ui.ProjectRow(fitted, table.Row{
				indicator,
				ui.StyleSubtle.Render(labels[i]),
				ui.StyleSubtle.Render(providers[i]),
				ui.StyleSubtle.Render(statuses[i]),
				ui.StyleSubtle.Render(healths[i]),
				ui.StyleSubtle.Render(cooldowns[i]),
			})
			continue
		}
		rows[i] = ui.ProjectRow(fitted, table.Row{
			indicator,
			labels[i],
			providers[i],
			ui.StyleSubtle.Render(statuses[i]),
			ui.StyleSubtle.Render(healths[i]),
			ui.StyleSubtle.Render(cooldowns[i]),
		})
	}

	m.switchAccountTable = ui.NewBossTable(cols, rows, len(rows)+1)
	m.switchAccountTable.SetCursor(0)
	m.switchAccountTable.SetWidth(ui.ColumnsWidth(cols))
}

// updateSwitchAccountSelect handles key input while the switch-account picker
// is showing. Esc cancels back to the chat list; Enter confirms the account,
// then either fires the RPC directly or — when the target chat is mid-turn
// (WORKING) — routes through the confirm dialog first (which forces the switch).
func (m Model) updateSwitchAccountSelect(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.pickingAccount = false
		return m, nil
	case "enter", " ", "space":
		idx := m.switchAccountTable.Cursor()
		showUnmanaged := shouldShowUnmanagedSwitchOption(m.switchAccounts, m.currentSwitchAccountID())
		if showUnmanaged && idx == 0 {
			m.switchSelectedAccount = ""
		} else {
			accountIndex := idx
			if showUnmanaged {
				accountIndex--
			}
			if accountIndex < 0 || accountIndex >= len(m.switchAccounts) {
				m.switchSelectedAccount = ""
			} else {
				account := m.switchAccounts[accountIndex]
				if reason := switchAccountDisabledReason(account, time.Now()); reason != "" {
					m.statusMsg = fmt.Sprintf("%s is %s — can't switch to it right now", ui.AgentAccountRowLabel(account), reason)
					return m, nil
				}
				m.switchSelectedAccount = account.GetId()
			}
		}
		m.pickingAccount = false
		// The TUI knows the chat's live status locally, so gate the interrupt on
		// a confirm dialog when the target chat is mid-turn rather than sending
		// Force=false and reacting to a FailedPrecondition round-trip.
		if m.daemonStatuses[m.switchTargetChatID] == ui.StatusWorking {
			m.confirm = confirmSwitch
			return m, nil
		}
		m.switching = true
		return m, m.switchAccountCmd(false)
	default:
		var cmd tea.Cmd
		m.switchAccountTable, cmd = m.switchAccountTable.Update(msg)
		ui.UpdateCursorColumn(&m.switchAccountTable)
		return m, cmd
	}
}

// switchAccountDisabledReason mirrors the web switch picker: disabled,
// health==failed, and cooling accounts are visible but not selectable.
func switchAccountDisabledReason(a *pb.AgentAccount, now time.Time) string {
	if a == nil {
		return ""
	}
	if a.GetStatus() == "disabled" {
		return "disabled"
	}
	if a.GetHealth() == "failed" {
		return "failed"
	}
	if ts := a.GetCooldownUntil(); ts != nil && ts.AsTime().After(now) {
		return "cooling"
	}
	return ""
}

func switchAccountCooldownLabel(a *pb.AgentAccount, now time.Time) string {
	if a == nil || a.GetCooldownUntil() == nil {
		return "—"
	}
	until := a.GetCooldownUntil().AsTime()
	if !until.After(now) {
		return "—"
	}
	return "cooling " + ui.CompactFutureDuration(until.Sub(now))
}

// switchAccountCmd runs SwitchSessionAgentAccount off the update loop. Captures
// everything by value so the model copy is safe under bubbletea.
func (m Model) switchAccountCmd(force bool) tea.Cmd {
	c := m.client
	ctx := m.ctx
	sessionID := m.sessionID
	accountID := m.switchSelectedAccount
	chatID := m.switchTargetChatID
	return func() tea.Msg {
		resp, err := c.SwitchSessionAgentAccount(ctx, &pb.SwitchSessionAgentAccountRequest{
			SessionId:      sessionID,
			AccountId:      accountID,
			AgentSessionId: proto.String(chatID),
			Force:          force,
		})
		return SwitchAgentAccountResultMsg{Resp: resp, Err: err}
	}
}
