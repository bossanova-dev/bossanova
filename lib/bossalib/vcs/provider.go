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

	// SearchPRsByTitleTag returns pull/merge requests across all states
	// (open, closed, and merged) whose title carries the given tracker tag
	// (e.g. "BOS-289", matched as the bracketed form "[BOS-289]"). It is used
	// to detect a ticket that has already shipped or is in flight on a sibling
	// branch that has no live session row. The caller is responsible for
	// filtering by state (e.g. treating only open/merged PRs as blocking).
	SearchPRsByTitleTag(ctx context.Context, repoPath, tag string) ([]PRSummary, error)

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

// ReviewObserver is an OPTIONAL Provider capability, type-asserted rather than
// required. A provider whose GetReviewComments filters non-actionable reviews
// out of its result cannot answer "did an external reviewer run at all?" from
// that result: the GitHub provider deliberately DROPS a bot's COMMENTED review
// once that bot's threads are all resolved, so a bot that reviewed and was fully
// addressed and a bot that never ran both leave zero bot entries behind. Any
// count taken after the filter therefore reports "no reviewer" on precisely the
// healthy, fully-addressed PRs that are ready to merge.
//
// A provider implements ReviewObserver to report the raw observation its
// filtering discards. It is kept off Provider on purpose: only the merge gate's
// observability warning needs the raw tally, so a provider that filters nothing
// — and every test double — stays complete without it. Callers MUST type-assert
// and degrade gracefully when it is absent.
type ReviewObserver interface {
	// GetReviewObservation reports the reviews the provider saw on a PR BEFORE
	// any actionable-comment filtering.
	GetReviewObservation(ctx context.Context, repoPath string, prID int) (ReviewObservation, error)
}

// ReviewObservation is the raw, pre-filtering review tally for a pull/merge
// request. It answers only "what was submitted", never "what is blocking" —
// the gate decision belongs to GetReviewComments.
type ReviewObservation struct {
	// Total counts reviews in any state from any author.
	Total int

	// Bot counts reviews authored by a bot account. Zero means no external
	// review bot submitted a review at all, which is the empty-safety-net
	// condition the merge gate warns on: the gate can only block on evidence a
	// reviewer produced, so an uninstalled, rate-limited or silent bot leaves
	// it with nothing to block on.
	Bot int
}

// ErrReviewThreadsUnverified is returned by GetReviewComments when the
// provider could not determine which review threads are unresolved — the
// thread query failed, its response was unparseable, or pagination could not
// continue. Callers must treat the review state as unknown rather than as
// "nothing is blocking": the merge gate blocks on it instead of proceeding.
var ErrReviewThreadsUnverified = errors.New("review thread state could not be verified")
