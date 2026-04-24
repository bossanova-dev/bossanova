// Package vcs defines VCS-agnostic interfaces for interacting with
// version control hosting services (GitHub, GitLab, etc.).
package vcs

import (
	"context"
	"errors"
)

// Provider is the interface that VCS hosting implementations must satisfy.
// GitHub is the initial implementation; GitLab and others can be added later.
type Provider interface {
	// CreateDraftPR creates a new draft pull/merge request.
	CreateDraftPR(ctx context.Context, opts CreatePROpts) (*PRInfo, error)

	// GetPRStatus returns the current status of a pull/merge request.
	GetPRStatus(ctx context.Context, repoPath string, prID int) (*PRStatus, error)

	// GetCheckResults returns CI check results for a pull/merge request.
	GetCheckResults(ctx context.Context, repoPath string, prID int) ([]CheckResult, error)

	// GetFailedCheckLogs returns the log output for a specific failed check.
	GetFailedCheckLogs(ctx context.Context, repoPath string, checkID string) (string, error)

	// MarkReadyForReview transitions a draft PR to ready for review.
	MarkReadyForReview(ctx context.Context, repoPath string, prID int) error

	// GetReviewComments returns review comments on a pull/merge request.
	GetReviewComments(ctx context.Context, repoPath string, prID int) ([]ReviewComment, error)

	// ListOpenPRs returns all open pull/merge requests for a repository.
	ListOpenPRs(ctx context.Context, repoPath string) ([]PRSummary, error)

	// ListClosedPRs returns recently-closed (not merged) pull/merge requests.
	ListClosedPRs(ctx context.Context, repoPath string) ([]PRSummary, error)

	// MergePR merges a pull/merge request using the given strategy
	// ("merge", "rebase", or "squash"). An empty strategy defaults to "merge".
	MergePR(ctx context.Context, repoPath string, prID int, strategy string) error

	// UpdatePRTitle updates the title of an existing pull/merge request.
	UpdatePRTitle(ctx context.Context, repoPath string, prID int, title string) error

	// GetPRMergeCommit returns the merge commit SHA the remote has recorded
	// for the given PR. Returns ErrPRNotMerged if the PR is not in a merged
	// state. Used by post-merge verification to confirm the merge actually
	// landed on the base branch.
	GetPRMergeCommit(ctx context.Context, repoPath string, prID int) (string, error)

	// GetAllowedMergeStrategies returns the strategies enabled on the remote
	// repository, in the order "merge", "squash", "rebase". Used as a
	// fallback when the bossanova-configured strategy is empty or disabled
	// upstream.
	GetAllowedMergeStrategies(ctx context.Context, repoPath string) ([]string, error)
}

// ErrPRNotMerged is returned by GetPRMergeCommit when the PR is not in a
// merged state (still open, closed without merge, etc.).
var ErrPRNotMerged = errors.New("PR is not merged")
