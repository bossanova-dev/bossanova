package db

import (
	"context"
	"fmt"
	"time"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
)

// SessionRecomputer is the small interface the recomputing store wrappers
// depend on. It is implemented by status.DisplayStatusComputer in production;
// keeping the interface here prevents an import cycle (db must not import
// status, which imports db).
type SessionRecomputer interface {
	Recompute(ctx context.Context, sessionID string) error
}

// SessionTransitionObserver is notified AFTER a session's persisted state has
// actually changed. broadcast.SubscriptionEvaluator implements it; the
// interface lives here for the same import-cycle reason SessionRecomputer does
// (db must not import broadcast, which imports db).
//
// The contract has three halves, and all three are enforced by
// RecomputingSessionStore rather than by the observer:
//
//   - EDGE-TRIGGERED. It is called only when the state the session ends up in
//     differs from the one it held before. A write that re-states the current
//     state is not a transition and must not reach the observer.
//   - EXACTLY ONCE per state change. Every store method that writes the state
//     column notifies at most once, so no code path can double-fire.
//   - BEST-EFFORT, AND SELF-LOGGING. The returned error never changes what the
//     store method returns or persists — a subscription that cannot fire must
//     not roll back the session transition that would have fired it. The store
//     therefore DISCARDS the error, which makes logging it the observer's
//     obligation: an implementation that reports upward without logging is
//     silent in production.
type SessionTransitionObserver interface {
	OnSessionState(ctx context.Context, sessionID string, to machine.State) error
}

// RecomputingSessionStore wraps a SessionStore so that any Update touching a
// composite-input field triggers a display-status recompute on the same
// session. The decorator's invariant is "synchronous recomputation at every
// write site that touches composite inputs" — so we trigger Recompute on
// every Update except when the only fields being written are the
// display-trio (DisplayLabel/DisplayIntent/DisplaySpinner). Those are the
// computer's own write-back, and recursing on them would cause a write storm.
//
// # It is also THE session state-transition seam (BOS-557)
//
// Roughly two dozen call sites across session/, server/ and plugin/ write the
// sessions.state column, through Update plus six conditional/atomic methods.
// They converge here: this decorator is constructed exactly once in production
// (cmd/main.go) and every one of those writers holds the decorated store, so a
// single optional SessionTransitionObserver on this type observes them all
// without instrumenting any subsystem. That is why standing broadcast
// subscriptions hook here and not in session/lifecycle.go, which is only one of
// the writers.
//
// The observer is layered ON this type rather than in a decorator wrapped
// around it deliberately: the conditional and orphan methods below reach the
// concrete store through TYPE ASSERTIONS on the embedded SessionStore, and an
// extra decorator sandwiched underneath would fail those assertions closed
// (silently disabling orphan resume) unless it re-forwarded every side
// interface. Extending the one decorator that already owns them has no such
// failure mode.
//
// KNOWN GAP — AdvanceOrphanedSessions is NOT observed. It is a bulk
// `UPDATE ... WHERE state = ?` over every stranded session, is promoted
// straight through the embedded interface without an override, and reports only
// a row count, so there is no per-session identity to notify with. It is a
// considered gap, not an oversight, and it is currently LATENT: it moves
// ImplementingPlan -> AwaitingChecks, neither of which is a trigger state, so no
// subscription can be missed by it today. Should its target ever become a
// terminal state, the evaluator's periodic ReconcileAll sweep is the standing
// safety net — it re-reads every session that still carries a live subscription
// and fires those already sitting in a trigger state.
type RecomputingSessionStore struct {
	SessionStore
	recomputer SessionRecomputer
	observer   SessionTransitionObserver
}

// NewRecomputingSessionStore wires inner with the given recomputer. Pass a
// no-op recomputer in tests that don't care about the side-effect; the
// wrapper does not nil-check so callers must supply something non-nil.
//
// The state-transition observer is optional and supplied separately through
// WithTransitionObserver: only the daemon wires one, and a nil observer is a
// clean no-op on every path.
func NewRecomputingSessionStore(inner SessionStore, recomputer SessionRecomputer) *RecomputingSessionStore {
	return &RecomputingSessionStore{SessionStore: inner, recomputer: recomputer}
}

// WithTransitionObserver attaches the state-transition observer and returns the
// store for chaining. Call it once, at construction, before the store is shared:
// it is a plain field write with no synchronisation, exactly like the recomputer
// the constructor takes.
func (s *RecomputingSessionStore) WithTransitionObserver(obs SessionTransitionObserver) *RecomputingSessionStore {
	s.observer = obs
	return s
}

