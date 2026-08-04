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
	"time"

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
	Get(agentSessionID string) *Entry
}

// StalledReader reports whether a chat has raised the agent-stalled attention
// (BOS-667). It is an OPTIONAL capability of the injected ChatStatusReader —
// *Tracker satisfies it; a test stub need not. When the reader does not satisfy
// it, no chat is ever treated as stalled, which is the fail-open direction that
// matters here: the only thing the stalled signal does in this file is SUPPRESS
// a waiting derivation, so a reader that cannot answer simply derives waiting.
type StalledReader interface {
	Stalled(agentSessionID string) bool
}

// WaitingWriter stores the reason a chat is parked on an external event
// (BOS-668). Like StalledReader it is an optional capability of the injected
// ChatStatusReader, so the derivation can hold its result where the TUI and the
// status RPCs already look without widening NewDisplayStatusComputer.
type WaitingWriter interface {
	SetWaiting(agentSessionID, reason string)
}

// WaitingLookup resolves the canonical reason a chat is parked on an external
// event, or "" when it is not parked on anything. The bossd wiring backs it with
// callback.WaitingReasonForChat over the GitHub callback store; keeping it an
// interface is what stops internal/status from importing internal/callback.
//
// An implementation MUST return the wording produced by
// displaystatus.CallbackWaitingReason rather than spelling it itself.
type WaitingLookup interface {
	WaitingReason(ctx context.Context, agentSessionID string) (string, error)
}

// WaitingLookupFunc adapts a function to WaitingLookup.
type WaitingLookupFunc func(ctx context.Context, agentSessionID string) (string, error)

// WaitingReason implements WaitingLookup.
func (f WaitingLookupFunc) WaitingReason(ctx context.Context, agentSessionID string) (string, error) {
	return f(ctx, agentSessionID)
}

// DisplayStatusComputer composes a session's unified display status by
// combining the session row, the in-memory display tracker, the chat status
// tracker, and the active-workflow store, then persists the result.
type DisplayStatusComputer struct {
	sessions  db.SessionStore
	display   *DisplayTracker
	chat      ChatStatusReader
	chats     db.AgentChatStore
	workflows db.WorkflowStore
	logger    zerolog.Logger
	// onUpdate, when non-nil, is invoked after a successful display-trio
	// write. It is the chokepoint used to fan recomputed labels out to the
	// reverse stream — without this, the initial DaemonSnapshot is the
	// only state bosso ever sees, and labels recomputed after startup
	// (the display poller's gh-pr-checks results, chat status changes,
	// workflow transitions) never reach the web UI.
	onUpdate func(ctx context.Context, sessionID string)

	// waiting, when non-nil, resolves the external event a chat is parked on
	// (BOS-668). Left nil the computer never derives a waiting state at all and
	// every chat keeps the status it reported — the pre-BOS-668 behaviour.
	waiting WaitingLookup
}

// SetWaitingLookup wires the resolver used to decide whether a working chat is
// really parked on an external event. Set separately from the constructor
// because the lookup is backed by the GitHub callback store, which the daemon
// builds after the computer.
func (c *DisplayStatusComputer) SetWaitingLookup(l WaitingLookup) {
	c.waiting = l
}

// SetOnUpdate wires a post-write callback. Called after Recompute writes a
// new (label, intent, spinner) trio to the DB. The callback receives the
// session_id so it can re-read the full row and project to pb.Session in
// whatever form the publisher needs — keeping this package free of an
// upstream / server import.
func (c *DisplayStatusComputer) SetOnUpdate(fn func(ctx context.Context, sessionID string)) {
	c.onUpdate = fn
}

