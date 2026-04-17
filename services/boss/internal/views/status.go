package views

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/spinner"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// Status constants (previously from bosspty package).
const (
	statusWorking  = "working"
	statusIdle     = "idle"
	statusQuestion = "question"
	statusStopped  = "stopped"
)

// newStatusSpinner creates an unstyled spinner for status display.
// Color is applied by renderPRDisplayStatus so the entire cell has a single ANSI wrap.
func newStatusSpinner() spinner.Model {
	return spinner.New(spinner.WithSpinner(spinner.Dot))
}

// chatStatusString converts a protobuf ChatStatus enum to a bosspty.Status* string.
func chatStatusString(s pb.ChatStatus) string {
	switch s {
	case pb.ChatStatus_CHAT_STATUS_WORKING:
		return statusWorking
	case pb.ChatStatus_CHAT_STATUS_IDLE:
		return statusIdle
	case pb.ChatStatus_CHAT_STATUS_QUESTION:
		return statusQuestion
	default:
		return statusStopped
	}
}

// styledPRStatus returns a styled label for a PR display status.
// Returns "" for unspecified/unknown statuses.
func styledPRStatus(sess *pb.Session, sp spinner.Model) string {
	switch sess.PrDisplayStatus {
	case pb.PRDisplayStatus_PR_DISPLAY_STATUS_MERGED:
		return styleStatusMuted.Render("✔ merged")
	case pb.PRDisplayStatus_PR_DISPLAY_STATUS_CLOSED:
		return styleStatusMuted.Render("closed")
	case pb.PRDisplayStatus_PR_DISPLAY_STATUS_APPROVED:
		return styleStatusSuccess.Render("✓ approved")
	case pb.PRDisplayStatus_PR_DISPLAY_STATUS_PASSING:
		return styleStatusSuccess.Render("✓ passing")
	case pb.PRDisplayStatus_PR_DISPLAY_STATUS_FAILING:
		return styleStatusDanger.Render("⨯ failing")
	case pb.PRDisplayStatus_PR_DISPLAY_STATUS_CONFLICT:
		return styleStatusDanger.Render("conflict")
	case pb.PRDisplayStatus_PR_DISPLAY_STATUS_REJECTED:
		return styleStatusDanger.Render("⨯ rejected")
	case pb.PRDisplayStatus_PR_DISPLAY_STATUS_DRAFT:
		return styleStatusMuted.Render("draft")
	case pb.PRDisplayStatus_PR_DISPLAY_STATUS_CHECKING:
		s := styleStatusWarning
		if sess.PrDisplayHasChangesRequested || sess.PrDisplayHasFailures {
			s = styleStatusDanger
		}
		return s.Render(sp.View() + "checking")
	default:
		return ""
	}
}

// styledWorkflowStatus returns a styled label for an active autopilot workflow.
// Returns "" when no active workflow is present.
func styledWorkflowStatus(sess *pb.Session, sp spinner.Model) string {
	switch sess.WorkflowDisplayStatus {
	case pb.WorkflowStatus_WORKFLOW_STATUS_RUNNING:
		return styleStatusInfo.Render(fmt.Sprintf("%srunning %d/%d", sp.View(), sess.WorkflowDisplayLeg, sess.WorkflowDisplayMaxLegs))
	case pb.WorkflowStatus_WORKFLOW_STATUS_PENDING:
		return styleStatusInfo.Render(sp.View() + "pending")
	case pb.WorkflowStatus_WORKFLOW_STATUS_PAUSED:
		return styleStatusWarning.Render(fmt.Sprintf("paused %d/%d", sess.WorkflowDisplayLeg, sess.WorkflowDisplayMaxLegs))
	case pb.WorkflowStatus_WORKFLOW_STATUS_FAILED:
		return styleStatusDanger.Render(fmt.Sprintf("failed %d/%d", sess.WorkflowDisplayLeg, sess.WorkflowDisplayMaxLegs))
	case pb.WorkflowStatus_WORKFLOW_STATUS_CANCELLED:
		return styleStatusMuted.Render("cancelled")
	default:
		return ""
	}
}

// renderPRDisplayStatus returns a styled status string for the unified STATUS column.
// Claude working status overrides all PR display statuses.
func renderPRDisplayStatus(sess *pb.Session, claudeStatus string, sp spinner.Model) string {
	if claudeStatus == statusQuestion {
		return styleStatusWarning.Render("? question")
	}
	if claudeStatus == statusWorking {
		return styleStatusSuccess.Render(sp.View() + "working")
	}
	if label := styledWorkflowStatus(sess, sp); label != "" {
		return label
	}
	if sess.IsRepairing {
		return styleStatusWarning.Render(sp.View() + "repairing")
	}
	if label := styledPRStatus(sess, sp); label != "" {
		return label
	}
	if claudeStatus == statusIdle {
		return styleStatusWarning.Render("idle")
	}
	return styleStatusMuted.Render("stopped")
}