// notifyTransition reports a real state change to the observer, if one is wired.
//
// The observer's error is deliberately swallowed — the same treatment the
// recomputer gets, and for a stronger reason: the transition has already
// committed, and surfacing a subscription failure would fail a session
// transition that actually happened.
//
// Dropping the value is only safe because the observer logs EVERY failure
// before returning it (it is the only layer that knows which subscription and
// which broadcast). That obligation is part of this interface's contract, not
// an incidental property of today's implementation: an observer that reported
// upward without logging would be silent in production, because this is the
// only caller and it discards what it is told.
//
// The context is DETACHED from the caller's. Every notify site runs AFTER the
// state write has committed, so the observer's work is post-commit work and must
// not inherit a cancellation scope that belonged to the decision to write. It
// otherwise fails in the worst possible way: the observer's fire wins its
// compare-and-swap and then loses the send to `context canceled`, burning a
// standing subscription that nothing can re-fire — the reconcile sweep only
// re-checks subscriptions that are still active. A client disconnecting between
// the commit and the send is enough to trigger it.
//
// Bounded, because detaching removes the caller's only leash: a wedged observer
// must not outlive the transition that provoked it, nor stall shutdown. The same
// WithoutCancel-plus-timeout shape guards broadcast.Sender.cleanUpUnmaterialised
// and cleanupFailedCreateSession, for the same reason.
func (s *RecomputingSessionStore) notifyTransition(ctx context.Context, id string, to machine.State) {
	if s.observer == nil {
		return
	}
	obsCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), transitionObserverTimeout)
	defer cancel()
	_ = s.observer.OnSessionState(obsCtx, id, to)
}

// transitionObserverTimeout bounds one detached observer notification.
// Generous: the observer resolves an audience and writes two transactions, and
// a timeout here silently drops a notification.
const transitionObserverTimeout = 30 * time.Second

// notifyPersistedState reports the state a session ACTUALLY holds, read back
// after the write.
//
// The atomic orphan/claim methods below transition to a state fixed in the
// store's SQL, not in a parameter, so the decorator has nothing to forward. The
// obvious alternative — a hand-copied machine.State literal at each call site —
// duplicates the SQL with no link between the copies: change the SQL and the
// observer starts reporting a transition that did not happen, which for a
// terminal class means firing every subscription on the session. Reading the row
// removes that class of drift entirely rather than guarding it with a test.
//
// A failed read notifies nothing — "cannot prove what happened" — and the
// reconcile sweep covers the miss, exactly as priorState does on the Update path.
func (s *RecomputingSessionStore) notifyPersistedState(ctx context.Context, id string) {
	if s.observer == nil {
		return
	}
	sess, err := s.Get(ctx, id)
	if err != nil || sess == nil {
		return
	}
	s.notifyTransition(ctx, id, sess.State)
}

// priorState reads the state a session holds BEFORE an Update is applied, so
// the caller can tell a real transition from a no-op re-write.
//
// params.State being non-nil is not enough on its own: several callers write
// the state they believe the session is already in, and firing a standing
// subscription on that would fire on a write rather than on a transition. The
// point read is skipped entirely when no observer is wired or no state is being
// written, so the common path costs nothing.
//
// A failed read reports (0, false): "cannot prove this is a transition", and the
// hook stays silent. The reconcile sweep covers the miss.
func (s *RecomputingSessionStore) priorState(ctx context.Context, id string, params UpdateSessionParams) (machine.State, bool) {
	if params.State == nil {
		return 0, false
	}
	return s.stateBefore(ctx, id)
}

// stateBefore is the parameter-free half of priorState, for the conditional
// writers whose target state is an argument rather than an UpdateSessionParams
// field. Same convention: (0, false) means "cannot prove this is a transition",
// and the point read is skipped entirely when no observer is wired.
//
// Get is not decorated (only the writers are), so this is a plain read of the
// inner store with no side effects.
func (s *RecomputingSessionStore) stateBefore(ctx context.Context, id string) (machine.State, bool) {
	if s.observer == nil {
		return 0, false
	}
	prev, err := s.Get(ctx, id)
	if err != nil || prev == nil {
		return 0, false
	}
	return prev.State, true
}

// changesState reports whether a conditional write from expectedStates to
// newState could actually have changed the state. A from-set every member of
// which already equals newState is a provable no-op even when the CAS matches a
// row. An empty set never advances, so it is not a transition either.
func changesState(newState int, expectedStates []int) bool {
	if len(expectedStates) == 0 {
		return false
	}
	for _, want := range expectedStates {
		if want != newState {
			return true
		}
	}
	return false
}

