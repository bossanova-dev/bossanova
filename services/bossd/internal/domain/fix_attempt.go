package domain

import "time"

// FixAttempt represents a fix attempt within a session.
type FixAttempt struct {
	ID        string
	SessionID string
	Trigger   FixAttemptTrigger
	Result    FixAttemptResult
	Error     *string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// FixAttemptTrigger represents what triggered a fix attempt.
type FixAttemptTrigger int

const (
	FixAttemptTriggerUnspecified FixAttemptTrigger = iota
	FixAttemptTriggerCheckFailure
	FixAttemptTriggerConflict
	FixAttemptTriggerReviewFeedback
)

// FixAttemptResult represents the outcome of a fix attempt.
type FixAttemptResult int

const (
	FixAttemptResultUnspecified FixAttemptResult = iota
	FixAttemptResultSuccess
	FixAttemptResultFailed
	FixAttemptResultIncomplete
)

func (t FixAttemptTrigger) String() string {
	switch t {
	case FixAttemptTriggerCheckFailure:
		return "check_failure"
	case FixAttemptTriggerConflict:
		return "conflict"
	case FixAttemptTriggerReviewFeedback:
		return "review_feedback"
	default:
		return "unspecified"
	}
}

func (r FixAttemptResult) String() string {
	switch r {
	case FixAttemptResultSuccess:
		return "success"
	case FixAttemptResultFailed:
		return "failed"
	case FixAttemptResultIncomplete:
		return "incomplete"
	default:
		return "unspecified"
	}
}
