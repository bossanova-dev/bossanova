// Package displaystatus computes a session's unified display status.
//
// This is the single source of truth for the session STATUS column shown in
// both the Boss TUI and the Bosso web UI. Clients pass in the relevant
// session/chat state via Input and receive an Output describing the label,
// the semantic intent (which clients map to colors/styles), and whether a
// spinner glyph should be rendered before the label.
package displaystatus

import (
	"fmt"
	"strings"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/sessionreason"
)

// Input bundles the inputs used by Compute.
//
// ChatStatus is passed separately from Session because not all callers will
// derive it from a Session field — some have it from a live daemon heartbeat.
// Workflow status, leg, and max-legs are read from the Session's
// WorkflowDisplay* fields populated by Phase 1.
type Input struct {
	// Session carries BlockedReason, DisplayStatus, DisplayIsRepairing, the
	// DisplayHas{Failures,ChangesRequested} flags, and the WorkflowDisplay*
	// fields. May be nil — Compute treats a nil Session like one with all
	// fields zero-valued.
	Session    *pb.Session
	ChatStatus pb.ChatStatus
	// ChatResetAt is the winning chat's usage-limit reset time when ChatStatus
	// is CHAT_STATUS_LIMITED (zero otherwise). Plumbed through so the reset
	// value is available to downstream consumers; the human-readable
	// "resets ~HH:MM" rendering is BOS-167 and deliberately not done here.
	ChatResetAt time.Time
}

// Output is the rendered result clients consume.
type Output struct {
	// Label is the visible text (e.g. "running 2/5", "✓ passing", "stopped").
	Label string
	// Intent is the semantic meaning. Clients map this to their styling
	// (lipgloss colors in the TUI; CSS classes in the web UI).
	Intent pb.DisplayIntent
	// Spinner is true when the algorithm wants a spinner glyph rendered
	// immediately before the label.
	Spinner bool
}

// QuestionLabel is the display label emitted when a chat requires a response.
const QuestionLabel = "? question"

// IsQuestionLabel reports whether label is the display status used for a chat
// that requires a response.
func IsQuestionLabel(label string) bool {
	return label == QuestionLabel
}

// Package-level label constants shared between the base cascade producer
// (prOutput) and the errored-recolor guard (isMutedTerminalPR) so the literal
// text lives in exactly one place.
const (
	labelMerged = "✓ merged"
	labelClosed = "closed"
)

// Compute runs the precedence cascade that determines a session's display
// status and then applies the errored-recolor overlay.
//
// The base cascade (baseStatus) is intentionally identical to the legacy
// renderPRDisplayStatus — every label, intent, and spinner flag matches. On top
// of it, an errored session (orphaned by a daemon restart, or blocked) keeps its
// REAL underlying status label and spinner — so a live working chat or a pending
// question is never hidden behind a static "orphaned" label — but its intent is
// recolored to DANGER so the error stays visible. Honest-green is preserved: a
// dead run's bootstrap-only passing PR shows "✓ passing" in red, never green.
// The one exception is a terminal muted PR label ("✓ merged" / "closed"): those
// are legitimate end states and stay MUTED, not alarmed red.
func Compute(in Input) Output {
	out := baseStatus(in)
	if errored(in) && !isMutedTerminalPR(out) {
		out.Intent = pb.DisplayIntent_DISPLAY_INTENT_DANGER
	}
	return out
}

// ComputeBase runs the precedence cascade WITHOUT the BOS-430 errored-recolor
// overlay — i.e. the exact shape Compute produced before that overlay existed.
//
// It exists for the apiversion down-converts that must reproduce a pre-overlay
// display for older clients while recomputing from a modified Input. In
// particular LimitedChatStatusChange (V20260706) remaps a "usage-limited…"
// session back to its idle-style fallback by recomputing with ChatStatus IDLE;
// because that client predates BOS-430 it must NOT observe the errored recolor,
// so it recomputes through ComputeBase rather than Compute. For a non-errored
// session ComputeBase and Compute are identical (the overlay is a no-op there).
func ComputeBase(in Input) Output {
	return baseStatus(in)
}

// errored reports whether the session is in an error state (orphaned by a daemon
// restart, or blocked). Mirrors HydrateBaseAttention's Blocked/Orphaned scope
// (services/bossd/internal/server/server.go) and reads only Session.State, which
// every Compute caller already populates — no new repo/chat plumbing.
func errored(in Input) bool {
	return in.Session != nil &&
		(in.Session.State == pb.SessionState_SESSION_STATE_ORPHANED ||
			in.Session.State == pb.SessionState_SESSION_STATE_BLOCKED)
}

