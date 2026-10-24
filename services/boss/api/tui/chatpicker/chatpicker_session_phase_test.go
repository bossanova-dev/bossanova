package chatpicker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/api/tui/ui"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestChatPickerPhase(t *testing.T) {
	m := newTestModel(&chatPickerStub{}, context.Background(), "s", "")
	chats := []*pb.AgentChat{{AgentSessionId: "working", Title: "Live chat", CreatedAt: timestamppb.Now()}, {AgentSessionId: "idle", Title: "Resting chat", CreatedAt: timestamppb.New(time.Now().Add(-time.Hour))}}
	updated, _ := m.Update(ChatsListedMsg{Chats: chats, DaemonStatuses: map[string]string{"working": ui.StatusWorking, "idle": ui.StatusIdle}, DaemonPhases: map[string]string{"working": "reviewing", "idle": "building"}})
	m = updated.(Model)
	updated, _ = m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	m = updated.(Model)
	rows := m.table.Rows()
	if got := ansi.Strip(rows[0][len(rows[0])-1]); got != m.spinner.View()+"reviewing" {
		t.Errorf("working row: %q", got)
	}
	if got := ansi.Strip(rows[1][len(rows[1])-1]); got != "idle" {
		t.Errorf("idle row: %q", got)
	}
	// Long phases grow the status column to its cap in display cells.
	m.daemonPhases["working"] = strings.Repeat("界", 20)
	m.buildTableRows()
	for _, col := range m.table.Columns() {
		if col.Title == "STATUS" && col.Width != 22+ui.TableColumnSep {
			t.Errorf("status width = %d, want %d", col.Width, 22+ui.TableColumnSep)
		}
	}
	wide := ansi.Strip(m.table.Rows()[0][len(rows[0])-1])
	if ansi.StringWidth(wide) > 22 || !strings.HasSuffix(wide, "…") {
		t.Errorf("wide picker phase must fit 22 cells: %q", wide)
	}
	// A refresh replaces the sparse map, clearing a previously displayed phase.
	updated, _ = m.Update(RefreshMsg{Session: &pb.Session{Id: "s"}, DaemonStatuses: map[string]string{"working": ui.StatusWorking, "idle": ui.StatusIdle}})
	m = updated.(Model)
	if got := ansi.Strip(m.table.Rows()[0][len(rows[0])-1]); !strings.Contains(got, "working") {
		t.Errorf("cleared phase: %q", got)
	}
}
