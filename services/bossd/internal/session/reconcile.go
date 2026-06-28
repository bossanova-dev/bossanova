package session

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
)

const defaultPRAssociationCacheTTL = 60 * time.Second

// LiveBranchResolver resolves the branch currently checked out in a worktree.
// Implemented by *git.Manager (see Manager.CurrentBranch). Optional: when nil,
// the reconciler matches PRs by the stored branch name only (legacy behavior).
type LiveBranchResolver interface {
	CurrentBranch(ctx context.Context, worktreePath string) (string, error)
}

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
	pr            vcs.PRSummary
	originURL     string
	matchedBranch string
}

// PRAssociationResolver attaches active sessions to existing PRs by exact head
// branch while caching PR listings per repo.
type PRAssociationResolver struct {
	sessions db.SessionStore
	repos    db.RepoStore
	provider vcs.Provider
	logger   zerolog.Logger
	branches LiveBranchResolver
	cronJobs db.CronJobStore
	notify   func(context.Context, *models.Session)

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

// WithBranchResolver attaches a live-branch resolver so the reconciler can
// match PRs against the worktree's current branch in addition to the stored
// branch name. Returns the resolver for chaining.
func (r *PRAssociationResolver) WithBranchResolver(branches LiveBranchResolver) *PRAssociationResolver {
	r.branches = branches
	return r
}

// WithCronJobs attaches a cron-job store so the reconciler can repair cron
// sessions whose title is still the cron job's name even though they already
// have a PR (the placeholder / pr_failed finalize branches never sync the
// agent's PR title onto the session row). Without it the stale-title repair
// pass is skipped. Returns the resolver for chaining.
func (r *PRAssociationResolver) WithCronJobs(cronJobs db.CronJobStore) *PRAssociationResolver {
	r.cronJobs = cronJobs
	return r
}

// WithUpdateNotifier attaches a callback invoked once per session the
// reconciler updates (PR association + rename to the PR title). It exists so
// the PR-title rename reaches the cloud/web: reconcile writes directly through
// the store, bypassing the Server's UpdateSession RPC, so without this hook no
// SessionDelta_KIND_UPDATED event is published and bosso shows a stale title
// until the next full daemon snapshot. Returns the resolver for chaining.
func (r *PRAssociationResolver) WithUpdateNotifier(notify func(context.Context, *models.Session)) *PRAssociationResolver {
	r.notify = notify
	return r
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

		// Correct a drifted branch name. The agent may have created its own
		// branch (e.g. cron sessions whose agent runs `git checkout -b`); the
		// matched PR proves the live branch is the real one, so persist it. This
		// also repairs the finalize/push path, which verifies the worktree is on
		// session.BranchName before pushing.
		if match.matchedBranch != "" && match.matchedBranch != sess.BranchName {
			branch := match.matchedBranch
			updateParams.BranchName = &branch
		}

		// Rename the session to the PR title now that we've identified the PR.
		// One-shot: once PRNumber is set, sessionNeedsPRAssociation returns false,
		// so this never repeatedly clobbers a later user-edited title.
		if title := strings.TrimSpace(match.pr.Title); title != "" {
			updateParams.Title = &title
		}

		clearDraftPRBlockedReasonUpdate(sess.BlockedReason, &updateParams)

		updatedSess, err := r.sessions.Update(ctx, sess.ID, updateParams)
		if err != nil {
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

		// Publish the update (PR association + rename to the PR title) so the
		// cloud/web stays in sync; this store write bypasses the Server RPC
		// that would otherwise emit the event.
		if r.notify != nil && updatedSess != nil {
			r.notify(ctx, updatedSess)
		}
	}

	updated += r.repairStaleCronTitles(ctx, sessions)

	return updated, nil
}

// repairStaleCronTitles renames cron sessions that already have a PR but whose
// title is still the cron job's name. The PR-association loop above only ever
// touches sessions whose PRNumber is nil, so a cron session that finalized via
// the placeholder / pr_failed branch keeps the cron job name forever even
// though the agent set a real title on the GitHub PR. This pass closes that gap
// (and auto-heals any already-stuck rows). It is a no-op without a cron-job
// store (see WithCronJobs).
func (r *PRAssociationResolver) repairStaleCronTitles(ctx context.Context, sessions []*models.Session) int64 {
	if r.cronJobs == nil {
		return 0
	}
	var repaired int64
	for _, sess := range sessions {
		if sess == nil || sess.ArchivedAt != nil || sess.PRNumber == nil ||
			sess.CronJobID == nil || *sess.CronJobID == "" {
			continue
		}
		updatedSess, changed, err := adoptPRTitleWhenCronTitleStale(
			ctx, r.sessions, r.repos, r.cronJobs, r.provider, r.logger, sess)
		if err != nil {
			r.logger.Warn().Err(err).
				Str("session", sess.ID).
				Msg("reconcile: repair stale cron title")
			continue
		}
		if !changed {
			continue
		}
		repaired++
		if r.notify != nil && updatedSess != nil {
			r.notify(ctx, updatedSess)
		}
	}
	return repaired
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

	// Pass 1: the stored branch name (cheap: no git shell-out). This is the
	// common case for ordinary sessions whose branch never drifts.
	if m := matchPRByBranch(prs, s.BranchName, repo.OriginURL); m != nil {
		return m, nil
	}

	// Pass 2: the worktree's live branch. Cron/auto-implement agents create
	// their own branch (e.g. `git checkout -b dave/won-...`) and open the PR
	// there, so the stored cron branch never matches. Resolve lazily so we only
	// shell out to git when the stored branch missed.
	if live := r.liveBranchForSession(ctx, s); live != "" && live != s.BranchName {
		if m := matchPRByBranch(prs, live, repo.OriginURL); m != nil {
			return m, nil
		}
	}

	return nil, nil
}

// matchPRByBranch returns the first open PR whose head branch equals branch.
func matchPRByBranch(prs []vcs.PRSummary, branch, originURL string) *prAssociationMatch {
	if branch == "" {
		return nil
	}
	for _, pr := range prs {
		if pr.HeadBranch == branch {
			return &prAssociationMatch{pr: pr, originURL: originURL, matchedBranch: branch}
		}
	}
	return nil
}

// liveBranchForSession resolves the worktree's current branch, or "" when it
// can't be determined (no resolver, no worktree path, detached HEAD, missing or
// archived worktree). Failures are logged at debug and swallowed so PR
// association degrades to stored-branch matching rather than erroring.
func (r *PRAssociationResolver) liveBranchForSession(ctx context.Context, s *models.Session) string {
	if r.branches == nil || s.WorktreePath == "" {
		return ""
	}
	branch, err := r.branches.CurrentBranch(ctx, s.WorktreePath)
	if err != nil {
		r.logger.Debug().Err(err).
			Str("session", s.ID).
			Str("worktree", s.WorktreePath).
			Msg("reconcile: resolve live worktree branch")
		return ""
	}
	return strings.TrimSpace(branch)
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
