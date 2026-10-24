package chatpicker

import (
	"context"
	"strings"
	"testing"

	"github.com/recurser/boss/api/tui/ui"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/bossalib/displaystatus"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestChatPicker_ArmedLineCarriesReasonVerbatim(t *testing.T) {
	m := Model{
		chats: []*pb.AgentChat{
			{AgentSessionId: "a", AgentName: "claude"},
			{AgentSessionId: "b", AgentName: "claude"},
		},
		daemonStatuses:       map[string]string{"a": ui.StatusWorking, "b": ui.StatusWaiting},
		daemonWaitingReasons: map[string]string{"b": testWaitingReason},
	}
	got := firstArmedLine(m)
	want := "New chat · " + testWaitingReason
	if got != want {
		t.Fatalf("armedLine = %q, want %q", got, want)
	}
}

func TestChatPicker_ArmedLine_EmptyWhenNoneWaiting(t *testing.T) {
	m := Model{
		chats:          []*pb.AgentChat{{AgentSessionId: "a", AgentName: "claude"}},
		daemonStatuses: map[string]string{"a": ui.StatusWorking},
	}
	if got := firstArmedLine(m); got != "" {
		t.Fatalf("armedLine = %q, want empty", got)
	}
}

func TestChatPicker_WaitingRendersBadgeNotReason(t *testing.T) {
	stub := &chatPickerStub{}
	m := newTestModel(stub, context.Background(), "session-1", "")
	updated, _ := m.Update(ChatsListedMsg{
		Chats: []*pb.AgentChat{
			{SessionId: "session-1", AgentSessionId: "agent-working", Title: "implementing", AgentName: "claude", CreatedAt: timestamppb.Now()},
			{SessionId: "session-1", AgentSessionId: "agent-waiting", Title: "awaiting CI", AgentName: "claude", CreatedAt: timestamppb.Now()},
		},
		DaemonStatuses: map[string]string{
			"agent-working": ui.StatusWorking,
			"agent-waiting": ui.StatusWaiting,
		},
		DaemonWaitingReasons: map[string]string{"agent-waiting": testWaitingReason},
	})
	m = updated.(Model)
	updated, _ = m.Update(tea.WindowSizeMsg{Width: 140, Height: 34})
	m = updated.(Model)
	m.session = &pb.Session{Id: "session-1"}

	rendered := stripANSI(m.View().Content)
	for _, token := range []string{displaystatus.WaitingLabel, "working"} {
		if !strings.Contains(rendered, token) {
			t.Fatalf("session detail missing %q in:\n%s", token, rendered)
		}
	}
	if strings.Contains(rendered, testWaitingReason) {
		t.Fatalf("session detail repeats the waiting reason:\n%s", rendered)
	}
}
