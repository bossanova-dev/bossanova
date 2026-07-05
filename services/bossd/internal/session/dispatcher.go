package session

import (
	"context"
	"fmt"
	"sync"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sessionreason"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
)

// SessionCompletionNotifier is called when a session reaches a terminal state
// (merged, closed, or blocked). The task orchestrator implements this to
// unblock its per-repo FIFO queue.
type SessionCompletionNotifier interface {
	HandleSessionCompleted(ctx context.Context, sessionID string, outcome models.TaskMappingStatus)
}

// DisplayStatusSetter lets the dispatcher push a terminal display status into
// the tracker the instant a PR reaches a terminal state, instead of waiting for
// the next display-poll cycle (which can be minutes away). Without this, the DB
// state flips to Merged/Closed while the in-memory tracker still holds the last
// polled status (e.g. Passing), and the STATUS column — computed from the
// tracker, not the DB state — stays stale until the poller catches up.
// Satisfied by *status.DisplayTracker.
type DisplayStatusSetter interface {
	Set(sessionID string, info vcs.DisplayInfo)
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
	completionNotifier SessionCompletionNotifier
	displayStatus      DisplayStatusSetter
	logger             zerolog.Logger
	mu                 sync.Mutex // see type doc: redundant given single-goroutine Run, kept as a safety net
}

// NewDispatcher creates a new event dispatcher.
func NewDispatcher(
	sessions db.SessionStore,
	repos db.RepoStore,
	provider vcs.Provider,
	logger zerolog.Logger,
) *Dispatcher {
	return &Dispatcher{
		sessions: sessions,
		repos:    repos,
		provider: provider,
		logger:   logger,
	}
}

// SetCompletionNotifier sets the notifier that is called when sessions
// reach terminal states. This uses a setter instead of a constructor
// parameter because the dispatcher is created before the orchestrator.
func (d *Dispatcher) SetCompletionNotifier(n SessionCompletionNotifier) {
	d.completionNotifier = n
}

