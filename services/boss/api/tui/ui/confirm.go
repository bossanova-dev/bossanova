package ui

import (
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// confirmPrompt is a value-safe one-shot y/n confirmation. It holds no pointer
// into any model: action is a self-contained command (it captures client,
// ctx, ids by value and returns a tea.Msg). Safe to copy with the model.
type ConfirmPrompt struct {
	Active bool
	Prompt string
	Action func() tea.Msg
}

// newConfirmPrompt builds an active prompt for the given action.
func NewConfirmPrompt(prompt string, action func() tea.Msg) ConfirmPrompt {
	return ConfirmPrompt{Active: true, Prompt: prompt, Action: action}
}

// update applies a key to the prompt. Returns the possibly cleared prompt and
// a command to run (the action on confirm, nil otherwise).
func (c ConfirmPrompt) Update(msg tea.KeyMsg) (ConfirmPrompt, tea.Cmd) {
	switch msg.String() {
	case "y", "enter":
		action := c.Action
		c = ConfirmPrompt{}
		if action != nil {
			return c, func() tea.Msg { return action() }
		}
	case "n", "esc":
		c = ConfirmPrompt{}
	}
	return c, nil
}

// footer renders the active danger-styled prompt plus the standard
// confirm/cancel action bar. The prompt is width-wrapped so long
// multi-sentence copy (e.g. the bound-account disable/remove warnings) wraps
// cleanly with the left padding preserved instead of hard-wrapping past the
// terminal edge. width == 0 (terminal size not yet known) falls back to no
// width constraint. Callers pass their model's width.
func (c ConfirmPrompt) Footer(width int) string {
	style := lipgloss.NewStyle().Padding(0, 2).Foreground(ColorDanger)
	if width > 0 {
		style = style.Width(width)
	}
	return style.Render(c.Prompt) + "\n" + StyleActionBar.Render("[y/enter] confirm  [n/esc] cancel")
}
