package ui

import "time"

// BlurredPollInterval limits background polling while preserving notifications.
const BlurredPollInterval = 10 * time.Second

// PollDue reports whether a view should poll at the current focus and time.
func PollDue(focused bool, lastPoll, now time.Time) bool {
	return focused || now.Sub(lastPoll) >= BlurredPollInterval
}
