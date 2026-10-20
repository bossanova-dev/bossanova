package views

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestChatPicker_ReplacedChatReadsAsReplacedNotFailed(t *testing.T) {
	m := NewChatPickerModel(&chatPickerStub{}, context.Background(), "session-1", "")
	updated, _ := m.Update(chatsListedMsg{chats: []*pb.AgentChat{
		{
			SessionId:      "session-1",
			AgentSessionId: "agent-replaced",
			Title:          "Repair pull request",
			CreatedAt:      timestamppb.Now(),
			ReclaimedAt:    timestamppb.Now(),
			ReclaimReason:  "auto-repair replacing idle chat after 5m4s of silence; threshold 5m0s",
		},
		{
			SessionId:      "session-1",
			AgentSessionId: "agent-failed",
			Title:          "Never came up",
			CreatedAt:      timestamppb.Now(),
			StartError:     "tmux launch failed: missing or unsuitable terminal",
		},
	}})
	m = updated.(ChatPickerModel)
	updated, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = updated.(ChatPickerModel)
	m.session = &pb.Session{Id: "session-1"}

	var replacedRow, failedRow string
	for _, line := range strings.Split(stripANSI(m.View().Content), "\n") {
		switch {
		case strings.Contains(line, "Repair pull request"):
			replacedRow = line
		case strings.Contains(line, "Never came up"):
			failedRow = line
		}
	}
	if !strings.Contains(replacedRow, "replaced") || strings.Contains(replacedRow, "failed") {
		t.Errorf("replaced chat row = %q, want a replaced badge and no failed badge", replacedRow)
	}
	if !strings.Contains(failedRow, "× failed") {
		t.Errorf("failed-start row = %q, want × failed kept for a chat that never came up", failedRow)
	}
}

// A chat working now has no failed start or replaced mark to report: the stale
// mark on its row loses to the live state, with the spinner. The chats that
// are not working keep their badges.
func TestChatPicker_WorkingChatShowsWorkingOverAStaleFailedOrReplacedMark(t *testing.T) {
	m := NewChatPickerModel(&chatPickerStub{}, context.Background(), "session-1", "")
	updated, _ := m.Update(chatsListedMsg{
		chats: []*pb.AgentChat{
			{SessionId: "session-1", AgentSessionId: "agent-resumed", Title: "Resumed after restart", CreatedAt: timestamppb.Now(), StartError: "daemon restarted mid-run"},
			{SessionId: "session-1", AgentSessionId: "agent-revived", Title: "Revived by a wake", CreatedAt: timestamppb.Now(), ReclaimedAt: timestamppb.Now()},
			{SessionId: "session-1", AgentSessionId: "agent-stopped", Title: "Still failed", CreatedAt: timestamppb.Now(), StartError: "daemon restarted mid-run"},
		},
		daemonStatuses: map[string]string{"agent-resumed": statusWorking, "agent-revived": statusWorking, "agent-stopped": statusStopped},
	})
	m = updated.(ChatPickerModel)
	updated, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = updated.(ChatPickerModel)
	m.session = &pb.Session{Id: "session-1"}

	frame := stripANSI(m.spinner.View())
	rows := map[string]string{}
	for _, line := range strings.Split(stripANSI(m.View().Content), "\n") {
		for _, title := range []string{"Resumed after restart", "Revived by a wake", "Still failed"} {
			if strings.Contains(line, title) {
				rows[title] = line
			}
		}
	}
	for _, title := range []string{"Resumed after restart", "Revived by a wake"} {
		row := rows[title]
		if !strings.Contains(row, frame+"working") || strings.Contains(row, "failed") || strings.Contains(row, "replaced") {
			t.Errorf("%s row = %q, want the spinner and working with no stale mark", title, row)
		}
	}
	if !strings.Contains(rows["Still failed"], "× failed") {
		t.Errorf("stopped chat row = %q, want × failed kept", rows["Still failed"])
	}
}