// isMutedTerminalPR reports whether the computed base status is a terminal muted
// PR label. A merged/closed PR is a legitimate end state, so an errored session
// whose real status is merged/closed stays MUTED rather than being alarmed red.
func isMutedTerminalPR(out Output) bool {
	return out.Label == labelMerged || out.Label == labelClosed
}

// PreErroredOutput reproduces the display Output that Compute produced for an
// errored (orphaned/blocked) session BEFORE the BOS-430 errored-recolor overlay
// existed. It is the exact inverse of that overlay and exists so the
// OrchestratorService apiversion down-convert (ErroredStatusChange, V20260718)
// can restore the prior observable behavior for clients pinned to an older
// version. sess is a served Session whose DisplayLabel/DisplayIntent/
// DisplaySpinner are the current (post-overlay) values.
//
// Pre-BOS-430 the two states behaved differently, so the inverse does too:
//   - ORPHANED short-circuited unconditionally to "orphaned" / DANGER / no
//     spinner — it won over every other branch, including a merged/closed PR —
//     so the inverse ignores the served label and returns that fixed tuple.
//   - BLOCKED had no special handling and fell through the base cascade, so only
//     its intent was recolored (label and spinner are already the base values);
//     the inverse restores the base intent from the served label.
//
// A session whose display was never computed (empty label) is returned
// unchanged: neither the old nor the new Compute wrote to it, so there is
// nothing to invert.
func PreErroredOutput(sess *pb.Session) Output {
	served := Output{
		Label:   sess.GetDisplayLabel(),
		Intent:  sess.GetDisplayIntent(),
		Spinner: sess.GetDisplaySpinner(),
	}
	if served.Label == "" {
		return served
	}
	switch sess.GetState() {
	case pb.SessionState_SESSION_STATE_ORPHANED:
		return Output{Label: "orphaned", Intent: pb.DisplayIntent_DISPLAY_INTENT_DANGER}
	case pb.SessionState_SESSION_STATE_BLOCKED:
		return Output{
			Label:   served.Label,
			Intent:  preErroredBlockedIntent(sess, served.Label, served.Intent),
			Spinner: served.Spinner,
		}
	default:
		// Not an errored session — nothing was recolored, so leave it be.
		return served
	}
}

// preErroredBlockedIntent returns the base (pre-recolor) intent baseStatus would
// have produced for a BLOCKED session showing servedLabel. The PR- and
// workflow-derived producers (prOutput/workflowOutput) are reused directly so
// the bulk of the mapping cannot drift from the cascade; only the chat- and
// transient-flag labels, whose intents are fixed, are enumerated. Any label
// whose base intent is already DANGER (⨯ failing/conflict/rejected, a
// prNeedsFix "working", a failing "checking") falls through to servedIntent —
// the recolor was a no-op there, so the served DANGER is already correct.
func preErroredBlockedIntent(sess *pb.Session, servedLabel string, servedIntent pb.DisplayIntent) pb.DisplayIntent {
	if out, ok := prOutput(sess); ok && out.Label == servedLabel {
		return out.Intent
	}
	if out, ok := workflowOutput(sess); ok && out.Label == servedLabel {
		return out.Intent
	}
	if servedLabel == "working" {
		if prNeedsFix(sess) {
			return pb.DisplayIntent_DISPLAY_INTENT_DANGER
		}
		return pb.DisplayIntent_DISPLAY_INTENT_SUCCESS
	}
	switch {
	case IsQuestionLabel(servedLabel),
		servedLabel == "? PR failed",
		servedLabel == "archiving",
		servedLabel == "repairing",
		servedLabel == "idle",
		strings.HasPrefix(servedLabel, "usage-limited"):
		return pb.DisplayIntent_DISPLAY_INTENT_WARNING
	case servedLabel == "initializing",
		servedLabel == "merging":
		return pb.DisplayIntent_DISPLAY_INTENT_INFO
	case servedLabel == "stopped":
		return pb.DisplayIntent_DISPLAY_INTENT_MUTED
	default:
		// Base intent was already DANGER (the recolor changed nothing), so the
		// served intent is the base intent.
		return servedIntent
	}
}

