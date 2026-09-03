package views

import (
	"strings"
	"testing"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// TestTruncateDisplay_MeasuresInCells pins the contract truncateDisplay
// actually owes its callers: the rendered result fits the caller's *display
// cell* budget. Measuring in the unit the implementation happens to count in
// would be circular — it would pass for any budget the code applied
// consistently, including a wrong one.
func TestTruncateDisplay_MeasuresInCells(t *testing.T) {
	for _, tc := range []struct {
		name     string
		in       string
		maxWidth int
	}{
		{"ascii", strings.Repeat("a", 40), 10},
		{"wide runes", strings.Repeat("世", 20), 11},
		{"mixed width", "abc" + strings.Repeat("世", 10) + "def", 9},
		{"combining marks", strings.Repeat("é", 30), 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateDisplay(tc.in, tc.maxWidth)
			if w := ansi.StringWidth(got); w > tc.maxWidth {
				t.Errorf("truncateDisplay(%q, %d) = %q, width %d cells, want at most %d",
					tc.in, tc.maxWidth, got, w, tc.maxWidth)
			}
			if !strings.HasSuffix(got, ellipsis) {
				t.Errorf("truncateDisplay(%q, %d) = %q, want the ellipsis marker %q on a truncated result",
					tc.in, tc.maxWidth, got, ellipsis)
			}
			if !utf8.ValidString(got) {
				t.Errorf("truncateDisplay(%q, %d) = %q, which is not valid UTF-8", tc.in, tc.maxWidth, got)
			}
		})
	}
}

// TestTruncateDisplay_FittingInputUnchanged guards the other direction: an
// over-eager budget that truncated everything would still satisfy the
// width assertion above, so pin that a string inside the budget is returned
// byte-for-byte.
func TestTruncateDisplay_FittingInputUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name     string
		in       string
		maxWidth int
	}{
		{"well under", "short", 40},
		{"exactly at the budget", "abcde", 5},
		{"wide runes exactly at the budget", "世界", 4},
		{"empty", "", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := truncateDisplay(tc.in, tc.maxWidth); got != tc.in {
				t.Errorf("truncateDisplay(%q, %d) = %q, want it returned unchanged", tc.in, tc.maxWidth, got)
			}
		})
	}
}

// TestTruncateDisplay_TinyBudgets covers the widths the old `maxWidth <= 3`
// dot-repeat branch used to special-case. The one-cell marker removes the
// branch, so these must neither panic nor exceed the budget.
func TestTruncateDisplay_TinyBudgets(t *testing.T) {
	in := strings.Repeat("a", 20)
	for _, maxWidth := range []int{0, 1, 2, 3} {
		got := truncateDisplay(in, maxWidth)
		if maxWidth <= 0 {
			// A non-positive budget is "uncapped" and returns the input.
			if got != in {
				t.Errorf("truncateDisplay(_, %d) = %q, want the input unchanged", maxWidth, got)
			}
			continue
		}
		if w := ansi.StringWidth(got); w > maxWidth {
			t.Errorf("truncateDisplay(_, %d) = %q, width %d cells, want at most %d", maxWidth, got, w, maxWidth)
		}
		// The width bound alone is satisfied by the empty string, which would
		// make this a vacuous budget test. Pin that a tiny budget still spends
		// itself on the marker rather than collapsing to nothing.
		if got == "" {
			t.Errorf("truncateDisplay(_, %d) = %q, want at least the ellipsis marker", maxWidth, got)
		}
		if !strings.HasSuffix(got, ellipsis) {
			t.Errorf("truncateDisplay(_, %d) = %q, want it to end in the ellipsis marker %q", maxWidth, got, ellipsis)
		}
	}
}

// TestTruncateDisplay_MultiLineBudgetsTheTotal pins the one place the cell
// budget is NOT the visual width, so the precondition in truncateDisplay's doc
// comment cannot rot into a false claim.
//
// ansi.StringWidth — what ansi.Truncate measures with — SUMS across newlines,
// while lipgloss.Width — what writeCLITableRow's padding measures with —
// returns the WIDEST line. On single-line input, which is all any caller feeds
// today, the two agree and none of this is observable. On multi-line input they
// do not, and the rune-by-rune builder this function replaced took the
// widest-line reading, so the behaviour changed. That is acceptable only while
// it is stated and pinned: an unpinned divergence between the cut and the
// padding is the kind of layout bug nothing goes red on.
func TestTruncateDisplay_MultiLineBudgetsTheTotal(t *testing.T) {
	const in = "aaaa\nbbbb"
	const maxWidth = 6

	// The premise: the two width functions genuinely disagree here. Without
	// this guard the assertion below would still pass if they ever converged,
	// silently making the test about nothing.
	if lipgloss.Width(in) >= ansi.StringWidth(in) {
		t.Fatalf("fixture does not exercise the divergence: lipgloss.Width=%d, ansi.StringWidth=%d",
			lipgloss.Width(in), ansi.StringWidth(in))
	}
	if lipgloss.Width(in) > maxWidth {
		t.Fatalf("fixture does not exercise the divergence: a widest-line reading (%d) must FIT in %d",
			lipgloss.Width(in), maxWidth)
	}

	// Documented behaviour: the total is budgeted, so this cuts even though the
	// widest line fits. Callers must pass single-line input.
	if got, want := truncateDisplay(in, maxWidth), "aaaa\nb"+ellipsis; got != want {
		t.Errorf("truncateDisplay(%q, %d) = %q, want %q — see the precondition on truncateDisplay",
			in, maxWidth, got, want)
	}
}

// TestTruncateDisplay_PreservesSGRSequences guards the reason this repo stopped
// byte-slicing styled strings: a naive cut splits a UTF-8 rune or strands an
// SGR escape, painting the rest of the screen in a leaked colour.
func TestTruncateDisplay_PreservesSGRSequences(t *testing.T) {
	styled := "\x1b[31m" + strings.Repeat("a", 40) + "\x1b[0m"
	got := truncateDisplay(styled, 10)

	if w := ansi.StringWidth(got); w > 10 {
		t.Errorf("styled truncation = %q, width %d cells, want at most 10", got, w)
	}
	if !utf8.ValidString(got) {
		t.Errorf("styled truncation = %q, which is not valid UTF-8", got)
	}
	// The escape sequences must not be cut mid-sequence: every ESC that
	// survives has to still terminate.
	if strings.Count(got, "\x1b[") > 0 && !strings.Contains(got, "m") {
		t.Errorf("styled truncation = %q stranded an unterminated escape sequence", got)
	}
	if strings.HasSuffix(got, "\x1b") || strings.HasSuffix(got, "\x1b[") {
		t.Errorf("styled truncation = %q ends inside an escape sequence", got)
	}
}
