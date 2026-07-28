// Package mergepolicy holds the decision logic shared by bossd's two merge
// entry points: the user-initiated MergeSession RPC in the server package,
// and the dependabot auto-merge path in taskorchestrator. Both need to pick
// a merge strategy compatible with the remote, and both need to verify that
// a PR gh reports as merged actually landed on origin/<base>.
//
// Keeping these decisions in one place prevents the two paths from drifting
// in behavior (e.g. different fallback orders or different "merge not on
// base" sentinels that errors.Is can't unify).
package mergepolicy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/recurser/bossalib/vcs"
)

// ErrMergeStrategyDisallowed is returned by ResolveStrategy when the remote
// has no merge strategies enabled at all. The user must enable at least one
// on GitHub before boss can merge.
var ErrMergeStrategyDisallowed = errors.New("no merge strategy is enabled on the remote repository")

// ErrMergeNotOnBase is returned by VerifyOnBase when a PR gh reports as
// merged does not have its merge commit as an ancestor of origin/<base>.
// This is the specific failure mode that the madverts-core incident
// exposed: gh said "merged" but main's history no longer contained the
// commit (force-push, branch-protection race, etc.).
var ErrMergeNotOnBase = errors.New("PR merge commit is not on base branch")

// ErrMergeStrategyIncompatible is returned by ResolveStrategyForBranch when
// the branch has merge commits GitHub's rebase-merge will structurally
// refuse, and no fallback strategy (squash) is enabled upstream to
// substitute. The sentinel's text is the literal, driver-distinguishable
// token: the server maps it to connect.CodeFailedPrecondition (never
// CodeInternal) so drivers and the repair loop treat it as terminal —
// requiring a user to change the branch or enable squash — rather than
// something worth retrying forever.
var ErrMergeStrategyIncompatible = errors.New("MERGE_STRATEGY_INCOMPATIBLE")

// preferenceOrder is the strategy order used when the configured strategy
// is empty or disabled upstream: prefer "merge" (preserves history), fall
// back to "squash", and use "rebase" only if nothing else is available.
var preferenceOrder = []string{"merge", "squash", "rebase"}

// ResolveStrategy picks a merge strategy for a PR merge. It prefers the
// boss-configured strategy when set and enabled upstream, falls back
// through preferenceOrder otherwise, and returns ErrMergeStrategyDisallowed
// when the remote has no strategies enabled.
//
// If the provider's GetAllowedMergeStrategies call fails (network, auth),
// the configured strategy is returned as-is so gh pr merge can surface the
// real error itself. Empty configured under that condition defaults to
// "merge" (GitHub's default).
func ResolveStrategy(ctx context.Context, provider vcs.Provider, repoPath, configured string) (string, error) {
	allowed, err := provider.GetAllowedMergeStrategies(ctx, repoPath)
	if err != nil {
		if configured != "" {
			return configured, nil
		}
		return "merge", nil
	}
	if len(allowed) == 0 {
		return "", ErrMergeStrategyDisallowed
	}
	if configured != "" && slices.Contains(allowed, configured) {
		return configured, nil
	}
	for _, s := range preferenceOrder {
		if slices.Contains(allowed, s) {
			return s, nil
		}
	}
	// allowed is non-empty but contains no standard strategy — return the
	// first one so gh can at least attempt it.
	return allowed[0], nil
}

// MergeCommitCounter is the narrow slice of a git manager needed to detect
// merge commits on a branch before choosing a merge strategy. Implemented
// by git.Manager.
type MergeCommitCounter interface {
	// CountMergeCommits reports how many merge commits exist on head that are
	// not already on origin/<base>.
	CountMergeCommits(ctx context.Context, localPath, base, head string) (int, error)
}

