package views

import tea "charm.land/bubbletea/v2"

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
