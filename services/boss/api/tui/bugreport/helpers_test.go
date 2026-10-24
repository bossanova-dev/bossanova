package bugreport

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func lineIndexContaining(t *testing.T, lines []string, want string) int {
	t.Helper()
	for i, line := range lines {
		if strings.Contains(line, want) {
			return i
		}
	}
	t.Fatalf("no rendered line contains %q; lines:\n%s", want, strings.Join(lines, "\n"))
	return -1
}
func keyPress(ch rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: ch, Text: string(ch)}
}

var cronPromptNewline = tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModAlt}
