package home

import (
	tea "charm.land/bubbletea/v2"
)

func keyPress(ch rune) tea.KeyPressMsg          { return tea.KeyPressMsg{Code: ch, Text: string(ch)} }
func specialKeyPress(code rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: code} }
func pasteText(text string) tea.PasteMsg        { return tea.PasteMsg{Content: text} }
func keyString(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEsc}
	case "/":
		return tea.KeyPressMsg{Code: '/'}
	default:
		if len(s) == 1 {
			return tea.KeyPressMsg{Code: []rune(s)[0]}
		}
		panic("unsupported key: " + s)
	}
}

const renderedHead = "0123456789abcdef0123456789abcdef01234567"

var knownParkCodes = []string{
	"always-human-path", "human-review:database", "human-review:security", "human-review:ui",
	"no-receipt", "repair-exhausted", "ledger-open", "extension-failed", "no-session",
	"worktree-diverged", "worktree-unsynced", "human-review:unproven",
}
