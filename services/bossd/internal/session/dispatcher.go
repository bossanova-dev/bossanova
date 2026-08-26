package session

import (
	"context"
	"fmt"
	"sync"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/safego"
	"github.com/recurser/bossalib/sessionreason"
	"github.com/recurser/bossalib/sqlutil"
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

// SessionArchiver archives a session by id, performing the full archive (stop
// agent, kill tmux, remove worktree) and emitting the stream update. Satisfied
// by the same archiver the task orchestrator uses for the BOS-101 dependabot
// auto-archive. Injected via a setter because the dispatcher is constructed
// before the archiver is wired.
type SessionArchiver interface {
	ArchiveSession(ctx context.Context, sessionID string) error
}

// SessionArchiverFunc adapts a plain func to the SessionArchiver interface.
type SessionArchiverFunc func(ctx context.Context, sessionID string) error

// ArchiveSession implements SessionArchiver.
func (f SessionArchiverFunc) ArchiveSession(ctx context.Context, id string) error {
	return f(ctx, id)
}

// ArchiveWorkerTracker receives an archive worker's completion channel, and
// the session it archives, so the daemon can join in-flight archives during
// shutdown — and name the session in a log line if it ever declines to. A nil
// tracker means the archive runs untracked (tests, and any caller with no
// shutdown coordination).
// Modelled on tccprobe.WorkerTracker, whose shape and nil semantics this
// deliberately mirrors.
//
// Joining bounds an archive to the daemon's lifetime; it does not promise the
// archive SUCCEEDS. The plugin host is closed before the shutdown wait, so a
// late archive can still fail its notify leg. What the join does protect is the
// archived_at write, because the database is closed by a defer that runs after
// that wait. The guarantee is therefore "completed, or failed with a logged
// error" rather than "silently truncated between removing the worktree and
// recording the archive" (BOS-923).
type ArchiveWorkerTracker func(sessionID string, done <-chan struct{})

// BaseAdvanceNotifier is told, once a PR merges, that the session's base
// branch has moved on. It backs the BOS-521 keep-current sweep, which rebases
// the repo's other in-flight branches onto the new base.
//
// The hook lives here rather than only on the merge RPC because a PR can merge
// without boss doing the merging — the GitHub UI, a merge queue, another tool.
// Every one of those funnels through this handler, so this is the seam that
// sees them all. Merges that DO go through boss fire both hooks; the second
// sweep finds every branch already current and skips.
type BaseAdvanceNotifier interface {
	NotifyBaseAdvanced(ctx context.Context, mergedSessionID string)
}

// BaseAdvanceNotifierFunc adapts a plain func to the BaseAdvanceNotifier
// interface.
type BaseAdvanceNotifierFunc func(ctx context.Context, mergedSessionID string)

// NotifyBaseAdvanced implements BaseAdvanceNotifier.
func (f BaseAdvanceNotifierFunc) NotifyBaseAdvanced(ctx context.Context, id string) { f(ctx, id) }

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
	archiver           SessionArchiver
	archiveTracker     ArchiveWorkerTracker
	baseAdvance        BaseAdvanceNotifier
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

// SetArchiver wires the archiver invoked after a PR merges when the repo's
// ShouldArchiveSessionsAfterMerge flag is on, together with the tracker that
// joins the resulting archive goroutine to daemon shutdown. Uses a setter (not a
// constructor param) because the dispatcher is created before the archiver is
// wired in main.go. nil-safe in both arguments: a nil archiver disables the
// post-merge archive automation, and a nil tracker leaves the archive untracked.
//
// track is a parameter of this setter rather than a separate SetArchiveTracker
// method so an archiver cannot be wired without a tracker decision being made at
// the same call site (BOS-923) — a seam a future launch point can forget is a
// fix with an expiry date.
func (d *Dispatcher) SetArchiver(a SessionArchiver, track ArchiveWorkerTracker) {
	d.archiver = a
	d.archiveTracker = track
}

// HasArchiveTracker reports whether an archive worker tracker is wired. It
// exists so a startup wiring test can assert that the production archiver call
// site supplied one: an omitted tracker has no shape at runtime, so it has to be
// asserted rather than merely defaulted to nil.
func (d *Dispatcher) HasArchiveTracker() bool { return d.archiveTracker != nil }

// SetBaseAdvanceNotifier wires the hook invoked after a PR merges, when the
// base branch it merged into has therefore advanced. Uses a setter (not a
// constructor param) because the dispatcher is created before the daemon
// server that implements it. nil-safe: leaving it unset disables the
// keep-current sweep on the webhook path.
func (d *Dispatcher) SetBaseAdvanceNotifier(n BaseAdvanceNotifier) {
	d.baseAdvance = n
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
		return d.handleChecksPassed(ctx, sm, sess, event)
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

func (d *Dispatcher) handleChecksPassed(ctx context.Context, sm *machine.Machine, sess *models.Session, event vcs.ChecksPassed) error {
	if err := sm.FireCtx(ctx, machine.ChecksPassed); err != nil {
		return fmt.Errorf("fire checks_passed: %w", err)
	}

	newState := int(sm.State())
	checkState := int(machine.CheckStatePassed)
	demonstrated := event.Demonstrated || event.HeadSHA == ""
	if !demonstrated {
		checkState = int(machine.CheckStateUnspecified)
	}
	checkHeadSHA := event.HeadSHA
	checkHeadSHAPtr := &checkHeadSHA
	checkObservedAt := sqlutil.TimeNow()
	checkObservedAtPtr := &checkObservedAt
	// Reaching green resets the fix-loop budget: the current commit passed CI,
	// so any prior failed-attempt tally is stale and a genuinely new failure
	// should start from a clean slate. Clear the last-counted attempt SHA too
	// so the next failure's first lap always counts (BOS-235).
	zeroAttempts := 0
	var clearHeadSHA *string // *nil → set column NULL
	if _, err := d.sessions.Update(ctx, sess.ID, db.UpdateSessionParams{
		State:                 &newState,
		LastCheckState:        &checkState,
		LastCheckStateHeadSHA: &checkHeadSHAPtr,
		LastCheckStateAt:      &checkObservedAtPtr,
		AttemptCount:          &zeroAttempts,
		LastAttemptHeadSHA:    &clearHeadSHA,
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
	checkHeadSHA := event.HeadSHA
	checkHeadSHAPtr := &checkHeadSHA
	checkObservedAt := sqlutil.TimeNow()
	checkObservedAtPtr := &checkObservedAt
	attemptCount := sm.Context().AttemptCount
	update := db.UpdateSessionParams{
		State:                 &newState,
		LastCheckState:        &checkState,
		LastCheckStateHeadSHA: &checkHeadSHAPtr,
		LastCheckStateAt:      &checkObservedAtPtr,
		AttemptCount:          &attemptCount,
		LastAttemptHeadSHA:    attemptHeadSHAUpdate(sm, event.HeadSHA),
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
	// The row may already be Merged when this webhook lands. MergeSession's
	// post-merge display refresh reconciles the session to Merged synchronously
	// before its RPC returns (and, more rarely, a scheduled display poll gets
	// there first), which is seconds ahead of GitHub's PR-merged delivery.
	// machine.Merged permits no outbound PRMerged, so firing again fails and
	// returns early — skipping the archive hook below. Since BOS-697 that hook
	// is one of three archive-after-merge triggers (see
	// archiveSessionAfterMergeIfEnabled) — all of them routed through
	// ArchiveSession, so all of them auto-delete-branches triggers too — but it
	// is the only one on this path. Treat an
	// already-Merged row as "the transition happened" and fall through to the
	// side effects, all of which are idempotent: the tracker Set and the state
	// Update are writes of the value already there, notifyCompletion is guarded
	// against duplicates by the orchestrator, and ArchiveSession (as wired,
	// Server.ArchiveSessionAndNotify) swallows the already-archived ErrNoRows.
	// A duplicate delivery that arrives once the archive has landed cannot
	// reach here at all — ListByRepoAndPR filters archived rows — and a
	// resurrected session is back on ImplementingPlan, so it takes the normal
	// fire path, not this one. Duplicates in the window before the async
	// archive completes, or on a repo with the archive flag off, do land here;
	// that is exactly what the idempotency above is for.
	//
	// Deliberately merge-only: handlePRClosed has no archive hook, so losing
	// its transition costs nothing that the reconcile has not already done.
	//
	// Deliberately an equality test, not sm.CanFire(PRMerged): CanFire would
	// also swallow a genuinely illegal Closed -> Merged fire and fall through to
	// the side effects. Do not "simplify" it to CanFire. Losing the transition
	// here no longer costs the archive: the hook now lives in the shared
	// archiveSessionAfterMergeIfEnabled, which the display poller's terminal
	// reconcile and the reconcile sweep call too (BOS-697), so a daemon that
	// never receives this webhook still archives.
	if sess.State != machine.Merged {
		if err := sm.FireCtx(ctx, machine.PRMerged); err != nil {
			return fmt.Errorf("fire pr_merged: %w", err)
		}
	}

	if err := d.persistTerminalPRState(ctx, sess, machine.Merged, vcs.DisplayStatusMerged, "PR merged", models.TaskMappingStatusCompleted); err != nil {
		return err
	}
	// Merge-only: the archive hook lives here, not in the shared
	// persistTerminalPRState, so a PR *close* never archives the session.
	d.archiveAfterMergeIfEnabled(ctx, sess)
	// The base this PR merged into has advanced (BOS-521). Fire even when the
	// merge came from boss itself: the sweep is idempotent, and this is the one
	// seam that also catches merges boss did not perform.
	if d.baseAdvance != nil {
		d.baseAdvance.NotifyBaseAdvanced(ctx, sess.ID)
	}
	return nil
}

// archiveAfterMergeIfEnabled archives the just-merged session when its repo has
// the ShouldArchiveSessionsAfterMerge flag on. Delegates to the package-level
// archiveSessionAfterMergeIfEnabled, which the display poller and the reconcile
// sweep share (BOS-697).
func (d *Dispatcher) archiveAfterMergeIfEnabled(ctx context.Context, sess *models.Session) {
	archiveSessionAfterMergeIfEnabled(ctx, d.repos, d.archiver, d.archiveTracker, d.logger, sess)
}

// archiveSessionAfterMergeIfEnabled archives a session that has just reached
// Merged, when its repo has the ShouldArchiveSessionsAfterMerge flag on.
//
// The archive runs asynchronously — it stops the agent, kills tmux, and removes
// the worktree, none of which must block the dispatcher's single event
// goroutine or a poll cycle — and is best-effort: the underlying archive is
// idempotent, so re-archiving an already-archived session is a no-op success.
// nil-safe: with no archiver wired the automation is off, and with no tracker
// wired the archive runs without being joined to daemon shutdown.
//
// Package-level rather than a Dispatcher method because three paths can land a
// session on Merged and every one of them must archive: the PR-merged webhook
// (Dispatcher.handlePRMerged), the display poller's terminal reconcile
// (DisplayPoller.reconcileNonTerminalToResolved, which also backs MergeSession's
// synchronous post-merge refresh), and the reconcile sweep that heals rows
// already stuck merged-but-unarchived.
//
// Idempotent covers re-archiving SEQUENTIALLY, not two archives in flight at
// once, and having three callers makes overlap reachable: a TUI merge archives
// from the post-merge refresh and again from the pr_merged webhook seconds
// later if the first has not yet reached sessions.Archive (ListByRepoAndPR's
// archived filter only excludes the row once it has), and the sweep re-spawns
// every tick while an archive is slow or failing. The residue is warn-log noise
// and a SetArchiving flag the first goroutine's defer clears while the second is
// still running; the destructive steps themselves tolerate it (the worktree
// archive falls back to os.RemoveAll, which is a no-op on a missing path). Named
// rather than fixed: a per-session in-flight lock belongs at the ArchiveSession
// chokepoint, not here.
func archiveSessionAfterMergeIfEnabled(
	ctx context.Context,
	repos db.RepoStore,
	archiver SessionArchiver,
	track ArchiveWorkerTracker,
	logger zerolog.Logger,
	sess *models.Session,
) {
	if archiver == nil || sess == nil {
		return
	}
	repo, err := repos.Get(ctx, sess.RepoID)
	if err != nil {
		logger.Warn().Err(err).Str("session", sess.ID).Msg("archive-after-merge: repo lookup failed")
		return
	}
	if !repo.ShouldArchiveSessionsAfterMerge {
		return
	}
	sessionID := sess.ID
	// Detach from ctx: the archive must complete even if the caller's ctx is
	// cancelled when its handler returns. safego.Go recovers panics.
	done := safego.Go(logger, func() {
		if err := archiver.ArchiveSession(context.Background(), sessionID); err != nil {
			logger.Warn().Err(err).Str("session", sessionID).Msg("archive-after-merge: archive failed")
			return
		}
		logger.Info().Str("session", sessionID).Msg("archive-after-merge: session archived")
	})
	// Hand the completion channel to shutdown coordination instead of dropping
	// it (BOS-923). The detachment above keeps the archive alive past its
	// caller's handler; without this join the daemon could still exit — closing
	// the database — between removing the worktree and writing archived_at,
	// leaving an active-looking session whose worktree is gone.
	if track != nil {
		track(sessionID, done)
	}
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
	if err := sm.FireCtx(ctx, machine.PRClosed); err != nil {
		return fmt.Errorf("fire pr_closed: %w", err)
	}

	return d.persistTerminalPRState(ctx, sess, machine.Closed, vcs.DisplayStatusClosed, "PR closed", models.TaskMappingStatusFailed)
}

func (d *Dispatcher) persistTerminalPRState(
	ctx context.Context,
	sess *models.Session,
	state machine.State,
	displayStatus vcs.DisplayStatus,
	logMessage string,
	outcome models.TaskMappingStatus,
) error {
	// Push the terminal display status into the tracker before the DB Update
	// below, so the recompute that Update triggers already sees the terminal
	// state instead of the stale last-polled status. Otherwise the STATUS column
	// lingers on e.g. "passing" until the display poller's next cycle.
	if d.displayStatus != nil {
		d.displayStatus.Set(sess.ID, vcs.DisplayInfo{Status: displayStatus})
	}

	nextState := int(state)
	params := db.UpdateSessionParams{State: &nextState}
	// A Blocked session advancing to a terminal state runs OnExit(actionClearBlocked)
	// in the machine, clearing BlockedReason + AttemptCount there. Persist the same
	// cleared fields (mirroring reconcileTerminalPRForBlockedSession) so a stale,
	// non-gating "finalize failed" hint does not linger on the terminal row: this
	// handler seeds the tracker with a terminal status, so the display poller skips
	// the session and nothing else would clear it (BOS-246).
	if sess.State == machine.Blocked {
		clearBlockMetadata(&params)
	}
	if _, err := d.sessions.Update(ctx, sess.ID, params); err != nil {
		return fmt.Errorf("update session: %w", err)
	}

	d.logger.Info().Str("session", sess.ID).Msg(logMessage)
	d.notifyCompletion(ctx, sess.ID, outcome)
	return nil
}
