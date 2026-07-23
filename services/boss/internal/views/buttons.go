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
		color := colorMuted
		if button.primary {
			color = colorSelected
		}

		style := lipgloss.NewStyle().Padding(0, 1).Foreground(color)
		if i == focused {
			style = style.Background(color).Foreground(lipgloss.Color("#FFFFFF")).Bold(true)
		} else {
			style = style.Faint(true)
		}
		rendered = append(rendered, style.Render(button.label))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, rendered...)
}
