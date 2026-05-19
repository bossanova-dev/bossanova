package session

import (
	"context"
	"errors"
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

// DefaultPollTimeout bounds the duration of a single poll iteration so
// a hung VCS provider call cannot wedge the polling loop indefinitely.
const DefaultPollTimeout = 30 * time.Second

// SessionEvent pairs a VCS event with the session it belongs to.
type SessionEvent struct {
	SessionID string
	Event     vcs.Event
}

// Poller periodically checks CI status for sessions in AwaitingChecks state
// and emits VCS events when status changes are detected.
type Poller struct {
	sessions    db.SessionStore
	repos       db.RepoStore
	provider    vcs.Provider
	interval    time.Duration
	pollTimeout time.Duration
	logger      zerolog.Logger
	done        chan struct{}

	// timeoutCount is only accessed from the Run goroutine.
	timeoutCount int
}

// NewPoller creates a new check poller. A zero pollTimeout selects
// DefaultPollTimeout.
func NewPoller(
	sessions db.SessionStore,
	repos db.RepoStore,
	provider vcs.Provider,
	interval time.Duration,
	pollTimeout time.Duration,
	logger zerolog.Logger,
) *Poller {
	if pollTimeout <= 0 {
		pollTimeout = DefaultPollTimeout
	}
	return &Poller{
		sessions:    sessions,
		repos:       repos,
		provider:    provider,
		interval:    interval,
		pollTimeout: pollTimeout,
		logger:      logger,
		done:        make(chan struct{}),
	}
}

// Run starts the polling loop. It sends events on the returned channel and
// stops when the context is cancelled. The caller must consume from the
// channel to prevent blocking.
func (p *Poller) Run(ctx context.Context) <-chan SessionEvent {
	ch := make(chan SessionEvent, 64)
	safego.Go(p.logger, func() {
		defer close(p.done)
		defer close(ch)

		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()

		// Poll immediately on start, then on each tick.
		p.runOnce(ctx, ch)

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.runOnce(ctx, ch)
			}
		}
	})
	return ch
}

// runOnce executes a single poll iteration bounded by pollTimeout.
// Consecutive timeouts are counted and logged so a slow-or-hung
// provider becomes visible in the logs.
func (p *Poller) runOnce(ctx context.Context, ch chan<- SessionEvent) {
	pollCtx, cancel := context.WithTimeout(ctx, p.pollTimeout)
	defer cancel()

	p.poll(pollCtx, ch)

	// Distinguish a timeout from ordinary parent-ctx cancellation.
	if ctx.Err() == nil && errors.Is(pollCtx.Err(), context.DeadlineExceeded) {
		p.timeoutCount++
		p.logger.Warn().
			Int("consecutive_timeouts", p.timeoutCount).
			Dur("timeout", p.pollTimeout).
			Msg("poller: poll iteration exceeded timeout")
	} else {
		p.timeoutCount = 0
	}
}

// Done returns a channel that is closed when the Run goroutine exits.
// Useful for coordinating shutdown.
func (p *Poller) Done() <-chan struct{} { return p.done }

// poll checks all sessions in pollable states and emits events.
//
// "Pollable" is the CI/review-cycle states where the state machine can
// react to ConflictDetected / ChecksFailed / ChecksPassed: AwaitingChecks,
// FixingChecks, GreenDraft, ReadyForReview. AwaitingChecks alone misses
// conflicts that surface after checks pass (main moves under a green PR)
// or mid-fix; without polling those states the session display flips to
// "conflict" via the display poller but the state machine never advances.
//
// Pre-PR states (Creating/Starting/Pushing/OpeningDraftPR), terminal-ish
// states (Finalizing/Blocked/Merged/Closed), and ImplementingPlan are
// excluded — either no PR exists or the lifecycle handles it elsewhere.
func (p *Poller) poll(ctx context.Context, ch chan<- SessionEvent) {
	// List all repos to find sessions across all repos.
	repos, err := p.repos.List(ctx)
	if err != nil {
		p.logger.Error().Err(err).Msg("poller: list repos")
		return
	}

	for _, repo := range repos {
		if ctx.Err() != nil {
			return
		}
		sessions, err := p.sessions.ListActive(ctx, repo.ID)
		if err != nil {
			p.logger.Error().Err(err).Str("repo", repo.ID).Msg("poller: list sessions")
			continue
		}

		for _, sess := range sessions {
			if ctx.Err() != nil {
				return
			}
			if !pollableState(sess.State) {
				continue
			}
			if sess.PRNumber == nil {
				continue
			}

			p.checkSession(ctx, ch, repo, sess)
		}
	}
}

