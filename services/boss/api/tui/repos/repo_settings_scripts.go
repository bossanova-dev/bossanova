package repos

import (
	"strings"

	"charm.land/bubbles/v2/textinput"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/api/tui/ui"
)

func (m RepoSettingsModel) renderScriptRows(cur rowID, keyWidth, rowWidth int) string {
	var b strings.Builder
	for _, field := range []struct {
		row   rowID
		label string
		value string
		input textinput.Model
	}{
		{repoSettingsRowSetupScript, "Setup command", m.repo.GetSetupScript(), m.setupInput},
		{repoSettingsRowServeScript, "Serve command", m.repo.GetServeScript(), m.serveInput},
	} {
		if m.editingField == field.row {
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("  " + field.label + ":"))
			b.WriteString("\n")
			b.WriteString(lipgloss.NewStyle().Padding(0, 4).Render(field.input.View()))
			b.WriteString("\n")
			continue
		}
		value := field.value
		if value == "" {
			value = "(none)"
		}
		b.WriteString(ui.RenderFieldRow(cur == field.row, ui.KeyValueLine(field.label, value, keyWidth, rowWidth)))
		b.WriteString("\n")
	}
	return b.String()
}