// Update delegates to the inner store and triggers a synchronous recompute
// for any write that could affect composite inputs. The recompute is
// best-effort: errors are swallowed because the original Update succeeded
// and surfacing a recompute failure would mask that.
//
// Composite inputs include State, ClaudeSessionID, and (when wired) any
// future direct writes to DisplayStatus/DisplayHasFailures/etc. Adding a new
// composite-input field to UpdateSessionParams will trigger recompute
// automatically — only the display-trio is excluded by isComputerSelfWrite.
func (s *RecomputingSessionStore) Update(ctx context.Context, id string, params UpdateSessionParams) (*models.Session, error) {
	// Read BEFORE delegating: afterwards the old state is gone, and the
	// transition observer must not fire on a write that changed nothing.
	prev, hadPrev := s.priorState(ctx, id, params)
	sess, err := s.SessionStore.Update(ctx, id, params)
	if err != nil {
		return sess, err
	}
	// Skip recompute for the computer's own writes (display-trio-only updates).
	// The computer writes back via Update with only DisplayLabel/DisplayIntent/
	// DisplaySpinner set; recursing on those would cause a write storm. Such a
	// write has State == nil, so it can never be a transition either and needs no
	// separate observer guard.
	if isComputerSelfWrite(params) {
		return sess, nil
	}
	_ = s.recomputer.Recompute(ctx, id)
	// Compare against the state the store actually persisted rather than the
	// requested one, so the observer is told the truth even if the write was
	// coerced. Recompute runs first so the display trio is already consistent by
	// the time a subscription fires off the back of this.
	if hadPrev && sess != nil && sess.State != prev {
		s.notifyTransition(ctx, id, sess.State)
	}
	return sess, nil
}

// UpdateStateConditional delegates the idempotency-gated conditional transition
// and notifies the transition observer when it wins.
//
// No point read is needed: expectedState IS the prior state (the CAS matched it
// in SQL) and the bool return is already the edge gate, so a lost CAS notifies
// nothing. The one shape that can match without changing anything —
// expectedState == newState — is excluded explicitly.
//
// It deliberately does NOT recompute. The conditional writers have never
// triggered a display recompute (they reached SQLite through the embedded
// interface before this override existed) and the poller reconciles them;
// adding one here would be an unrelated behaviour change.
func (s *RecomputingSessionStore) UpdateStateConditional(ctx context.Context, id string, newState, expectedState int) (bool, error) {
	advanced, err := s.SessionStore.UpdateStateConditional(ctx, id, newState, expectedState)
	if err == nil && advanced && newState != expectedState {
		s.notifyTransition(ctx, id, machine.State(newState))
	}
	return advanced, err
}

// UpdateStateConditionalFrom is the from-set variant, with the same edge gate:
// the CAS matched one of expectedStates, so a true return means the row moved
// unless every member of the set already equals newState.
func (s *RecomputingSessionStore) UpdateStateConditionalFrom(ctx context.Context, id string, newState int, expectedStates []int) (bool, error) {
	advanced, err := s.SessionStore.UpdateStateConditionalFrom(ctx, id, newState, expectedStates)
	if err == nil && advanced && changesState(newState, expectedStates) {
		s.notifyTransition(ctx, id, machine.State(newState))
	}
	return advanced, err
}

// OrphanHeadlessRun delegates the atomic state-and-marker stamp and recomputes
// display state when it wins the transition.
func (s *RecomputingSessionStore) OrphanHeadlessRun(ctx context.Context, id, reason string) (bool, error) {
	inner, ok := s.SessionStore.(OrphanHeadlessRunStore)
	if !ok {
		return false, fmt.Errorf("session store does not support atomic orphan marker")
	}
	advanced, err := inner.OrphanHeadlessRun(ctx, id, reason)
	if err == nil && advanced {
		_ = s.recomputer.Recompute(ctx, id)
		// ImplementingPlan -> Orphaned: an `errored` outcome that never passes
		// through Update. Read back rather than asserted, so the notification
		// cannot drift from the store's SQL.
		s.notifyPersistedState(ctx, id)
	}
	return advanced, err
}

// ClaimUnarchivedOrphan delegates the atomic marker-checked unarchived claim
// and recomputes display state when it wins the transition.
func (s *RecomputingSessionStore) ClaimUnarchivedOrphan(ctx context.Context, id, reason string) (bool, error) {
	inner, ok := s.SessionStore.(UnarchivedOrphanClaimStore)
	if !ok {
		return false, fmt.Errorf("session store does not support atomic unarchived orphan claim")
	}
	advanced, err := inner.ClaimUnarchivedOrphan(ctx, id, reason)
	if err == nil && advanced {
		_ = s.recomputer.Recompute(ctx, id)
		// Orphaned -> ImplementingPlan. Not a trigger state, but reported for the
		// same reason every other transition is: the hook's job is to observe
		// transitions, and classifying them belongs to the observer.
		s.notifyPersistedState(ctx, id)
	}
	return advanced, err
}

