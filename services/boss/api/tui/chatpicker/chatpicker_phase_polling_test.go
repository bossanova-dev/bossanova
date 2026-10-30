package chatpicker

import (
	"context"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type phasePollingStub struct{ chatPickerStub }

func (s *phasePollingStub) ListChats(context.Context, string) ([]*pb.AgentChat, error) {
	return []*pb.AgentChat{{AgentSessionId: "working", CreatedAt: timestamppb.Now()}}, nil
}
func (s *phasePollingStub) GetChatStatuses(context.Context, string) ([]*pb.ChatStatusEntry, error) {
	return []*pb.ChatStatusEntry{{AgentSessionId: "working", Status: pb.ChatStatus_CHAT_STATUS_WORKING, Phase: "reviewing"}}, nil
}

func TestChatPickerPhasePolling(t *testing.T) {
	c := &phasePollingStub{}
	m := newTestModel(c, context.Background(), "s", "")
	listed := m.listChats()().(chatPickerReply).msg.(ChatsListedMsg)
	if listed.DaemonPhases["working"] != "reviewing" {
		t.Errorf("picker list poll lost phase: %v", listed.DaemonPhases)
	}
	refreshed := m.refreshStatuses()().(chatPickerReply).msg.(RefreshMsg)
	if refreshed.DaemonPhases["working"] != "reviewing" {
		t.Errorf("picker refresh poll lost phase: %v", refreshed.DaemonPhases)
	}
}

func TestChatPickerBlurFreezesSpinnerAndFocusRestarts_ChatPicker_AC17(t *testing.T) {
	m := newTestModel(&phasePollingStub{}, context.Background(), "s", "")
	m.chats = []*pb.AgentChat{{AgentSessionId: "first"}}
	m.buildTableRows()
	next, _ := m.Update(tea.BlurMsg{})
	m = next.(Model)
	m.chats = append(m.chats, &pb.AgentChat{AgentSessionId: "second"})
	next, cmd := m.Update(spinner.TickMsg{})
	m = next.(Model)
	if cmd != nil || len(m.table.Rows()) != 1 {
		t.Fatalf("blurred spinner did work: cmd present=%v rows=%d", cmd != nil, len(m.table.Rows()))
	}
	next, cmd = m.Update(tea.FocusMsg{})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("focus did not restart refresh and spinner")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok || len(batch) != 2 {
		t.Fatalf("focus command = %T, want refresh and spinner batch", cmd())
	}
	if _, ok := batch[0]().(chatPickerReply).msg.(RefreshMsg); !ok {
		t.Fatal("focus did not refresh")
	}
	tick, ok := batch[1]().(spinner.TickMsg)
	if !ok {
		t.Fatal("focus did not restart spinner")
	}
	next, cmd = m.Update(tick)
	m = next.(Model)
	if cmd == nil || len(m.table.Rows()) != 2 {
		t.Fatal("focused spinner did not resume rebuilding")
	}
	_, cmd = m.Update(tea.FocusMsg{})
	if cmd != nil {
		t.Fatal("repeated focus duplicated command chains")
	}
}

func TestChatPickerFocusPollCadence_ChatPicker_AC17(t *testing.T) {
	now := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	m := newTestModel(&phasePollingStub{}, context.Background(), "s", "")
	m.now = func() time.Time { return now }
	// Without focus messages, each two-second tick still refreshes.
	for range 2 {
		now = now.Add(2 * time.Second)
		next, cmd := m.Update(tickMsg{})
		m = next.(Model)
		assertPickerTickRefresh(t, cmd, true)
	}
	next, _ := m.Update(tea.BlurMsg{})
	m = next.(Model)
	last := now
	for _, tc := range []struct {
		elapsed time.Duration
		refresh bool
	}{
		{2 * time.Second, false}, {10*time.Second - time.Nanosecond, false},
		{10 * time.Second, true}, {12 * time.Second, false}, {20 * time.Second, true},
	} {
		now = last.Add(tc.elapsed)
		next, cmd := m.Update(tickMsg{})
		m = next.(Model)
		assertPickerTickRefresh(t, cmd, tc.refresh)
	}
	now = now.Add(time.Second)
	next, _ = m.Update(tea.FocusMsg{})
	m = next.(Model)
	if !m.lastRefreshAt.Equal(now) {
		t.Fatal("focus refresh did not reset the polling interval")
	}
	now = now.Add(2 * time.Second)
	_, cmd := m.Update(tickMsg{})
	assertPickerTickRefresh(t, cmd, true)
}

func assertPickerTickRefresh(t *testing.T, cmd tea.Cmd, want bool) {
	t.Helper()
	if cmd == nil {
		t.Fatal("tick chain stopped")
	}
	msg := cmd()
	batch, refreshed := msg.(tea.BatchMsg)
	if refreshed != want {
		t.Fatalf("tick refresh = %v, want %v (message %T)", refreshed, want, msg)
	}
	if refreshed {
		if len(batch) != 2 {
			t.Fatalf("tick has %d commands, want refresh and timer", len(batch))
		}
		if _, ok := batch[0]().(chatPickerReply).msg.(RefreshMsg); !ok {
			t.Fatal("tick did not issue refresh")
		}
	} else if _, ok := msg.(tickMsg); !ok {
		t.Fatalf("tick did not reschedule: %T", msg)
	}
}

func TestChatPickerSetFocusedStartsBlurred_ChatPicker_AC17(t *testing.T) {
	m := newTestModel(&phasePollingStub{}, context.Background(), "s", "")
	if m.lastRefreshAt.IsZero() {
		t.Fatal("initial fetch time was not seeded")
	}
	now := m.lastRefreshAt.Add(2 * time.Second)
	m.now = func() time.Time { return now }
	m.SetFocused(false)
	_, cmd := m.Update(spinner.TickMsg{})
	if cmd != nil {
		t.Fatal("initially blurred spinner started animating")
	}
	_, cmd = m.Update(tickMsg{})
	assertPickerTickRefresh(t, cmd, false)
	m.SetFocused(true)
	_, cmd = m.Update(tickMsg{})
	assertPickerTickRefresh(t, cmd, true)
}
