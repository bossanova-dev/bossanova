package views

import (
	"fmt"

	"charm.land/bubbles/v2/spinner"
	bosspty "github.com/recurser/boss/internal/pty"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
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
		return bosspty.StatusWorking
	case pb.ChatStatus_CHAT_STATUS_IDLE:
		return bosspty.StatusIdle
	case pb.ChatStatus_CHAT_STATUS_QUESTION:
		return bosspty.StatusQuestion
	default:
		return bosspty.StatusStopped
	}
}

// mergeStatus prefers the local (real-time PTY) status over the daemon
// (heartbeat-based) status, falling back to daemon when the local process
// is not alive.
func mergeStatus(local, daemon string) string {
	if local != bosspty.StatusStopped {
		return local
	}
	return daemon
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

// renderPRDisplayStatus returns a styled status string for the unified STATUS column.
// Claude working status overrides all PR display statuses.
func renderPRDisplayStatus(sess *pb.Session, claudeStatus string, sp spinner.Model) string {
	if claudeStatus == bosspty.StatusQuestion {
		return styleStatusWarning.Render("? question")
	}
	if claudeStatus == bosspty.StatusWorking {
		return styleStatusSuccess.Render(sp.View() + "working")
	}
	if sess.IsRepairing {
		return styleStatusWarning.Render(sp.View() + "repairing")
	}
	if label := styledPRStatus(sess, sp); label != "" {
		return label
	}
	if claudeStatus == bosspty.StatusIdle {
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
	case bosspty.StatusQuestion:
		return styleStatusWarning.Render("? question")
	case bosspty.StatusWorking:
		return styleStatusSuccess.Render(sp.View() + "working")
	case bosspty.StatusIdle:
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