// ResurrectToState delegates the atomic un-archive-and-live-state write and,
// when it wins, recomputes display state and reports the transition. Both are
// required: RecomputingSessionStore embeds SessionStore, so without this
// override a new state-writing method is promoted straight through and skips
// the BOS-557 transition seam entirely.
//
// The recompute is unconditional on a winning write: the row's display state
// was computed for an archived terminal session and is stale the moment the
// resurrect lands, whatever the state column did.
//
// The notify is NOT, and cannot be inferred from the write winning. Every other
// notifyPersistedState caller is edge-triggered structurally, because its CAS
// pins the prior state to a different value (AND state = ?). This CAS pins only
// archived_at, so the prior state is unconstrained — and an archived row can
// already be wearing newState, because Lifecycle.ArchiveSession writes no state
// column, so a session archived mid-flight stays in ImplementingPlan. Firing
// there would report a transition that did not happen, which the
// SessionTransitionObserver contract forbids. So the pre-write state is point-read
// and the notify gated on it actually changing; a read that fails stays silent,
// per stateBefore, and the reconcile sweep covers the miss.
//
// The state is still read BACK rather than asserted, for the reason
// notifyPersistedState documents: the target state is a parameter here, but the
// row is what actually happened.
func (s *RecomputingSessionStore) ResurrectToState(ctx context.Context, id string, newState int) (bool, error) {
	prev, prevKnown := s.stateBefore(ctx, id)
	resurrected, err := s.SessionStore.ResurrectToState(ctx, id, newState)
	if err == nil && resurrected {
		_ = s.recomputer.Recompute(ctx, id)
		if prevKnown && int(prev) != newState {
			s.notifyPersistedState(ctx, id)
		}
	}
	return resurrected, err
}

// RollbackFailedResurrect delegates the compensating re-archive and recomputes
// display state — and deliberately notifies NOTHING.
//
// The carve-out is the same one CommitOrphanResume documents, for a stronger
// reason: this write restores the pre-resurrect state, which for the shape this
// exists to undo is Merged, and TriggerClassFor maps Merged to ClassCompleted.
// Notifying would re-fire every standing completion subscription for a
// completion that never re-happened — the session merely failed to come back.
// The recompute still runs, because the row's display state was recomputed for
// the live session the failed resurrect briefly advertised.
//
// The silence is unconditional, so for a row archived in some other state the
// reverse transition (ImplementingPlan back to that state) also goes
// unreported. That is deliberate: the observer's last-known state is
// reconciled by the standing ReconcileAll sweep, and re-firing on a
// compensating write is the failure mode this carve-out exists to prevent.
func (s *RecomputingSessionStore) RollbackFailedResurrect(ctx context.Context, id string, archivedAt time.Time, restoreState, expectState int) (bool, error) {
	restored, err := s.SessionStore.RollbackFailedResurrect(ctx, id, archivedAt, restoreState, expectState)
	if err == nil && restored {
		_ = s.recomputer.Recompute(ctx, id)
	}
	return restored, err
}

// CommitOrphanResume writes agent_session_id and blocked_reason but NOT the
// state column (the session is already in ImplementingPlan, put there by the
// preceding ClaimUnarchivedOrphan, which is where the transition was observed).
// It therefore notifies no transition — reporting one here would double-fire
// the claim's.
func (s *RecomputingSessionStore) CommitOrphanResume(ctx context.Context, id, reason string, priorAgentSession *string, newAgentSessionID string) (bool, error) {
	inner, ok := s.SessionStore.(OrphanResumeStore)
	if !ok {
		return false, fmt.Errorf("session store does not support orphan resume commit")
	}
	advanced, err := inner.CommitOrphanResume(ctx, id, reason, priorAgentSession, newAgentSessionID)
	if err == nil && advanced {
		_ = s.recomputer.Recompute(ctx, id)
	}
	return advanced, err
}

func (s *RecomputingSessionStore) ReleaseOrphanResumeClaim(ctx context.Context, id, reason string, priorAgentSession *string) (bool, error) {
	inner, ok := s.SessionStore.(OrphanResumeStore)
	if !ok {
		return false, fmt.Errorf("session store does not support orphan resume release")
	}
	advanced, err := inner.ReleaseOrphanResumeClaim(ctx, id, reason, priorAgentSession)
	if err == nil && advanced {
		_ = s.recomputer.Recompute(ctx, id)
		// ImplementingPlan -> Orphaned, read back from the row.
		s.notifyPersistedState(ctx, id)
	}
	return advanced, err
}

