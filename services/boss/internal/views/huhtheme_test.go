package views

import (
	"image/color"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

// focusGutterGlyph is the rune the focused field's left border paints. It is a
// deliberate second reference to lipgloss's border definition rather than a read
// of the constructed style: deriving it from the theme would make every gutter
// assertion below tautological. The cost is that swapping the theme to another
// border fails these tests until this is pointed at the new border too — which
// is the intended trade, since the gutter is a user-visible contract.
var focusGutterGlyph = []rune(lipgloss.ThickBorder().Left)[0]

// paintedIn reports whether rendered output carries c as an SGR colour. lipgloss
// emits colours as an "r;g;b" triple, so this renders a probe in c and looks for
// that triple — no palette hex is hard-coded, and the check works for foreground
// and background alike. It matches the bare triple rather than a "38;2;"-prefixed
// sequence so one helper covers both, which means it cannot tell apart two palette
// entries that share a hex — colorSelected and colorInfo are both #4CA7F8, so
// "painted in colorSelected" here means "painted in that colour", not "reached via
// that constant".
func paintedIn(t *testing.T, rendered string, c color.Color) bool {
	t.Helper()

	probe := lipgloss.NewStyle().Foreground(c).Render("x")
	const fgPrefix = "38;2;"
	start := strings.Index(probe, fgPrefix)
	if start < 0 {
		t.Fatalf("cannot derive the SGR triple for %v: lipgloss rendered %q without a %q prefix", c, probe, fgPrefix)
	}
	rest := probe[start+len(fgPrefix):]
	end := strings.IndexByte(rest, 'm')
	if end < 0 {
		t.Fatalf("cannot derive the SGR triple for %v: unterminated sequence in %q", c, probe)
	}
	return strings.Contains(rendered, rest[:end])
}

// themeVariants resolves the shared huh theme for both light and dark
// backgrounds. Every focus-affordance invariant must hold for both, so each
// test walks this table rather than assuming a single resolution.
func themeVariants() []themeVariant {
	return []themeVariant{
		{name: "dark", isDark: true},
		{name: "light", isDark: false},
	}
}

type themeVariant struct {
	name   string
	isDark bool
}

// TestBossHuhTheme_BaseWidthParity is the no-jitter invariant: the focused
// field's gutter and the blurred field's placeholder gutter must reserve the
// same number of columns, so field text does not shift horizontally as focus
// moves down the form. This is the assertion most likely to catch a regression
// (e.g. someone dropping the blurred HiddenBorder), and it is a property a
// still screenshot cannot prove.
func TestBossHuhTheme_BaseWidthParity(t *testing.T) {
	const content = "Display name"

	for _, variant := range themeVariants() {
		t.Run(variant.name, func(t *testing.T) {
			s := bossHuhTheme().Theme(variant.isDark)

			focused := s.Focused.Base.Render(content)
			blurred := s.Blurred.Base.Render(content)

			if fw, bw := lipgloss.Width(focused), lipgloss.Width(blurred); fw != bw {
				t.Errorf("focused Base width = %d, blurred Base width = %d; they must match or form text jitters as focus moves\nfocused: %q\nblurred: %q",
					fw, bw, focused, blurred)
			}
			if fh, bh := lipgloss.Height(focused), lipgloss.Height(blurred); fh != bh {
				t.Errorf("focused Base height = %d, blurred Base height = %d; they must match", fh, bh)
			}
		})
	}
}

// TestBossHuhTheme_BaseFocusIsVisuallyDistinct pins that a distinction actually
// exists: rendering identical content through the focused and blurred Base must
// not produce identical output, otherwise there is no cue about where focus sits.
func TestBossHuhTheme_BaseFocusIsVisuallyDistinct(t *testing.T) {
	const content = "Display name"

	for _, variant := range themeVariants() {
		t.Run(variant.name, func(t *testing.T) {
			s := bossHuhTheme().Theme(variant.isDark)

			focused := s.Focused.Base.Render(content)
			blurred := s.Blurred.Base.Render(content)

			if focused == blurred {
				t.Fatalf("focused and blurred Base render identically (%q); the focused field has no visual affordance", focused)
			}
			if !strings.ContainsRune(focused, focusGutterGlyph) {
				t.Errorf("focused Base %q does not draw the focus gutter glyph %q", focused, string(focusGutterGlyph))
			}
			if strings.ContainsRune(blurred, focusGutterGlyph) {
				t.Errorf("blurred Base %q draws the focus gutter glyph %q; only the focused field may", blurred, string(focusGutterGlyph))
			}
			// The glyph alone is not the affordance: an uncoloured ┃ reads as
			// ordinary box drawing. The proof capture is plain text and cannot
			// see colour, so this assertion is the only guard on the "gutter in
			// colorSelected" half of the contract.
			if !paintedIn(t, focused, colorSelected) {
				t.Errorf("focused Base %q does not paint the gutter in colorSelected; an uncoloured gutter is not a focus cue", focused)
			}
		})
	}
}

// TestBossHuhTheme_CardTracksBase pins the one part of the theme no form
// currently exercises: huh's Note field renders through Card, which ThemeBase
// copies from its own Base before this theme replaces Base. Without the
// re-copy a note's gutter would track focus but stay uncoloured, disagreeing
// with every other field — and repo_add_test.go's gutterRuns() would count it.
func TestBossHuhTheme_CardTracksBase(t *testing.T) {
	const content = "Note body"

	for _, variant := range themeVariants() {
		t.Run(variant.name, func(t *testing.T) {
			s := bossHuhTheme().Theme(variant.isDark)

			if got, want := s.Focused.Card.Render(content), s.Focused.Base.Render(content); got != want {
				t.Errorf("focused Card renders %q, want the focused Base's %q; a Note's gutter must match every other field's", got, want)
			}
			if got, want := s.Blurred.Card.Render(content), s.Blurred.Base.Render(content); got != want {
				t.Errorf("blurred Card renders %q, want the blurred Base's %q", got, want)
			}
		})
	}
}

// TestBossHuhTheme_BlurredSelectSelectorIsDimmed proves a blurred Select's
// chevron no longer competes with the genuinely focused field for attention.
func TestBossHuhTheme_BlurredSelectSelectorIsDimmed(t *testing.T) {
	for _, variant := range themeVariants() {
		t.Run(variant.name, func(t *testing.T) {
			s := bossHuhTheme().Theme(variant.isDark)

			// Render with no argument: the selector's own SetString value is
			// what huh paints, and passing "" would append a joined space.
			focused := s.Focused.SelectSelector.Render()
			blurred := s.Blurred.SelectSelector.Render()

			if focused == blurred {
				t.Errorf("blurred select selector renders identically to the focused one (%q); a blurred chevron must be dimmed", focused)
			}
			if lipgloss.Width(focused) != lipgloss.Width(blurred) {
				t.Errorf("select selector widths differ (focused %d, blurred %d); dim the chevron, do not remove it — removing shifts the option text",
					lipgloss.Width(focused), lipgloss.Width(blurred))
			}
			// "Different from focused" would also be satisfied by some other
			// loud colour; the criterion is specifically that the chevron stops
			// claiming the selection colour.
			if paintedIn(t, blurred, colorSelected) {
				t.Errorf("blurred select selector %q is still painted in colorSelected; it competes with the genuinely focused field", blurred)
			}
		})
	}
}

// TestBossHuhTheme_BlurredConfirmButtonIsDimmed proves an unfocused Confirm's
// affirmative button stops carrying the strongest visual weight on the screen.
func TestBossHuhTheme_BlurredConfirmButtonIsDimmed(t *testing.T) {
	for _, variant := range themeVariants() {
		t.Run(variant.name, func(t *testing.T) {
			s := bossHuhTheme().Theme(variant.isDark)

			focused := s.Focused.FocusedButton.Render("Add Repository")
			blurred := s.Blurred.FocusedButton.Render("Add Repository")

			if focused == blurred {
				t.Errorf("blurred confirm button renders identically to the focused one (%q)", focused)
			}
			if lipgloss.Width(focused) != lipgloss.Width(blurred) {
				t.Errorf("confirm button widths differ (focused %d, blurred %d); dimming must not resize the button",
					lipgloss.Width(focused), lipgloss.Width(blurred))
			}
			if paintedIn(t, blurred, colorSelected) {
				t.Errorf("a blurred Confirm's affirmative button %q still uses the colorSelected background; it out-weighs the focused field", blurred)
			}
		})
	}
}

// TestBossHuhTheme_BlurredConfirmKeepsItsSelection guards the cost of dimming a
// blurred Confirm: huh picks FocusedButton for the option currently selected and
// BlurredButton for the other, so if both blurred styles collapse onto the same
// rendering, a blurred Confirm no longer shows its own value. cron_form.go's
// "Enabled" and "Run setup command" toggles are read exactly that way — while
// focus sits on some other field.
func TestBossHuhTheme_BlurredConfirmKeepsItsSelection(t *testing.T) {
	const label = "Yes"

	for _, variant := range themeVariants() {
		t.Run(variant.name, func(t *testing.T) {
			s := bossHuhTheme().Theme(variant.isDark)

			selected := s.Blurred.FocusedButton.Render(label)
			unselected := s.Blurred.BlurredButton.Render(label)

			if selected == unselected {
				t.Errorf("a blurred Confirm renders its selected and unselected buttons identically (%q); its value becomes unreadable while focus is elsewhere", selected)
			}
			if lipgloss.Width(selected) != lipgloss.Width(unselected) {
				t.Errorf("blurred confirm button widths differ (selected %d, unselected %d); the two states must occupy the same columns",
					lipgloss.Width(selected), lipgloss.Width(unselected))
			}
		})
	}
}

// TestBossHuhTheme_BlurredDescriptionIsDimmed keeps the blurred description
// consistent with the already-dimmed blurred title. Note this pins an END STATE
// the theme already satisfied before the focus-affordance work — the focused
// description is itself faint, so the blurred one inherited the dimming — rather
// than a behaviour this change introduced. It is still worth asserting: it fails
// if the blurred description ever stops being faint. It says nothing about the
// focused description, which re-applies faint unconditionally below it.
func TestBossHuhTheme_BlurredDescriptionIsDimmed(t *testing.T) {
	for _, variant := range themeVariants() {
		t.Run(variant.name, func(t *testing.T) {
			s := bossHuhTheme().Theme(variant.isDark)

			dimmed := s.Blurred.Description.Render("hint")
			undimmed := s.Blurred.Description.Faint(false).Render("hint")
			if dimmed == undimmed {
				t.Errorf("blurred description renders undimmed (%q); it should be faint like the blurred title", dimmed)
			}
		})
	}
}
