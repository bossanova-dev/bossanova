package db

import (
	"context"

	"github.com/recurser/bossalib/models"
)

// SessionRecomputer is the small interface the recomputing store wrappers
// depend on. It is implemented by status.DisplayStatusComputer in production;
// keeping the interface here prevents an import cycle (db must not import
// status, which imports db).
type SessionRecomputer interface {
	Recompute(ctx context.Context, sessionID string) error
}

// RecomputingSessionStore wraps a SessionStore so that any Update touching a
// composite-input field triggers a display-status recompute on the same
// session. The decorator's invariant is "synchronous recomputation at every
// write site that touches composite inputs" — so we trigger Recompute on
// every Update except when the only fields being written are the
// display-trio (DisplayLabel/DisplayIntent/DisplaySpinner). Those are the
// computer's own write-back, and recursing on them would cause a write storm.
type RecomputingSessionStore struct {
	SessionStore
	recomputer SessionRecomputer
}

// NewRecomputingSessionStore wires inner with the given recomputer. Pass a
// no-op recomputer in tests that don't care about the side-effect; the
// wrapper does not nil-check so callers must supply something non-nil.
func NewRecomputingSessionStore(inner SessionStore, recomputer SessionRecomputer) *RecomputingSessionStore {
	return &RecomputingSessionStore{SessionStore: inner, recomputer: recomputer}
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
	sess, err := s.SessionStore.Update(ctx, id, params)
	if err != nil {
		return sess, err
	}
	// Skip recompute for the computer's own writes (display-trio-only updates).
	// The computer writes back via Update with only DisplayLabel/DisplayIntent/
	// DisplaySpinner set; recursing on those would cause a write storm.
	if isComputerSelfWrite(params) {
		return sess, nil
	}
	_ = s.recomputer.Recompute(ctx, id)
	return sess, nil
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
		p.ClaudeSessionID == nil &&
		p.PRNumber == nil &&
		p.PRURL == nil &&
		p.TrackerID == nil &&
		p.TrackerURL == nil &&
		p.TmuxSessionName == nil &&
		p.LastCheckState == nil &&
		p.AutomationEnabled == nil &&
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
