package ui

import (
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// toastDuration is how long a rotation toast stays visible before it
// auto-dismisses. Long enough to read a one-line rotation notice, short enough
// that a burst of rotations doesn't leave a stale line lingering.
const toastDuration = 6 * time.Second

// toastExpireMsg fires toastDuration after a toast is shown. The id guards
// against a superseded toast's timer clearing a newer one.
type ToastExpireMsg struct{ ID int }

// toastModel is a single-slot, auto-dismissing, Esc-dismissible toast line.
// It is non-blocking: it only renders a line under the banner and never
// captures input except Esc while a toast is visible, so a fresh rotation
// notice can be dismissed without also triggering the active view's back
// action.
type ToastModel struct {
	Text string
	ID   int
}

func newToastModel() ToastModel { return ToastModel{} }

// Show replaces the visible toast with text and returns a command that fires a
// toastExpireMsg tagged with this toast's id after toastDuration. Bumping the
// id first means a rapid second Show supersedes the first: the earlier timer's
// id no longer matches, so it is ignored when it fires.
func (t ToastModel) Show(text string) (ToastModel, tea.Cmd) {
	t.Text = text
	t.ID++
	id := t.ID
	return t, tea.Tick(toastDuration, func(time.Time) tea.Msg { return ToastExpireMsg{ID: id} })
}

// Update processes a message for the toast. The bool return reports whether the
// key was consumed by the toast (true only for Esc while a toast is visible) so
// the caller can stop propagating that key to the active view.
func (t ToastModel) Update(msg tea.Msg) (ToastModel, bool) {
	switch m := msg.(type) {
	case ToastExpireMsg:
		if m.ID == t.ID {
			t.Text = ""
		}
		return t, false
	case tea.KeyPressMsg:
		if t.Text != "" && m.Code == tea.KeyEsc {
			t.Text = ""
			return t, true // consumed: caller must not route this Esc further
		}
	}
	return t, false
}

// View renders the toast line followed by a blank separator line, or "" when no
// toast is visible. The toast owns that blank line (rather than app.go adding
// it) so Height below stays honest by construction: whatever View emits, Height
// counts — including wrapped continuation lines (BOS-506).
func (t ToastModel) View(width int) string {
	if t.Text == "" {
		return ""
	}
	return styleToast.Width(width).Render(t.Text) + "\n"
}

// Height reports how many terminal lines View(width) occupies, so App.View can
// reserve them out of the height it hands the active view. 0 when hidden.
func (t ToastModel) Height(width int) int {
	if t.Text == "" {
		return 0
	}
	return lipgloss.Height(t.View(width))
}
