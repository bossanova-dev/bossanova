package home

import (
	"context"
	"reflect"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"

	"github.com/recurser/boss/api/tui/ui"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/bossalib/displaystatus"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// Focus after a background notification opens the waiting session's chat.
func TestHomeFocusOpensPendingQuestionChat(t *testing.T) {
	q := &pb.Session{Id: "s1", DisplayLabel: displaystatus.QuestionLabel}
	h := Model{
		Sessions:                  []*pb.Session{q},
		Focused:                   false,
		PendingAttentionSessionID: "s1",
	}

	updated, cmd := h.Update(tea.FocusMsg{})
	hm := updated.(Model)

	if !hm.Focused {
		t.Error("focused should be true after FocusMsg")
	}
	if hm.PendingAttentionSessionID != "" {
		t.Error("pending marker should be cleared after handling focus")
	}
	if cmd == nil {
		t.Fatal("expected a navigation command")
	}
	sv, ok := cmd().(ui.SwitchViewMsg)
	if !ok {
		t.Fatalf("expected switchViewMsg, got %T", cmd())
	}
	if sv.View != ui.ViewChatPicker || sv.SessionID != "s1" {
		t.Errorf("got view=%v sessionID=%q, want ViewChatPicker/s1", sv.View, sv.SessionID)
	}
}

// If the question resolved before the user clicked, focus clears the marker but
// does not hijack the view.
func TestHomeFocusSkipsResolvedQuestion(t *testing.T) {
	resolved := &pb.Session{Id: "s1", DisplayLabel: "working"}
	h := Model{Sessions: []*pb.Session{resolved}, PendingAttentionSessionID: "s1"}

	updated, cmd := h.Update(tea.FocusMsg{})
	if updated.(Model).PendingAttentionSessionID != "" {
		t.Error("pending marker should be cleared even when not navigating")
	}
	if cmd == nil || updated.(Model).NextSessionPollID != 1 {
		t.Fatal("resolved attention should refresh sessions on focus")
	}
	if _, ok := cmd().(tea.BatchMsg); !ok {
		t.Fatal("resolved attention should poll without navigating")
	}
}

func TestHomeFocusWithoutPendingRefreshes(t *testing.T) {
	h := Model{}
	updated, cmd := h.Update(tea.FocusMsg{})
	if !updated.(Model).Focused {
		t.Error("focused should be true after FocusMsg")
	}
	if cmd == nil || updated.(Model).NextSessionPollID != 1 {
		t.Fatal("focus without pending attention should refresh sessions")
	}
	if _, ok := cmd().(tea.BatchMsg); !ok {
		t.Fatal("focus without pending attention should poll without navigating")
	}
}

func TestHomeBlurClearsFocus(t *testing.T) {
	h := Model{Focused: true}
	updated, _ := h.Update(tea.BlurMsg{})
	if updated.(Model).Focused {
		t.Error("focused should be false after BlurMsg")
	}
}

// A new question recorded while boss is backgrounded becomes the pending target;
// while focused, nothing is recorded (the user is already looking).
func TestHomeRecordsPendingOnlyWhenBackgrounded_Sessions_AC131(t *testing.T) {
	q := &pb.Session{Id: "s1", DisplayLabel: displaystatus.QuestionLabel}

	blurred := NewModel(nil, context.Background(), nil)
	blurred.Focused = false
	seeded, _ := blurred.Update(SessionListMsg{})
	updated, _ := seeded.(Model).Update(SessionListMsg{Sessions: []*pb.Session{q}})
	if got := updated.(Model).PendingAttentionSessionID; got != "s1" {
		t.Errorf("blurred: pending = %q, want s1", got)
	}

	focused := NewModel(nil, context.Background(), nil) // focused defaults true
	seeded, _ = focused.Update(SessionListMsg{})
	updated, _ = seeded.(Model).Update(SessionListMsg{Sessions: []*pb.Session{q}})
	if got := updated.(Model).PendingAttentionSessionID; got != "" {
		t.Errorf("focused: pending = %q, want empty", got)
	}
}

func TestHomeBlurredPollCadence_Sessions_AC130_AC131(t *testing.T) {
	h := NewModel(nil, context.Background(), nil)
	now := h.StartedAt
	h.Now = func() time.Time { return now }
	next, _ := h.Update(tea.BlurMsg{})
	h = next.(Model)
	for _, step := range []struct {
		elapsed time.Duration
		pollID  uint64
	}{
		{2 * time.Second, 1}, {8 * time.Second, 1}, {10 * time.Second, 2},
		{12 * time.Second, 2}, {20 * time.Second, 3},
	} {
		now = h.StartedAt.Add(step.elapsed)
		next, cmd := h.Update(TickMsg{})
		h = next.(Model)
		if h.NextSessionPollID != step.pollID {
			t.Errorf("at %s: poll ID = %d, want %d", step.elapsed, h.NextSessionPollID, step.pollID)
		}
		if cmd == nil {
			t.Errorf("at %s: tick chain stopped", step.elapsed)
		}
	}
}

func TestHomeFocusRestoresPollingAndSpinner_Sessions_AC130(t *testing.T) {
	h := NewModel(nil, context.Background(), nil)
	now := h.StartedAt
	h.Now = func() time.Time { return now }
	next, _ := h.Update(tea.BlurMsg{})
	h = next.(Model)
	now = now.Add(time.Second)
	next, cmd := h.Update(tea.FocusMsg{})
	h = next.(Model)
	if h.NextSessionPollID != 2 {
		t.Fatalf("focus poll ID = %d, want 2", h.NextSessionPollID)
	}
	if cmd == nil {
		t.Fatal("focus should return poll and spinner commands")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok || len(batch) < 2 {
		t.Fatalf("focus command = %T, want batch with spinner restart", cmd())
	}
	// The final command restarts animation; leave the nil-client poll unexecuted.
	if msg := batch[len(batch)-1](); msg == nil {
		t.Fatal("spinner restart returned nil")
	} else if _, ok := msg.(spinner.TickMsg); !ok {
		t.Fatalf("restart returned %T, want spinner.TickMsg", msg)
	}
	next, cmd = h.Update(tea.FocusMsg{})
	h = next.(Model)
	if cmd != nil || h.NextSessionPollID != 2 {
		t.Fatal("repeated focus should be a no-op")
	}
	now = now.Add(2 * time.Second)
	next, cmd = h.Update(TickMsg{})
	if next.(Model).NextSessionPollID != 3 || cmd == nil {
		t.Fatal("focused tick should resume normal polling")
	}
}

func TestHomeNoFocusEventsKeepsPolling_Sessions_AC130(t *testing.T) {
	h := NewModel(nil, context.Background(), nil)
	now := h.StartedAt
	h.Now = func() time.Time { return now }
	for range 3 {
		now = now.Add(2 * time.Second)
		previous := h.NextSessionPollID
		next, cmd := h.Update(TickMsg{})
		h = next.(Model)
		if !h.Focused || h.NextSessionPollID != previous+1 || cmd == nil {
			t.Fatal("terminal without focus events should poll every tick")
		}
	}
}

func TestHomeBlurredSpinnerFreezesRows_Sessions_AC130(t *testing.T) {
	h := NewModel(nil, context.Background(), nil)
	h.Loading = false
	h.Width, h.Height = 160, 60
	h.Spinner.Spinner = spinner.Spinner{Frames: []string{"A ", "B "}}
	h.Sessions = []*pb.Session{{Id: "s1", DisplayLabel: ui.StatusWorking, DisplaySpinner: true}}
	h.BuildTableRows()
	rows := h.Table.Rows()
	frame := h.Spinner.View()
	next, _ := h.Update(tea.BlurMsg{})
	h = next.(Model)
	next, cmd := h.Update(h.Spinner.Tick())
	h = next.(Model)
	if cmd != nil || !reflect.DeepEqual(h.Table.Rows(), rows) || h.Spinner.View() != frame {
		t.Fatal("blurred spinner should leave frames and rows frozen without scheduling a tick")
	}
	next, _ = h.Update(tea.FocusMsg{})
	h = next.(Model)
	next, cmd = h.Update(h.Spinner.Tick())
	h = next.(Model)
	if reflect.DeepEqual(h.Table.Rows(), rows) || h.Spinner.View() == frame || cmd == nil {
		t.Fatal("focused spinner should animate rows and schedule its next tick")
	}
}
