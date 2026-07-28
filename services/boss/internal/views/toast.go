package views

import (
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// toastDuration is how long a rotation toast stays visible before it
// auto-dismisses. Long enough to read a one-line rotation notice, short enough
// that a burst of rotations doesn't leave a stale line lingering.
const toastDuration = 6 * time.Second

// toastExpireMsg fires toastDuration after a toast is shown. The id guards
// against a superseded toast's timer clearing a newer one.
type toastExpireMsg struct{ id int }

// toastModel is a single-slot, auto-dismissing, Esc-dismissible toast line.
// It is non-blocking: it only renders a line under the banner and never
// captures input except Esc while a toast is visible, so a fresh rotation
// notice can be dismissed without also triggering the active view's back
// action.
type toastModel struct {
	text string
	id   int
}

func newToastModel() toastModel { return toastModel{} }

// Show replaces the visible toast with text and returns a command that fires a
// toastExpireMsg tagged with this toast's id after toastDuration. Bumping the
// id first means a rapid second Show supersedes the first: the earlier timer's
// id no longer matches, so it is ignored when it fires.
func (t toastModel) Show(text string) (toastModel, tea.Cmd) {
	t.text = text
	t.id++
	id := t.id
	return t, tea.Tick(toastDuration, func(time.Time) tea.Msg { return toastExpireMsg{id: id} })
}

// Update processes a message for the toast. The bool return reports whether the
// key was consumed by the toast (true only for Esc while a toast is visible) so
// the caller can stop propagating that key to the active view.
func (t toastModel) Update(msg tea.Msg) (toastModel, bool) {
	switch m := msg.(type) {
	case toastExpireMsg:
		if m.id == t.id {
			t.text = ""
		}
		return t, false
	case tea.KeyPressMsg:
		if t.text != "" && m.Code == tea.KeyEsc {
			t.text = ""
			return t, true // consumed: caller must not route this Esc further
		}
	}
	return t, false
}

// View renders the toast line followed by a blank separator line, or "" when no
// toast is visible. The toast owns that blank line (rather than app.go adding
// it) so Height below stays honest by construction: whatever View emits, Height
// counts — including wrapped continuation lines (BOS-506).
func (t toastModel) View(width int) string {
	if t.text == "" {
		return ""
	}
	return styleToast.Width(width).Render(t.text) + "\n"
}

// Height reports how many terminal lines View(width) occupies, so App.View can
// reserve them out of the height it hands the active view. 0 when hidden.
func (t toastModel) Height(width int) int {
	if t.text == "" {
		return 0
	}
	return lipgloss.Height(t.View(width))
}

// detectNewRotationEvents diffs the newest rotation-event id per session
// against prev, returning the refreshed map and a toast string for every
// session whose newest rotation-event id changed. The first observation
// (prev == nil) seeds the map without emitting any toasts, so a TUI restart
// does not replay historical rotations.
func detectNewRotationEvents(prev map[string]string, sessions []*pb.Session) (map[string]string, []string) {
	next := make(map[string]string, len(sessions))
	var toasts []string
	for _, sess := range sessions {
		if sess == nil {
			continue
		}
		evs := sess.GetRotationEvents()
		if len(evs) == 0 {
			continue
		}
		latest := evs[0] // hydrated newest-first
		next[sess.GetId()] = latest.GetId()
		if prev == nil {
			continue // seed pass: record state without toasting
		}
		if prev[sess.GetId()] != latest.GetId() {
			// A generic (UNSPECIFIED) event has no outcome label (BOS-432), so
			// fall back to its detail — and a bare "rotation event" only if
			// neither is present — so the toast is never a bare "<title>: ".
			summary := rotationEventLabel(latest)
			if summary == "" {
				summary = rotationEventDetail(latest)
			}
			if summary == "" {
				summary = "rotation event"
			}
			toasts = append(toasts, sess.GetTitle()+": "+summary)
		}
	}
	return next, toasts
}
