package views

import (
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// confirmPrompt is a value-safe one-shot y/n confirmation. It holds no pointer
// into any model: action is a self-contained command (it captures client,
// ctx, ids by value and returns a tea.Msg). Safe to copy with the model.
type confirmPrompt struct {
	active bool
	prompt string
	action func() tea.Msg
}

// newConfirmPrompt builds an active prompt for the given action.
func newConfirmPrompt(prompt string, action func() tea.Msg) confirmPrompt {
	return confirmPrompt{active: true, prompt: prompt, action: action}
}

// update applies a key to the prompt. Returns the possibly cleared prompt and
// a command to run (the action on confirm, nil otherwise).
func (c confirmPrompt) update(msg tea.KeyMsg) (confirmPrompt, tea.Cmd) {
	switch msg.String() {
	case "y", "enter":
		action := c.action
		c = confirmPrompt{}
		if action != nil {
			return c, func() tea.Msg { return action() }
		}
	case "n", "esc":
		c = confirmPrompt{}
	}
	return c, nil
}

// footer renders the active danger-styled prompt plus the standard
// confirm/cancel action bar. The prompt is width-wrapped so long
// multi-sentence copy (e.g. the bound-account disable/remove warnings) wraps
// cleanly with the left padding preserved instead of hard-wrapping past the
// terminal edge. width == 0 (terminal size not yet known) falls back to no
// width constraint. Callers pass their model's width.
func (c confirmPrompt) footer(width int) string {
	style := lipgloss.NewStyle().Padding(0, 2).Foreground(colorDanger)
	if width > 0 {
		// KNOWN BUG, do NOT copy: .Width() already includes the padding, so
		// subtracting it renders this prompt 4 columns narrower than the
		// terminal. BOS-531 removed the identical mistake from renderError but
		// left this untested path alone; widen it under its own change.
		style = style.Width(width - 4)
	}
	return style.Render(c.prompt) + "\n" + styleActionBar.Render("[y/enter] confirm  [n/esc] cancel")
}
