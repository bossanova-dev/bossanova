package server

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestSummarizeDuplicateSessionPlan_FirstLinePlusEllipsis pins the summary the
// user actually reads inside duplicateSessionAlreadyExistsError: a multi-line
// plan collapses to its first line, and an over-long line ends in the house
// ellipsis marker. The marker is appended past the rune limit rather than
// inside it, so the limit itself is unchanged.
func TestSummarizeDuplicateSessionPlan_FirstLinePlusEllipsis(t *testing.T) {
	t.Run("multi-line plan keeps only the first line", func(t *testing.T) {
		got := summarizeDuplicateSessionPlan("first line\nsecond line\nthird line")
		if got != "first line" {
			t.Errorf("summarizeDuplicateSessionPlan(...) = %q, want %q", got, "first line")
		}
		if strings.Contains(got, "\n") {
			t.Errorf("summary %q still contains a newline", got)
		}
	})

	t.Run("carriage return also ends the first line", func(t *testing.T) {
		if got := summarizeDuplicateSessionPlan("first line\r\nsecond"); got != "first line" {
			t.Errorf("summarizeDuplicateSessionPlan(...) = %q, want %q", got, "first line")
		}
	})

	t.Run("over-long first line truncates with the ellipsis marker", func(t *testing.T) {
		long := strings.Repeat("x", duplicateSessionPlanSummaryLimit+20)
		got := summarizeDuplicateSessionPlan(long + "\nsecond line")

		if !strings.HasSuffix(got, ellipsis) {
			t.Errorf("summary %q does not end in the ellipsis marker %q", got, ellipsis)
		}
		if n := utf8.RuneCountInString(got); n != duplicateSessionPlanSummaryLimit+1 {
			t.Errorf("summary is %d runes, want %d (the limit plus the appended marker)",
				n, duplicateSessionPlanSummaryLimit+1)
		}
	})

	t.Run("multi-byte first line truncates on a rune boundary", func(t *testing.T) {
		long := strings.Repeat("世", duplicateSessionPlanSummaryLimit+5)
		got := summarizeDuplicateSessionPlan(long)

		if !utf8.ValidString(got) {
			t.Errorf("summary %q is not valid UTF-8", got)
		}
		if !strings.HasSuffix(got, ellipsis) {
			t.Errorf("summary %q does not end in the ellipsis marker %q", got, ellipsis)
		}
	})

	t.Run("short plan is returned unchanged", func(t *testing.T) {
		if got := summarizeDuplicateSessionPlan("tidy up the parser"); got != "tidy up the parser" {
			t.Errorf("summarizeDuplicateSessionPlan(...) = %q, want it unchanged", got)
		}
	})
}
