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
func VerifyOnBase(
	ctx context.Context,
	provider vcs.Provider,
	verifier BaseVerifier,
	localPath, originURL, base string,
	prID int,
) error {
	mergeSHA, err := provider.GetPRMergeCommit(ctx, originURL, prID)
	if err != nil {
		return fmt.Errorf("query PR merge commit: %w", err)
	}

	if err := verifier.FetchBase(ctx, localPath, base); err != nil {
		return fmt.Errorf("fetch origin/%s for verification: %w", base, err)
	}

	target := "refs/remotes/origin/" + base
	ok, err := verifier.IsAncestor(ctx, localPath, mergeSHA, target)
	if err != nil {
		return fmt.Errorf("check ancestor of %s: %w", target, err)
	}
	if ok {
		return nil
	}

	return fmt.Errorf(
		"%w: merge commit %s for PR #%d is not on %s; the remote history may have been rewritten after the merge",
		ErrMergeNotOnBase, mergeSHA, prID, target,
	)
}