// ResolveStrategyForBranch wraps ResolveStrategy with merge-commit
// awareness: GitHub's rebase-merge structurally refuses any PR branch that
// contains a merge commit ("GraphQL: This branch can't be rebased"), so a
// resolved "rebase" strategy is downgraded to "squash" when the branch has
// merge commits and squash is enabled upstream — squash flattens the
// history GitHub won't linearize, so it succeeds where rebase can't.
//
// Every failure mode here fails OPEN to the resolved strategy (empty
// substitution reason, nil error): this function only ever prevents a
// strategy substitution, never blocks the merge itself. The one deliberate
// exception is when merge commits are present, only rebase is configured,
// and there is no squash fallback to substitute — that combination cannot
// succeed, so it returns ErrMergeStrategyIncompatible instead of letting
// MergePR fail with GitHub's opaque GraphQL error and the repair loop
// retry it forever.
func ResolveStrategyForBranch(
	ctx context.Context,
	provider vcs.Provider,
	originURL, configured string,
	counter MergeCommitCounter,
	localPath, base, head string,
) (strategy string, substituted string, err error) {
	resolved, err := ResolveStrategy(ctx, provider, originURL, configured)
	if err != nil {
		return "", "", err
	}
	if resolved != "rebase" {
		// Only rebase-merge has GitHub's merge-commit restriction; merge
		// and squash strategies are untouched.
		return resolved, "", nil
	}

	// Fail open when there's nothing to count against: the orchestrator's
	// auto-merge path has no local head branch for some tasks, and the
	// server's test doubles carry a nil worktree manager.
	if counter == nil || localPath == "" || base == "" || head == "" {
		return resolved, "", nil
	}

	n, err := counter.CountMergeCommits(ctx, localPath, base, head)
	if err != nil {
		// Fail open, mirroring the same philosophy ResolveStrategy already
		// uses for provider errors: let MergePR attempt the merge and
		// surface the real error itself rather than blocking here on an
		// error from an auxiliary check.
		return resolved, "", nil
	}
	if n == 0 {
		return resolved, "", nil
	}

	allowed, err := provider.GetAllowedMergeStrategies(ctx, originURL)
	if err != nil {
		// Fail open — same reasoning as the counter error above.
		return resolved, "", nil
	}
	if slices.Contains(allowed, "squash") {
		reason := fmt.Sprintf("branch has %d merge commit(s), which rebase-merge cannot handle; substituted squash", n)
		return "squash", reason, nil
	}

	return "", "", fmt.Errorf("%w: branch has %d merge commit(s), repo strategy is rebase", ErrMergeStrategyIncompatible, n)
}

// IsRebaseRefusal reports whether a MergePR failure is GitHub's structural
// refusal to rebase-merge a branch that contains a merge commit.
//
// This is a post-failure backstop, not the primary defense —
// ResolveStrategyForBranch is meant to catch the incompatibility before
// MergePR is ever called. Matching on error text is inherently brittle
// (GitHub can change the message, and it's not a documented API contract),
// so this exists only to classify a failure that slipped past the
// pre-check, e.g. because CountMergeCommits itself failed open.
func IsRebaseRefusal(strategy string, err error) bool {
	if err == nil || strategy != "rebase" {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "can't be rebased") || strings.Contains(msg, "cannot be rebased")
}

// SquashFallback decides how to recover from a MergePR failure that GitHub
// refused because the branch cannot be rebase-merged. It is the post-failure
// twin of ResolveStrategyForBranch's pre-check, and it lives here rather than
// inside the RPC handler for the same reason the pre-check does: a merge
// policy embedded in a caller is unreusable and drifts, which is exactly what
// this package exists to prevent.
//
// It is NOT a claim that both merge entry points retry with squash. The
// server's MergeSession does; taskorchestrator's unattended dependabot
// auto-merge deliberately does NOT — it classifies the refusal with
// IsRebaseRefusal and enqueues a repair session instead, because an
// unattended run should not silently change the landed-history shape. Do not
// "unify" the orchestrator onto this function without an explicit decision to
// change that behaviour.
//
// The return shape mirrors ResolveStrategyForBranch's
// (strategy, substituted, err):
//
//   - ("squash", reason, nil) — retry the merge with squash; reason is the
//     human-readable note for the log and the RPC response detail.
//   - ("", "", nil) — mergeErr is not a rebase refusal; the caller surfaces it
//     unchanged.
//   - ("", reason, nil) — a rebase refusal whose fallback could not be
//     confirmed (the allowed-strategies read failed). Fails OPEN to today's
//     behaviour: the caller surfaces mergeErr unchanged, and reason explains
//     why no retry was attempted so the degradation is never silent.
//   - ("", "", err) — a rebase refusal with squash provably disabled. err wraps
//     both ErrMergeStrategyIncompatible and mergeErr, so the caller maps it to
//     a terminal FailedPrecondition rather than a retryable CodeInternal.
func SquashFallback(
	ctx context.Context,
	provider vcs.Provider,
	originURL, strategy string,
	mergeErr error,
) (fallback string, reason string, err error) {
	if !IsRebaseRefusal(strategy, mergeErr) {
		return "", "", nil
	}

	allowed, allowedErr := provider.GetAllowedMergeStrategies(ctx, originURL)
	if allowedErr != nil {
		return "", fmt.Sprintf(
			"could not read the remote's allowed merge strategies, so the squash fallback was skipped: %v",
			allowedErr,
		), nil
	}
	if !slices.Contains(allowed, "squash") {
		return "", "", fmt.Errorf("%w: %w", ErrMergeStrategyIncompatible, mergeErr)
	}

	return "squash", "rebase merge was refused by the remote; retried with squash", nil
}

