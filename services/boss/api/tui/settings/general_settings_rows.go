package settings

import (
	"fmt"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/api/tui/ui"
	"github.com/recurser/bossalib/kit/config"
	"github.com/recurser/bossalib/kit/telemetry"
)

// sectionKeyWidths measures each section independently; hidden tracing fields
// still contribute so toggling tracing never changes its value column.
func (m GeneralSettingsModel) sectionKeyWidths() []int {
	widths := make([]int, len(m.rows))
	for start := 0; start < len(m.rows); {
		end := start + 1
		for end < len(m.rows) && !m.rows[end].IsHeader {
			end++
		}
		var keys []ui.KeyValue
		if m.rows[start].Kind == settingsRowKindTracingHeader {
			keys = append(keys, ui.KeyValue{Key: "PostHog project token"}, ui.KeyValue{Key: "PostHog host"})
		}
		for _, row := range m.rows[start:end] {
			switch row.Kind { //nolint:exhaustive // only value rows contribute to the column
			case settingsRowKindString, settingsRowKindEnum, settingsRowKindWorktree,
				settingsRowKindPollInterval, settingsRowKindDefaultAgent, settingsRowKindPostHogToken,
				settingsRowKindPostHogHost, settingsRowKindDaemonName, settingsRowKindMCPToken:
				keys = append(keys, ui.KeyValue{Key: row.Label})
			}
		}
		width := ui.KeyColumnWidth(keys...)
		for i := start; i < end; i++ {
			widths[i] = width
		}
		start = end
	}
	return widths
}

