package domain

import "time"

// PendingTrackerMerge is a merged session whose post-merge tracker write did
// not land: the next try is due at NextAttemptAt, after AttemptCount tries,
// the latest of which failed with LastReason.
type PendingTrackerMerge struct {
	SessionID     string
	AttemptCount  int
	NextAttemptAt time.Time
	LastReason    string
}
