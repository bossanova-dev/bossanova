package session

import (
	"context"
	"errors"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/domain/mergepolicy"
)

// repairableConflictBlock reports whether the PR is blocked on a conflict that
// repair can fix. A non-nil error means the answer is unknown this poll (the
// allowed merge strategies could not be read), so the caller keeps its previous
// state rather than guessing conflict or no conflict.
func repairableConflictBlock(ctx context.Context, provider vcs.Provider, repo *models.Repo, prStatus *vcs.PRStatus, logger zerolog.Logger, caller string) (bool, error) {
	if prStatus == nil || repo == nil {
		return false, nil
	}

	// First pass with rebaseStrategy=false: this classifies every
	// strategy-independent state (merge conflict, review-required, unstable,
	// unknown) without a network call. We only fall through to the merge
	// policy when the PR is blocked *solely* on rebaseability — the one case
	// whose repairability depends on how the repo merges. The second
	// ConflictBlockKind call below then reclassifies with that answer applied.
	kind := prStatus.ConflictBlockKind(false)
	if kind.Repairable() || !prStatus.HasRebaseConflictBlock() {
		return kind.Repairable(), nil
	}

	// A rebase-only block is a conflict only when it keeps the PR from
	// merging. MergeSession retries a rebase refusal with squash, so with
	// squash enabled the PR lands as it is and repair has nothing to fix
	// (PR #3220: GitHub said MERGEABLE/CLEAN, canBeRebased=false, and repair
	// ran, found nothing, and was blocked on the base moves that followed).
	//
	// No dated apiversion bump: this corrects the display_status bossd serves
	// for that PR shape (CONFLICT becomes the PR's real status) for every pin.
	// The boss TUI stamps its built Current but can point at a remote daemon of
	// another version, and the web reaches it through version-skewed bosso. A
	// response carries neither rebaseability nor the repo's allowed strategies,
	// so a down-convert cannot tell this shape from any other, and restoring
	// CONFLICT would re-show the false conflict this corrects. The same holds
	// for a poll whose allowed strategies cannot be read: it keeps the status
	// the last poll served instead of showing CONFLICT. See
	// docs/structure/contracts.md#skipping-a-bump-justify-it-in-the-diff.
	blocks, err := mergepolicy.RebaseRefusalBlocksMerge(ctx, provider, repo.OriginURL, string(repo.MergeStrategy))
	if errors.Is(err, mergepolicy.ErrMergeStrategyDisallowed) {
		// Nothing can merge the PR, so repair has nothing to fix either.
		logger.Warn().Err(err).Str("repo", repo.ID).Str("caller", caller).Msg("conflict classification: no merge strategy is enabled")
		return false, nil
	}
	if err != nil {
		return false, err
	}

	return prStatus.ConflictBlockKind(blocks).Repairable(), nil
}