// baseStatus runs the precedence cascade that determines a session's base
// display status, before the errored-recolor overlay in Compute.
//
// Precedence (highest first):
//  1. ChatStatus QUESTION → "? question" / WARNING / no spinner
//     1b. ChatStatus LIMITED → "usage-limited (resets ~HH:MM)" (or "usage-limited"
//     when no reset time is known) / WARNING / no spinner (below QUESTION,
//     above WORKING and the PR-derived labels)
//  2. Draft PR failure    → "? PR failed" / WARNING / no spinner
//  3. DisplaySettingUp    → "initializing" / INFO / spinner
//     3b. DisplayMerging  → "merging" / INFO / spinner (a PR merge in flight;
//     wins over the stale PR-derived labels so a passing/approved session shows
//     "merging" for the merge's full duration)
//     3c. ArchivePending  → "archiving" / WARNING / spinner (an archive in
//     flight; wins over the stale MERGED label so an auto-archiving session
//     shows "archiving" until the archive completes or errors)
//  4. ChatStatus WORKING  → "working"    / SUCCESS / spinner
//  5. Active workflow     → "running L/M", "pending", "paused L/M",
//     "failed L/M", "cancelled" with matching intents
//  6. DisplayIsRepairing  → "repairing" / WARNING / spinner
//  7. PR DisplayStatus    → "✓ merged", "closed", "✓ approved", "✓ review", "✓ passing",
//     "⨯ failing", "⨯ conflict", "⨯ rejected", "draft", "checking"
//  8. ChatStatus IDLE     → "idle" / WARNING
//  9. default             → "stopped" / MUTED
func baseStatus(in Input) Output {
	if in.ChatStatus == pb.ChatStatus_CHAT_STATUS_QUESTION {
		return Output{Label: QuestionLabel, Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING}
	}
	// A usage-limit banner ranks immediately below QUESTION: a limited chat
	// wins over WORKING/IDLE/the PR-derived labels but a live question still
	// takes priority. When Input.ChatResetAt is known we compose the reset
	// time into the label ("usage-limited (resets ~HH:MM)"); otherwise we fall
	// back to a bare "usage-limited".
	if in.ChatStatus == pb.ChatStatus_CHAT_STATUS_LIMITED {
		label := "usage-limited"
		if !in.ChatResetAt.IsZero() {
			label += " (resets ~" + in.ChatResetAt.Format("15:04") + ")"
		}
		return Output{Label: label, Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING}
	}
	if in.Session != nil && sessionreason.IsDraftPRCreationFailure(in.Session.BlockedReason) {
		return Output{Label: "? PR failed", Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING}
	}
	if in.Session != nil && in.Session.DisplaySettingUp {
		return Output{Label: "initializing", Intent: pb.DisplayIntent_DISPLAY_INTENT_INFO, Spinner: true}
	}
	// A PR merge is in flight (MergeSession set the transient flag). This must
	// win over the stale PR-derived labels below (branch 7) so a passing/approved
	// session renders "merging" for the full merge duration instead of a green
	// label the merge is about to invalidate.
	if in.Session != nil && in.Session.DisplayMerging {
		return Output{Label: "merging", Intent: pb.DisplayIntent_DISPLAY_INTENT_INFO, Spinner: true}
	}
	// An archive is in flight for this session — the daemon set the transient
	// archive_pending flag via DisplayTracker.SetArchiving around the single
	// Lifecycle.ArchiveSession chokepoint (any trigger: auto-after-merge, manual,
	// dependabot). This must win over the stale MERGED PR label below (branch 7)
	// so an auto-archiving session renders "archiving" for the archive's full
	// duration instead of "✓ merged", matching the manual-archive list override
	// (renderArchivingStatus) and the BOS-425 session-detail view. Placed right
	// after merging — both are transient post-terminal overrides — so a live
	// QUESTION above still wins, but the archive beats the merged PR label.
	if in.Session != nil && in.Session.ArchivePending {
		return Output{Label: "archiving", Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING, Spinner: true}
	}
	if in.ChatStatus == pb.ChatStatus_CHAT_STATUS_WORKING {
		intent := pb.DisplayIntent_DISPLAY_INTENT_SUCCESS
		if prNeedsFix(in.Session) {
			intent = pb.DisplayIntent_DISPLAY_INTENT_DANGER
		}
		return Output{Label: "working", Intent: intent, Spinner: true}
	}
	if out, ok := workflowOutput(in.Session); ok {
		return out
	}
	if in.Session != nil && in.Session.DisplayIsRepairing {
		return Output{Label: "repairing", Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING, Spinner: true}
	}
	if out, ok := prOutput(in.Session); ok {
		return out
	}
	if in.ChatStatus == pb.ChatStatus_CHAT_STATUS_IDLE {
		return Output{Label: "idle", Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING}
	}
	return Output{Label: "stopped", Intent: pb.DisplayIntent_DISPLAY_INTENT_MUTED}
}