// NewDisplayStatusComputer constructs a computer wired to the given inputs.
// Any field may be nil — Recompute degrades gracefully when an input is
// unavailable (e.g. tests that don't wire the chat tracker).
func NewDisplayStatusComputer(
	sessions db.SessionStore,
	display *DisplayTracker,
	chat ChatStatusReader,
	chats db.AgentChatStore,
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
			pbSess.DisplayStatus = pb.DisplayStatus(clampInt32(int(e.Status)))
			pbSess.DisplayHasFailures = e.HasFailures
			pbSess.DisplayHasChangesRequested = e.HasChangesRequested
			pbSess.DisplayIsRepairing = e.IsRepairing
			pbSess.DisplaySettingUp = e.SettingUp
			pbSess.DisplayMerging = e.Merging
			pbSess.ArchivePending = e.Archiving
			pbSess.PrMergeable = e.Mergeable
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
					pbSess.WorkflowDisplayLeg = clampInt32(best.FlightLeg)
					pbSess.WorkflowDisplayMaxLegs = clampInt32(best.MaxLegs)
				}
			}
		}
	}

	// Resolve chat status by aggregating across every chat in the session.
	// A session can have multiple chats — when any one of them is asking a
	// question or actively working, the session-level label must reflect
	// that rather than falling through to the PR display status. Reading
	// only sess.AgentSessionID would miss this: that field is written at
	// session create / resurrect time and is not updated when the user adds
	// a new chat, so it can keep pointing at a now-stopped chat while a
	// freshly-created sibling is the one actually working. Precedence
	// (QUESTION > LIMITED > WORKING > IDLE > STOPPED) mirrors
	// Server.GetSessionStatuses so the chat picker and the session list agree.
	chatStatus := pb.ChatStatus_CHAT_STATUS_STOPPED
	var chatResetAt time.Time
	if c.chats != nil && c.chat != nil {
		chatList, listErr := c.chats.ListBySession(ctx, sessionID)
		if listErr == nil {
			// Pass 1: resolve every chat's status. This is deliberately its own
			// pass and not fused with the fold below, because deriveChatStatus
			// is not a pure read — it is also what CLEARS a stale waiting reason
			// off a chat that has stopped being parked. Short-circuiting the
			// fold (QUESTION wins outright) must therefore never skip a later
			// chat's derivation: a session whose first chat asks a question
			// would otherwise strand a drained callback's reason on a sibling
			// forever, since Tracker.Update preserves the marker and Cleanup
			// only fires past StaleThreshold.
			type resolvedChat struct {
				status  pb.ChatStatus
				resetAt time.Time
			}
			resolved := make([]resolvedChat, 0, len(chatList))
			for _, chat := range chatList {
				e := c.chat.Get(chat.AgentSessionID)
				if e == nil {
					continue
				}
				// A working chat parked on an external event reads as WAITING
				// from here down.
				resolved = append(resolved, resolvedChat{
					status:  c.deriveChatStatus(ctx, chat.AgentSessionID, e.Status),
					resetAt: e.ResetAt,
				})
			}

			// Pass 2: fold the resolved statuses into the session-level label.
			// Pure, so the QUESTION short-circuit costs nothing.
			for _, r := range resolved {
				if r.status == pb.ChatStatus_CHAT_STATUS_QUESTION {
					chatStatus = pb.ChatStatus_CHAT_STATUS_QUESTION
					chatResetAt = time.Time{}
					break
				}
				// LIMITED ranks just below QUESTION (which short-circuits above),
				// so it beats WORKING/IDLE/STOPPED but never overrides a question.
				if r.status == pb.ChatStatus_CHAT_STATUS_LIMITED {
					chatStatus = pb.ChatStatus_CHAT_STATUS_LIMITED
					chatResetAt = r.resetAt
				}
				// A QUESTION chat short-circuits the loop above, so chatStatus
				// is never QUESTION here — WORKING can upgrade STOPPED/IDLE
				// without guarding against a QUESTION it can't observe.
				if r.status == pb.ChatStatus_CHAT_STATUS_WORKING && chatStatus != pb.ChatStatus_CHAT_STATUS_LIMITED {
					chatStatus = pb.ChatStatus_CHAT_STATUS_WORKING
				}
				// WAITING sits below WORKING in the AGGREGATE (it only upgrades
				// STOPPED/IDLE, and the WORKING branch above freely overwrites
				// it) even though it outranks WORKING for a single chat. A
				// session holding one parked chat and one genuinely working chat
				// has live work in it and must not read as idle-ish; the parked
				// chat still carries its own reason for the per-chat view.
				if r.status == pb.ChatStatus_CHAT_STATUS_WAITING &&
					(chatStatus == pb.ChatStatus_CHAT_STATUS_STOPPED || chatStatus == pb.ChatStatus_CHAT_STATUS_IDLE) {
					chatStatus = pb.ChatStatus_CHAT_STATUS_WAITING
				}
				if r.status == pb.ChatStatus_CHAT_STATUS_IDLE && chatStatus == pb.ChatStatus_CHAT_STATUS_STOPPED {
					chatStatus = pb.ChatStatus_CHAT_STATUS_IDLE
				}
			}
		}
	}

	out := displaystatus.Compute(displaystatus.Input{
		Session:     pbSess,
		ChatStatus:  chatStatus,
		ChatResetAt: chatResetAt,
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
	if c.onUpdate != nil {
		c.onUpdate(ctx, sessionID)
	}
	return nil
}

// deriveChatStatus resolves one chat's effective status, rewriting a WORKING
// chat that is parked on an external event to CHAT_STATUS_WAITING and storing
// the reason on the tracker (BOS-668). It returns reported unchanged for every
// other status, and always writes the chat's current reason — including "" —
// so a reason can never outlive the condition that produced it.
//
// The precedence it implements, for a single chat, is
//
//	LIMITED > QUESTION > STALLED-attention > WAITING > WORKING
//
// read as: waiting loses to everything that demands a human. LIMITED and
// QUESTION are statuses the chat itself reports, so they are simply never
// rewritten. The stalled attention is different in kind — it rides ON a chat
// still reporting WORKING — so it has to be checked explicitly, and it is the
// one guard whose absence would be invisible: a stalled chat that also happens
// to hold an armed callback would be soothed into "waiting" and its
// ATTENTION_REASON_AGENT_STALLED would look like the system working as intended.
//
// Over-reporting waiting is the failure mode to guard against, so every
// uncertain path — no lookup wired, a lookup error, an empty reason — resolves
// to "not waiting".
func (c *DisplayStatusComputer) deriveChatStatus(ctx context.Context, agentSessionID string, reported pb.ChatStatus) pb.ChatStatus {
	writer, canWrite := c.chat.(WaitingWriter)

	eligible := reported == pb.ChatStatus_CHAT_STATUS_WORKING && c.waiting != nil
	if eligible {
		if stalled, ok := c.chat.(StalledReader); ok && stalled.Stalled(agentSessionID) {
			eligible = false
		}
	}

	reason := ""
	if eligible {
		got, err := c.waiting.WaitingReason(ctx, agentSessionID)
		if err != nil {
			c.logger.Warn().Err(err).
				Str("agent_session_id", agentSessionID).
				Msg("recompute: waiting-reason lookup failed; treating chat as working")
		} else {
			reason = got
		}
	}

	if canWrite {
		writer.SetWaiting(agentSessionID, reason)
	}
	if reason == "" {
		return reported
	}
	return pb.ChatStatus_CHAT_STATUS_WAITING
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
		State:          pb.SessionState(clampInt32(int(s.State))),
		BlockedReason:  s.BlockedReason,
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
