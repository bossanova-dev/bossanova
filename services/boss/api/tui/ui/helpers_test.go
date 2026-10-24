package ui

import (
	"regexp"

	tea "charm.land/bubbletea/v2"
)

func stripANSI(s string) string {
	re := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	return re.ReplaceAllString(s, "")
}
func keyPress(ch rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: ch, Text: string(ch)} }
