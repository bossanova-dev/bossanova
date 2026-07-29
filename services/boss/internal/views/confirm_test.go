package views

import (
	"testing"

	"charm.land/lipgloss/v2"
)

func TestConfirmPromptFooterWidth(t *testing.T) {
	prompt := newConfirmPrompt("Remove the account with a deliberately long email address?", nil)
	for _, width := range []int{40, 60, 72, 80, 100, 140} {
		t.Run("known width", func(t *testing.T) {
			if got := lipgloss.Width(prompt.footer(width)); got != width {
				t.Errorf("footer(%d) width = %d, want %d", width, got, width)
			}
		})
	}
	if got := lipgloss.Width(prompt.footer(0)); got == 0 {
		t.Error("footer(0) rendered no unconstrained content")
	}
}
