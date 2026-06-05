package session

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
)

const defaultPRAssociationCacheTTL = 60 * time.Second

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
	return NewPRAssociationResolver(sessions, repos, provider, logger).Reconcile(ctx)
}

type prCacheEntry struct {
	expiresAt time.Time
	prs       []vcs.PRSummary
}

type prAssociationMatch struct {
	pr        vcs.PRSummary
	originURL string
}

// PRAssociationResolver attaches active sessions to existing PRs by exact head
// branch while caching PR listings per repo.
type PRAssociationResolver struct {
	sessions db.SessionStore
	repos    db.RepoStore
	provider vcs.Provider
	logger   zerolog.Logger

	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time
	prCache map[string]prCacheEntry
}

// NewPRAssociationResolver creates a PR association resolver with the default
// PR cache settings.
func NewPRAssociationResolver(
	sessions db.SessionStore,
	repos db.RepoStore,
	provider vcs.Provider,
	logger zerolog.Logger,
) *PRAssociationResolver {
	return &PRAssociationResolver{
		sessions: sessions,
		repos:    repos,
		provider: provider,
		logger:   logger,
		ttl:      defaultPRAssociationCacheTTL,
		now:      time.Now,
		prCache:  make(map[string]prCacheEntry),
	}
}

func (r *PRAssociationResolver) SetTTLForTest(ttl time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.ttl = ttl
}

func (r *PRAssociationResolver) SetNowForTest(now func() time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if now == nil {
		now = time.Now
	}
	r.now = now
}

// Reconcile scans active sessions missing a PR number and attaches exact
// matching PRs.
func (r *PRAssociationResolver) Reconcile(ctx context.Context) (int64, error) {
	active, err := r.sessions.ListActive(ctx, "")
	if err != nil {
		return 0, fmt.Errorf("list active sessions: %w", err)
	}

	return r.ReconcileSessions(ctx, active)
}

// ReconcileSessions reconciles only the supplied active sessions. It exists for
// caller-scoped repair paths, such as list-time dynamic discovery for already
// visible rows. The full Reconcile method remains the startup/periodic scanner.
func (r *PRAssociationResolver) ReconcileSessions(ctx context.Context, sessions []*models.Session) (int64, error) {
	var updated int64
	for _, sess := range sessions {
		if !sessionNeedsPRAssociation(sess) {
			continue
		}

		match, err := r.findPRMatchForSession(ctx, sess)
		if err != nil {
			r.logger.Warn().Err(err).
				Str("session", sess.ID).
				Str("repo_id", sess.RepoID).
				Str("branch", sess.BranchName).
				Msg("reconcile: find PR for session")
			continue
		}
		if match == nil {
			continue
		}

		prNum := match.pr.Number
		prNumPtr := &prNum
		prURL := constructPRURL(match.originURL, match.pr.Number)
		prURLPtr := &prURL

		updateParams := db.UpdateSessionParams{
			PRNumber: &prNumPtr,
			PRURL:    &prURLPtr,
		}
		clearDraftPRBlockedReasonUpdate(sess.BlockedReason, &updateParams)

		if _, err := r.sessions.Update(ctx, sess.ID, updateParams); err != nil {
			r.logger.Warn().Err(err).
				Str("session", sess.ID).
				Int("pr", match.pr.Number).
				Msg("reconcile: update session")
			continue
		}

		updated++
		r.logger.Info().
			Str("session", sess.ID).
			Str("branch", sess.BranchName).
			Int("pr", match.pr.Number).
			Msg("reconciled session with existing PR")
	}

	return updated, nil
}

func sessionNeedsPRAssociation(sess *models.Session) bool {
	return sess != nil &&
		sess.ArchivedAt == nil &&
		sess.PRNumber == nil &&
		sess.BranchName != ""
}

// findPRMatchForSession returns the first open PR whose head branch exactly
// matches the session branch, or nil when none match. Closed and merged PRs are
// intentionally ignored (see prsForRepo) so a dead PR is never auto-attached to
// a live session.
func (r *PRAssociationResolver) findPRMatchForSession(ctx context.Context, s *models.Session) (*prAssociationMatch, error) {
	if s == nil || s.BranchName == "" || s.PRNumber != nil {
		return nil, nil
	}

	repo, err := r.repos.Get(ctx, s.RepoID)
	if err != nil {
		return nil, fmt.Errorf("get repo %q for session %q: %w", s.RepoID, s.ID, err)
	}

	prs, err := r.prsForRepo(ctx, repo.ID, repo.OriginURL)
	if err != nil {
		return nil, err
	}

	for _, pr := range prs {
		if pr.HeadBranch != s.BranchName {
			continue
		}

		return &prAssociationMatch{
			pr:        pr,
			originURL: repo.OriginURL,
		}, nil
	}

	return nil, nil
}

func (r *PRAssociationResolver) prsForRepo(ctx context.Context, repoID, originURL string) ([]vcs.PRSummary, error) {
	cacheKey := repoID
	if cacheKey == "" {
		cacheKey = originURL
	}

	now := r.now()
	r.mu.Lock()
	if cached, ok := r.prCache[cacheKey]; ok && now.Before(cached.expiresAt) {
		r.mu.Unlock()
		return clonePRSummaries(cached.prs), nil
	}
	r.mu.Unlock()

	openPRs, err := r.provider.ListOpenPRs(ctx, originURL)
	if err != nil {
		return nil, fmt.Errorf("list open PRs for repo %q: %w", repoID, err)
	}

	// Only open PRs are candidates. A closed or merged PR must never be
	// auto-attached to a live session, and skipping the closed-PR listing also
	// avoids a second GitHub API call per repo. See
	// docs/plans/2026-06-03-dynamic-pr-discovery.md.
	candidates := make([]vcs.PRSummary, 0, len(openPRs))
	for _, pr := range openPRs {
		if pr.State == vcs.PRStateOpen {
			candidates = append(candidates, pr)
		}
	}

	r.mu.Lock()
	r.prCache[cacheKey] = prCacheEntry{
		expiresAt: r.now().Add(r.ttl),
		prs:       clonePRSummaries(candidates),
	}
	r.mu.Unlock()
	return candidates, nil
}

func clonePRSummaries(prs []vcs.PRSummary) []vcs.PRSummary {
	if len(prs) == 0 {
		return nil
	}
	cloned := make([]vcs.PRSummary, len(prs))
	copy(cloned, prs)
	return cloned
}

func clearDraftPRBlockedReasonUpdate(reason *string, params *db.UpdateSessionParams) {
	if !isDraftPRBlockedReason(reason) {
		return
	}

	var cleared *string
	params.BlockedReason = &cleared
}

// constructPRURL is a package-local alias for vcs.ConstructPRURL.
func constructPRURL(originURL string, prNumber int) string {
	return vcs.ConstructPRURL(originURL, prNumber)
}