// SetDisplayStatusSetter wires the tracker that the dispatcher pokes when a PR
// reaches a terminal state. Uses a setter (not a constructor param) because the
// dispatcher is created before the tracker is fully wired in main.go. nil-safe:
// leaving it unset simply falls back to the display poller correcting the
// status on its next cycle.
func (d *Dispatcher) SetDisplayStatusSetter(s DisplayStatusSetter) {
	d.displayStatus = s
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
		AttemptCount:     sess.AttemptCount,
		MaxAttempts:      machine.MaxAttempts,
		SkipAttemptCount: shouldSkipAttempt(sess, ev.Event),
	})

	switch event := ev.Event.(type) {
	case vcs.ChecksPassed:
		return d.handleChecksPassed(ctx, sm, sess)
	case vcs.ChecksFailed:
		return d.handleChecksFailed(ctx, sm, sess, event)
	case vcs.ConflictDetected:
		return d.handleConflictDetected(ctx, sm, sess, event)
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

// shouldSkipAttempt reports whether the fix-loop lap driven by this event
// should be free (not consume an attempt). A lap is free only when the PR head
// SHA carried by the event matches the SHA at which the last attempt was
// counted: CI is merely re-settling on an unchanged commit and no fix was
// pushed. Laps with an unknown head SHA, or the first lap on a PR (no recorded
// last-counted SHA), or a SHA that differs from the last counted one (a real
// fix pushed a new commit) always count — preserving genuine fix-loop
// exhaustion (BOS-235). Only ChecksFailed / ConflictDetected are head-SHA
// gated; review feedback (ReviewSubmitted) is a real actionable finding, is
// deduped by LastObservedReviewState, and always counts.
//
// Note the head-SHA premise is a clean fit for check failures (a real fix
// pushes a new commit) but only a partial fit for conflicts, whose appearance
// can be driven by the base branch advancing rather than the head moving: a
// persistent conflict at a stable head SHA is counted once and then gated free,
// so the machine alone may not reach FixLoopExhausted for it. That is
// deliberate and safe here — the live merge gate (liveMergeBlocked) still
// refuses to merge a truly-conflicted PR, and the repair plugin has its own
// independent escalation lane (maxRepairLoopAttempts + backoff + blocked
// reason) for a genuinely stuck conflict.
func shouldSkipAttempt(sess *models.Session, ev vcs.Event) bool {
	var headSHA string
	switch e := ev.(type) {
	case vcs.ChecksFailed:
		headSHA = e.HeadSHA
	case vcs.ConflictDetected:
		headSHA = e.HeadSHA
	default:
		return false
	}
	if headSHA == "" || sess.LastAttemptHeadSHA == nil {
		return false
	}
	return *sess.LastAttemptHeadSHA == headSHA
}

// attemptHeadSHAUpdate returns an UpdateSessionParams field value that records
// headSHA as the last-counted attempt SHA, but only when the lap actually
// counted (skip is false) and the head SHA is known. Returns nil (don't touch
// the column) otherwise. The double pointer follows the nullable-string
// convention in UpdateSessionParams.
func attemptHeadSHAUpdate(sm *machine.Machine, headSHA string) **string {
	if sm.Context().SkipAttemptCount || headSHA == "" {
		return nil
	}
	head := headSHA
	headPtr := &head
	return &headPtr
}

func (d *Dispatcher) handleChecksPassed(ctx context.Context, sm *machine.Machine, sess *models.Session) error {
	if err := sm.FireCtx(ctx, machine.ChecksPassed); err != nil {
		return fmt.Errorf("fire checks_passed: %w", err)
	}

	newState := int(sm.State())
	checkState := int(machine.CheckStatePassed)
	// Reaching green resets the fix-loop budget: the current commit passed CI,
	// so any prior failed-attempt tally is stale and a genuinely new failure
	// should start from a clean slate. Clear the last-counted attempt SHA too
	// so the next failure's first lap always counts (BOS-235).
	zeroAttempts := 0
	var clearHeadSHA *string // *nil → set column NULL
	if _, err := d.sessions.Update(ctx, sess.ID, db.UpdateSessionParams{
		State:              &newState,
		LastCheckState:     &checkState,
		AttemptCount:       &zeroAttempts,
		LastAttemptHeadSHA: &clearHeadSHA,
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
		State:              &newState,
		LastCheckState:     &checkState,
		AttemptCount:       &attemptCount,
		LastAttemptHeadSHA: attemptHeadSHAUpdate(sm, event.HeadSHA),
	}

	// Reaching Blocked from these handlers means the fix loop exhausted its
	// max attempts (the only path here is fixOrBlock). Use the shared reason
	// so every genuine fix-loop exhaustion reads identically in the UI.
	if sm.State() == machine.Blocked {
		reason := sessionreason.FixLoopExhausted()
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

	// On a FixingChecks transition the dispatcher does no repair itself: the
	// repair plugin reacts to the resulting FAILING display status (gated by the
	// repo's auto-repair toggle). The dispatcher only owns the state machine.
	return nil
}

func (d *Dispatcher) handleConflictDetected(ctx context.Context, sm *machine.Machine, sess *models.Session, event vcs.ConflictDetected) error {
	if err := sm.FireCtx(ctx, machine.ConflictDetected); err != nil {
		return fmt.Errorf("fire conflict_detected: %w", err)
	}

	newState := int(sm.State())
	attemptCount := sm.Context().AttemptCount
	update := db.UpdateSessionParams{
		State:              &newState,
		AttemptCount:       &attemptCount,
		LastAttemptHeadSHA: attemptHeadSHAUpdate(sm, event.HeadSHA),
	}

	// Reaching Blocked from these handlers means the fix loop exhausted its
	// max attempts (the only path here is fixOrBlock). Use the shared reason
	// so every genuine fix-loop exhaustion reads identically in the UI.
	if sm.State() == machine.Blocked {
		reason := sessionreason.FixLoopExhausted()
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

	// Repair (if enabled) is driven by the repair plugin off the resulting
	// CONFLICT display status; the dispatcher only owns the state machine.
	return nil
}

func (d *Dispatcher) handleReviewSubmitted(ctx context.Context, sm *machine.Machine, sess *models.Session, event vcs.ReviewSubmitted) error {
	reviewState := int(event.State)
	if event.State != vcs.ReviewStateChangesRequested {
		if _, err := d.sessions.Update(ctx, sess.ID, db.UpdateSessionParams{
			LastObservedReviewState: &reviewState,
		}); err != nil {
			return fmt.Errorf("update session review state: %w", err)
		}
		d.logger.Info().
			Str("session", sess.ID).
			Int("reviewState", int(event.State)).
			Msg("review submitted without changes requested, skipping fix loop")
		return nil
	}

	if err := sm.FireCtx(ctx, machine.ReviewSubmitted); err != nil {
		return fmt.Errorf("fire review_submitted: %w", err)
	}

	newState := int(sm.State())
	attemptCount := sm.Context().AttemptCount
	update := db.UpdateSessionParams{
		State:                   &newState,
		AttemptCount:            &attemptCount,
		LastObservedReviewState: &reviewState,
	}

	// Reaching Blocked from these handlers means the fix loop exhausted its
	// max attempts (the only path here is fixOrBlock). Use the shared reason
	// so every genuine fix-loop exhaustion reads identically in the UI.
	if sm.State() == machine.Blocked {
		reason := sessionreason.FixLoopExhausted()
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

	// Repair (if enabled) is driven by the repair plugin off the resulting
	// REJECTED display status; the dispatcher only owns the state machine.
	return nil
}

func (d *Dispatcher) handlePRMerged(ctx context.Context, sm *machine.Machine, sess *models.Session) error {
	wasBlocked := sess.State == machine.Blocked
	if err := sm.FireCtx(ctx, machine.PRMerged); err != nil {
		return fmt.Errorf("fire pr_merged: %w", err)
	}

	// Push the terminal display status into the tracker before the DB Update
	// below, so the recompute that Update triggers already sees "merged"
	// instead of the stale last-polled status. Otherwise the STATUS column
	// lingers on e.g. "passing" until the display poller's next cycle.
	if d.displayStatus != nil {
		d.displayStatus.Set(sess.ID, vcs.DisplayInfo{Status: vcs.DisplayStatusMerged})
	}

	mergedState := int(machine.Merged)
	params := db.UpdateSessionParams{State: &mergedState}
	// A Blocked session advancing to a terminal state runs OnExit(actionClearBlocked)
	// in the machine, clearing BlockedReason + AttemptCount there. Persist the same
	// cleared fields (mirroring reconcileTerminalPRForBlockedSession) so a stale,
	// non-gating "finalize failed" hint does not linger on the merged row: this
	// handler seeds the tracker with a terminal status, so the display poller skips
	// the session and nothing else would clear it (BOS-246).
	if wasBlocked {
		clearBlockMetadata(&params)
	}
	if _, err := d.sessions.Update(ctx, sess.ID, params); err != nil {
		return fmt.Errorf("update session: %w", err)
	}

	d.logger.Info().Str("session", sess.ID).Msg("PR merged")

	d.notifyCompletion(ctx, sess.ID, models.TaskMappingStatusCompleted)
	return nil
}

// clearBlockMetadata sets the UpdateSessionParams fields that OnExit(actionClearBlocked)
// resets in the machine when a Blocked session leaves that state — the persisted block
// reason, attempt count, and last-attempt SHA — so the DB row matches the machine and
// no stale non-gating block hint survives on a terminal (merged/closed) session.
func clearBlockMetadata(params *db.UpdateSessionParams) {
	zeroAttempts := 0
	var clearReason *string  // *nil → set blocked_reason NULL
	var clearHeadSHA *string // *nil → set last_attempt_head_sha NULL
	params.AttemptCount = &zeroAttempts
	params.BlockedReason = &clearReason
	params.LastAttemptHeadSHA = &clearHeadSHA
}

func (d *Dispatcher) handlePRClosed(ctx context.Context, sm *machine.Machine, sess *models.Session) error {
	wasBlocked := sess.State == machine.Blocked
	if err := sm.FireCtx(ctx, machine.PRClosed); err != nil {
		return fmt.Errorf("fire pr_closed: %w", err)
	}

	// See handlePRMerged: set the tracker before the DB Update so the STATUS
	// column reflects "closed" immediately rather than on the next poll cycle.
	if d.displayStatus != nil {
		d.displayStatus.Set(sess.ID, vcs.DisplayInfo{Status: vcs.DisplayStatusClosed})
	}

	closedState := int(machine.Closed)
	params := db.UpdateSessionParams{State: &closedState}
	// See handlePRMerged: a Blocked session going terminal must persist the block
	// metadata that OnExit(actionClearBlocked) cleared in the machine, or the stale
	// hint lingers on the closed row.
	if wasBlocked {
		clearBlockMetadata(&params)
	}
	if _, err := d.sessions.Update(ctx, sess.ID, params); err != nil {
		return fmt.Errorf("update session: %w", err)
	}

	d.logger.Info().Str("session", sess.ID).Msg("PR closed")

	d.notifyCompletion(ctx, sess.ID, models.TaskMappingStatusFailed)
	return nil
}
