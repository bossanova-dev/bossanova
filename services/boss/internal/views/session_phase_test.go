package views

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestHomeSessionPhase(t *testing.T) {
	h := HomeModel{spinner: newStatusSpinner(), daemonPhases: map[string]string{"s": "reviewing"}}
	for _, label := range []string{"working", "? question", "waiting"} {
		sess := &pb.Session{Id: "s", DisplayLabel: label, DisplaySpinner: true, DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_DANGER}
		got := h.renderSessionStatus(sess)
		want := renderDisplayStatus(sess, h.spinner)
		if label == "working" {
			want = styleStatusDanger.Render(h.spinner.View() + "reviewing")
		}
		if got != want {
			t.Errorf("label %q: got %q, want %q", label, got, want)
		}
	}
	h.daemonPhases["s"] = strings.Repeat("界", 20)
	got := ansi.Strip(h.renderSessionStatus(&pb.Session{Id: "s", DisplayLabel: "working", DisplaySpinner: true}))
	if ansi.StringWidth(got) > 16 || !strings.HasSuffix(got, "…") {
		t.Errorf("phase must fit 16 cells with ellipsis: %q", got)
	}
}

func TestChatPickerPhase(t *testing.T) {
	m := NewChatPickerModel(&chatPickerStub{}, context.Background(), "s", "")
	chats := []*pb.ClaudeChat{{AgentSessionId: "working", Title: "Live chat", CreatedAt: timestamppb.Now()}, {AgentSessionId: "idle", Title: "Resting chat", CreatedAt: timestamppb.New(time.Now().Add(-time.Hour))}}
	updated, _ := m.Update(chatsListedMsg{chats: chats, daemonStatuses: map[string]string{"working": statusWorking, "idle": statusIdle}, daemonPhases: map[string]string{"working": "reviewing", "idle": "building"}})
	m = updated.(ChatPickerModel)
	updated, _ = m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	m = updated.(ChatPickerModel)
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
		if col.Title == "STATUS" && col.Width != 22+tableColumnSep {
			t.Errorf("status width = %d, want %d", col.Width, 22+tableColumnSep)
		}
	}
	wide := ansi.Strip(m.table.Rows()[0][len(rows[0])-1])
	if ansi.StringWidth(wide) > 22 || !strings.HasSuffix(wide, "…") {
		t.Errorf("wide picker phase must fit 22 cells: %q", wide)
	}
	// A refresh replaces the sparse map, clearing a previously displayed phase.
	updated, _ = m.Update(chatPickerRefreshMsg{session: &pb.Session{Id: "s"}, daemonStatuses: map[string]string{"working": statusWorking, "idle": statusIdle}})
	m = updated.(ChatPickerModel)
	if got := ansi.Strip(m.table.Rows()[0][len(rows[0])-1]); !strings.Contains(got, "working") {
		t.Errorf("cleared phase: %q", got)
	}
}

// Phase polling uses real additive status entries, rather than model-only seeds.
type phasePollingStub struct{ chatPickerStub }

func (s *phasePollingStub) ListChats(context.Context, string) ([]*pb.ClaudeChat, error) {
	return []*pb.ClaudeChat{{AgentSessionId: "working", CreatedAt: timestamppb.Now()}}, nil
}

func (s *phasePollingStub) GetChatStatuses(context.Context, string) ([]*pb.ChatStatusEntry, error) {
	return []*pb.ChatStatusEntry{{AgentSessionId: "working", Status: pb.ChatStatus_CHAT_STATUS_WORKING, Phase: "reviewing"}}, nil
}
func (s *phasePollingStub) GetSessionStatuses(context.Context, []string) ([]*pb.SessionStatusEntry, error) {
	return []*pb.SessionStatusEntry{{SessionId: "s", Status: pb.ChatStatus_CHAT_STATUS_WORKING, Phase: "reviewing"}}, nil
}
func (s *phasePollingStub) ListSessionsWithReadFailures(context.Context, *pb.ListSessionsRequest, client.SessionReadOptions) ([]*pb.Session, []*pb.OrganizationSessionReadFailure, error) {
	return []*pb.Session{{Id: "s", DisplayLabel: "working", DisplaySpinner: true}}, nil, nil
}
func TestPhasePolling(t *testing.T) {
	c := &phasePollingStub{}
	msg := fetchSessions(c, context.Background(), 0, 0)().(sessionListMsg)
	if msg.daemonPhases["s"] != "reviewing" {
		t.Fatalf("home poll lost phase: %v", msg.daemonPhases)
	}
	h := NewHomeModel(c, context.Background(), nil)
	updated, _ := h.applySessionList(msg)
	h = updated.(HomeModel)
	if got := ansi.Strip(h.renderSessionStatus(h.sessions[0])); got != h.spinner.View()+"reviewing" {
		t.Errorf("home poll phase %q", got)
	}
	m := NewChatPickerModel(c, context.Background(), "s", "")
	listed := m.listChats()().(chatsListedMsg)
	if listed.daemonPhases["working"] != "reviewing" {
		t.Errorf("picker list poll lost phase: %v", listed.daemonPhases)
	}
	refreshed := m.refreshStatuses()().(chatPickerRefreshMsg)
	if refreshed.daemonPhases["working"] != "reviewing" {
		t.Errorf("picker refresh poll lost phase: %v", refreshed.daemonPhases)
	}
}
