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

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// Input bundles the inputs used by Compute.
//
// ChatStatus is passed separately from Session because not all callers will
// derive it from a Session field — some have it from a live daemon heartbeat.
// Workflow status, leg, and max-legs are read from the Session's
// WorkflowDisplay* fields populated by Phase 1.
type Input struct {
	// Session carries DisplayStatus, DisplayIsRepairing, the
	// DisplayHas{Failures,ChangesRequested} flags, and the WorkflowDisplay*
	// fields. May be nil — Compute treats a nil Session like one with all
	// fields zero-valued.
	Session    *pb.Session
	ChatStatus pb.ChatStatus
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

// Compute runs the 7-branch precedence cascade that determines a session's
// display status. The algorithm is intentionally identical to the legacy
// renderPRDisplayStatus — every label, intent, and spinner flag matches.
//
// Precedence (highest first):
//  1. ChatStatus QUESTION → "? question" / WARNING / no spinner
//  2. ChatStatus WORKING  → "working"    / SUCCESS / spinner
//  3. Active workflow     → "running L/M", "pending", "paused L/M",
//     "failed L/M", "cancelled" with matching intents
//  4. DisplayIsRepairing  → "repairing" / WARNING / spinner
//  5. PR DisplayStatus    → "✔ merged", "closed", "✓ approved", "✓ passing",
//     "⨯ failing", "conflict", "⨯ rejected", "draft", "checking"
//  6. ChatStatus IDLE     → "idle" / WARNING
//  7. default             → "stopped" / MUTED
func Compute(in Input) Output {
	if in.ChatStatus == pb.ChatStatus_CHAT_STATUS_QUESTION {
		return Output{Label: "? question", Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING}
	}
	if in.ChatStatus == pb.ChatStatus_CHAT_STATUS_WORKING {
		return Output{Label: "working", Intent: pb.DisplayIntent_DISPLAY_INTENT_SUCCESS, Spinner: true}
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
		return Output{Label: "✔ merged", Intent: pb.DisplayIntent_DISPLAY_INTENT_MUTED}, true
	case pb.DisplayStatus_DISPLAY_STATUS_CLOSED:
		return Output{Label: "closed", Intent: pb.DisplayIntent_DISPLAY_INTENT_MUTED}, true
	case pb.DisplayStatus_DISPLAY_STATUS_APPROVED:
		return Output{Label: "✓ approved", Intent: pb.DisplayIntent_DISPLAY_INTENT_SUCCESS}, true
	case pb.DisplayStatus_DISPLAY_STATUS_PASSING:
		return Output{Label: "✓ passing", Intent: pb.DisplayIntent_DISPLAY_INTENT_SUCCESS}, true
	case pb.DisplayStatus_DISPLAY_STATUS_FAILING:
		return Output{Label: "⨯ failing", Intent: pb.DisplayIntent_DISPLAY_INTENT_DANGER}, true
	case pb.DisplayStatus_DISPLAY_STATUS_CONFLICT:
		return Output{Label: "conflict", Intent: pb.DisplayIntent_DISPLAY_INTENT_DANGER}, true
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
