// Package status — DisplayStatusComputer composes a session's display label
// from per-session inputs (chat status, active workflows, display tracker, PR
// status) and persists the result onto the sessions row.
//
// The composition algorithm itself lives in lib/bossalib/displaystatus.Compute;
// this file is the bossd-side glue that hydrates a *pb.Session from the
// in-memory trackers and the DB, then writes the three composite columns back
// to the row when they change.
package status

import (
	"context"
	"database/sql"
	"errors"

	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/rs/zerolog"
)

// Recomputer is the minimal interface trackers and store wrappers depend on.
// Returning an interface (rather than passing the concrete *DisplayStatusComputer)
// keeps the internal/db package free of an internal/status dependency and
// makes tests trivial to fake.
type Recomputer interface {
	Recompute(ctx context.Context, sessionID string) error
}

// noopRecomputer is the safe default for callers that haven't wired a real
// computer (most unit tests). It satisfies the interface without doing
// anything so trackers and store wrappers don't need nil checks scattered
// through every write site.
type noopRecomputer struct{}

// NoopRecomputer returns a Recomputer whose Recompute is a no-op. Tests that
// construct trackers or stores in isolation pass this rather than threading a
// real DisplayStatusComputer through the test setup.
func NoopRecomputer() Recomputer { return noopRecomputer{} }

func (noopRecomputer) Recompute(context.Context, string) error { return nil }

// ChatStatusReader reads the latest cached chat status for a claude_id. The
// concrete *Tracker satisfies it; the indirection lets tests inject a stub.
type ChatStatusReader interface {
	Get(claudeID string) *Entry
}

// DisplayStatusComputer composes a session's unified display status by
// combining the session row, the in-memory display tracker, the chat status
// tracker, and the active-workflow store, then persists the result.
type DisplayStatusComputer struct {
	sessions  db.SessionStore
	display   *DisplayTracker
	chat      ChatStatusReader
	chats     db.ClaudeChatStore
	workflows db.WorkflowStore
	logger    zerolog.Logger
}

// NewDisplayStatusComputer constructs a computer wired to the given inputs.
// Any field may be nil — Recompute degrades gracefully when an input is
// unavailable (e.g. tests that don't wire the chat tracker).
func NewDisplayStatusComputer(
	sessions db.SessionStore,
	display *DisplayTracker,
	chat ChatStatusReader,
	chats db.ClaudeChatStore,
	workflows db.WorkflowStore,
	logger zerolog.Logger,
) *DisplayStatusComputer {
	return &DisplayStatusComputer{
		sessions:  sessions,
		display:   display,
		chat:      chat,
		chats:     chats,
		workflows: workflows,
		logger:    logger,
	}
}

