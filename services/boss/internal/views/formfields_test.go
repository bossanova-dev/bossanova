package views

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// TestBossSelectHeight_ContentFitAndCap pins the two halves of the select
// sizing rule: a short option list gets exactly the lines it needs (huh pads a
// Select's block to the Height it is given, so anything larger is trailing
// blank lines), and a long one is capped so the form cannot push the action bar
// off a short terminal. It pins the rule rather than reproducing a past bug:
// pre-BOS-567 every call site passed the constant 6 regardless of option count,
// which is the trailing whitespace the rendered-output tests in cron_form_test.go
// are the red-first guards for.
func TestBossSelectHeight_ContentFitAndCap(t *testing.T) {
	tests := []struct {
		name        string
		numOptions  int
		headerLines int
		want        int
	}{
		{name: "two options fit under the cap", numOptions: 2, headerLines: 1, want: 3},
		{name: "long list is capped", numOptions: 20, headerLines: 1, want: 6},
		{name: "exactly at the cap", numOptions: 5, headerLines: 1, want: 6},
		{name: "a description line counts toward the cap", numOptions: 4, headerLines: 2, want: 6},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := bossSelectHeight(tt.numOptions, tt.headerLines); got != tt.want {
				t.Errorf("bossSelectHeight(%d, %d) = %d, want %d",
					tt.numOptions, tt.headerLines, got, tt.want)
			}
		})
	}
}

// TestFieldGutter_MatchesHuhThemeBase is the "one definition" proof for the
// focus gutter: after BOS-567 the chrome lives in fieldGutter, and bossHuhTheme
// consumes it rather than building a look-alike inline. If the two ever drift,
// a settings row editor and a huh field would show subtly different gutters,
// which is exactly the inconsistency this work exists to remove.
//
// It checks the two properties a viewer can see — reserved width and the gutter
// glyph — for both focus states across both theme resolutions, rather than
// comparing the styles themselves, which would pass even if the theme rendered
// them differently.
func TestFieldGutter_MatchesHuhThemeBase(t *testing.T) {
	const content = "Display name"

	for _, variant := range themeVariants() {
		t.Run(variant.name, func(t *testing.T) {
			s := bossHuhTheme().Theme(variant.isDark)

			states := []struct {
				name    string
				focused bool
				base    lipgloss.Style
			}{
				{name: "focused", focused: true, base: s.Focused.Base},
				{name: "blurred", focused: false, base: s.Blurred.Base},
			}

			for _, st := range states {
				t.Run(st.name, func(t *testing.T) {
					gutter := fieldGutter(st.focused).Render(content)
					themed := st.base.Render(content)

					if gw, tw := lipgloss.Width(gutter), lipgloss.Width(themed); gw != tw {
						t.Errorf("fieldGutter(%t) renders %d columns, theme Base renders %d; the two definitions have drifted\ngutter: %q\ntheme:  %q",
							st.focused, gw, tw, gutter, themed)
					}
					if got, want := strings.ContainsRune(gutter, focusGutterGlyph), strings.ContainsRune(themed, focusGutterGlyph); got != want {
						t.Errorf("fieldGutter(%t) draws the gutter glyph %q = %t, theme Base = %t",
							st.focused, string(focusGutterGlyph), got, want)
					}
					if st.focused && !paintedIn(t, gutter, colorSelected) {
						t.Errorf("fieldGutter(true) %q does not paint the gutter in colorSelected", gutter)
					}
				})
			}
		})
	}
}

// TestBossFormWrapWidth_MatchesBothFocusStates pins that there is a single
// answer to "how wide is a form field's content area". huh sizes a Text field's
// textarea to width - activeStyles().Base.GetHorizontalFrameSize(), and
// activeStyles() swaps with focus — so if the focused and blurred gutters ever
// reserved different frames, a field would rewrap as focus arrived and any
// content measured against bossFormWrapWidth would be wrong for one of the two
// states. It also proves the width is derived rather than a hardcoded 68.
func TestBossFormWrapWidth_MatchesBothFocusStates(t *testing.T) {
	got := bossFormWrapWidth()

	for _, variant := range themeVariants() {
		t.Run(variant.name, func(t *testing.T) {
			s := bossHuhTheme().Theme(variant.isDark)

			for name, frame := range map[string]int{
				"focused": s.Focused.Base.GetHorizontalFrameSize(),
				"blurred": s.Blurred.Base.GetHorizontalFrameSize(),
			} {
				if want := bossFormWidth - frame; got != want {
					t.Errorf("bossFormWrapWidth() = %d, but huh gives a %s field %d columns (%d - %d frame)",
						got, name, want, bossFormWidth, frame)
				}
			}
		})
	}

	if got <= 0 || got >= bossFormWidth {
		t.Errorf("bossFormWrapWidth() = %d, want a positive width inside bossFormWidth (%d)", got, bossFormWidth)
	}
}

