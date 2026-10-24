package cron

import (
	"testing"

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func assertTableRowsMatchColumns(t *testing.T, cols []table.Column, rows []table.Row) {
	t.Helper()
	for i, row := range rows {
		if len(row) != len(cols) {
			t.Fatalf("row %d has %d cells for %d columns", i, len(row), len(cols))
		}
	}
}
func columnTitles(cols []table.Column) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Title
	}
	return out
}
func stripANSI(s string) string { return ansi.Strip(s) }

var focusGutterGlyph = []rune(lipgloss.ThickBorder().Left)[0]

func runCmd(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	return cmd()
}
func keyPress(ch rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: ch, Text: string(ch)}
}
