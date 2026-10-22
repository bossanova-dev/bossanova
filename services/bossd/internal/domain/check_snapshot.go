package domain

import "time"

// CheckSnapshot is a single per-poll record of what the DisplayPoller saw
// for a session's CI checks plus the DisplayStatus it computed from them.
// Used by `boss session checks <id>` to answer "why did bossd think this
// PR was passing?" without re-running gh by hand.
type CheckSnapshot struct {
	ID             int64
	SessionID      string
	PolledAt       time.Time
	HeadSHA        string
	RawJSON        string
	ComputedStatus int
}