// Recompute reads all inputs for sessionID, runs displaystatus.Compute, and
// writes the resulting (label, intent, spinner) back onto the session row if
// any of the three values changed. The write is gated on inequality so
// repeated calls with no input changes are a no-op (idempotent), and so a
// recompute triggered by a write to display fields can't loop on itself.
func (c *DisplayStatusComputer) Recompute(ctx context.Context, sessionID string) error {
	if c == nil || c.sessions == nil {
		return nil
	}

	sess, err := c.sessions.Get(ctx, sessionID)
	if err != nil {
		// Session may have been deleted between trigger and recompute (lost
		// race). That's expected during teardown; log at debug. Any other
		// lookup error (DB connection issue, corruption, etc.) is operationally
		// meaningful and should surface at warn.
		if errors.Is(err, sql.ErrNoRows) {
			c.logger.Debug().Err(err).Str("session_id", sessionID).Msg("recompute: session lookup failed")
		} else {
			c.logger.Warn().Err(err).Str("session_id", sessionID).Msg("recompute: session lookup failed")
		}
		return nil
	}

	pbSess := sessionToProto(sess)

	// Hydrate display tracker fields onto the proto session.
	if c.display != nil {
		if e := c.display.Get(sessionID); e != nil {
			pbSess.DisplayStatus = pb.DisplayStatus(e.Status)
			pbSess.DisplayHasFailures = e.HasFailures
			pbSess.DisplayHasChangesRequested = e.HasChangesRequested
			pbSess.DisplayIsRepairing = e.IsRepairing
		}
	}

	// Hydrate active workflow fields. Mirrors the per-session selection in
	// server.ListSessions: prefer the highest-priority active workflow.
	if c.workflows != nil {
		active, wfErr := c.workflows.ListActiveBySessionIDs(ctx, []string{sessionID})
		if wfErr == nil {
			var best *models.Workflow
			for _, w := range active {
				if best == nil || workflowPriority(w.Status) > workflowPriority(best.Status) {
					best = w
				}
			}
			if best != nil {
				// Don't surface stale workflow status for sessions whose PRs
				// are merged or closed — matches server.ListSessions.
				if pbSess.DisplayStatus != pb.DisplayStatus_DISPLAY_STATUS_MERGED &&
					pbSess.DisplayStatus != pb.DisplayStatus_DISPLAY_STATUS_CLOSED {
					pbSess.WorkflowDisplayStatus = workflowStatusToProto(best.Status)
					pbSess.WorkflowDisplayLeg = int32(best.FlightLeg)
					pbSess.WorkflowDisplayMaxLegs = int32(best.MaxLegs)
				}
			}
		}
	}

	// Resolve chat status. The session's claude_session_id is the claude_id
	// the tracker is keyed by; fall back to looking up the most-recent chat
	// for the session if the field isn't set.
	chatStatus := pb.ChatStatus_CHAT_STATUS_STOPPED
	claudeID := ""
	if sess.ClaudeSessionID != nil {
		claudeID = *sess.ClaudeSessionID
	}
	if claudeID == "" && c.chats != nil {
		// Pick the most recent chat for the session as the representative
		// status source. ListBySession is ordered DESC by created_at in the
		// SQLite store, so chats[0] is the newest.
		if chats, listErr := c.chats.ListBySession(ctx, sessionID); listErr == nil && len(chats) > 0 {
			claudeID = chats[0].ClaudeID
		}
	}
	if claudeID != "" && c.chat != nil {
		if e := c.chat.Get(claudeID); e != nil {
			chatStatus = e.Status
		}
	}

	out := displaystatus.Compute(displaystatus.Input{
		Session:    pbSess,
		ChatStatus: chatStatus,
	})

	// Skip the UPDATE when nothing changed — keeps recompute idempotent and
	// avoids spurious updated_at bumps.
	if sess.DisplayLabel == out.Label &&
		sess.DisplayIntent == int32(out.Intent) &&
		sess.DisplaySpinner == out.Spinner {
		return nil
	}

	intent := int32(out.Intent)
	if _, err := c.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		DisplayLabel:   &out.Label,
		DisplayIntent:  &intent,
		DisplaySpinner: &out.Spinner,
	}); err != nil {
		// A failed write here means a stale display label persists on the row
		// — operationally meaningful, so log at warn. Lost-race deletes
		// (sql.ErrNoRows) are still possible but rare enough at this point
		// that we don't bother demoting them.
		c.logger.Warn().Err(err).Str("session_id", sessionID).Msg("recompute: update failed")
		return nil
	}
	return nil
}

// sessionToProto builds a minimal *pb.Session for the display computer's
// hydration step. We deliberately avoid importing server.SessionToProto to
// keep status from depending on server (server already depends on status).
func sessionToProto(s *models.Session) *pb.Session {
	if s == nil {
		return nil
	}
	return &pb.Session{
		Id:             s.ID,
		State:          pb.SessionState(s.State),
		DisplayLabel:   s.DisplayLabel,
		DisplayIntent:  pb.DisplayIntent(s.DisplayIntent),
		DisplaySpinner: s.DisplaySpinner,
	}
}

// workflowPriority is duplicated from server/convert.go to avoid an import
// cycle. Keep in sync; the values matter only relative to each other.
func workflowPriority(s models.WorkflowStatus) int {
	switch s {
	case models.WorkflowStatusRunning:
		return 4
	case models.WorkflowStatusPending:
		return 3
	case models.WorkflowStatusPaused:
		return 2
	case models.WorkflowStatusFailed, models.WorkflowStatusCancelled:
		return 1
	default:
		return 0
	}
}

// workflowStatusToProto mirrors server/convert.go. Kept private here for the
// same reason as workflowPriority.
func workflowStatusToProto(s models.WorkflowStatus) pb.WorkflowStatus {
	switch s {
	case models.WorkflowStatusPending:
		return pb.WorkflowStatus_WORKFLOW_STATUS_PENDING
	case models.WorkflowStatusRunning:
		return pb.WorkflowStatus_WORKFLOW_STATUS_RUNNING
	case models.WorkflowStatusPaused:
		return pb.WorkflowStatus_WORKFLOW_STATUS_PAUSED
	case models.WorkflowStatusCompleted:
		return pb.WorkflowStatus_WORKFLOW_STATUS_COMPLETED
	case models.WorkflowStatusFailed:
		return pb.WorkflowStatus_WORKFLOW_STATUS_FAILED
	case models.WorkflowStatusCancelled:
		return pb.WorkflowStatus_WORKFLOW_STATUS_CANCELLED
	default:
		return pb.WorkflowStatus_WORKFLOW_STATUS_UNSPECIFIED
	}
}
