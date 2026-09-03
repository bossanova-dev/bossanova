package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTruncateString_ReservesOneRuneForTheMarker pins A3: the marker is one
// rune, so the budget reserves one rune for it and a truncated result is
// exactly maxRunes runes — not maxRunes-2, which is what a three-rune
// reservation left behind once the marker shrank to one.
func TestTruncateString_ReservesOneRuneForTheMarker(t *testing.T) {
	for _, tc := range []struct {
		name     string
		in       string
		maxRunes int
	}{
		{"ascii", strings.Repeat("a", 100), 30},
		{"multi-byte runes", strings.Repeat("世", 100), 50},
		{"mixed", strings.Repeat("aé世", 50), 44},
		{"one over the budget", strings.Repeat("a", 31), 30},
		{"budget of one", "abcdef", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateString(tc.in, tc.maxRunes)
			if n := utf8.RuneCountInString(got); n != tc.maxRunes {
				t.Errorf("truncateString(_, %d) = %q, %d runes, want exactly %d",
					tc.maxRunes, got, n, tc.maxRunes)
			}
			if !strings.HasSuffix(got, ellipsis) {
				t.Errorf("truncateString(_, %d) = %q, want it to end in %q", tc.maxRunes, got, ellipsis)
			}
			if !utf8.ValidString(got) {
				t.Errorf("truncateString(_, %d) = %q, which is not valid UTF-8", tc.maxRunes, got)
			}
		})
	}
}

// TestTruncateString_FittingInputUnchanged keeps the no-op path honest, so an
// over-eager budget cannot silently truncate everything.
func TestTruncateString_FittingInputUnchanged(t *testing.T) {
	for _, tc := range []struct {
		in       string
		maxRunes int
	}{
		{"short", 30},
		{"", 5},
		{strings.Repeat("世", 10), 10}, // exactly at the budget
	} {
		if got := truncateString(tc.in, tc.maxRunes); got != tc.in {
			t.Errorf("truncateString(%q, %d) = %q, want it returned unchanged", tc.in, tc.maxRunes, got)
		}
	}
}

// TestTruncateString_NonPositiveBudget guards the guard: a zero or negative
// budget must not panic on a negative slice bound.
func TestTruncateString_NonPositiveBudget(t *testing.T) {
	for _, maxRunes := range []int{0, -1} {
		if got := truncateString("abcdef", maxRunes); got != "" {
			t.Errorf("truncateString(_, %d) = %q, want the empty string", maxRunes, got)
		}
	}
}
