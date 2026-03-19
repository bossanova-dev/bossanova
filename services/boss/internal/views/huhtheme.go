package views

import (
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
)

// bossHuhTheme returns a huh theme derived from ThemeBase that matches the
// boss TUI colour palette defined in theme.go.
func bossHuhTheme() huh.Theme {
	return huh.ThemeFunc(func(isDark bool) *huh.Styles {
		t := huh.ThemeBase(isDark)

		// Remove the left border that ThemeBase adds — we don't use it.
		t.Focused.Base = lipgloss.NewStyle().PaddingLeft(1)
		t.Blurred.Base = lipgloss.NewStyle().PaddingLeft(1)

		// Selector → chevron matching table cursor, colorSelected (blue).
		t.Focused.SelectSelector = t.Focused.SelectSelector.SetString(cursorChevron + " ").Foreground(colorSelected)
		t.Focused.NextIndicator = t.Focused.NextIndicator.Foreground(colorSelected)
		t.Focused.PrevIndicator = t.Focused.PrevIndicator.Foreground(colorSelected)
		t.Focused.FocusedButton = t.Focused.FocusedButton.
			Foreground(lipgloss.Color("#FFFFFF")).Background(colorSelected)
		t.Focused.BlurredButton = t.Focused.BlurredButton.
			Foreground(lipgloss.Color("#FFFFFF")).Background(colorMuted)

		// Selected option → bold.
		t.Focused.SelectedOption = t.Focused.SelectedOption.Bold(true)

		// Description → faint (matches styleSubtle).
		t.Focused.Description = t.Focused.Description.Faint(true)

		// Title → bold.
		t.Focused.Title = t.Focused.Title.Bold(true)

		// Errors → colorDanger.
		t.Focused.ErrorMessage = t.Focused.ErrorMessage.Foreground(colorDanger)
		t.Focused.ErrorIndicator = t.Focused.ErrorIndicator.Foreground(colorDanger)

		// Cursor → colorSelected.
		t.Focused.TextInput.Cursor = t.Focused.TextInput.Cursor.Foreground(colorSelected)

		// Blurred mirrors focused but dimmed where applicable.
		t.Blurred.SelectSelector = t.Focused.SelectSelector
		t.Blurred.SelectedOption = t.Focused.SelectedOption.Faint(true)
		t.Blurred.Description = t.Focused.Description
		t.Blurred.Title = t.Focused.Title.Faint(true)
		t.Blurred.ErrorMessage = t.Focused.ErrorMessage
		t.Blurred.ErrorIndicator = t.Focused.ErrorIndicator
		t.Blurred.FocusedButton = t.Focused.FocusedButton
		t.Blurred.BlurredButton = t.Focused.BlurredButton
		t.Blurred.TextInput = t.Focused.TextInput
		t.Blurred.NextIndicator = lipgloss.NewStyle()
		t.Blurred.PrevIndicator = lipgloss.NewStyle()

		t.Group.Title = t.Focused.Title
		t.Group.Description = t.Focused.Description

		return t
	})
}
