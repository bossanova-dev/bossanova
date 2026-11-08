package models

import "time"

// TrackerRank is the build-order rank of a session's linked tracker issue.
// A nil Session.TrackerRank means no rank has ever been recorded.
type TrackerRank struct {
	Tier                 int32
	TierSource           string
	DeadlineOn           *string
	DeadlineSource       string
	IsScheduled          bool
	UnblockedTicketCount int32
	RankedAt             time.Time
}
