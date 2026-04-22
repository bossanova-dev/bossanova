package session

import (
	"context"
	"fmt"
	"sync"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/safego"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
)

// FixHandler handles automated fix attempts for sessions. It is satisfied by
// *FixLoop and exists to decouple the Dispatcher from the concrete FixLoop type.
type FixHandler interface {
	HandleCheckFailure(ctx context.Context, sessionID string, failedChecks []vcs.CheckResult) error
	HandleConflict(ctx context.Context, sessionID string) error
	HandleReviewFeedback(ctx context.Context, sessionID string, comments []vcs.ReviewComment) error
}

// SessionCompletionNotifier is called when a session reaches a terminal state
// (merged, closed, or blocked). The task orchestrator implements this to
// unblock its per-repo FIFO queue.
type SessionCompletionNotifier interface {
	HandleSessionCompleted(ctx context.Context, sessionID string, outcome models.TaskMappingStatus)
}

// Dispatcher consumes VCS events from the poller and applies the
// corresponding state machine transitions and database updates.
//
// Concurrency model: Run reads from a single events channel in a single
// goroutine, so per-session event ordering is preserved by construction.
// The only other path that calls dispatch handlers is in-process tests
// that also drive Run. d.mu is retained as belt-and-suspenders so an
// accidental future caller cannot interleave a partial state transition.
// Plugin callbacks (NotifyStatusChange) are dispatched by the plugin
// host and never invoke dispatcher methods directly.
type Dispatcher struct {
	sessions           db.SessionStore
	repos              db.RepoStore
	provider           vcs.Provider
	fixLoop            FixHandler
	completionNotifier SessionCompletionNotifier
	logger             zerolog.Logger
	mu                 sync.Mutex // see type doc: redundant given single-goroutine Run, kept as a safety net
}

// NewDispatcher creates a new event dispatcher.
func NewDispatcher(
	sessions db.SessionStore,
	repos db.RepoStore,
	provider vcs.Provider,
	fixLoop FixHandler,
	logger zerolog.Logger,
) *Dispatcher {
	return &Dispatcher{
		sessions: sessions,
		repos:    repos,
		provider: provider,
		fixLoop:  fixLoop,
		logger:   logger,
	}
}

// SetCompletionNotifier sets the notifier that is called when sessions
// reach terminal states. This uses a setter instead of a constructor
// parameter because the dispatcher is created before the orchestrator.
func (d *Dispatcher) SetCompletionNotifier(n SessionCompletionNotifier) {
	d.completionNotifier = n
}

// notifyCompletion calls the completion notifier if one is set.
func (d *Dispatcher) notifyCompletion(ctx context.Context, sessionID string, outcome models.TaskMappingStatus) {
	if d.completionNotifier != nil {
		d.completionNotifier.HandleSessionCompleted(ctx, sessionID, outcome)
	}
}

// Run consumes events from the channel and dispatches them until the
// channel is closed or the context is cancelled.
func (d *Dispatcher) Run(ctx context.Context, events <-chan SessionEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if err := d.dispatch(ctx, ev); err != nil {
				d.logger.Error().Err(err).
					Str("session", ev.SessionID).
					Str("event", fmt.Sprintf("%T", ev.Event)).
					Msg("dispatch failed")
			}
		}
	}
}

// dispatch routes a single event to the appropriate handler.
func (d *Dispatcher) dispatch(ctx context.Context, ev SessionEvent) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	sess, err := d.sessions.Get(ctx, ev.SessionID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}

	sm := machine.NewWithContext(sess.State, &machine.SessionContext{
		AttemptCount: sess.AttemptCount,
		MaxAttempts:  machine.MaxAttempts,
	})

	switch event := ev.Event.(type) {
	case vcs.ChecksPassed:
		return d.handleChecksPassed(ctx, sm, sess)
	case vcs.ChecksFailed:
		return d.handleChecksFailed(ctx, sm, sess, event)
	case vcs.ConflictDetected:
		return d.handleConflictDetected(ctx, sm, sess)
	case vcs.ReviewSubmitted:
		return d.handleReviewSubmitted(ctx, sm, sess, event)
	case vcs.PRMerged:
		return d.handlePRMerged(ctx, sm, sess)
	case vcs.PRClosed:
		return d.handlePRClosed(ctx, sm, sess)
	default:
		d.logger.Warn().
			Str("type", fmt.Sprintf("%T", ev.Event)).
			Msg("unhandled event type")
		return nil
	}
}

