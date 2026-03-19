package views

import (
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

// renderStatus returns a styled status string.
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
