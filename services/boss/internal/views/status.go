package views

import (
	"charm.land/bubbles/v2/spinner"
	"charm.land/lipgloss/v2"
	bosspty "github.com/recurser/boss/internal/pty"
)

// newStatusSpinner creates a spinner configured for status display.
func newStatusSpinner() spinner.Model {
	s := spinner.New(spinner.WithSpinner(spinner.Dot))
	s.Style = lipgloss.NewStyle().Foreground(colorGreen)
	return s
}

// renderStatus returns a styled status string.
// When status is "working", the spinner view is prepended.
func renderStatus(status string, sp spinner.Model) string {
	switch status {
	case bosspty.StatusWorking:
		return sp.View() + lipgloss.NewStyle().Foreground(colorGreen).Render("working")
	case bosspty.StatusIdle:
		return lipgloss.NewStyle().Foreground(colorYellow).Render("idle")
	default: // StatusStopped
		return lipgloss.NewStyle().Foreground(colorGray).Render("stopped")
	}
}