func (d *Dispatcher) handleChecksPassed(ctx context.Context, sm *machine.Machine, sess *models.Session) error {
	if err := sm.FireCtx(ctx, machine.ChecksPassed); err != nil {
		return fmt.Errorf("fire checks_passed: %w", err)
	}

	newState := int(sm.State())
	checkState := int(machine.CheckStatePassed)
	if _, err := d.sessions.Update(ctx, sess.ID, db.UpdateSessionParams{
		State:          &newState,
		LastCheckState: &checkState,
	}); err != nil {
		return fmt.Errorf("update session: %w", err)
	}

	d.logger.Info().
		Str("session", sess.ID).
		Str("newState", sm.State().String()).
		Msg("checks passed")

	// If we transitioned to GreenDraft, mark the PR ready for review.
	if sm.State() == machine.GreenDraft && sess.PRNumber != nil {
		repo, err := d.repos.Get(ctx, sess.RepoID)
		if err != nil {
			d.logger.Warn().Err(err).Str("session", sess.ID).Msg("failed to get repo for mark ready")
			return nil
		}
		if !repo.CanAutoMerge {
			d.logger.Info().Str("session", sess.ID).Msg("auto-merge disabled, skipping mark ready for review")
			return nil
		}
		if err := d.provider.MarkReadyForReview(ctx, repo.OriginURL, *sess.PRNumber); err != nil {
			d.logger.Warn().Err(err).Str("session", sess.ID).Msg("failed to mark ready for review")
		} else {
			// Fire PlanComplete → ReadyForReview.
			if err := sm.FireCtx(ctx, machine.PlanComplete); err == nil {
				readyState := int(machine.ReadyForReview)
				if _, err := d.sessions.Update(ctx, sess.ID, db.UpdateSessionParams{
					State: &readyState,
				}); err != nil {
					d.logger.Warn().Err(err).Str("session", sess.ID).Msg("failed to update to ready_for_review")
				}
				d.logger.Info().Str("session", sess.ID).Msg("marked ready for review")
			}
		}
	}

	return nil
}

func (d *Dispatcher) handleChecksFailed(ctx context.Context, sm *machine.Machine, sess *models.Session, event vcs.ChecksFailed) error {
	if err := sm.FireCtx(ctx, machine.ChecksFailed); err != nil {
		return fmt.Errorf("fire checks_failed: %w", err)
	}

	newState := int(sm.State())
	checkState := int(machine.CheckStateFailed)
	attemptCount := sm.Context().AttemptCount
	update := db.UpdateSessionParams{
		State:          &newState,
		LastCheckState: &checkState,
		AttemptCount:   &attemptCount,
	}

	if sm.State() == machine.Blocked {
		reason := sm.Context().BlockedReason
		reasonPtr := &reason
		update.BlockedReason = &reasonPtr
	}

	if _, err := d.sessions.Update(ctx, sess.ID, update); err != nil {
		return fmt.Errorf("update session: %w", err)
	}

	d.logger.Info().
		Str("session", sess.ID).
		Str("newState", sm.State().String()).
		Int("failedChecks", len(event.FailedChecks)).
		Msg("checks failed")

	// If blocked, the session is terminal from the orchestrator's perspective.
	if sm.State() == machine.Blocked {
		d.notifyCompletion(ctx, sess.ID, models.TaskMappingStatusFailed)
	}

	// Kick off the fix loop if we transitioned to FixingChecks.
	if sm.State() == machine.FixingChecks && d.fixLoop != nil {
		if !sess.AutomationEnabled {
			d.logger.Info().Str("session", sess.ID).Msg("automation disabled, skipping fix loop for check failure")
			return nil
		}
		safego.Go(d.logger, func() {
			if err := d.fixLoop.HandleCheckFailure(ctx, sess.ID, event.FailedChecks); err != nil {
				d.logger.Error().Err(err).Str("session", sess.ID).Msg("fix loop: check failure handler failed")
			}
		})
	}

	return nil
}

