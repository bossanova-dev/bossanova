package session

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
)

// ReconcilePRAssociations scans active sessions that are missing a PR number
// and attempts to match them to existing PRs by branch name. This handles
// sessions created before a PR existed or where PR creation happened
// out-of-band (e.g. manually via the GitHub UI).
//
// It returns the number of sessions that were updated.
func ReconcilePRAssociations(
	ctx context.Context,
	sessions db.SessionStore,
	repos db.RepoStore,
	provider vcs.Provider,
	logger zerolog.Logger,
) (int64, error) {
	allRepos, err := repos.List(ctx)
	if err != nil {
		return 0, fmt.Errorf("list repos: %w", err)
	}

	var updated int64
	for _, repo := range allRepos {
		active, err := sessions.ListActive(ctx, repo.ID)
		if err != nil {
			logger.Warn().Err(err).Str("repo", repo.ID).Msg("reconcile: list active sessions")
			continue
		}

		// Filter to sessions missing a PR with a non-empty branch name.
		var orphaned []*orphanedSession
		for _, sess := range active {
			if sess.PRNumber == nil && sess.BranchName != "" {
				orphaned = append(orphaned, &orphanedSession{
					id:         sess.ID,
					branchName: sess.BranchName,
				})
			}
		}
		if len(orphaned) == 0 {
			continue
		}

		// Fetch PRs for this repo (only repos with orphaned sessions).
		openPRs, err := provider.ListOpenPRs(ctx, repo.OriginURL)
		if err != nil {
			logger.Warn().Err(err).Str("repo", repo.ID).Msg("reconcile: list open PRs")
			continue
		}
		closedPRs, err := provider.ListClosedPRs(ctx, repo.OriginURL)
		if err != nil {
			logger.Warn().Err(err).Str("repo", repo.ID).Msg("reconcile: list closed PRs")
			continue
		}

		// Build branch→PR map. Open PRs take precedence over closed.
		prByBranch := make(map[string]vcs.PRSummary)
		for _, pr := range closedPRs {
			prByBranch[pr.HeadBranch] = pr
		}
		for _, pr := range openPRs {
			prByBranch[pr.HeadBranch] = pr // overwrites closed
		}

		// Match and update orphaned sessions.
		for _, o := range orphaned {
			pr, ok := prByBranch[o.branchName]
			if !ok {
				continue
			}

			prNum := pr.Number
			prNumPtr := &prNum
			prURL := constructPRURL(repo.OriginURL, pr.Number)
			prURLPtr := &prURL
			if _, err := sessions.Update(ctx, o.id, db.UpdateSessionParams{
				PRNumber: &prNumPtr,
				PRURL:    &prURLPtr,
			}); err != nil {
				logger.Warn().Err(err).
					Str("session", o.id).
					Int("pr", pr.Number).
					Msg("reconcile: update session")
				continue
			}

			updated++
			logger.Info().
				Str("session", o.id).
				Str("branch", o.branchName).
				Int("pr", pr.Number).
				Msg("reconciled session with existing PR")
		}
	}

	return updated, nil
}

// orphanedSession is a lightweight struct for tracking sessions that need PR
// reconciliation, avoiding carrying the full models.Session around.
type orphanedSession struct {
	id         string
	branchName string
}

// constructPRURL is a package-local alias for vcs.ConstructPRURL.
func constructPRURL(originURL string, prNumber int) string {
	return vcs.ConstructPRURL(originURL, prNumber)
}
