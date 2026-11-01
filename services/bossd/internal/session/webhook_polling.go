package session

import (
	"context"
	"slices"
	"time"

	"github.com/recurser/bossalib/models"
)

// webhookSafetyNetInterval is how often the GitHub pollers still poll a PR in
// a repo whose webhooks are known to reach this daemon (WebhookHealth). Every
// change on such a PR arrives as a webhook that refreshes it immediately — the
// display refresh, the callback evaluation and the state-machine PollPR — so
// the scheduled poll only has to catch a delivery bosso dropped.
const webhookSafetyNetInterval = 10 * time.Minute

// parkedFallbackInterval is how often the display poller still reads a parked
// (NEEDS_HUMAN) PR when no branch push can announce a base move: the daemon
// has no webhook tracker (no Boss Cloud login) or the repo's webhooks are
// unhealthy. A longer configured display interval still wins.
const parkedFallbackInterval = 5 * time.Minute

// WebhookHealth reports whether GitHub webhooks are currently reaching this
// daemon for a repo (identified by origin URL or "owner/name").
// upstream.WebhookHealth implements it.
type WebhookHealth interface {
	WebhookDeliveryHealthy(repo string) bool
}

// DisplayWebhookHealth is WebhookHealth plus the display poller's report of a
// PR change it found by polling (NotePolledChange), the evidence behind
// Repo.is_missing_webhooks. upstream.WebhookHealth implements it.
type DisplayWebhookHealth interface {
	WebhookHealth
	NotePolledChange(repo string, since time.Time)
}

// parkedInterval is the poll interval for a parked (NEEDS_HUMAN) session with
// no pending base move. Nothing on a parked PR changes until a person acts or
// its base branch moves, and a base move arrives as a branch push
// (NoteBaseMoved), so a webhook-healthy repo backs off to the safety net; any
// other repo falls back to parkedFallbackInterval. Neither shortens a longer
// configured interval.
func (p *DisplayPoller) parkedInterval(repoOriginURL string) time.Duration {
	if p.health != nil && p.health.WebhookDeliveryHealthy(repoOriginURL) {
		return max(p.interval, webhookSafetyNetInterval)
	}
	return max(p.interval, parkedFallbackInterval)
}

// NoteBaseMoved marks every active session whose PR targets branch in the repo
// due for its next scheduled read, and returns their PR numbers. It reads
// nothing inline: GitHub computes mergeability lazily after a base push, so a
// read in the same second usually reports it unknown. Until a read reports the
// PR's mergeability, or webhookSafetyNetInterval passes, the session keeps the
// configured interval (baseMovePending).
func (p *DisplayPoller) NoteBaseMoved(ctx context.Context, repoOriginURL, branch string) []int {
	repo, err := p.repos.GetByOriginURL(ctx, repoOriginURL)
	if err != nil {
		p.logger.Debug().Err(err).Str("repo_origin_url", repoOriginURL).Msg("display poller: base move for an unknown repo")
		return nil
	}
	sessions, err := p.sessions.ListActive(ctx, repo.ID)
	if err != nil {
		p.logger.Warn().Err(err).Str("repo", repo.ID).Msg("display poller: list sessions for a base move")
		return nil
	}
	candidates := make([]*models.Session, 0, len(sessions))
	for _, sess := range sessions {
		if sess.PRNumber != nil && !isSettledEnd(p.tracker.Get(sess.ID)) {
			candidates = append(candidates, sess)
		}
	}
	now := time.Now()
	var prs []int
	p.lastPollMu.Lock()
	defer p.lastPollMu.Unlock()
	for _, sess := range candidates {
		if p.prBaseBranchLocked(sess) != branch {
			continue
		}
		if p.baseMoved == nil {
			p.baseMoved = make(map[string]time.Time)
		}
		p.baseMoved[sess.ID] = now
		delete(p.lastPoll, sess.ID)
		if !slices.Contains(prs, *sess.PRNumber) {
			prs = append(prs, *sess.PRNumber)
		}
	}
	return prs
}

// prBaseBranchLocked is the base branch of a session's PR: the poller's last
// read of that PR, or the session's own base before the first read. Callers
// hold lastPollMu.
func (p *DisplayPoller) prBaseBranchLocked(sess *models.Session) string {
	if obs, ok := p.observed[sess.ID]; ok && obs.prNumber == *sess.PRNumber && obs.baseBranch != "" {
		return obs.baseBranch
	}
	return sess.BaseBranch
}

// baseMovePending reports whether the session's PR base branch moved less than
// webhookSafetyNetInterval ago and no read has reported its mergeability since.
func (p *DisplayPoller) baseMovePending(sessionID string, now time.Time) bool {
	p.lastPollMu.Lock()
	defer p.lastPollMu.Unlock()
	at, ok := p.baseMoved[sessionID]
	return ok && now.Sub(at) < webhookSafetyNetInterval
}
