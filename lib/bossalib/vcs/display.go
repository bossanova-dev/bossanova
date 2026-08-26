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
	checkVerdict := EvaluateChecks(pr.HeadSHA, checks, nil)
	hasFailed := checkVerdict.State == CheckVerdictFailing
	checksChecking := checkVerdict.State == CheckVerdictPending ||
		checkVerdict.Reason == CheckVerdictReasonUnreadable ||
		checkVerdict.Reason == CheckVerdictReasonUnclassified

	// If all checks are complete and some failed, it's failing.
	if hasFailed {
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

	// If checks are still running or unclassifiable, it's checking (with metadata flags for styling).
	if checksChecking {
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
	if checkVerdict.IsGreen() && !mergeableUnknown {
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
// (`merge blocked: gate=<slug>; ...`). boss-epic parses gate=<slug>.
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

// remedyNote says what actually clears a review block, and is deliberately
// narrower than "post a new review".
//
// The two remedies are NOT interchangeable, and the ordering here is load-
// bearing. A newer approving or dismissing review from the blocking login
// always clears the block (a later *empty* COMMENTED review does not, which is
// why this does not say "any later review"). Resolving threads only clears a
// block that was SYNTHESIZED out of a bot's unresolved threads: that promotion
// is recomputed from scratch on every review read and gated on the login still
// owning a BLOCKING one — unresolved and not outdated, per the next paragraph —
// so emptying that set drops it.
//
// Per BOS-665 that promotion now also skips a thread GitHub has marked
// OUTDATED — one anchored to a diff hunk that no longer exists at HEAD, whose
// isResolved stays false forever. Previously such a thread kept blocking, which
// livelocked PRs against noisy review bots: nothing an operator could do at
// HEAD would ever clear it. Outdated threads are therefore already
// non-blocking, and the note must not send an operator hunting for the ones
// GitHub hides from the default PR view.
//
// Accepted tradeoff: this genuinely reduces gate strictness. A bot's finding
// now stops blocking if the code under it is merely rewritten, even when the
// finding was never actually addressed. Two things bound that, and both are
// why it is acceptable rather than merely tolerated:
//
//  1. Bot-scoped only. The provider consults blocking-thread authors only
//     when it found bot review users at all, and those are logins with a
//     `[bot]` suffix whose LATEST review is COMMENTED. Human reviewers, and
//     native CHANGES_REQUESTED reviews, never reach this code path.
//  2. These bots re-review every push. The same behaviour that created the
//     livelock re-raises a still-valid concern against the new hunk, so the
//     loop's cause is also the fix's safety net. This holds for the
//     bossanova-driven ways threads go outdated too, not just an author
//     rewriting the hunk: a `should_keep_branches_current` rebase can
//     mass-outdate threads without anything being addressed, but the
//     force-push it emits is a `synchronize` event, so the same re-review
//     fires.
//
// A native CHANGES_REQUESTED review — an ordinary human request, or a bot that
// can submit one directly — is returned by GetReviewComments with that state
// regardless of thread state, so resolving its threads changes nothing and
// ComputeDisplayStatus keeps rejecting. Stating thread resolution
// unconditionally would send an operator to a remedy that cannot work, so it is
// scoped to the synthesized case. It cannot be resolved by inspecting the login
// either: a `[bot]` suffix does not imply the block was synthesized.
//
// It says "owned by", not "from": the no-login branch below must not contain a
// "from ..." clause, which is the attribution TestDeriveMergeBlockReviewDetail
// pins as absent there.
const remedyNote = "clears when the blocking login submits a newer approving/dismissing review; when the block was synthesized out of a bot's review threads, resolving every unresolved, non-outdated review thread owned by that login also clears it — outdated threads are already non-blocking, so they need no action"

// reviewBlockDetail builds the human-readable one-liner for a review gate,
// naming the outstanding reviewers when their logins are known and degrading
// gracefully to a count-agnostic phrasing when they are not. Both phrasings
// carry the remedy clause: a blocked operator needs to know what clears it.
func reviewBlockDetail(changesRequestedBy []string) string {
	if len(changesRequestedBy) == 0 {
		return "an outstanding changes-requested review blocks the merge; " + divergenceNote + "; " + remedyNote
	}
	noun := "review"
	if len(changesRequestedBy) > 1 {
		noun = "reviews"
	}
	return fmt.Sprintf("%d outstanding changes-requested %s from %s; %s; %s",
		len(changesRequestedBy), noun, strings.Join(changesRequestedBy, ", "), divergenceNote, remedyNote)
}
