package views

import (
	"image/color"

	"charm.land/bubbles/v2/spinner"
	"charm.land/lipgloss/v2"
	bosspty "github.com/recurser/boss/internal/pty"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// newStatusSpinner creates an unstyled spinner for status display.
// Color is applied by renderStatus so the entire cell has a single ANSI wrap.
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

// renderStatus returns a styled status string for chat-level display.
// The spinner + label are wrapped in a single Render call to avoid
// intermediate ANSI resets that interfere with the table's Selected style.
func renderStatus(status string, sp spinner.Model) string {
	switch status {
	case bosspty.StatusWorking:
		return lipgloss.NewStyle().Foreground(colorSuccess).Render(sp.View() + "working")
	case bosspty.StatusIdle:
		return lipgloss.NewStyle().Foreground(colorWarning).Render("idle")
	default: // StatusStopped
		return lipgloss.NewStyle().Foreground(colorMuted).Render("stopped")
	}
}

// renderSessionStatus returns a styled status for the session list.
// When the PTY is active it shows working/idle; when stopped it falls back
// to the session's state-machine state which is more informative.
func renderSessionStatus(ptyStatus string, sessState pb.SessionState, sp spinner.Model) string {
	switch ptyStatus {
	case bosspty.StatusWorking:
		return lipgloss.NewStyle().Foreground(colorSuccess).Render(sp.View() + "working")
	case bosspty.StatusIdle:
		return lipgloss.NewStyle().Foreground(colorWarning).Render("idle")
	}

	// PTY stopped — show session state instead.
	label := StateLabel(sessState)
	color := stateColor(sessState)
	return lipgloss.NewStyle().Foreground(color).Render(label)
}

// stateColor maps a session state to its display color.
func stateColor(state pb.SessionState) color.Color {
	switch state {
	case pb.SessionState_SESSION_STATE_AWAITING_CHECKS:
		return colorWarning
	case pb.SessionState_SESSION_STATE_GREEN_DRAFT,
		pb.SessionState_SESSION_STATE_READY_FOR_REVIEW,
		pb.SessionState_SESSION_STATE_MERGED:
		return colorSuccess
	case pb.SessionState_SESSION_STATE_BLOCKED:
		return colorDanger
	case pb.SessionState_SESSION_STATE_CREATING_WORKTREE,
		pb.SessionState_SESSION_STATE_STARTING_CLAUDE,
		pb.SessionState_SESSION_STATE_PUSHING_BRANCH,
		pb.SessionState_SESSION_STATE_OPENING_DRAFT_PR,
		pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
		pb.SessionState_SESSION_STATE_FIXING_CHECKS:
		return colorInfo
	default:
		return colorMuted
	}
}
