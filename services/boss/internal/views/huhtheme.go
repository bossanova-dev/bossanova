package views

import (
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
)

// huhButtonChip is a huh Confirm button: the shared buttonStyle colour rule
// wearing huh's own chip geometry. The geometry is applied here rather than
// inherited from ThemeBase because ThemeBase has already set a foreground and
// background on its button style, and lipgloss's Inherit will not overwrite
// values the receiver has set.
func huhButtonChip(focused, primary bool) lipgloss.Style {
	return buttonStyle(focused, primary).
		Padding(0, buttonChipPaddingX).
		MarginRight(buttonChipMarginRight)
}

// bossHuhTheme returns a huh theme derived from ThemeBase that matches the
// boss TUI colour palette defined in theme.go.
func bossHuhTheme() huh.Theme {
	return huh.ThemeFunc(func(isDark bool) *huh.Styles {
		t := huh.ThemeBase(isDark)

		// The left border is the focus indicator: the focused field paints a
		// thick gutter in colorSelected, and the blurred field reserves the
		// same column with a hidden (blank) border. Both must keep the same
		// width or every field jitters horizontally as focus moves. The chrome
		// itself lives in fieldGutter (formfields.go) so the hand-rolled
		// settings row editors share this one definition rather than a
		// look-alike.
		t.Focused.Base = fieldGutter(true)
		t.Blurred.Base = fieldGutter(false)
		// ThemeBase copied Card from its own Base before we reassigned Base, so
		// without this a huh.NewNote() would keep ThemeBase's border: it tracks
		// focus, but uncoloured, so a note's gutter would silently disagree
		// with every other field's.
		t.Focused.Card = t.Focused.Base
		t.Blurred.Card = t.Blurred.Base

		// Selector → chevron matching table cursor, colorSelected (blue).
		t.Focused.SelectSelector = t.Focused.SelectSelector.SetString(cursorChevron + " ").Foreground(colorSelected)
		t.Focused.NextIndicator = t.Focused.NextIndicator.Foreground(colorSelected)
		t.Focused.PrevIndicator = t.Focused.PrevIndicator.Foreground(colorSelected)
		// MultiSelect option prefixes → the shared checkbox glyphs
		// (formfields.go), so huh's one multi-select in the TUI — the add-repo
		// Automation field — cannot render a different checkbox from the
		// hand-rolled settings rows. ThemeBase renders the selected prefix as
		// "[•] ", which was BOS-726's bug.
		//
		// Both focus states are assigned for the same reason Card is above:
		// ThemeBase does `t.Blurred = t.Focused` at the end of its own
		// construction, i.e. before this func is handed the value, so setting
		// only Focused leaves a blurred multi-select still showing "[•]" —
		// invisible in a focused screenshot. The prefixes stay unstyled because
		// ThemeBase colours neither and no boss screen colours a checkbox
		// glyph; SetString on a fresh style rather than on ThemeBase's is the
		// same restate-don't-inherit rule huhButtonChip follows.
		//
		// checkboxPrefix, not checkboxChecked + " ": huh joins this style's
		// String() straight onto the option label, so the trailing space is
		// part of the prefix here and part of renderCheckboxLabel there. Taking
		// both from one function is what makes the two idioms share all four
		// columns rather than only the three bracket characters.
		t.Focused.SelectedPrefix = lipgloss.NewStyle().SetString(checkboxPrefix(true))
		t.Focused.UnselectedPrefix = lipgloss.NewStyle().SetString(checkboxPrefix(false))
		t.Blurred.SelectedPrefix = t.Focused.SelectedPrefix
		t.Blurred.UnselectedPrefix = t.Focused.UnselectedPrefix

		// Buttons: colours from the shared buttonStyle (formfields.go), so
		// huh's Confirm chips and the hand-rolled renderButtonRow rows cannot
		// drift apart; huh's own chip geometry is restated alongside it.
		t.Focused.FocusedButton = huhButtonChip(true, true)
		t.Focused.BlurredButton = huhButtonChip(true, false)

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

		// Blurred mirrors focused but dimmed where applicable. The select
		// chevron is dimmed rather than removed — removing it would shift the
		// option text sideways when focus arrives.
		t.Blurred.SelectSelector = t.Focused.SelectSelector.Foreground(colorMuted)
		t.Blurred.SelectedOption = t.Focused.SelectedOption.Faint(true)
		// The focused description is already faint (above), so this is
		// belt-and-braces rather than a behaviour change: it keeps the blurred
		// description dimmed even if the focused one ever stops being.
		t.Blurred.Description = t.Focused.Description.Faint(true)
		t.Blurred.Title = t.Focused.Title.Faint(true)
		t.Blurred.ErrorMessage = t.Focused.ErrorMessage
		t.Blurred.ErrorIndicator = t.Focused.ErrorIndicator
		// A blurred Confirm still has to show which option is currently
		// selected — cron_form.go's "Enabled" / "Run setup command" toggles are
		// read at a glance while focus sits elsewhere. So the two blurred button
		// styles must stay distinguishable from each other: the selected one
		// keeps a filled chip (muted rather than colorSelected, so it no longer
		// out-weighs the focused field) while the unselected one drops its fill
		// entirely. Padding is untouched, so both keep the focused widths.
		t.Blurred.FocusedButton = huhButtonChip(true, false)
		t.Blurred.BlurredButton = huhButtonChip(false, false)
		t.Blurred.TextInput = t.Focused.TextInput
		t.Blurred.NextIndicator = lipgloss.NewStyle()
		t.Blurred.PrevIndicator = lipgloss.NewStyle()

		t.Group.Title = t.Focused.Title
		t.Group.Description = t.Focused.Description

		return t
	})
}
