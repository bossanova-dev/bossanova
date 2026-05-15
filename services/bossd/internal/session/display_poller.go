package session

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/safego"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/status"
)

const (
	webhookHealthyWindow   = 5 * time.Minute
	webhookHealthyInterval = 5 * time.Minute
)

// DisplayPoller periodically polls PR status, checks, and reviews for all
// active sessions with PRs and updates the DisplayTracker with computed display statuses.
type DisplayPoller struct {
	sessions  db.SessionStore
	repos     db.RepoStore
	provider  vcs.Provider
	tracker   *status.DisplayTracker
	snapshots db.CheckSnapshotStore // optional; nil disables persistence
	interval  time.Duration
	logger    zerolog.Logger
	done      chan struct{}

	refreshMu            sync.Mutex
	latestWebhookRefresh map[string]time.Time
	lastPollMu           sync.Mutex
	lastPoll             map[string]time.Time
}

// NewDisplayPoller creates a new display status poller.
func NewDisplayPoller(
	sessions db.SessionStore,
	repos db.RepoStore,
	provider vcs.Provider,
	tracker *status.DisplayTracker,
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
		done:     make(chan struct{}),
	}
}

// SetSnapshotStore wires an optional CheckSnapshotStore. When set, every
// successful pollSession persists what the daemon saw + the DisplayStatus
// it computed, so `boss session checks <id>` can show the timeline.
// nil-safe — leaving the store unset disables persistence (handy for
// tests that don't want SQLite writes on every tick).
func (p *DisplayPoller) SetSnapshotStore(s db.CheckSnapshotStore) {
	p.snapshots = s
}

