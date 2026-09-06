package vcs

import (
	"errors"
	"fmt"
)

// ErrRepoNotReady is returned when a repository does not have enough commit
// history to support pull request creation (e.g. the repo only contains an
// --allow-empty initial commit with no real content).
var ErrRepoNotReady = errors.New("repository is not ready for pull requests: push at least one commit with content before creating a session")

// ErrPRAlreadyExists is returned when the remote already has an open pull
// request for the requested head/base branch pair.
var ErrPRAlreadyExists = errors.New("pull request already exists")

// ErrGitHubAuthUnavailable is returned when the daemon's gh CLI could not
// authenticate to GitHub at all — it is logged out, or (the case this sentinel
// was added for) it cannot read its stored token and silently fell back to
// unauthenticated requests.
//
// It exists so the session layer can branch on a sentinel rather than on gh's
// error text: the provider owns the string signatures, and everything upstream
// tests it with errors.Is. It is deliberately distinct from a transient failure
// — an auth outage persists until an operator fixes credentials, and retrying it
// only burns budget while reporting the wrong cause.
var ErrGitHubAuthUnavailable = errors.New("the daemon's gh CLI could not authenticate to GitHub")

// PRState represents the state of a pull/merge request.
type PRState int

const (
	PRStateOpen PRState = iota + 1
	PRStateClosed
	PRStateMerged
)

// CheckStatus represents the status of a CI check run.
type CheckStatus int

const (
	CheckStatusQueued CheckStatus = iota + 1
	CheckStatusInProgress
	CheckStatusCompleted
)

// CheckConclusion represents the conclusion of a completed CI check.
type CheckConclusion int

const (
	CheckConclusionSuccess CheckConclusion = iota + 1
	CheckConclusionFailure
	CheckConclusionNeutral
	CheckConclusionCancelled
	CheckConclusionSkipped
	CheckConclusionTimedOut
)

// ReviewState represents the state of a PR review.
type ReviewState int

const (
	ReviewStateUnspecified ReviewState = iota
	ReviewStateApproved
	ReviewStateChangesRequested
	ReviewStateCommented
	ReviewStateDismissed
	ReviewStateRequired
)

// MergeStateStatus represents GitHub's mergeStateStatus value for a PR.
type MergeStateStatus int

const (
	MergeStateStatusUnspecified MergeStateStatus = iota
	MergeStateStatusClean
	MergeStateStatusBlocked
	MergeStateStatusBehind
	MergeStateStatusDirty
	MergeStateStatusDraft
	MergeStateStatusHasHooks
	MergeStateStatusUnknown
	MergeStateStatusUnstable
)

// ConflictBlockKind classifies why a PR cannot currently be merged.
type ConflictBlockKind int

const (
	ConflictBlockNone ConflictBlockKind = iota
	ConflictBlockMerge
	ConflictBlockRebase
	ConflictBlockReviewRequired
	ConflictBlockUnstable
	ConflictBlockUnknown
)

func (k ConflictBlockKind) Repairable() bool {
	return k == ConflictBlockMerge || k == ConflictBlockRebase
}

// String returns a stable, human-readable label for the block kind. It is the
// single source of truth for rendering a ConflictBlockKind (e.g. in repair
// error messages), so callers must not maintain their own enum→string maps.
func (k ConflictBlockKind) String() string {
	switch k {
	case ConflictBlockNone:
		return "none"
	case ConflictBlockMerge:
		return "merge"
	case ConflictBlockRebase:
		return "rebase"
	case ConflictBlockReviewRequired:
		return "review-required"
	case ConflictBlockUnstable:
		return "unstable"
	case ConflictBlockUnknown:
		return "unknown"
	default:
		return fmt.Sprintf("unrecognized(%d)", int(k))
	}
}

// PRStatus represents the current status of a pull/merge request.
type PRStatus struct {
	State               PRState
	Mergeable           *bool
	Rebaseable          *bool
	MergeStateStatus    MergeStateStatus
	Draft               bool
	Title               string
	HeadBranch          string
	BaseBranch          string
	HeadSHA             string
	LatestReviewState   ReviewState
	ReviewDecisionState ReviewState
}

// IsReviewRequiredBlock reports GitHub's "blocked only because approval is
// required" state. GitHub can mark these PRs rebaseable:false even though no
// code conflict exists.
func (pr *PRStatus) IsReviewRequiredBlock() bool {
	if pr == nil {
		return false
	}

	reviewState := pr.ReviewDecisionState
	if reviewState == ReviewStateUnspecified {
		reviewState = pr.LatestReviewState
	}

	return pr.Mergeable != nil &&
		*pr.Mergeable &&
		pr.MergeStateStatus == MergeStateStatusBlocked &&
		reviewState == ReviewStateRequired
}

// HasConflictBlock reports strategy-independent PR states that should trigger
// conflict repair.
func (pr *PRStatus) HasConflictBlock() bool {
	return pr.ConflictBlockKind(false) == ConflictBlockMerge
}

// HasRebaseConflictBlock reports PR states that only block rebase-and-merge.
func (pr *PRStatus) HasRebaseConflictBlock() bool {
	if pr == nil {
		return false
	}
	return pr.Rebaseable != nil && !*pr.Rebaseable && !pr.IsReviewRequiredBlock()
}

// ConflictBlockKind reports the PR block category after applying the repository
// merge strategy. Only repairable kinds should trigger conflict repair.
func (pr *PRStatus) ConflictBlockKind(rebaseStrategy bool) ConflictBlockKind {
	if pr == nil {
		return ConflictBlockNone
	}
	if pr.IsReviewRequiredBlock() {
		return ConflictBlockReviewRequired
	}
	if pr.MergeStateStatus == MergeStateStatusDirty {
		return ConflictBlockMerge
	}
	if pr.Mergeable != nil && !*pr.Mergeable {
		return ConflictBlockMerge
	}
	switch pr.MergeStateStatus {
	case MergeStateStatusUnstable, MergeStateStatusBlocked, MergeStateStatusHasHooks:
		return ConflictBlockUnstable
	case MergeStateStatusUnknown, MergeStateStatusUnspecified:
		if pr.Mergeable == nil {
			return ConflictBlockUnknown
		}
	case MergeStateStatusClean, MergeStateStatusBehind, MergeStateStatusDirty, MergeStateStatusDraft:
	}
	if rebaseStrategy && pr.HasRebaseConflictBlock() {
		return ConflictBlockRebase
	}
	return ConflictBlockNone
}

// CheckResult represents the result of a single CI check.
type CheckResult struct {
	ID     string
	Name   string
	Status CheckStatus
	// Unclassified means the provider could not classify the upstream status
	// string; verdict evaluation treats it as unknown, never as a pass.
	Unclassified bool
	Conclusion   *CheckConclusion
}

// ReviewComment represents a review comment on a PR.
type ReviewComment struct {
	Author string
	Body   string
	State  ReviewState
	Path   *string
	Line   *int
}

// PRSummary is a lightweight representation of a PR for listing.
type PRSummary struct {
	Number     int
	Title      string
	HeadBranch string
	State      PRState
	Author     string
}

// CreatePROpts contains options for creating a new pull/merge request.
type CreatePROpts struct {
	RepoPath   string
	HeadBranch string
	BaseBranch string
	Title      string
	Body       string
	Draft      bool
}

// PRInfo is the result of creating a pull/merge request.
type PRInfo struct {
	Number int
	URL    string
}

// ChecksOverall represents the aggregate status of all checks on a PR.
type ChecksOverall int

const (
	ChecksOverallPending ChecksOverall = iota + 1
	ChecksOverallPassed
	ChecksOverallFailed
)
