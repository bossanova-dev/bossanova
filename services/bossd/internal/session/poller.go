package session

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/safego"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
)

// DefaultPollInterval is the default interval between CI check polls.
const DefaultPollInterval = 2 * time.Minute

// SessionEvent pairs a VCS event with the session it belongs to.
type SessionEvent struct {
	SessionID string
	Event     vcs.Event
}

// Poller periodically checks CI status for sessions in AwaitingChecks state
// and emits VCS events when status changes are detected.
type Poller struct {
	sessions db.SessionStore
	repos    db.RepoStore
	provider vcs.Provider
	interval time.Duration
	logger   zerolog.Logger
}

// NewPoller creates a new check poller.
func NewPoller(
	sessions db.SessionStore,
	repos db.RepoStore,
	provider vcs.Provider,
	interval time.Duration,
	logger zerolog.Logger,
) *Poller {
	return &Poller{
		sessions: sessions,
		repos:    repos,
		provider: provider,
		interval: interval,
		logger:   logger,
	}
}

// Run starts the polling loop. It sends events on the returned channel and
// stops when the context is cancelled. The caller must consume from the
// channel to prevent blocking.
func (p *Poller) Run(ctx context.Context) <-chan SessionEvent {
	ch := make(chan SessionEvent, 64)
	safego.Go(p.logger, func() {
		defer close(ch)

		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()

		// Poll immediately on start, then on each tick.
		p.poll(ctx, ch)

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.poll(ctx, ch)
			}
		}
	})
	return ch
}

// poll checks all sessions in AwaitingChecks state and emits events.
func (p *Poller) poll(ctx context.Context, ch chan<- SessionEvent) {
	// List all repos to find sessions across all repos.
	repos, err := p.repos.List(ctx)
	if err != nil {
		p.logger.Error().Err(err).Msg("poller: list repos")
		return
	}

	for _, repo := range repos {
		sessions, err := p.sessions.ListActive(ctx, repo.ID)
		if err != nil {
			p.logger.Error().Err(err).Str("repo", repo.ID).Msg("poller: list sessions")
			continue
		}

		for _, sess := range sessions {
			if sess.State != machine.AwaitingChecks {
				continue
			}
			if sess.PRNumber == nil {
				continue
			}

			p.checkSession(ctx, ch, repo, sess)
		}
	}
}

// checkSession polls a single session's PR status and check results,
// emitting events as needed.
func (p *Poller) checkSession(ctx context.Context, ch chan<- SessionEvent, repo *models.Repo, sess *models.Session) {
	prID := *sess.PRNumber
	repoPath := repo.OriginURL

	p.logger.Debug().
		Str("session", sess.ID).
		Int("pr", prID).
		Msg("polling checks")

	// Check PR status for merge/close/conflict.
	prStatus, err := p.provider.GetPRStatus(ctx, repoPath, prID)
	if err != nil {
		p.logger.Warn().Err(err).Str("session", sess.ID).Msg("poller: get PR status")
		return
	}

	switch prStatus.State {
	case vcs.PRStateMerged:
		p.emit(ctx, ch, sess.ID, vcs.PRMerged{PRID: prID})
		return
	case vcs.PRStateClosed:
		p.emit(ctx, ch, sess.ID, vcs.PRClosed{PRID: prID})
		return
	default:
	}

	// Check for merge conflicts.
	if prStatus.Mergeable != nil && !*prStatus.Mergeable {
		p.emit(ctx, ch, sess.ID, vcs.ConflictDetected{PRID: prID})
		return
	}

	// Check CI results.
	checks, err := p.provider.GetCheckResults(ctx, repoPath, prID)
	if err != nil {
		p.logger.Warn().Err(err).Str("session", sess.ID).Msg("poller: get check results")
		return
	}

	if len(checks) == 0 {
		// No checks yet — wait for next poll.
		return
	}

	overall := aggregateChecks(checks)
	switch overall {
	case vcs.ChecksOverallPassed:
		p.emit(ctx, ch, sess.ID, vcs.ChecksPassed{PRID: prID})
	case vcs.ChecksOverallFailed:
		var failed []vcs.CheckResult
		for _, c := range checks {
			if c.Conclusion != nil && *c.Conclusion == vcs.CheckConclusionFailure {
				failed = append(failed, c)
			}
		}
		p.emit(ctx, ch, sess.ID, vcs.ChecksFailed{PRID: prID, FailedChecks: failed})
	default:
		// ChecksOverallPending — do nothing, wait for next poll.
	}
}

// emit sends a SessionEvent on the channel, respecting context cancellation.
func (p *Poller) emit(ctx context.Context, ch chan<- SessionEvent, sessionID string, event vcs.Event) {
	select {
	case ch <- SessionEvent{SessionID: sessionID, Event: event}:
	case <-ctx.Done():
	}
}

// aggregateChecks computes the overall check status from individual results.
func aggregateChecks(checks []vcs.CheckResult) vcs.ChecksOverall {
	allCompleted := true
	for _, c := range checks {
		if c.Status != vcs.CheckStatusCompleted {
			allCompleted = false
			continue
		}
		if c.Conclusion != nil && *c.Conclusion == vcs.CheckConclusionFailure {
			return vcs.ChecksOverallFailed
		}
	}
	if allCompleted {
		return vcs.ChecksOverallPassed
	}
	return vcs.ChecksOverallPending
}
