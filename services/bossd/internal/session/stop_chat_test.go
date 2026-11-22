package session

import (
	"errors"
	"strings"
	"testing"

	"github.com/recurser/bossd/internal/domain"
)

func TestStopChat_ClearsThePointerThenEndsOnlyThatChatsPane(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	tmuxName := "boss-aaaaaaaa-dddddddd"
	h := reapIdleChatHarness(t, &tmuxName)

	if err := h.lc.StopChat(t.Context(), "sess-1", "agent-idle"); err != nil {
		t.Fatalf("StopChat: %v", err)
	}
	call := h.findCall("kill-session")
	if call == nil || strings.Join(call.args, " ") != "-t "+tmuxName {
		t.Fatalf("kill-session call = %+v, want -t %s", call, tmuxName)
	}
	if len(h.chats.tmuxNameUpdates) != 1 || h.chats.tmuxNameUpdates[0].name != nil {
		t.Fatalf("tmux name updates = %+v, want the pointer cleared once", h.chats.tmuxNameUpdates)
	}
}

func TestStopChat_LeavesAStoppedOrForeignChatAlone(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	t.Run("no pane", func(t *testing.T) {
		h := reapIdleChatHarness(t, nil)
		if err := h.lc.StopChat(t.Context(), "sess-1", "agent-idle"); err != nil {
			t.Fatalf("StopChat: %v, want nil for a chat already stopped", err)
		}
		if call := h.findCall("kill-session"); call != nil {
			t.Fatal("tmux kill-session ran for a chat with no pane")
		}
	})
	t.Run("another session", func(t *testing.T) {
		tmuxName := "boss-aaaaaaaa-dddddddd"
		h := reapIdleChatHarness(t, &tmuxName)
		if err := h.lc.StopChat(t.Context(), "sess-2", "agent-idle"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("StopChat err = %v, want ErrNotFound", err)
		}
		if call := h.findCall("kill-session"); call != nil {
			t.Fatal("tmux kill-session ran for a chat of another session")
		}
	})
}
