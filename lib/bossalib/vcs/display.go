package vcs

// PRDisplayStatus represents the unified display status for a PR in the TUI.
type PRDisplayStatus int

// Values must match the proto PRDisplayStatus enum (which starts with UNSPECIFIED = 0).
const (
	PRDisplayStatusUnspecified PRDisplayStatus = 0
	PRDisplayStatusIdle        PRDisplayStatus = 1
	PRDisplayStatusChecking    PRDisplayStatus = 2
	PRDisplayStatusFailing     PRDisplayStatus = 3
	PRDisplayStatusConflict    PRDisplayStatus = 4
	PRDisplayStatusReviewed    PRDisplayStatus = 5
	PRDisplayStatusPassing     PRDisplayStatus = 6
	PRDisplayStatusMerged      PRDisplayStatus = 7
	PRDisplayStatusClosed      PRDisplayStatus = 8
)

// PRDisplayInfo holds the computed display status and metadata for a PR.
type PRDisplayInfo struct {
	Status      PRDisplayStatus
	HasFailures bool
}

// ComputeDisplayStatus derives a unified display status from PR state, CI checks,
// and review comments. Priority: Merged > Closed > Conflict > Failing > Checking > Reviewed > Passing > Idle.
func ComputeDisplayStatus(pr *PRStatus, checks []CheckResult, reviews []ReviewComment) PRDisplayInfo {
	if pr == nil {
		return PRDisplayInfo{Status: PRDisplayStatusIdle}
	}

	// Terminal states first.
	if pr.State == PRStateMerged {
		return PRDisplayInfo{Status: PRDisplayStatusMerged}
	}
	if pr.State == PRStateClosed {
		return PRDisplayInfo{Status: PRDisplayStatusClosed}
	}

	// Conflict detection.
	if pr.Mergeable != nil && !*pr.Mergeable {
		return PRDisplayInfo{Status: PRDisplayStatusConflict}
	}

	// Analyze CI checks.
	hasRunning := false
	hasFailed := false
	hasChecks := len(checks) > 0

	for _, c := range checks {
		if c.Status != CheckStatusCompleted {
			hasRunning = true
			continue
		}
		if c.Conclusion != nil && *c.Conclusion == CheckConclusionFailure {
			hasFailed = true
		}
	}

	// If all checks are complete and some failed, it's failing.
	if hasChecks && !hasRunning && hasFailed {
		return PRDisplayInfo{Status: PRDisplayStatusFailing}
	}

	// If checks are still running, it's checking (with failure flag if some already failed).
	if hasRunning {
		return PRDisplayInfo{Status: PRDisplayStatusChecking, HasFailures: hasFailed}
	}

	// Review analysis: look for outstanding changes_requested.
	hasChangesRequested := false
	for _, r := range reviews {
		if r.State == ReviewStateChangesRequested {
			hasChangesRequested = true
			break
		}
	}

	if hasChangesRequested {
		return PRDisplayInfo{Status: PRDisplayStatusReviewed}
	}

	// All checks green, no conflicts, no outstanding reviews = passing.
	if hasChecks && !hasFailed {
		return PRDisplayInfo{Status: PRDisplayStatusPassing}
	}

	return PRDisplayInfo{Status: PRDisplayStatusIdle}
}