func prNeedsFix(sess *pb.Session) bool {
	if sess == nil {
		return false
	}
	switch sess.DisplayStatus {
	case pb.DisplayStatus_DISPLAY_STATUS_FAILING,
		pb.DisplayStatus_DISPLAY_STATUS_CONFLICT,
		pb.DisplayStatus_DISPLAY_STATUS_REJECTED:
		return true
	case pb.DisplayStatus_DISPLAY_STATUS_CHECKING:
		return sess.DisplayHasFailures || sess.DisplayHasChangesRequested
	default:
		return false
	}
}

func workflowOutput(sess *pb.Session) (Output, bool) {
	if sess == nil {
		return Output{}, false
	}
	leg := sess.WorkflowDisplayLeg
	maxLegs := sess.WorkflowDisplayMaxLegs
	switch sess.WorkflowDisplayStatus {
	case pb.WorkflowStatus_WORKFLOW_STATUS_RUNNING:
		return Output{
			Label:   fmt.Sprintf("running %d/%d", leg, maxLegs),
			Intent:  pb.DisplayIntent_DISPLAY_INTENT_INFO,
			Spinner: true,
		}, true
	case pb.WorkflowStatus_WORKFLOW_STATUS_PENDING:
		return Output{
			Label:   "pending",
			Intent:  pb.DisplayIntent_DISPLAY_INTENT_INFO,
			Spinner: true,
		}, true
	case pb.WorkflowStatus_WORKFLOW_STATUS_PAUSED:
		return Output{
			Label:  fmt.Sprintf("paused %d/%d", leg, maxLegs),
			Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING,
		}, true
	case pb.WorkflowStatus_WORKFLOW_STATUS_FAILED:
		return Output{
			Label:  fmt.Sprintf("failed %d/%d", leg, maxLegs),
			Intent: pb.DisplayIntent_DISPLAY_INTENT_DANGER,
		}, true
	case pb.WorkflowStatus_WORKFLOW_STATUS_CANCELLED:
		return Output{
			Label:  "cancelled",
			Intent: pb.DisplayIntent_DISPLAY_INTENT_MUTED,
		}, true
	default:
		return Output{}, false
	}
}

func prOutput(sess *pb.Session) (Output, bool) {
	if sess == nil {
		return Output{}, false
	}
	switch sess.DisplayStatus {
	case pb.DisplayStatus_DISPLAY_STATUS_MERGED:
		return Output{Label: labelMerged, Intent: pb.DisplayIntent_DISPLAY_INTENT_MUTED}, true
	case pb.DisplayStatus_DISPLAY_STATUS_CLOSED:
		return Output{Label: labelClosed, Intent: pb.DisplayIntent_DISPLAY_INTENT_MUTED}, true
	case pb.DisplayStatus_DISPLAY_STATUS_APPROVED:
		return Output{Label: "✓ approved", Intent: pb.DisplayIntent_DISPLAY_INTENT_SUCCESS}, true
	case pb.DisplayStatus_DISPLAY_STATUS_PASSING:
		return Output{Label: "✓ passing", Intent: pb.DisplayIntent_DISPLAY_INTENT_SUCCESS}, true
	case pb.DisplayStatus_DISPLAY_STATUS_REVIEW:
		return Output{Label: "✓ review", Intent: pb.DisplayIntent_DISPLAY_INTENT_SUCCESS}, true
	case pb.DisplayStatus_DISPLAY_STATUS_FAILING:
		return Output{Label: "⨯ failing", Intent: pb.DisplayIntent_DISPLAY_INTENT_DANGER}, true
	case pb.DisplayStatus_DISPLAY_STATUS_CONFLICT:
		return Output{Label: "⨯ conflict", Intent: pb.DisplayIntent_DISPLAY_INTENT_DANGER}, true
	case pb.DisplayStatus_DISPLAY_STATUS_REJECTED:
		return Output{Label: "⨯ rejected", Intent: pb.DisplayIntent_DISPLAY_INTENT_DANGER}, true
	case pb.DisplayStatus_DISPLAY_STATUS_DRAFT:
		return Output{Label: "draft", Intent: pb.DisplayIntent_DISPLAY_INTENT_MUTED}, true
	case pb.DisplayStatus_DISPLAY_STATUS_CHECKING:
		intent := pb.DisplayIntent_DISPLAY_INTENT_WARNING
		if sess.DisplayHasChangesRequested || sess.DisplayHasFailures {
			intent = pb.DisplayIntent_DISPLAY_INTENT_DANGER
		}
		return Output{Label: "checking", Intent: intent, Spinner: true}, true
	default:
		return Output{}, false
	}
}
