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
	updated, _ := m.Update(chatsListedMsg{chats: []*pb.ClaudeChat{
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