// renderSessionPRStatus returns a styled PR status label for display next to
// a session title (e.g. "checking", "failing"). Returns "" when there is no
// meaningful PR status to show (idle / unspecified).
func renderSessionPRStatus(sess *pb.Session, sp spinner.Model) string {
	return styledPRStatus(sess, sp)
}

// renderClaudeStatus returns a styled status string for a Claude process
// (working/idle/stopped) without PR display context.
func renderClaudeStatus(status string, sp spinner.Model) string {
	switch status {
	case statusQuestion:
		return styleStatusWarning.Render("? question")
	case statusWorking:
		return styleStatusSuccess.Render(sp.View() + "working")
	case statusIdle:
		return styleStatusWarning.Render("idle")
	default:
		return styleStatusMuted.Render("stopped")
	}
}

// renderPRLink returns an underlined, OSC 8 hyperlinked PR label (e.g. "#12")
// that opens the PR URL on cmd+click. Returns plain label if no URL is available.
// Uses raw ANSI underline escapes (not lipgloss) so the table's row-level
// foreground color is inherited rather than overridden.
func renderPRLink(sess *pb.Session) string {
	if sess == nil || sess.PrNumber == nil {
		return ""
	}
	label := fmt.Sprintf("#%d", *sess.PrNumber)
	underlined := "\x1b[4m" + label + "\x1b[24m"
	if sess.PrUrl != nil && *sess.PrUrl != "" {
		return fmt.Sprintf("\x1b]8;;%s\x1b\\%s\x1b]8;;\x1b\\", *sess.PrUrl, underlined)
	}
	return underlined
}

// renderMutedPRLink returns a muted, strikethrough, underlined, OSC 8
// hyperlinked PR label for merged/closed rows. Uses raw ANSI escapes (not
// lipgloss) to avoid SGR resets that break the OSC 8 hyperlink context.
func renderMutedPRLink(sess *pb.Session) string {
	if sess == nil || sess.PrNumber == nil {
		return ""
	}
	label := fmt.Sprintf("#%d", *sess.PrNumber)
	// SGR 38;2;98;98;98 = muted gray foreground (#626262)
	// SGR 9 = strikethrough, SGR 4 = underline
	styled := "\x1b[38;2;98;98;98;9;4m" + label + "\x1b[39;29;24m"
	if sess.PrUrl != nil && *sess.PrUrl != "" {
		return fmt.Sprintf("\x1b]8;;%s\x1b\\%s\x1b]8;;\x1b\\", *sess.PrUrl, styled)
	}
	return styled
}

// renderTrackerLink replaces the [tracker_id] portion of a session title with
// an OSC 8 hyperlinked + underlined version. Returns the original title if
// the session has no tracker ID or the ID is not found in the title.
func renderTrackerLink(sess *pb.Session, title string) string {
	if sess == nil || sess.TrackerId == nil || *sess.TrackerId == "" {
		return title
	}
	target := "[" + *sess.TrackerId + "]"
	idx := strings.Index(title, target)
	if idx < 0 {
		return title
	}
	underlined := "\x1b[4m" + target + "\x1b[24m"
	var linked string
	if sess.TrackerUrl != nil && *sess.TrackerUrl != "" {
		linked = fmt.Sprintf("\x1b]8;;%s\x1b\\%s\x1b]8;;\x1b\\", *sess.TrackerUrl, underlined)
	} else {
		linked = underlined
	}
	return title[:idx] + linked + title[idx+len(target):]
}

// renderMutedTrackerLink replaces the [tracker_id] portion of a session title
// with a muted, strikethrough, underlined, OSC 8 hyperlinked version for
// merged/closed rows.
func renderMutedTrackerLink(sess *pb.Session, title string) string {
	if sess == nil || sess.TrackerId == nil || *sess.TrackerId == "" {
		return title
	}
	target := "[" + *sess.TrackerId + "]"
	idx := strings.Index(title, target)
	if idx < 0 {
		return title
	}
	// SGR 38;2;98;98;98 = muted gray foreground (#626262)
	// SGR 9 = strikethrough, SGR 4 = underline
	styled := "\x1b[38;2;98;98;98;9;4m" + target + "\x1b[39;29;24m"
	var linked string
	if sess.TrackerUrl != nil && *sess.TrackerUrl != "" {
		linked = fmt.Sprintf("\x1b]8;;%s\x1b\\%s\x1b]8;;\x1b\\", *sess.TrackerUrl, styled)
	} else {
		linked = styled
	}
	return title[:idx] + linked + title[idx+len(target):]
}
