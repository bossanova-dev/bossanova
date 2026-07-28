package views

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

// sgrColourRe matches lipgloss's truecolour SGR sequences — "38;2;r;g;b" for a
// foreground, "48;2;r;g;b" for a background — capturing the role and the triple
// separately so a foreground and a background of the same colour never compare
// equal.
var sgrColourRe = regexp.MustCompile(`([34]8);2;(\d+;\d+;\d+)`)

// buttonColours returns the sorted set of colour roles present in rendered.
// It deliberately ignores every other attribute: bold, faint, padding and
// margins are each call site's own business, and only the colour rule is shared.
func buttonColours(rendered string) []string {
	var out []string
	for _, m := range sgrColourRe.FindAllStringSubmatch(rendered, -1) {
		out = append(out, m[1]+":"+m[2])
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// TestButtonChrome_ColoursAgreeAcrossBothCallSites is the BOS-567 de-duplication
// contract for buttonStyle. The TUI draws buttons two ways — huh's Confirm
// fields through bossHuhTheme, and account_register.go's hand-rolled Yes/No
// through renderButtonRow — and the two must not drift into look-alike colour
// rules. Colour is all that is compared, because the geometry legitimately
// differs (see TestButtonChrome_GeometryStaysPerCallSite).
//
// Each case pairs a theme style with the renderButtonRow output that must share
// its colours, and is named for the buttonStyle(focused, primary) rule the two
// have in common — not for a UI state, because the two idioms do not map onto
// these rules the same way. huh's Confirm has one FocusedButton style, applied
// to whichever choice is currently set (field_confirm.go View), so its theme
// cannot tell "Yes" from "No": a selected "Cancel" wears the primary fill,
// where renderButtonRow's focused secondary wears the muted one. That
// difference predates BOS-567 and survives it unchanged — this test is a lock
// on the shared colour vocabulary, not a claim that the states coincide.
func TestButtonChrome_ColoursAgreeAcrossBothCallSites(t *testing.T) {
	const label = "Yes"

	for _, variant := range themeVariants() {
		t.Run(variant.name, func(t *testing.T) {
			s := bossHuhTheme().Theme(variant.isDark)

			for _, tc := range []struct {
				name string
				huh  lipgloss.Style
				row  string
			}{
				// buttonStyle(true, true) — the filled primary chip. huh puts
				// it on the set choice of a focused Confirm; renderButtonRow
				// puts it on a primary button under its cursor.
				{"filled primary", s.Focused.FocusedButton, renderButtonRow([]button{{label: label, primary: true}}, 0)},
				// buttonStyle(true, false) — the filled secondary chip. huh
				// puts it on the *unset* choice of a focused Confirm, and on
				// the set choice of a blurred one (so a blurred Confirm stops
				// out-weighing the genuinely focused field); renderButtonRow
				// puts it on a secondary button under its cursor.
				{"filled secondary (huh: unset choice)", s.Focused.BlurredButton, renderButtonRow([]button{{label: label}}, 0)},
				{"filled secondary (huh: blurred field)", s.Blurred.FocusedButton, renderButtonRow([]button{{label: label}}, 0)},
				// buttonStyle(false, false) — no fill, the colour as text.
				{"unfilled secondary", s.Blurred.BlurredButton, renderButtonRow([]button{{label: label}}, -1)},
			} {
				t.Run(tc.name, func(t *testing.T) {
					got, want := buttonColours(tc.row), buttonColours(tc.huh.Render(label))
					if !slices.Equal(got, want) {
						t.Errorf("renderButtonRow paints %v but the huh theme paints %v for the same button state; both must read from buttonStyle",
							got, want)
					}
				})
			}
		})
	}
}

// TestButtonChrome_GeometryStaysPerCallSite records the one thing the
// buttonStyle extraction deliberately did NOT unify. huh pads a Confirm chip by
// two columns and adds a right margin; the hand-rolled rows use a tighter
// one-column chip and space their buttons themselves. Folding either into
// buttonStyle would restyle a live screen, which BOS-567 is not doing — so the
// widths are pinned here instead.
func TestButtonChrome_GeometryStaysPerCallSite(t *testing.T) {
	const label = "Yes"

	huhChip := bossHuhTheme().Theme(true).Focused.FocusedButton.Render(label)
	if got, want := lipgloss.Width(huhChip), len(label)+2*buttonChipPaddingX+buttonChipMarginRight; got != want {
		t.Errorf("huh Confirm chip width = %d, want %d; the theme must keep huh's own chip geometry", got, want)
	}

	rowChip := renderButtonRow([]button{{label: label, primary: true}}, 0)
	if got, want := lipgloss.Width(rowChip), len(label)+2; got != want {
		t.Errorf("hand-rolled button chip width = %d, want %d; the row chip stays one column tighter than huh's", got, want)
	}
}

func TestRenderButtonRowRendersFocusedAndUnfocusedButtons(t *testing.T) {
	buttons := []button{{label: "Yes", primary: true}, {label: "No"}}

	yesFocused := renderButtonRow(buttons, 0)
	noFocused := renderButtonRow(buttons, 1)

	for _, row := range []string{yesFocused, noFocused} {
		for _, label := range []string{"Yes", "No"} {
			if !strings.Contains(row, label) {
				t.Fatalf("button row missing %q: %q", label, row)
			}
		}
	}
	if yesFocused == noFocused {
		t.Fatalf("focused button rendering should change: %q", yesFocused)
	}
	if !strings.Contains(yesFocused, "48;2;76;167;248") {
		t.Fatalf("focused primary button missing selected-color background: %q", yesFocused)
	}
	if !strings.Contains(noFocused, "48;2;98;98;98") {
		t.Fatalf("focused non-primary button missing muted-color background: %q", noFocused)
	}
}

func TestRenderButtonRowIgnoresOutOfRangeFocus(t *testing.T) {
	buttons := []button{{label: "Yes", primary: true}, {label: "No"}}

	for _, focused := range []int{-1, len(buttons)} {
		t.Run("out-of-range", func(t *testing.T) {
			row := renderButtonRow(buttons, focused)
			for _, label := range []string{"Yes", "No"} {
				if !strings.Contains(row, label) {
					t.Fatalf("button row missing %q: %q", label, row)
				}
			}
		})
	}
}
