package views

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// Focus after a background notification opens the waiting session's chat.
func TestHomeFocusOpensPendingQuestionChat(t *testing.T) {
	q := &pb.Session{Id: "s1", DisplayLabel: displaystatus.QuestionLabel}
	h := HomeModel{
		sessions:                  []*pb.Session{q},
		focused:                   false,
		pendingAttentionSessionID: "s1",
	}

	updated, cmd := h.Update(tea.FocusMsg{})
	hm := updated.(HomeModel)

	if !hm.focused {
		t.Error("focused should be true after FocusMsg")
	}
	if hm.pendingAttentionSessionID != "" {
		t.Error("pending marker should be cleared after handling focus")
	}
	if cmd == nil {
		t.Fatal("expected a navigation command")
	}
	sv, ok := cmd().(switchViewMsg)
	if !ok {
		t.Fatalf("expected switchViewMsg, got %T", cmd())
	}
	if sv.view != ViewChatPicker || sv.sessionID != "s1" {
		t.Errorf("got view=%v sessionID=%q, want ViewChatPicker/s1", sv.view, sv.sessionID)
	}
}

// If the question resolved before the user clicked, focus clears the marker but
// does not hijack the view.
func TestHomeFocusSkipsResolvedQuestion(t *testing.T) {
	resolved := &pb.Session{Id: "s1", DisplayLabel: "working"}
	h := HomeModel{sessions: []*pb.Session{resolved}, pendingAttentionSessionID: "s1"}

	updated, cmd := h.Update(tea.FocusMsg{})
	if updated.(HomeModel).pendingAttentionSessionID != "" {
		t.Error("pending marker should be cleared even when not navigating")
	}
	if cmd != nil {
		t.Error("should not navigate when the session no longer needs attention")
	}
}

func TestHomeFocusWithoutPendingDoesNothing(t *testing.T) {
	h := HomeModel{}
	updated, cmd := h.Update(tea.FocusMsg{})
	if !updated.(HomeModel).focused {
		t.Error("focused should be true after FocusMsg")
	}
	if cmd != nil {
		t.Error("no pending session means no navigation")
	}
}

func TestHomeBlurClearsFocus(t *testing.T) {
	h := HomeModel{focused: true}
	updated, _ := h.Update(tea.BlurMsg{})
	if updated.(HomeModel).focused {
		t.Error("focused should be false after BlurMsg")
	}
}

// A new question recorded while boss is backgrounded becomes the pending target;
// while focused, nothing is recorded (the user is already looking).
func TestHomeRecordsPendingOnlyWhenBackgrounded(t *testing.T) {
	q := &pb.Session{Id: "s1", DisplayLabel: displaystatus.QuestionLabel}

	blurred := NewHomeModel(nil, context.Background(), nil)
	blurred.focused = false
	updated, _ := blurred.Update(sessionListMsg{sessions: []*pb.Session{q}})
	if got := updated.(HomeModel).pendingAttentionSessionID; got != "s1" {
		t.Errorf("blurred: pending = %q, want s1", got)
	}

	focused := NewHomeModel(nil, context.Background(), nil) // focused defaults true
	updated, _ = focused.Update(sessionListMsg{sessions: []*pb.Session{q}})
	if got := updated.(HomeModel).pendingAttentionSessionID; got != "" {
		t.Errorf("focused: pending = %q, want empty", got)
	}
}
