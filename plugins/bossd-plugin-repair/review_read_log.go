package main

import "github.com/rs/zerolog"

// reviewReadLevel is the level for a failed review comment read: Warn, or
// Debug for a repeat of a quota refusal (ResourceExhausted from the host)
// inside the same quota window, so an exhausted quota logs once per window
// instead of once per sweep.
func (m *repairMonitor) reviewReadLevel(err error) zerolog.Level {
	return m.reviewReadLog.Level("review comments", err)
}