// BaseVerifier is the narrow slice of a git manager needed to verify that a
// PR merge commit actually landed on origin/<base>. Implemented by
// git.Manager and the orchestrator's BaseBranchSyncer.
type BaseVerifier interface {
	// FetchBase fetches origin/<base> so refs/remotes/origin/<base>
	// reflects the current remote state.
	FetchBase(ctx context.Context, localPath, base string) error

	// IsAncestor reports whether ref is an ancestor of target in the repo
	// at localPath. Returns (false, nil) when it isn't — only git
	// invocation failures produce errors.
	IsAncestor(ctx context.Context, localPath, ref, target string) (bool, error)
}

// ErrMergeVerifyInfra marks a VerifyOnBase failure that could not COMPLETE the
// check (provider query error, git fetch error, git invocation error) — as
// opposed to ErrMergeNotOnBase, which is a completed check with a negative
// answer. Callers may re-read the PR via the provider API and treat an
// API-confirmed merge as successful; they must NOT do that for
// ErrMergeNotOnBase.
var ErrMergeVerifyInfra = errors.New("merge verification could not be completed")

// VerifyOnBase fetches origin/<base> and confirms the merge commit gh
// recorded for the PR is now an ancestor of the remote-tracking ref. This
// catches:
//   - History rewrites on origin/<base> after gh pr merge (the madverts case).
//   - Branch-protection races where the merge was queued but not applied.
//   - Merges that landed on a branch other than the one boss expected.
//
// On verification failure, returns an error wrapping ErrMergeNotOnBase with
// enough detail (merge SHA, PR number, target ref) for the user to decide
// whether to cherry-pick, re-merge, or investigate a rewrite.
//
// The three steps before the ancestor comparison (query, fetch, ancestor
// check) can each fail without ever completing the check — e.g. an SSH
// timeout on the verification fetch. Those are wrapped with
// ErrMergeVerifyInfra in addition to the underlying error (two %w verbs, so
// errors.Is still finds both) so callers can distinguish "couldn't tell"
// from "checked, and it's not there." ErrMergeNotOnBase itself is a
// completed check and is never wrapped with ErrMergeVerifyInfra.
func VerifyOnBase(
	ctx context.Context,
	provider vcs.Provider,
	verifier BaseVerifier,
	localPath, originURL, base string,
	prID int,
) error {
	mergeSHA, err := provider.GetPRMergeCommit(ctx, originURL, prID)
	if err != nil {
		return fmt.Errorf("%w: query PR merge commit: %w", ErrMergeVerifyInfra, err)
	}

	if err := verifier.FetchBase(ctx, localPath, base); err != nil {
		return fmt.Errorf("%w: fetch origin/%s for verification: %w", ErrMergeVerifyInfra, base, err)
	}

	target := "refs/remotes/origin/" + base
	ok, err := verifier.IsAncestor(ctx, localPath, mergeSHA, target)
	if err != nil {
		return fmt.Errorf("%w: check ancestor of %s: %w", ErrMergeVerifyInfra, target, err)
	}
	if ok {
		return nil
	}

	return fmt.Errorf(
		"%w: merge commit %s for PR #%d is not on %s; the remote history may have been rewritten after the merge",
		ErrMergeNotOnBase, mergeSHA, prID, target,
	)
}
