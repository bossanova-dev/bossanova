package session

import (
	"context"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/mergepolicy"
)

func repairableConflictBlock(ctx context.Context, provider vcs.Provider, repo *models.Repo, prStatus *vcs.PRStatus, logger zerolog.Logger, caller string) bool {
	return conflictBlockKind(ctx, provider, repo, prStatus, logger, caller).Repairable()
}

func conflictBlockKind(ctx context.Context, provider vcs.Provider, repo *models.Repo, prStatus *vcs.PRStatus, logger zerolog.Logger, caller string) vcs.ConflictBlockKind {
	if prStatus == nil || repo == nil {
		return vcs.ConflictBlockNone
	}

	// First pass with rebaseStrategy=false: this classifies every
	// strategy-independent state (merge conflict, review-required, unstable,
	// unknown) without a network call. We only fall through to resolve the
	// repo merge strategy when the PR is blocked *solely* on rebaseability —
	// the one case whose repairability depends on whether the repo merges via
	// rebase. The second ConflictBlockKind(rebaseStrategy) call below then
	// reclassifies with that strategy applied.
	kind := prStatus.ConflictBlockKind(false)
	if kind.Repairable() || !prStatus.HasRebaseConflictBlock() {
		return kind
	}

	rebaseStrategy := false
	strategy, err := mergepolicy.ResolveStrategy(ctx, provider, repo.OriginURL, string(repo.MergeStrategy))
	if err != nil {
		logger.Warn().Err(err).Str("repo", repo.ID).Str("caller", caller).Msg("resolve merge strategy for conflict classification")
		return vcs.ConflictBlockUnknown
	}
	rebaseStrategy = strategy == string(models.MergeStrategyRebase)

	return prStatus.ConflictBlockKind(rebaseStrategy)
}
