package domain

// FinalizeResult is the outcome of a FinalizeSession call. Outcome maps 1:1
// to the cron_jobs.last_run_outcome column FinalizeSession writes. NoOp is
// true when the conditional state transition no-op'd (duplicate Stop event
// or a session that never reached ImplementingPlan) — the hook endpoint
// returns 200 either way, but the caller uses this to log the distinction.
// Deleted is true when the finalize path already removed the session row and
// worktree, so the caller must not record the deleted session ID or transition
// the (now-gone) row to Blocked.
type FinalizeResult struct {
	Outcome CronJobOutcome
	NoOp    bool
	Deleted bool
	// Err, when non-nil, is the underlying failure behind a *_failed outcome.
	// It is not returned as a top-level error so the hook endpoint still
	// reports success — the outcome column is already recorded.
	Err error
}