// renderRow writes a single non-header row to b.
func (m GeneralSettingsModel) renderRow(b *strings.Builder, i int, row settingsRow, editing bool, keyWidth int) {
	focused := i == m.cursor && !editing

	// Editing branches show the input inline.
	if m.editingRow == i {
		switch row.Kind { //nolint:exhaustive // only edit-capable kinds need a branch
		case settingsRowKindWorktree:
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("  Worktree base directory:"))
			b.WriteString("\n")
			b.WriteString(lipgloss.NewStyle().Padding(0, 4).Render(m.worktreeDirInput.View()))
			b.WriteString("\n")
			return
		case settingsRowKindPollInterval:
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("  Poll interval (seconds):"))
			b.WriteString("\n")
			b.WriteString(lipgloss.NewStyle().Padding(0, 4).Render(m.pollIntervalInput.View()))
			b.WriteString("\n")
			return
		case settingsRowKindString:
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(fmt.Sprintf("  %s:", row.Label)))
			b.WriteString("\n")
			b.WriteString(lipgloss.NewStyle().Padding(0, 4).Render(m.stringInput.View()))
			b.WriteString("\n")
			return
		case settingsRowKindPostHogToken, settingsRowKindPostHogHost:
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(fmt.Sprintf("  %s:", row.Label)))
			b.WriteString("\n")
			b.WriteString(lipgloss.NewStyle().Padding(0, 4).Render(m.stringInput.View()))
			b.WriteString("\n")
			return
		case settingsRowKindDaemonName:
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(fmt.Sprintf("  %s:", row.Label)))
			b.WriteString("\n")
			b.WriteString(lipgloss.NewStyle().Padding(0, 4).Render(m.stringInput.View()))
			b.WriteString("\n")
			// Same helper-text chrome as the Settings hub's section descriptions
			// (settings.go): faint, indented 4, padded 2 on the right — so this
			// hint reads as secondary rather than competing with the value.
			b.WriteString(lipgloss.NewStyle().Padding(0, 2, 0, 4).Render(ui.StyleSubtle.Render(
				fmt.Sprintf("Blank uses this machine's hostname (%s); %s.", m.hostnameForDisplay(), daemonRestartHint))))
			b.WriteString("\n")
			return
		}
	}

	valueLine := func(value string) string {
		return ui.KeyValueLine(row.Label, value, keyWidth, m.width-ui.FieldRowGutterColumn-2)
	}
	var line string
	switch row.Kind { //nolint:exhaustive // header rows take an early return path in View
	case settingsRowKindMCPToken:
		value := "(loading)"
		if m.mcpToken != "" {
			value = m.mcpToken
			if !m.mcpTokenRevealed {
				suffix := value
				if len(suffix) > 4 {
					suffix = suffix[len(suffix)-4:]
				}
				value = "••••" + suffix
			}
		}
		line = valueLine(value)
	case settingsRowKindBool:
		line = ui.RenderCheckboxLabel(config.PluginConfigBool(&m.settings, row.Plugin, row.Key), row.Label)
	case settingsRowKindAgentEnabled:
		line = ui.RenderCheckboxLabel(pluginEnabled(m.settings, row.Plugin), row.Label)
	case settingsRowKindString:
		val := config.PluginConfigString(&m.settings, row.Plugin, row.Key)
		if val == "" {
			val = "(not set)"
		}
		line = valueLine(val)
	case settingsRowKindEnum:
		val := config.PluginConfigString(&m.settings, row.Plugin, row.Key)
		if val == "" && len(row.Allowed) > 0 {
			if row.Default != "" {
				val = row.Default + " (default)"
			} else {
				// When Allowed[0] is the empty string the plugin advertises ""
				// as the explicit "use plugin default" sentinel; render it as a
				// named reset state so the row reads cleanly.
				if row.Allowed[0] == "" {
					val = "use the CLI default"
				} else {
					val = row.Allowed[0] + " (default)"
				}
			}
		}
		line = valueLine(val)
	case settingsRowKindWorktree:
		line = valueLine(m.settings.WorktreeBaseDir)
	case settingsRowKindPollInterval:
		intervalStr := "30 (default)"
		if m.settings.PollIntervalSeconds > 0 {
			intervalStr = strconv.Itoa(m.settings.PollIntervalSeconds)
		}
		line = valueLine(intervalStr)
	case settingsRowKindEventTracing:
		line = ui.RenderCheckboxLabel(m.settings.EventTracingEnabled, row.Label)
	case settingsRowKindErrorTracking:
		// A checkbox rather than a "Label: ON" value row, because this row and
		// settingsRowKindEventTracing directly above are indistinguishable
		// toggles — both declared "toggle" at their enum, both flipping a plain
		// bool on the same keypress, both labelled "Enable …", and emitted as
		// adjacent lines. The remaining "%s: %s" arms below carry real values.
		line = ui.RenderCheckboxLabel(m.settings.ErrorTrackingEnabled, row.Label)
	case settingsRowKindRotation:
		line = ui.RenderCheckboxLabel(m.settings.ManagedAccounts.ManagedAgentAccountsEnabled(), row.Label)
	case settingsRowKindDisplayOpenPorts:
		line = ui.RenderCheckboxLabel(m.settings.DisplayOpenPorts, row.Label)
	case settingsRowKindNotifications:
		line = ui.RenderCheckboxLabel(config.NotificationsEnabled(m.settings), row.Label)
	case settingsRowKindPostHogToken:
		val := m.settings.PostHogProjectToken
		if val == "" {
			val = "(not set)"
		}
		line = valueLine(val)
	case settingsRowKindPostHogHost:
		val := m.settings.PostHogHost
		if val == "" {
			val = telemetry.DefaultHost
		}
		line = valueLine(val)
	case settingsRowKindDefaultAgent:
		val := m.settings.DefaultAgent
		if val == "" {
			val = "(unset)"
		}
		line = valueLine(val)
	case settingsRowKindDaemonName:
		// Resolve through config.DaemonDisplayName against the DERIVED machine
		// default m.hostname already holds — never a local re-implementation and
		// never a placeholder — so the value shown is exactly what bossd will
		// advertise after its next start. bossd composes the same two helpers in
		// the same order (services/bossd/cmd/daemon_identity.go).
		switch resolved := config.DaemonDisplayName(m.settings, m.hostname); {
		case strings.TrimSpace(m.settings.DaemonName) != "":
			line = valueLine(fmt.Sprintf("%s (%s)", resolved, daemonRestartHint))
		case strings.TrimSpace(resolved) == "":
			// os.Hostname() failed, the derivation found nothing to offer in its
			// place, and there is no override — so bossd would advertise an
			// empty hostname, which bosso rejects at registration.
			// Rendering a placeholder here would read as a name; this is a
			// prompt to set one.
			line = valueLine(fmt.Sprintf("(machine hostname unavailable — set a name; %s)", daemonRestartHint))
		default:
			line = valueLine(fmt.Sprintf("%s (machine hostname; %s)", resolved, daemonRestartHint))
		}
	}

	b.WriteString(ui.RenderFieldRow(focused, line))
	b.WriteString("\n")
}
