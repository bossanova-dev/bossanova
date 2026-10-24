package chatpicker

import (
	"strings"
	"testing"

	"github.com/recurser/boss/api/tui/ui"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestChatPickerBuildTableRows_ShowsDeletingStatusForMatchingChat(t *testing.T) {
	m := Model{
		spinner:                ui.NewStatusSpinner(),
		deletingAgentSessionID: "agent-1",
		chats: []*pb.AgentChat{
			{AgentSessionId: "agent-1", Title: "first", CreatedAt: timestamppb.Now()},
			{AgentSessionId: "agent-2", Title: "second", CreatedAt: timestamppb.Now()},
		},
	}

	m.buildTableRows()

	rows := m.table.Rows()
	if len(rows) != 2 {
		t.Fatalf("table rows = %d, want 2", len(rows))
	}
	if got := rows[0][4]; !strings.Contains(got, "deleting") {
		t.Fatalf("deleting chat STATUS = %q, want deleting", got)
	}
	if got := rows[0][4]; strings.Contains(got, "  deleting") {
		t.Fatalf("deleting chat STATUS = %q, want one space before deleting", got)
	}
	if got := rows[1][4]; strings.Contains(got, "deleting") {
		t.Fatalf("non-deleting chat STATUS = %q, want normal status", got)
	}
}
