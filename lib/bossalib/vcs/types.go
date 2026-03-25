package vcs

import "errors"

// ErrRepoNotReady is returned when a repository does not have enough commit
// history to support pull request creation (e.g. the repo only contains an
// --allow-empty initial commit with no real content).
var ErrRepoNotReady = errors.New("repository is not ready for pull requests: push at least one commit with content before creating a session")

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
	ReviewStateApproved ReviewState = iota + 1
	ReviewStateChangesRequested
	ReviewStateCommented
	ReviewStateDismissed
)

// PRStatus represents the current status of a pull/merge request.
type PRStatus struct {
	State      PRState
	Mergeable  *bool
	Draft      bool
	Title      string
	HeadBranch string
	BaseBranch string
}

// CheckResult represents the result of a single CI check.
type CheckResult struct {
	ID         string
	Name       string
	Status     CheckStatus
	Conclusion *CheckConclusion
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