// pollableState reports whether the poller should inspect a session in
// this state. The set must stay in lockstep with the state machine's
// permits for ChecksPassed / ChecksFailed / ConflictDetected.
func pollableState(s machine.State) bool {
	switch s {
	case machine.AwaitingChecks, machine.FixingChecks,
		machine.GreenDraft, machine.ReadyForReview:
		return true
	default:
		return false
	}
}

// checkSession polls a single session's PR status and check results,
// emitting events as needed.
//
// Every emit is gated by sm.CanFire so we never push a state-machine
// event the dispatcher would have to reject. This matters because:
//   - The dispatcher's handle{X} methods return an error on rejection,
//     which the run loop logs every poll cycle (~2min) — log noise per
//     green PR with each new poll.
//   - For self-transitions (e.g. ConflictDetected from a state that
//     permits it via fixOrBlock), each re-fire bumps AttemptCount via
//     OnEntry, eventually Blocking the session even while the repair
//     plugin is still working on the fix.
//
// Constructing a per-session machine on each poll is cheap and keeps
// the poller's emission set automatically in sync with machine.go.
func (p *Poller) checkSession(ctx context.Context, ch chan<- SessionEvent, repo *models.Repo, sess *models.Session) {
	prID := *sess.PRNumber
	repoPath := repo.OriginURL

	p.logger.Debug().
		Str("session", sess.ID).
		Int("pr", prID).
		Msg("polling checks")

	// Build the same SessionContext the dispatcher uses (HasPR + AttemptCount)
	// so CanFire's dynamic guards (fixOrBlock / retryOrBlock) evaluate
	// correctly. Without HasPR, planCompleteDestination would route wrong;
	// without AttemptCount the dynamic transitions would always pick the
	// "under-max" branch.
	sm := machine.NewWithContext(sess.State, &machine.SessionContext{
		AttemptCount: sess.AttemptCount,
		MaxAttempts:  machine.MaxAttempts,
		HasPR:        sess.PRNumber != nil,
	})
	emitIf := func(ev machine.Event, vcsEvent vcs.Event) bool {
		if !sm.CanFire(ev) {
			p.logger.Debug().
				Str("session", sess.ID).
				Str("state", sess.State.String()).
				Str("event", ev.String()).
				Msg("poller: skipping emit; event not permitted in current state")
			return false
		}
		p.emit(ctx, ch, sess.ID, vcsEvent)
		return true
	}

	// Check PR status for merge/close/conflict.
	prStatus, err := p.provider.GetPRStatus(ctx, repoPath, prID)
	if err != nil {
		p.logger.Warn().Err(err).Str("session", sess.ID).Msg("poller: get PR status")
		return
	}

	switch prStatus.State {
	case vcs.PRStateMerged:
		emitIf(machine.PRMerged, vcs.PRMerged{PRID: prID})
		return
	case vcs.PRStateClosed:
		emitIf(machine.PRClosed, vcs.PRClosed{PRID: prID})
		return
	default:
	}

	// Check for merge conflicts.
	if prStatus.Mergeable != nil && !*prStatus.Mergeable {
		emitIf(machine.ConflictDetected, vcs.ConflictDetected{PRID: prID})
		return
	}

	// Check CI results.
	checks, err := p.provider.GetCheckResults(ctx, repoPath, prID)
	if err != nil {
		p.logger.Warn().Err(err).Str("session", sess.ID).Msg("poller: get check results")
		return
	}

	if len(checks) > 0 {
		overall := aggregateChecks(checks)
		switch overall {
		case vcs.ChecksOverallPassed:
			if emitIf(machine.ChecksPassed, vcs.ChecksPassed{PRID: prID}) {
				return
			}
		case vcs.ChecksOverallFailed:
			var failed []vcs.CheckResult
			for _, c := range checks {
				if c.Conclusion != nil && *c.Conclusion == vcs.CheckConclusionFailure {
					failed = append(failed, c)
				}
			}
			if emitIf(machine.ChecksFailed, vcs.ChecksFailed{PRID: prID, FailedChecks: failed}) {
				return
			}
		default:
			// ChecksOverallPending — do nothing, wait for next poll.
		}
	}

	if prStatus.LatestReviewState != vcs.ReviewStateUnspecified &&
		prStatus.LatestReviewState != vcs.ReviewState(sess.LastObservedReviewState) {
		event := vcs.ReviewSubmitted{PRID: prID, State: prStatus.LatestReviewState}
		if prStatus.LatestReviewState == vcs.ReviewStateChangesRequested {
			comments, err := p.provider.GetReviewComments(ctx, repoPath, prID)
			if err != nil {
				p.logger.Warn().Err(err).Str("session", sess.ID).Msg("poller: get review comments")
				return
			}
			event.Comments = comments
		}
		emitIf(machine.ReviewSubmitted, event)
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