// Run starts the polling loop in a background goroutine. It stops when the
// context is cancelled.
func (p *DisplayPoller) Run(ctx context.Context) {
	safego.Go(p.logger, func() {
		defer close(p.done)

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

// Done returns a channel closed when Run's goroutine exits.
func (p *DisplayPoller) Done() <-chan struct{} { return p.done }

func (p *DisplayPoller) recordRefresh(repo string, ts time.Time) {
	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()
	if p.latestWebhookRefresh == nil {
		p.latestWebhookRefresh = make(map[string]time.Time)
	}
	if prev, ok := p.latestWebhookRefresh[repo]; !ok || ts.After(prev) {
		p.latestWebhookRefresh[repo] = ts
	}
}

func (p *DisplayPoller) intervalFor(repo string, now time.Time) time.Duration {
	p.refreshMu.Lock()
	last, ok := p.latestWebhookRefresh[repo]
	if ok && now.Sub(last) > webhookHealthyWindow {
		delete(p.latestWebhookRefresh, repo)
		ok = false
	}
	p.refreshMu.Unlock()
	if ok && now.Sub(last) <= webhookHealthyWindow {
		return webhookHealthyInterval
	}
	return p.interval
}

func (p *DisplayPoller) markPolled(sessionID string, ts time.Time) {
	p.lastPollMu.Lock()
	defer p.lastPollMu.Unlock()
	if p.lastPoll == nil {
		p.lastPoll = make(map[string]time.Time)
	}
	p.lastPoll[sessionID] = ts
}

func (p *DisplayPoller) shouldPollSession(sessionID, repo string, now time.Time) bool {
	interval := p.intervalFor(repo, now)

	p.lastPollMu.Lock()
	defer p.lastPollMu.Unlock()
	if last, ok := p.lastPoll[sessionID]; ok && now.Sub(last) < interval {
		return false
	}
	if p.lastPoll == nil {
		p.lastPoll = make(map[string]time.Time)
	}
	p.lastPoll[sessionID] = now
	return true
}

func (p *DisplayPoller) pruneWebhookRefreshes(now time.Time) {
	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()
	for repo, last := range p.latestWebhookRefresh {
		if now.Sub(last) > webhookHealthyWindow {
			delete(p.latestWebhookRefresh, repo)
		}
	}
}

func (p *DisplayPoller) pruneLastPoll(active map[string]struct{}) {
	p.lastPollMu.Lock()
	defer p.lastPollMu.Unlock()
	for sessionID := range p.lastPoll {
		if _, ok := active[sessionID]; !ok {
			delete(p.lastPoll, sessionID)
		}
	}
}

func (p *DisplayPoller) RefreshPR(ctx context.Context, repoOriginURL string, prNumber int) error {
	now := time.Now()
	repo, err := p.repos.GetByOrigin(ctx, repoOriginURL)
	if err != nil {
		return fmt.Errorf("display poller: get repo by origin %q: %w", repoOriginURL, err)
	}

	sessions, err := p.sessions.ListActive(ctx, repo.ID)
	if err != nil {
		return fmt.Errorf("display poller: list active sessions for repo %q: %w", repo.ID, err)
	}

	refreshed := 0
	refreshedSessions := make([]string, 0, len(sessions))
	for _, sess := range sessions {
		if sess.PRNumber == nil || *sess.PRNumber != prNumber {
			continue
		}
		if entry := p.tracker.Get(sess.ID); entry != nil && entry.Status == vcs.DisplayStatusMerged {
			continue
		}
		p.pollSession(ctx, repo.OriginURL, sess.ID, *sess.PRNumber)
		refreshed++
		refreshedSessions = append(refreshedSessions, sess.ID)
	}
	if refreshed > 0 {
		for _, sessionID := range refreshedSessions {
			p.markPolled(sessionID, now)
		}
		p.recordRefresh(repo.OriginURL, now)
	}

	p.logger.Info().
		Str("repo_origin_url", repoOriginURL).
		Int("pr_number", prNumber).
		Int("sessions_refreshed", refreshed).
		Msg("display poller: refresh pr")
	return nil
}

// poll iterates all active sessions with PRs and updates display statuses.
func (p *DisplayPoller) poll(ctx context.Context) {
	now := time.Now()
	p.pruneWebhookRefreshes(now)
	activeSessions := make(map[string]struct{})
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
			if entry := p.tracker.Get(sess.ID); entry != nil && entry.Status == vcs.DisplayStatusMerged {
				continue
			}
			activeSessions[sess.ID] = struct{}{}
			if !p.shouldPollSession(sess.ID, repo.OriginURL, now) {
				continue
			}
			p.pollSession(ctx, repo.OriginURL, sess.ID, *sess.PRNumber)
		}
	}
	p.pruneLastPoll(activeSessions)
}

// pollSession fetches PR status, checks, and reviews for a single session
// and updates the tracker with the computed display status.
func (p *DisplayPoller) pollSession(ctx context.Context, repoPath, sessionID string, prNumber int) {
	prStatus, err := p.provider.GetPRStatus(ctx, repoPath, prNumber)
	if err != nil {
		p.logger.Warn().Err(err).Str("session", sessionID).Msg("display poller: get PR status")
		return
	}

	if prStatus.State == vcs.PRStateMerged || prStatus.State == vcs.PRStateClosed {
		info := vcs.ComputeDisplayStatus(prStatus, nil, nil)
		info.HeadSHA = prStatus.HeadSHA
		p.tracker.Set(sessionID, info)
		p.persistSnapshot(ctx, sessionID, prStatus, nil, info)
		return
	}

	// Skip checks and reviews for draft PRs — they aren't ready for review
	// so CI results and review comments are not actionable. This saves 2 API
	// calls per draft PR per poll cycle.
	if prStatus.Draft {
		info := vcs.ComputeDisplayStatus(prStatus, nil, nil)
		info.HeadSHA = prStatus.HeadSHA
		p.tracker.Set(sessionID, info)
		return
	}

	// On any inputs error, skip the update rather than recomputing with empty
	// results. A transient GitHub API blip would otherwise collapse a
	// "Failing" or "Rejected" row to "Idle" / "Passing" — silently
	// disabling the repair plugin (which only triggers on
	// FAILING/CONFLICT/REJECTED). The previous tracker entry sticks; the
	// next poll cycle retries.
	checks, err := p.provider.GetCheckResults(ctx, repoPath, prNumber)
	if err != nil {
		p.logger.Warn().Err(err).Str("session", sessionID).Msg("display poller: get check results; preserving previous status")
		return
	}

	reviews, err := p.provider.GetReviewComments(ctx, repoPath, prNumber)
	if err != nil {
		p.logger.Warn().Err(err).Str("session", sessionID).Msg("display poller: get review comments; preserving previous status")
		return
	}

	info := vcs.ComputeDisplayStatus(prStatus, checks, reviews)
	info.HeadSHA = prStatus.HeadSHA
	p.tracker.Set(sessionID, info)
	p.persistSnapshot(ctx, sessionID, prStatus, checks, info)
}

func (p *DisplayPoller) persistSnapshot(ctx context.Context, sessionID string, prStatus *vcs.PRStatus, checks []vcs.CheckResult, info vcs.DisplayInfo) {
	if p.snapshots != nil {
		raw, err := json.Marshal(checks)
		if err != nil {
			p.logger.Warn().Err(err).Str("session", sessionID).Msg("display poller: marshal checks for snapshot")
			return
		}
		if err := p.snapshots.Insert(ctx, db.CheckSnapshot{
			SessionID:      sessionID,
			HeadSHA:        prStatus.HeadSHA,
			RawJSON:        string(raw),
			ComputedStatus: int(info.Status),
		}); err != nil {
			p.logger.Warn().Err(err).Str("session", sessionID).Msg("display poller: persist check snapshot")
		}
	}
}
