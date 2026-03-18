package views

import (
	"charm.land/bubbles/v2/spinner"
	"charm.land/lipgloss/v2"
	bosspty "github.com/recurser/boss/internal/pty"
)

// newStatusSpinner creates an unstyled spinner for status display.
// Color is applied by renderStatus so the entire cell has a single ANSI wrap.
func newStatusSpinner() spinner.Model {
	return spinner.New(spinner.WithSpinner(spinner.Dot))
}

// renderStatus returns a styled status string.
// The spinner + label are wrapped in a single Render call to avoid
// intermediate ANSI resets that interfere with the table's Selected style.
func renderStatus(status string, sp spinner.Model) string {
	switch status {
	case bosspty.StatusWorking:
		return lipgloss.NewStyle().Foreground(colorSuccess).Render(sp.View() + "working")
	case bosspty.StatusIdle:
		return lipgloss.NewStyle().Foreground(colorWarning).Render("idle")
	default: // StatusStopped
		return lipgloss.NewStyle().Foreground(colorMuted).Render("stopped")
	}
}
