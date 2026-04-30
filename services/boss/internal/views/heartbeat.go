package views

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/boss/internal/client"
	bosspty "github.com/recurser/boss/internal/pty"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// heartbeatInterval is how often the boss TUI snapshots its in-process
// PTY chats and reports them to bossd via ReportChatStatus. The daemon's
// status.Tracker treats entries older than 5×this value as stale, so 3s
// matches the legacy daemon-side tmux poller (PollInterval = 3s).
const heartbeatInterval = 3 * time.Second

// heartbeatTickMsg fires every heartbeatInterval to drive the chat-status
// reporting loop. Handled at the App.Update level so it survives view
// switches and the bug-report modal.
type heartbeatTickMsg struct{}

func heartbeatTickCmd() tea.Cmd {
	return tea.Tick(heartbeatInterval, func(time.Time) tea.Msg {
		return heartbeatTickMsg{}
	})
}

// ptyStatusToChatStatus converts a boss-side pty.Manager status string into
// the protobuf ChatStatus enum the daemon's tracker expects.
func ptyStatusToChatStatus(s string) pb.ChatStatus {
	switch s {
	case bosspty.StatusWorking:
		return pb.ChatStatus_CHAT_STATUS_WORKING
	case bosspty.StatusIdle:
		return pb.ChatStatus_CHAT_STATUS_IDLE
	case bosspty.StatusQuestion:
		return pb.ChatStatus_CHAT_STATUS_QUESTION
	case bosspty.StatusStopped:
		return pb.ChatStatus_CHAT_STATUS_STOPPED
	default:
		return pb.ChatStatus_CHAT_STATUS_UNSPECIFIED
	}
}

// sendHeartbeatsCmd snapshots the boss-side PTY manager and pushes the
// per-chat statuses to bossd. Fire-and-forget — a transient RPC failure
// just delays cross-client visibility until the next tick.
func sendHeartbeatsCmd(ctx context.Context, c client.BossClient, manager *bosspty.Manager) tea.Cmd {
	return func() tea.Msg {
		if manager == nil || c == nil {
			return nil
		}
		statuses := manager.AllStatuses()
		if len(statuses) == 0 {
			return nil
		}
		reports := make([]*pb.ChatStatusReport, 0, len(statuses))
		for claudeID, info := range statuses {
			reports = append(reports, &pb.ChatStatusReport{
				ClaudeId:     claudeID,
				Status:       ptyStatusToChatStatus(info.Status),
				LastOutputAt: timestamppb.New(info.LastWrite),
			})
		}
		_ = c.ReportChatStatus(ctx, reports)
		return nil
	}
}
