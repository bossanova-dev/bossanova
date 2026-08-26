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

// DefaultPollTimeout bounds the time spent polling a single session so a
// hung VCS provider call cannot consume the whole sweep and starve the
// remaining sessions. It is a per-session budget, not a per-iteration one:
// a slow session times out on its own and the sweep proceeds to the next.
const DefaultPollTimeout = 30 * time.Second

// SessionEvent pairs a VCS event with the session it belongs to.
type SessionEvent struct {
	SessionID string
	Event     vcs.Event
}

// Poller periodically checks CI status for sessions in AwaitingChecks state
// and emits VCS events when status changes are detected.
type Poller struct {
	sessions       db.SessionStore
	repos          db.RepoStore
	provider       vcs.Provider
	interval       time.Duration
	sessionTimeout time.Duration
	logger         zerolog.Logger
	done           chan struct{}
	firstPoll      chan struct{}
}

// NewPoller creates a new check poller. A zero sessionTimeout selects
// DefaultPollTimeout.
func NewPoller(
	sessions db.SessionStore,
	repos db.RepoStore,
	provider vcs.Provider,
	interval time.Duration,
	sessionTimeout time.Duration,
	logger zerolog.Logger,
) *Poller {
	if sessionTimeout <= 0 {
		sessionTimeout = DefaultPollTimeout
	}
	return &Poller{
		sessions:       sessions,
		repos:          repos,
		provider:       provider,
		interval:       interval,
		sessionTimeout: sessionTimeout,
		logger:         logger,
		done:           make(chan struct{}),
		firstPoll:      make(chan struct{}),
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
		p.poll(ctx, ch)
		close(p.firstPoll)

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

// Done returns a channel that is closed when the Run goroutine exits.
// Useful for coordinating shutdown.
func (p *Poller) Done() <-chan struct{} { return p.done }

// FirstPollDone returns a channel that is closed once Run's immediate
// start-up poll has finished. Callers that seed sessions after starting the
// poller can wait on it so that start-up poll is guaranteed to observe the
// pre-seed world rather than racing the seed.
func (p *Poller) FirstPollDone() <-chan struct{} { return p.firstPoll }

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
	repos, err := p.listRepos(ctx)
	if err != nil {
		p.logger.Error().Err(err).Msg("poller: list repos")
		return
	}

	for _, repo := range repos {
		if ctx.Err() != nil {
			return
		}
		sessions, err := p.listActiveSessions(ctx, repo.ID)
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

			p.checkSessionBounded(ctx, ch, repo, sess)
		}
	}
}

// listRepos and listActiveSessions bound each store read with the same
// per-session timeout budget. Without this, the sweep's list phase ran on the
// parent Run context, which only cancels on shutdown: a wedged store read (a
// locked or stuck SQLite connection) would block the poller goroutine
// indefinitely. Bounding here keeps the list phase from hanging the sweep,
// while checkSessionBounded retains its own independent timeout for the
// per-session VCS calls.
func (p *Poller) listRepos(ctx context.Context) ([]*models.Repo, error) {
	listCtx, cancel := context.WithTimeout(ctx, p.sessionTimeout)
	defer cancel()
	return p.repos.List(listCtx)
}

func (p *Poller) listActiveSessions(ctx context.Context, repoID string) ([]*models.Session, error) {
	listCtx, cancel := context.WithTimeout(ctx, p.sessionTimeout)
	defer cancel()
	return p.sessions.ListActive(listCtx, repoID)
}

// checkSessionBounded runs checkSession under a per-session timeout derived
// from the parent context. Bounding each session independently (rather than
// the whole sweep) means one hung VCS provider call times out on its own and
// the sweep continues to the remaining sessions, instead of consuming a
// single shared budget and starving everyone after the slow session.
func (p *Poller) checkSessionBounded(ctx context.Context, ch chan<- SessionEvent, repo *models.Repo, sess *models.Session) {
	sessCtx, cancel := context.WithTimeout(ctx, p.sessionTimeout)
	defer cancel()

	p.checkSession(sessCtx, ch, repo, sess)

	// Distinguish a per-session timeout from ordinary parent-ctx cancellation
	// (shutdown) so a slow-or-hung provider stays visible — now naming the
	// exact session/PR rather than just the iteration.
	if ctx.Err() == nil && errors.Is(sessCtx.Err(), context.DeadlineExceeded) {
		p.logger.Warn().
			Str("session", sess.ID).
			Int("pr", *sess.PRNumber).
			Dur("timeout", p.sessionTimeout).
			Msg("poller: session poll exceeded timeout")
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

	// Check for merge conflicts. Carry the head SHA so the dispatcher can
	// head-SHA-gate attempt counting (BOS-235): a conflict re-observed on an
	// unchanged commit is a free settle lap, not a fresh attempt.
	if repairableConflictBlock(ctx, p.provider, repo, prStatus, p.logger, "poller") {
		emitIf(machine.ConflictDetected, vcs.ConflictDetected{PRID: prID, HeadSHA: prStatus.HeadSHA})
		return
	}

	// Check CI results.
	checks, err := p.provider.GetCheckResults(ctx, repoPath, prID)
	if err != nil {
		p.logger.Warn().Err(err).Str("session", sess.ID).Msg("poller: get check results")
		return
	}

	verdict := vcs.EvaluateChecks(prStatus.HeadSHA, checks, nil)
	switch verdict.State {
	case vcs.CheckVerdictGreen:
		if emitIf(machine.ChecksPassed, vcs.ChecksPassed{PRID: prID, HeadSHA: verdict.HeadSHA, Demonstrated: verdict.DemonstratedPass()}) {
			return
		}
	case vcs.CheckVerdictFailing:
		var failed []vcs.CheckResult
		for _, c := range checks {
			if c.Conclusion != nil && isFailingCheckConclusion(*c.Conclusion) {
				failed = append(failed, c)
			}
		}
		if emitIf(machine.ChecksFailed, vcs.ChecksFailed{PRID: prID, FailedChecks: failed, HeadSHA: prStatus.HeadSHA}) {
			return
		}
	case vcs.CheckVerdictPending, vcs.CheckVerdictUnknown:
		// Do nothing, wait for next poll.
	}

	if reviewSubmittedState(prStatus.LatestReviewState) &&
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

func reviewSubmittedState(state vcs.ReviewState) bool {
	switch state {
	case vcs.ReviewStateApproved, vcs.ReviewStateChangesRequested, vcs.ReviewStateCommented, vcs.ReviewStateDismissed:
		return true
	default:
		return false
	}
}

// emit sends a SessionEvent on the channel, respecting context cancellation.
func (p *Poller) emit(ctx context.Context, ch chan<- SessionEvent, sessionID string, event vcs.Event) {
	select {
	case ch <- SessionEvent{SessionID: sessionID, Event: event}:
	case <-ctx.Done():
	}
}

func isFailingCheckConclusion(c vcs.CheckConclusion) bool {
	switch c {
	case vcs.CheckConclusionFailure, vcs.CheckConclusionCancelled, vcs.CheckConclusionTimedOut:
		return true
	case vcs.CheckConclusionSuccess, vcs.CheckConclusionNeutral, vcs.CheckConclusionSkipped:
		return false
	default:
		return false
	}
}
