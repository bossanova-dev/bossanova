package domain

import "time"

// ResumeAttempt is one automatic resume of a dead run (BOS-1537), persisted
// so the resume budget and the fix sentence survive a daemon restart. BudgetKey
// identifies the run the attempt belongs to (a cron fire, or the original
// prompt's chat), so a new run starts a fresh budget. AttemptNumber is
// 1-based within (SessionID, BudgetKey). FailureClass is one of the
// session_resume_attempts.failure_class CHECK values (see
// resumepolicy.FailureClass); FailureDetail is free text for diagnostics.
type ResumeAttempt struct {
	ID            string
	SessionID     string
	BudgetKey     string
	AttemptNumber int
	AttemptedAt   time.Time
	FailureClass  string
	FailureDetail string
	CreatedAt     time.Time
}