func (s *RecomputingSessionStore) ReparkOrphanResume(ctx context.Context, id, reason string, priorAgentSession *string, newAgentSessionID string) (bool, error) {
	inner, ok := s.SessionStore.(OrphanResumeStore)
	if !ok {
		return false, fmt.Errorf("session store does not support orphan resume repark")
	}
	advanced, err := inner.ReparkOrphanResume(ctx, id, reason, priorAgentSession, newAgentSessionID)
	if err == nil && advanced {
		_ = s.recomputer.Recompute(ctx, id)
		// ImplementingPlan -> Orphaned, read back from the row.
		s.notifyPersistedState(ctx, id)
	}
	return advanced, err
}

// isComputerSelfWrite reports whether the only non-nil fields in params are
// the display-trio (DisplayLabel/DisplayIntent/DisplaySpinner). When that
// holds, the write originated from DisplayStatusComputer.Recompute writing
// back its own output, and we must skip the recompute trigger to avoid
// infinite recursion.
//
// Any other non-nil field — even paired with the display-trio — is treated
// as a composite-input write and triggers recompute. This keeps the guard
// future-proof: new fields added to UpdateSessionParams default to
// triggering recompute, which is the correct behavior for any new
// composite input.
func isComputerSelfWrite(p UpdateSessionParams) bool {
	// At least one of the display-trio must be set; otherwise this isn't
	// the computer's write at all (an Update with no fields set is degenerate
	// but should not be classified as a self-write).
	if p.DisplayLabel == nil && p.DisplayIntent == nil && p.DisplaySpinner == nil {
		return false
	}
	// All non-display-trio fields must be nil. Enumerated explicitly to keep
	// the check obvious at the call site and to force conscious review when
	// new fields land in UpdateSessionParams.
	return p.Title == nil &&
		p.State == nil &&
		p.WorktreePath == nil &&
		p.BranchName == nil &&
		p.AgentSessionID == nil &&
		p.PRNumber == nil &&
		p.PRURL == nil &&
		p.TrackerID == nil &&
		p.TrackerURL == nil &&
		p.TmuxSessionName == nil &&
		p.LastCheckState == nil &&
		p.LastCheckStateHeadSHA == nil &&
		p.LastCheckStateAt == nil &&
		p.IsAutomationEnabled == nil &&
		p.AttemptCount == nil &&
		p.BlockedReason == nil &&
		p.ArchivedAt == nil
}

// RecomputingWorkflowStore wraps a WorkflowStore so workflow lifecycle
// transitions (Create, Update) trigger a display recompute on the workflow's
// session. Workflow status is one of the four inputs to the composite, so
// every transition matters.
type RecomputingWorkflowStore struct {
	WorkflowStore
	recomputer SessionRecomputer
}

// NewRecomputingWorkflowStore wires inner with recomputer.
func NewRecomputingWorkflowStore(inner WorkflowStore, recomputer SessionRecomputer) *RecomputingWorkflowStore {
	return &RecomputingWorkflowStore{WorkflowStore: inner, recomputer: recomputer}
}

// Create delegates and triggers a recompute on the new workflow's session.
func (s *RecomputingWorkflowStore) Create(ctx context.Context, params CreateWorkflowParams) (*models.Workflow, error) {
	w, err := s.WorkflowStore.Create(ctx, params)
	if err != nil {
		return w, err
	}
	_ = s.recomputer.Recompute(ctx, params.SessionID)
	return w, nil
}

// Update delegates and triggers a recompute on the workflow's session.
// We resolve the session ID from the returned workflow rather than asking
// the caller to pass it, which keeps the call sites identical to the bare
// store and means the wrapper is transparent.
func (s *RecomputingWorkflowStore) Update(ctx context.Context, id string, params UpdateWorkflowParams) (*models.Workflow, error) {
	w, err := s.WorkflowStore.Update(ctx, id, params)
	if err != nil {
		return w, err
	}
	if w != nil {
		_ = s.recomputer.Recompute(ctx, w.SessionID)
	}
	return w, nil
}

// FailOrphaned delegates without triggering recomputes — it runs once at
// daemon startup before any sessions are observed, and the startup backfill
// in cmd/main.go handles the catch-up recompute for every active session.
func (s *RecomputingWorkflowStore) FailOrphaned(ctx context.Context) (int64, error) {
	return s.WorkflowStore.FailOrphaned(ctx)
}
