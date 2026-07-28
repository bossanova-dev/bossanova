package views

import "charm.land/lipgloss/v2"

type button struct {
	label   string
	primary bool
}

// renderButtonRow renders a single-line selectable button row. An invalid focus
// index leaves every button unfocused.
func renderButtonRow(buttons []button, focused int) string {
	rendered := make([]string, 0, len(buttons))
	for i, button := range buttons {
		// Colours come from the shared buttonStyle (formfields.go); the tighter
		// one-column chip and the bold/faint emphasis are this row's own.
		style := buttonStyle(i == focused, button.primary).Padding(0, 1)
		if i == focused {
			style = style.Bold(true)
		} else {
			style = style.Faint(true)
		}
		rendered = append(rendered, style.Render(button.label))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, rendered...)
}
