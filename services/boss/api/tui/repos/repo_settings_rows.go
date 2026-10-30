package repos

import "github.com/recurser/boss/api/tui/ui"

// renderIntegrationHeader renders an integration checkbox header row through
// the shared renderCheckboxLabel, so it wears the same checkbox glyph as the
// automation toggles above it. The checkbox reflects expansion state, not a
// persisted enabled flag.
func (m RepoSettingsModel) renderIntegrationHeader(label string, expanded bool, row, cur rowID) string {
	focused := cur == row && m.editingField == repoSettingsRowNone
	return ui.RenderFieldRow(focused, ui.RenderCheckboxLabel(expanded, label)) + "\n"
}

// repoSettingsChildIndent is how many columns an integration's child field rows
// sit right of their header row, marking them as nested under it. It is the
// indent the chevron idiom baked into its own cursor strings before BOS-567.
const repoSettingsChildIndent = 2

// renderChildField renders an indented integration child field row. When missing
// is true (partial-config validation), the value is shown in red.
func (m RepoSettingsModel) renderChildField(label, value string, row, cur rowID, missing bool, keyWidth, maxWidth int) string {
	focused := cur == row && m.editingField == repoSettingsRowNone
	if missing {
		value = ui.StyleStatusDanger.Render(value)
	}
	return ui.RenderIndentedFieldRow(focused, repoSettingsChildIndent, ui.KeyValueLine(label, value, keyWidth, maxWidth)) + "\n"
}
