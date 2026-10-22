package domain

import "time"

// RotationEvent is one persisted rotation-decision audit record (BOS-176).
// Account fields carry labels/ids only — never credentials.
type RotationEvent struct {
	ID          string
	SessionID   string
	ChatID      string
	Provider    string
	Trigger     string // proto enum name, e.g. "ROTATION_TRIGGER_USAGE_LIMITED"
	FromAccount string
	ToAccount   string
	ResetAt     *time.Time
	Outcome     string // proto enum name, e.g. "ROTATION_OUTCOME_ROTATED"
	Detail      string
	CreatedAt   time.Time
}
