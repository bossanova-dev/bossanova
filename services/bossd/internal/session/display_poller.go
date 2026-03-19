package session

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/safego"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/status"
)

// DisplayPoller periodically polls PR status, checks, and reviews for all
// active sessions with PRs and updates the PRTracker with computed display statuses.
type DisplayPoller struct {
	sessions db.SessionStore
	repos    db.RepoStore
	provider vcs.Provider
	tracker  *status.PRTracker
	interval time.Duration
	logger   zerolog.Logger
}

// NewDisplayPoller creates a new display status poller.
func NewDisplayPoller(
	sessions db.SessionStore,
	repos db.RepoStore,
	provider vcs.Provider,
	tracker *status.PRTracker,
	interval time.Duration,
	logger zerolog.Logger,
) *DisplayPoller {
	return &DisplayPoller{
		sessions: sessions,
		repos:    repos,
		provider: provider,
		tracker:  tracker,
		interval: interval,
		logger:   logger,
	}
}

// Run starts the polling loop in a background goroutine. It stops when the
// context is cancelled.
func (p *DisplayPoller) Run(ctx context.Context) {
	safego.Go(p.logger, func() {
		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()

		// Poll immediately on start for initial state.
		p.poll(ctx)

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.poll(ctx)
			}
		}
	})
}

// poll iterates all active sessions with PRs and updates display statuses.
func (p *DisplayPoller) poll(ctx context.Context) {
	repos, err := p.repos.List(ctx)
	if err != nil {
		p.logger.Error().Err(err).Msg("display poller: list repos")
		return
	}

	for _, repo := range repos {
		sessions, err := p.sessions.ListActive(ctx, repo.ID)
		if err != nil {
			p.logger.Error().Err(err).Str("repo", repo.ID).Msg("display poller: list sessions")
			continue
		}

		for _, sess := range sessions {
			if sess.PRNumber == nil {
				continue
			}
			// Skip merged PRs — terminal state, no further polling needed.
			if entry := p.tracker.Get(sess.ID); entry != nil && entry.Status == vcs.PRDisplayStatusMerged {
				continue
			}
			p.pollSession(ctx, repo.OriginURL, sess.ID, *sess.PRNumber)
		}
	}
}

// pollSession fetches PR status, checks, and reviews for a single session
// and updates the tracker with the computed display status.
func (p *DisplayPoller) pollSession(ctx context.Context, repoPath, sessionID string, prNumber int) {
	prStatus, err := p.provider.GetPRStatus(ctx, repoPath, prNumber)
	if err != nil {
		p.logger.Warn().Err(err).Str("session", sessionID).Msg("display poller: get PR status")
		return
	}

	var checks []vcs.CheckResult
	if results, err := p.provider.GetCheckResults(ctx, repoPath, prNumber); err != nil {
		p.logger.Warn().Err(err).Str("session", sessionID).Msg("display poller: get check results")
	} else {
		checks = results
	}

	var reviews []vcs.ReviewComment
	if comments, err := p.provider.GetReviewComments(ctx, repoPath, prNumber); err != nil {
		p.logger.Warn().Err(err).Str("session", sessionID).Msg("display poller: get review comments")
	} else {
		reviews = comments
	}

	info := vcs.ComputeDisplayStatus(prStatus, checks, reviews)
	p.tracker.Set(sessionID, info)
}