func (d *Dispatcher) handleConflictDetected(ctx context.Context, sm *machine.Machine, sess *models.Session) error {
	if err := sm.FireCtx(ctx, machine.ConflictDetected); err != nil {
		return fmt.Errorf("fire conflict_detected: %w", err)
	}

	newState := int(sm.State())
	attemptCount := sm.Context().AttemptCount
	update := db.UpdateSessionParams{
		State:        &newState,
		AttemptCount: &attemptCount,
	}

	if sm.State() == machine.Blocked {
		reason := sm.Context().BlockedReason
		reasonPtr := &reason
		update.BlockedReason = &reasonPtr
	}

	if _, err := d.sessions.Update(ctx, sess.ID, update); err != nil {
		return fmt.Errorf("update session: %w", err)
	}

	d.logger.Info().
		Str("session", sess.ID).
		Str("newState", sm.State().String()).
		Msg("conflict detected")

	// If blocked, the session is terminal from the orchestrator's perspective.
	if sm.State() == machine.Blocked {
		d.notifyCompletion(ctx, sess.ID, models.TaskMappingStatusFailed)
	}

	// Kick off the fix loop if we transitioned to FixingChecks.
	if sm.State() == machine.FixingChecks && d.fixLoop != nil {
		repo, err := d.repos.Get(ctx, sess.RepoID)
		if err != nil {
			d.logger.Warn().Err(err).Str("session", sess.ID).Msg("failed to get repo for conflict automation check")
			return nil
		}
		if !repo.CanAutoResolveConflicts {
			d.logger.Info().Str("session", sess.ID).Msg("auto-resolve conflicts disabled, skipping fix loop")
			return nil
		}
		safego.Go(d.logger, func() {
			if err := d.fixLoop.HandleConflict(ctx, sess.ID); err != nil {
				d.logger.Error().Err(err).Str("session", sess.ID).Msg("fix loop: conflict handler failed")
			}
		})
	}

	return nil
}

func (d *Dispatcher) handleReviewSubmitted(ctx context.Context, sm *machine.Machine, sess *models.Session, event vcs.ReviewSubmitted) error {
	if err := sm.FireCtx(ctx, machine.ReviewSubmitted); err != nil {
		return fmt.Errorf("fire review_submitted: %w", err)
	}

	newState := int(sm.State())
	attemptCount := sm.Context().AttemptCount
	update := db.UpdateSessionParams{
		State:        &newState,
		AttemptCount: &attemptCount,
	}

	if sm.State() == machine.Blocked {
		reason := sm.Context().BlockedReason
		reasonPtr := &reason
		update.BlockedReason = &reasonPtr
	}

	if _, err := d.sessions.Update(ctx, sess.ID, update); err != nil {
		return fmt.Errorf("update session: %w", err)
	}

	d.logger.Info().
		Str("session", sess.ID).
		Str("newState", sm.State().String()).
		Int("comments", len(event.Comments)).
		Msg("review submitted")

	// If blocked, the session is terminal from the orchestrator's perspective.
	if sm.State() == machine.Blocked {
		d.notifyCompletion(ctx, sess.ID, models.TaskMappingStatusFailed)
	}

	// Kick off the fix loop if we transitioned to FixingChecks.
	if sm.State() == machine.FixingChecks && d.fixLoop != nil {
		repo, err := d.repos.Get(ctx, sess.RepoID)
		if err != nil {
			d.logger.Warn().Err(err).Str("session", sess.ID).Msg("failed to get repo for review automation check")
			return nil
		}
		if !repo.CanAutoAddressReviews {
			d.logger.Info().Str("session", sess.ID).Msg("auto-address reviews disabled, skipping fix loop")
			return nil
		}
		safego.Go(d.logger, func() {
			if err := d.fixLoop.HandleReviewFeedback(ctx, sess.ID, event.Comments); err != nil {
				d.logger.Error().Err(err).Str("session", sess.ID).Msg("fix loop: review handler failed")
			}
		})
	}

	return nil
}

func (d *Dispatcher) handlePRMerged(ctx context.Context, sm *machine.Machine, sess *models.Session) error {
	if err := sm.FireCtx(ctx, machine.PRMerged); err != nil {
		return fmt.Errorf("fire pr_merged: %w", err)
	}

	mergedState := int(machine.Merged)
	if _, err := d.sessions.Update(ctx, sess.ID, db.UpdateSessionParams{
		State: &mergedState,
	}); err != nil {
		return fmt.Errorf("update session: %w", err)
	}

	d.logger.Info().Str("session", sess.ID).Msg("PR merged")

	d.notifyCompletion(ctx, sess.ID, models.TaskMappingStatusCompleted)
	return nil
}

func (d *Dispatcher) handlePRClosed(ctx context.Context, sm *machine.Machine, sess *models.Session) error {
	if err := sm.FireCtx(ctx, machine.PRClosed); err != nil {
		return fmt.Errorf("fire pr_closed: %w", err)
	}

	closedState := int(machine.Closed)
	if _, err := d.sessions.Update(ctx, sess.ID, db.UpdateSessionParams{
		State: &closedState,
	}); err != nil {
		return fmt.Errorf("update session: %w", err)
	}

	d.logger.Info().Str("session", sess.ID).Msg("PR closed")

	d.notifyCompletion(ctx, sess.ID, models.TaskMappingStatusFailed)
	return nil
}