// TestBossTextLines_ClampsAndWraps pins the sizing rule for a huh Text field:
// it opens at one row rather than the textarea's hardcoded default of six, it
// grows row-for-row with wrapped content, and it stops at bossTextMaxLines so a
// pasted essay scrolls internally instead of pushing the action bar off screen.
//
// The one-rune-under / one-rune-over pair is the load-bearing case: it proves
// the wrap width was derived from the theme (bossFormWrapWidth) rather than
// guessed, because a guess one column out flips exactly that boundary.
func TestBossTextLines_ClampsAndWraps(t *testing.T) {
	wrap := bossFormWrapWidth()

	tests := []struct {
		name  string
		value string
		want  int
	}{
		{name: "empty opens at the minimum", value: "", want: bossTextMinLines},
		{name: "three hard lines", value: "one\ntwo\nthree", want: 3},
		{name: "forty lines are capped", value: strings.TrimSuffix(strings.Repeat("x\n", 40), "\n"), want: bossTextMaxLines},
		{name: "one rune short of the wrap width", value: strings.Repeat("a", wrap-1), want: 1},
		{name: "one rune over the wrap width", value: strings.Repeat("a", wrap+1), want: 2},
		// The textarea closes a logical line with `>=`, not `>`, so a line
		// that exactly fills the width costs a second row — the one the cursor
		// sits on. Counting this as 1 is what scrolled the box and hid the
		// text the user had just typed.
		{name: "exactly the wrap width takes the cursor's row", value: strings.Repeat("a", wrap), want: 2},
		{name: "an exactly-full line among others", value: strings.Repeat("a", wrap) + "\nb", want: 3},
		// Same boundary reached with real words rather than one long run, so
		// the rule is not an artefact of how lipgloss breaks an unbroken word.
		{name: "a full line of words", value: strings.Repeat("abc ", wrap/4-1) + "abcd", want: 2},
		// The case only the width bound catches: the row reaches the edge with
		// a trailing space, which lipgloss has already padded away by the time
		// the last row can be measured. This is the shape ordinary prose lands
		// in while it is being typed.
		{name: "a full line ending in a space", value: strings.Repeat("a", wrap-1) + " ", want: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := bossTextLines(tt.value, wrap); got != tt.want {
				t.Errorf("bossTextLines(%d runes, wrap %d) = %d, want %d",
					len([]rune(tt.value)), wrap, got, tt.want)
			}
		})
	}
}

// TestBossTextPendingInsert_CoversEveryInsertingKey pins the message shapes a
// Text field's host must grow for *before* huh sees them. Missing one is a
// silent regression of the scroll fix, not a compile error: the box is a row
// short only for the key that was missed, and only once the content reaches
// the wrap width.
func TestBossTextPendingInsert_CoversEveryInsertingKey(t *testing.T) {
	tests := []struct {
		name string
		msg  tea.Msg
		want string
	}{
		{name: "a printable rune", msg: keyPress('a'), want: "a"},
		{name: "alt+enter inserts a line break", msg: cronPromptNewline, want: "\n"},
		{name: "ctrl+j inserts a line break", msg: tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl}, want: "\n"},
		{name: "a bracketed paste", msg: tea.PasteMsg{Content: "two\nlines"}, want: "two\nlines"},
		// A bare enter advances the form rather than inserting anything.
		{name: "plain enter inserts nothing", msg: tea.KeyPressMsg{Code: tea.KeyEnter}, want: ""},
		{name: "tab inserts nothing", msg: tea.KeyPressMsg{Code: tea.KeyTab}, want: ""},
		{name: "backspace inserts nothing", msg: tea.KeyPressMsg{Code: tea.KeyBackspace}, want: ""},
		{name: "a resize inserts nothing", msg: tea.WindowSizeMsg{Width: 80, Height: 40}, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := bossTextPendingInsert(tt.msg); got != tt.want {
				t.Errorf("bossTextPendingInsert(%#v) = %q, want %q", tt.msg, got, tt.want)
			}
		})
	}
}

// TestBossConfirm_ButtonsAreLeftAligned pins BOS-567 ask #4. huh centres a
// Confirm's buttons inside a block as wide as that field's own title and
// description (field_confirm.go View), so on the cron form each toggle's
// Yes/No floated at a different horizontal position — the longer the
// description, the further right. bossConfirm left-aligns them, so every
// confirm in the TUI starts its affirmative at the same column.
func TestBossConfirm_ButtonsAreLeftAligned(t *testing.T) {
	column := func(description string) int {
		t.Helper()
		field := bossConfirm().
			Title("Run setup command").
			Description(description).
			Affirmative("Yes").
			Negative("No")
		field.WithTheme(bossHuhTheme())
		field.WithWidth(bossFormWidth)

		for _, line := range strings.Split(stripANSI(field.View()), "\n") {
			if i := strings.Index(line, "Yes"); i >= 0 {
				return i
			}
		}
		t.Fatalf("confirm rendered no affirmative button:\n%s", field.View())
		return -1
	}

	long := column("Run the repo setup script before the agent. Turn off to keep light jobs fast. Default on.")
	short := column("Run this job on its schedule.")

	if long != short {
		t.Errorf("affirmative starts at column %d under a long description and %d under a short one; "+
			"the buttons are being centred against the description width",
			long, short)
	}
}
