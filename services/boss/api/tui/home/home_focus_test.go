package home

import (
	"context"
	"testing"

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
	if cmd != nil {
		t.Error("should not navigate when the session no longer needs attention")
	}
}

func TestHomeFocusWithoutPendingDoesNothing(t *testing.T) {
	h := Model{}
	updated, cmd := h.Update(tea.FocusMsg{})
	if !updated.(Model).Focused {
		t.Error("focused should be true after FocusMsg")
	}
	if cmd != nil {
		t.Error("no pending session means no navigation")
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
func TestHomeRecordsPendingOnlyWhenBackgrounded(t *testing.T) {
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
