package vcs

// DisplayStatus represents the unified display status for a session in the TUI.
type DisplayStatus int

// Values must match the proto DisplayStatus enum (which starts with UNSPECIFIED = 0).
const (
	DisplayStatusUnspecified DisplayStatus = 0
	DisplayStatusIdle        DisplayStatus = 1
	DisplayStatusChecking    DisplayStatus = 2
	DisplayStatusFailing     DisplayStatus = 3
	DisplayStatusConflict    DisplayStatus = 4
	DisplayStatusRejected    DisplayStatus = 5
	DisplayStatusPassing     DisplayStatus = 6
	DisplayStatusMerged      DisplayStatus = 7
	DisplayStatusClosed      DisplayStatus = 8
	DisplayStatusDraft       DisplayStatus = 9
	DisplayStatusApproved    DisplayStatus = 10
)

// DisplayInfo holds the computed display status and metadata for a session.
type DisplayInfo struct {
	Status              DisplayStatus
	HasFailures         bool
	HasChangesRequested bool
	HeadSHA             string
}

// ComputeDisplayStatus derives a unified display status from PR state, CI checks,
// and review comments. Priority: Merged > Closed > Conflict > Failing > Checking > Rejected > Approved > Passing > Idle.
func ComputeDisplayStatus(pr *PRStatus, checks []CheckResult, reviews []ReviewComment) DisplayInfo {
	if pr == nil {
		return DisplayInfo{Status: DisplayStatusIdle}
	}

	// Terminal states first.
	if pr.State == PRStateMerged {
		return DisplayInfo{Status: DisplayStatusMerged}
	}
	if pr.State == PRStateClosed {
		return DisplayInfo{Status: DisplayStatusClosed}
	}

	// Draft PRs are not ready for review — other statuses become noise.
	if pr.Draft {
		return DisplayInfo{Status: DisplayStatusDraft}
	}

	// Conflict detection.
	if pr.Mergeable != nil && !*pr.Mergeable {
		return DisplayInfo{Status: DisplayStatusConflict}
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
		return DisplayInfo{Status: DisplayStatusFailing}
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
		return DisplayInfo{Status: DisplayStatusChecking, HasFailures: hasFailed, HasChangesRequested: hasChangesRequested}
	}

	// Rejected takes priority — any outstanding changes_requested blocks approval.
	if hasChangesRequested {
		return DisplayInfo{Status: DisplayStatusRejected}
	}

	// When mergeable is unknown we can't confirm passing/approved — show "checking".
	mergeableUnknown := pr.Mergeable == nil

	if hasApproval && !mergeableUnknown {
		return DisplayInfo{Status: DisplayStatusApproved}
	}

	// All checks green, no conflicts, no outstanding reviews = passing.
	if hasChecks && !hasFailed && !mergeableUnknown {
		return DisplayInfo{Status: DisplayStatusPassing}
	}

	// Mergeability hasn't been confirmed — show "checking" until resolved.
	if mergeableUnknown {
		return DisplayInfo{Status: DisplayStatusChecking, HasChangesRequested: hasChangesRequested}
	}

	return DisplayInfo{Status: DisplayStatusIdle}
}
