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
	PRDisplayStatusRejected    PRDisplayStatus = 5
	PRDisplayStatusPassing     PRDisplayStatus = 6
	PRDisplayStatusMerged      PRDisplayStatus = 7
	PRDisplayStatusClosed      PRDisplayStatus = 8
	PRDisplayStatusDraft       PRDisplayStatus = 9
	PRDisplayStatusApproved    PRDisplayStatus = 10
)

// PRDisplayInfo holds the computed display status and metadata for a PR.
type PRDisplayInfo struct {
	Status              PRDisplayStatus
	HasFailures         bool
	HasChangesRequested bool
}

// ComputeDisplayStatus derives a unified display status from PR state, CI checks,
// and review comments. Priority: Merged > Closed > Conflict > Failing > Checking > Rejected > Approved > Passing > Idle.
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

	// Draft PRs are not ready for review — other statuses become noise.
	if pr.Draft {
		return PRDisplayInfo{Status: PRDisplayStatusDraft}
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

	// Review analysis: use each author's latest review (reviews arrive chronologically).
	latestByAuthor := make(map[string]ReviewState)
	for _, r := range reviews {
		latestByAuthor[r.Author] = r.State
	}
	hasChangesRequested := false
	hasApproval := false
	for _, state := range latestByAuthor {
		if state == ReviewStateChangesRequested {
			hasChangesRequested = true
		}
		if state == ReviewStateApproved {
			hasApproval = true
		}
	}

	// If checks are still running, it's checking (with metadata flags for styling).
	if hasRunning {
		return PRDisplayInfo{Status: PRDisplayStatusChecking, HasFailures: hasFailed, HasChangesRequested: hasChangesRequested}
	}

	// Rejected takes priority — any outstanding changes_requested blocks approval.
	if hasChangesRequested {
		return PRDisplayInfo{Status: PRDisplayStatusRejected}
	}

	// When mergeable is unknown we can't confirm passing/approved — show "checking".
	mergeableUnknown := pr.Mergeable == nil

	if hasApproval && !mergeableUnknown {
		return PRDisplayInfo{Status: PRDisplayStatusApproved}
	}

	// All checks green, no conflicts, no outstanding reviews = passing.
	if hasChecks && !hasFailed && !mergeableUnknown {
		return PRDisplayInfo{Status: PRDisplayStatusPassing}
	}

	// Mergeability hasn't been confirmed — show "checking" until resolved.
	if mergeableUnknown {
		return PRDisplayInfo{Status: PRDisplayStatusChecking, HasChangesRequested: hasChangesRequested}
	}

	return PRDisplayInfo{Status: PRDisplayStatusIdle}
}
