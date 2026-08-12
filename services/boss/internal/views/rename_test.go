package views

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func TestNewRenamePromptPrefillsTheCurrentTitle(t *testing.T) {
	r, cmd := newRenamePrompt("sess-1", "Add dark mode")

	if !r.Active() {
		t.Fatal("newRenamePrompt returned an inactive prompt")
	}
	if got := r.SessionID(); got != "sess-1" {
		t.Fatalf("SessionID() = %q, want sess-1", got)
	}
	if got := r.Value(); got != "Add dark mode" {
		t.Fatalf("Value() = %q, want the current title", got)
	}
	if cmd == nil {
		t.Fatal("newRenamePrompt returned no focus command")
	}
	// Focus mutates the input, so it has to happen before the struct is copied
	// into the return value. An unfocused textinput drops every key it is sent,
	// which would leave the prompt on screen and completely inert.
	if !r.input.Focused() {
		t.Fatal("the returned prompt's input is not focused; typing would be silently discarded")
	}
	// Pre-filling is only useful if the caret lands after the text: a caret at
	// column 0 turns "append a word" into "prepend a word".
	if got, want := r.input.Position(), len("Add dark mode"); got != want {
		t.Fatalf("caret at %d, want %d (end of the pre-filled title)", got, want)
	}
}

// TestRenamePromptTypesKeysWithNoText pins the Bubble Tea v2 quirk that
// renamePrompt.Update exists to absorb: textinput inserts Key.Text, so a key
// press carrying only a printable Code types nothing at all. The prompt routes
// messages through normalizePrintableKey (filter.go) to fill that in.
//
// Falsification: delete the normalizePrintableKey call in renamePrompt.Update
// (forward msg straight to r.input.Update) and this test fails — the value stays
// "Ad" because the synthesized 'd' press inserts nothing.
func TestRenamePromptTypesKeysWithNoText(t *testing.T) {
	r, _ := newRenamePrompt("sess-1", "Ad")

	r, _ = r.Update(tea.KeyPressMsg{Code: 'd'})

	if got := r.Value(); got != "Add" {
		t.Fatalf("Value() = %q, want Add; a Code-only key press must still type", got)
	}
}

// TestRenamePromptBoundsAnOversizedPaste covers the height side of the editor.
// tableHeight reserves renamePromptLines for the footer and the status line
// echoes the accepted title back, so an unbounded paste — a URL, a whole log
// line, a clipboard full of prose — would wrap to arbitrarily many rows and
// squeeze the board off the screen. The limit is the only thing standing
// between a stray paste and an unusable home view.
//
// Falsification: remove `input.CharLimit = renameCharLimit` from
// newRenamePrompt and this fails with the full 500 characters retained.
// Performed once and the assignment restored.
func TestRenamePromptBoundsAnOversizedPaste(t *testing.T) {
	r, _ := newRenamePrompt("sess-1", "")

	r, _ = r.Update(tea.PasteMsg{Content: strings.Repeat("x", 500)})

	if got := len(r.Value()); got != renameCharLimit {
		t.Fatalf("pasted title kept %d characters, want it clamped to renameCharLimit (%d)", got, renameCharLimit)
	}
}

func TestRenamePromptAcceptsPastedText(t *testing.T) {
	r, _ := newRenamePrompt("sess-1", "")

	r, _ = r.Update(tea.PasteMsg{Content: "Pasted title"})

	if got := r.Value(); got != "Pasted title" {
		t.Fatalf("Value() = %q, want the pasted text", got)
	}
}

func TestRenamePromptValueTrimsSurroundingSpace(t *testing.T) {
	r, _ := newRenamePrompt("sess-1", "   spaced out  ")

	if got := r.Value(); got != "spaced out" {
		t.Fatalf("Value() = %q, want the trimmed title", got)
	}
}

func TestRenamePromptValueIsEmptyForWhitespaceOnly(t *testing.T) {
	r, _ := newRenamePrompt("sess-1", "   \t ")

	if got := r.Value(); got != "" {
		t.Fatalf("Value() = %q, want empty so the commit path can refuse it", got)
	}
}

func TestRenamePromptUpdateClearsAStaleError(t *testing.T) {
	r, _ := newRenamePrompt("sess-1", "")
	r = r.withEmptyTitleError()

	r, _ = r.Update(tea.KeyPressMsg{Code: 'a'})

	if strings.Contains(r.footer(80), "Title cannot be empty") {
		t.Fatal("the complaint survived the next keystroke; it must clear as soon as the operator types")
	}
}

func TestRenamePromptFooterAdvertisesItsOwnKeysOnly(t *testing.T) {
	r, _ := newRenamePrompt("sess-1", "Add dark mode")
	footer := r.footer(80)

	for _, want := range []string{"Rename:", "Add dark mode", "[enter] rename", "[esc] cancel"} {
		if !strings.Contains(footer, want) {
			t.Fatalf("footer is missing %q:\n%s", want, footer)
		}
	}
	// The shortcut is hidden by design (BOS-837): the editor must not advertise
	// the key that opened it, or it stops being hidden the moment it is used.
	if strings.Contains(footer, "[r]") {
		t.Fatalf("footer advertises the hidden shortcut:\n%s", footer)
	}
}

// TestRenamePromptFooterHeightNeverGrows guards the reservation contract:
// tableHeight reserves lineCount() content rows for this footer and nothing
// more, so neither a long title nor the empty-title complaint nor a narrow
// terminal may cost an extra rendered line. Everything the footer draws is
// clamped rather than wrapped for exactly this reason.
func TestRenamePromptFooterHeightNeverGrows(t *testing.T) {
	baseline, _ := newRenamePrompt("sess-1", "Add dark mode")
	want := lipgloss.Height(baseline.footer(80))

	for _, tc := range []struct {
		name  string
		width int
		build func() renamePrompt
	}{
		{"with an error", 80, func() renamePrompt {
			r, _ := newRenamePrompt("sess-1", "")
			return r.withEmptyTitleError()
		}},
		{"title longer than the input", 80, func() renamePrompt {
			r, _ := newRenamePrompt("sess-1", strings.Repeat("long title ", 20))
			return r
		}},
		{"narrow terminal", 30, func() renamePrompt {
			r, _ := newRenamePrompt("sess-1", strings.Repeat("long title ", 20))
			return r.withEmptyTitleError()
		}},
		{"unknown terminal width", 0, func() renamePrompt {
			r, _ := newRenamePrompt("sess-1", "Add dark mode")
			return r
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			footer := tc.build().footer(tc.width)
			if got := lipgloss.Height(footer); got != want {
				t.Fatalf("footer rendered %d lines, want the baseline %d:\n%s", got, want, footer)
			}
		})
	}
}

func TestRenamePromptFooterShowsTheEmptyTitleComplaint(t *testing.T) {
	r, _ := newRenamePrompt("sess-1", "")
	footer := r.withEmptyTitleError().footer(80)

	if !strings.Contains(footer, "Title cannot be empty") {
		t.Fatalf("footer does not surface the error:\n%s", footer)
	}
}

func TestZeroRenamePromptIsInactive(t *testing.T) {
	// The commit and cancel paths both close the editor by assigning the zero
	// value, so the zero value has to read as "no rename in progress".
	var r renamePrompt
	if r.Active() {
		t.Fatal("the zero renamePrompt reports itself active")
	}
}
