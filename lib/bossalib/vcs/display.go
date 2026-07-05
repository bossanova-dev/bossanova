package vcs

import (
	"fmt"
	"sort"
	"strings"
)

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
	DisplayStatusReview      DisplayStatus = 11
)

// DisplayInfo holds the computed display status and metadata for a session.
type DisplayInfo struct {
	Status              DisplayStatus
	HasFailures         bool
	HasChangesRequested bool
	// ChangesRequestedBy lists the sorted, deduped logins whose latest review
	// is CHANGES_REQUESTED. Populated only when HasChangesRequested is true and
	// the reviews carry an author login; empty otherwise (e.g. when the block
	// comes from pr.LatestReviewState, which has no associated author).
	ChangesRequestedBy []string
	HeadSHA            string
	// Mergeable is the PR's mergeability as last polled: nil when unknown/not
	// yet fetched, true when mergeable, false when conflicting. Surfaced so a
	// conflict-after-green is detectable without attempting a merge. Set by the
	// display poller from the PR fetch, not derived by ComputeDisplayStatus.
	Mergeable *bool
}

// ComputeDisplayStatus derives a unified display status from PR state, CI checks,
// and review comments. Priority: Merged > Closed > Draft > Conflict > Failing > Checking > Rejected > Review > Approved > Passing > Idle.
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

	reviewRequiredBlock := pr.IsReviewRequiredBlock()

	// Conflict detection. GitHub can report a PR as generally mergeable while
	// also marking it as not rebaseable, which blocks rebase-and-merge. A
	// BLOCKED + REVIEW_REQUIRED gate is not a conflict; it is waiting on approval.
	if pr.HasConflictBlock() {
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
		if c.Conclusion == nil {
			continue
		}
		// Treat anything that completed but didn't pass cleanly as a
		// failure for repair-trigger purposes. CANCELLED most commonly
		// means "the workflow was cancelled because a sibling job failed
		// first" — GitHub itself flags such PRs as mergeStateStatus
		// UNSTABLE — and TIMED_OUT is unambiguously a failure to deliver
		// a green check. NEUTRAL and SKIPPED stay non-failures: they
		// signal "intentionally not run" and don't block.
		switch *c.Conclusion {
		case CheckConclusionFailure, CheckConclusionCancelled, CheckConclusionTimedOut:
			hasFailed = true
		case CheckConclusionSuccess, CheckConclusionNeutral, CheckConclusionSkipped:
			// non-failures — explicit no-op to satisfy exhaustive linter
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
	switch pr.LatestReviewState {
	case ReviewStateChangesRequested:
		hasChangesRequested = true
	case ReviewStateApproved:
		hasApproval = true
	case ReviewStateUnspecified, ReviewStateCommented, ReviewStateDismissed, ReviewStateRequired:
	}
	var changesRequestedBy []string
	for author, state := range latestByAuthor {
		if state == ReviewStateChangesRequested {
			hasChangesRequested = true
			if author != "" {
				changesRequestedBy = append(changesRequestedBy, author)
			}
		}
		if state == ReviewStateApproved {
			hasApproval = true
		}
	}
	sort.Strings(changesRequestedBy)

	// If checks are still running, it's checking (with metadata flags for styling).
	if hasRunning {
		return DisplayInfo{Status: DisplayStatusChecking, HasFailures: hasFailed, HasChangesRequested: hasChangesRequested, ChangesRequestedBy: changesRequestedBy}
	}

	// Rejected takes priority — any outstanding changes_requested blocks approval.
	if hasChangesRequested {
		return DisplayInfo{Status: DisplayStatusRejected, ChangesRequestedBy: changesRequestedBy}
	}

	// When mergeable is unknown we can't confirm passing/approved — show "checking".
	mergeableUnknown := pr.Mergeable == nil

	if reviewRequiredBlock && !mergeableUnknown {
		return DisplayInfo{Status: DisplayStatusReview}
	}

	if hasApproval && !mergeableUnknown {
		return DisplayInfo{Status: DisplayStatusApproved}
	}

	// All checks green, no conflicts, no outstanding reviews = passing.
	if hasChecks && !hasFailed && !mergeableUnknown {
		return DisplayInfo{Status: DisplayStatusPassing}
	}

	// Mergeability hasn't been confirmed — show "checking" until resolved.
	if mergeableUnknown {
		return DisplayInfo{Status: DisplayStatusChecking, HasChangesRequested: hasChangesRequested, ChangesRequestedBy: changesRequestedBy}
	}

	return DisplayInfo{Status: DisplayStatusIdle}
}

// MergeGate mirrors the proto MergeBlock.Gate enum values EXACTLY (keep in sync).
type MergeGate int

const (
	MergeGateUnspecified MergeGate = iota // 0
	MergeGateNone                         // 1
	MergeGateReview                       // 2
	MergeGateCI                           // 3
	MergeGatePending                      // 4
	MergeGateConflict                     // 5
	MergeGateBaseSync                     // 6
	MergeGateDraft                        // 7
)

// Slug is the stable lowercase token used in the merge_session error prefix
// (`merge blocked: gate=<slug>; ...`). bs-epic parses gate=<slug>.
func (g MergeGate) Slug() string {
	switch g {
	case MergeGateNone:
		return "none"
	case MergeGateReview:
		return "review"
	case MergeGateCI:
		return "ci"
	case MergeGatePending:
		return "pending"
	case MergeGateConflict:
		return "conflict"
	case MergeGateBaseSync:
		return "base_sync"
	case MergeGateDraft:
		return "draft"
	case MergeGateUnspecified:
		return "unspecified"
	default:
		return "unspecified"
	}
}

// MergeBlockReason describes why a PR can't merge, derived purely from display state.
type MergeBlockReason struct {
	Gate              MergeGate
	Detail            string
	BlockingReviewers []string
	Status            DisplayStatus
}

// DeriveMergeBlock maps a DisplayStatus (+ metadata) to a structured merge-block
// reason. This is the single source of truth for the enum→gate mapping (do not
// duplicate it). It is pure: it only reads the passed display state.
func DeriveMergeBlock(status DisplayStatus, hasFailures bool, changesRequestedBy []string) MergeBlockReason {
	reason := MergeBlockReason{
		Status:            status,
		BlockingReviewers: changesRequestedBy,
	}

	switch status {
	case DisplayStatusRejected:
		reason.Gate = MergeGateReview
		reason.Detail = reviewBlockDetail(changesRequestedBy)
	case DisplayStatusFailing:
		reason.Gate = MergeGateCI
		reason.Detail = "one or more CI checks are failing"
	case DisplayStatusChecking:
		reason.Gate = MergeGatePending
		reason.Detail = "CI checks are still running or mergeability is unknown"
	case DisplayStatusConflict:
		reason.Gate = MergeGateConflict
		reason.Detail = "the PR has a merge conflict with its base branch"
	case DisplayStatusDraft:
		reason.Gate = MergeGateDraft
		reason.Detail = "the PR is a draft"
	case DisplayStatusUnspecified, DisplayStatusIdle, DisplayStatusPassing,
		DisplayStatusMerged, DisplayStatusClosed, DisplayStatusApproved,
		DisplayStatusReview:
		reason.Gate = MergeGateNone
	default:
		reason.Gate = MergeGateNone
	}

	return reason
}

// divergenceNote explains that the daemon's merge gate trusts its own
// review/CI tracker, so GitHub may still report the PR mergeable.
const divergenceNote = "the daemon's merge gate trusts its own review/CI tracker — GitHub may still report the PR mergeable"

// reviewBlockDetail builds the human-readable one-liner for a review gate,
// naming the outstanding reviewers when their logins are known and degrading
// gracefully to a count-agnostic phrasing when they are not.
func reviewBlockDetail(changesRequestedBy []string) string {
	if len(changesRequestedBy) == 0 {
		return "an outstanding changes-requested review blocks the merge; " + divergenceNote
	}
	noun := "review"
	if len(changesRequestedBy) > 1 {
		noun = "reviews"
	}
	return fmt.Sprintf("%d outstanding changes-requested %s from %s; %s",
		len(changesRequestedBy), noun, strings.Join(changesRequestedBy, ", "), divergenceNote)
}
