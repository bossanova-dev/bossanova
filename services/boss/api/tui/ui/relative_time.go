package ui

import (
	"fmt"
	"time"
)

// --- Relative time helpers ---

// RelTimeAgo formats a past time as "Xm ago", "Xh ago", etc.
func RelTimeAgo(t time.Time) string {
	return FormatRelAgo(time.Since(t))
}

// FormatRelAgo formats an elapsed duration as "just now", "Xm ago", "Xh ago",
// or "Xd ago". Split from RelTimeAgo so the threshold logic is unit-testable
// with exact durations rather than wall-clock time.
func FormatRelAgo(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// RelTimeFuture formats a future time as "in Xm", "in Xh", etc.
func RelTimeFuture(t time.Time) string {
	return FormatRelFuture(time.Until(t))
}

// FormatRelFuture formats a remaining duration as "now", "in Xs", "in Xm",
// "in Xh", or "in Xd". Split from RelTimeFuture so the threshold logic is
// unit-testable with exact durations rather than wall-clock time.
func FormatRelFuture(d time.Duration) string {
	if d <= 0 {
		return "now"
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("in %ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("in %dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("in %dh", int(d.Hours()))
	default:
		return fmt.Sprintf("in %dd", int(d.Hours()/24))
	}
}
