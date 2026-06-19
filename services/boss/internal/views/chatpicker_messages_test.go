package views

import (
	"context"
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestChatPicker_MergeResultMsg pins the merge-result handler: a failure
// surfaces a status message and leaves merged=false, while success flips
// merged=true with no error banner. Guards the `msg.err != nil` branch.
func TestChatPicker_MergeResultMsg(t *testing.T) {
	t.Run("error surfaces status and does not mark merged", func(t *testing.T) {
		m := NewChatPickerModel(&chatPickerStub{}, context.Background(), "session-1", "")
		m.merging = true

		updated, _ := m.Update(mergeResultMsg{err: errors.New("boom")})
		m = updated.(ChatPickerModel)

		if m.merging {
			t.Error("merging should be cleared after a merge result")
		}
		if m.merged {
			t.Error("merged must stay false when the merge failed")
		}
		if m.statusMsg == "" {
			t.Error("expected a failure status message, got empty")
		}
	})

	t.Run("success marks merged with no status banner", func(t *testing.T) {
		m := NewChatPickerModel(&chatPickerStub{}, context.Background(), "session-1", "")
		m.merging = true

		updated, _ := m.Update(mergeResultMsg{err: nil})
		m = updated.(ChatPickerModel)

		if !m.merged {
			t.Error("merged must be true after a successful merge")
		}
		if m.statusMsg != "" {
			t.Errorf("successful merge should not set a status message, got %q", m.statusMsg)
		}
	})
}

// TestChatPicker_ArchiveResultMsg pins the archive-result handler: failure
// surfaces a status message and leaves archived=false; success flips
// archived=true. Guards the `msg.err != nil` branch.
func TestChatPicker_ArchiveResultMsg(t *testing.T) {
	t.Run("error surfaces status and does not mark archived", func(t *testing.T) {
		m := NewChatPickerModel(&chatPickerStub{}, context.Background(), "session-1", "")
		m.archiving = true

		updated, _ := m.Update(archiveResultMsg{err: errors.New("boom")})
		m = updated.(ChatPickerModel)

		if m.archiving {
			t.Error("archiving should be cleared after an archive result")
		}
		if m.archived {
			t.Error("archived must stay false when the archive failed")
		}
		if m.statusMsg == "" {
			t.Error("expected a failure status message, got empty")
		}
	})

	t.Run("success marks archived with no status banner", func(t *testing.T) {
		m := NewChatPickerModel(&chatPickerStub{}, context.Background(), "session-1", "")
		m.archiving = true

		updated, _ := m.Update(archiveResultMsg{err: nil})
		m = updated.(ChatPickerModel)

		if !m.archived {
			t.Error("archived must be true after a successful archive")
		}
		if m.statusMsg != "" {
			t.Errorf("successful archive should not set a status message, got %q", m.statusMsg)
		}
	})
}

// TestChatPicker_WebOpenResultMsg pins the web-open handler: only a failure
// surfaces a status message. Guards the `msg.err != nil` branch.
func TestChatPicker_WebOpenResultMsg(t *testing.T) {
	t.Run("error surfaces status", func(t *testing.T) {
		m := NewChatPickerModel(&chatPickerStub{}, context.Background(), "session-1", "")

		updated, _ := m.Update(webOpenResultMsg{err: errors.New("no browser")})
		m = updated.(ChatPickerModel)

		if m.statusMsg == "" {
			t.Error("expected a failure status message, got empty")
		}
	})

	t.Run("success leaves status untouched", func(t *testing.T) {
		m := NewChatPickerModel(&chatPickerStub{}, context.Background(), "session-1", "")

		updated, _ := m.Update(webOpenResultMsg{err: nil})
		m = updated.(ChatPickerModel)

		if m.statusMsg != "" {
			t.Errorf("successful web open should not set a status message, got %q", m.statusMsg)
		}
	})
}

// TestChatPicker_ChatDeletedMsg_ErrPath pins the delete-failure handler: the
// in-flight delete marker is cleared, a failure banner is surfaced, and the
// chat is left in place (not removed). Guards both the deleting-marker match
// (`msg.agentSessionID == m.deletingAgentSessionID`) and the `msg.err != nil`
// early return.
func TestChatPicker_ChatDeletedMsg_ErrPath(t *testing.T) {
	m := seedChatPicker(&chatPickerStub{}, "")
	m.deletingAgentSessionID = "agent-1"

	updated, _ := m.Update(chatDeletedMsg{agentSessionID: "agent-1", err: errors.New("delete failed")})
	m = updated.(ChatPickerModel)

	if m.deletingAgentSessionID != "" {
		t.Errorf("deletingAgentSessionID should be cleared for the matching chat, got %q", m.deletingAgentSessionID)
	}
	if m.statusMsg == "" {
		t.Error("expected a delete-failure status message, got empty")
	}
	if len(m.chats) != 1 {
		t.Errorf("a failed delete must not drop the chat, got %d chats", len(m.chats))
	}
}

// TestChatPicker_TKey_NewTabGuards pins the [t] open-in-new-tab guards: a
// session with a worktree path yields a command, while a missing worktree
// path yields none. Guards the session-nil check and the empty-path check.
func TestChatPicker_TKey_NewTabGuards(t *testing.T) {
	t.Run("session with worktree path dispatches new tab", func(t *testing.T) {
		m := seedChatPicker(&chatPickerStub{}, "")
		m.newTabSupported = true
		m.session = &pb.Session{WorktreePath: "/tmp/worktree"}

		_, cmd := m.Update(keyPress('t'))
		if cmd == nil {
			t.Fatal("expected a new-tab command for a session with a worktree path")
		}
	})

	t.Run("empty worktree path dispatches nothing", func(t *testing.T) {
		m := seedChatPicker(&chatPickerStub{}, "")
		m.newTabSupported = true
		m.session = &pb.Session{WorktreePath: ""}

		_, cmd := m.Update(keyPress('t'))
		if cmd != nil {
			t.Fatal("expected no command when the worktree path is empty")
		}
	})
}

// TestChatPicker_DKey_DeleteConfirmGuard pins the [d] guard: with a selected
// chat and no delete in flight, pressing d opens the delete confirmation.
// Guards the `m.deletingAgentSessionID == ""` check.
func TestChatPicker_DKey_DeleteConfirmGuard(t *testing.T) {
	m := seedChatPicker(&chatPickerStub{}, "")

	updated, _ := m.Update(keyPress('d'))
	m = updated.(ChatPickerModel)

	if m.confirm != confirmDelete {
		t.Errorf("pressing d on a selected chat should arm confirmDelete, got %v", m.confirm)
	}
}

// TestChatPicker_EnterKey_AttachesSelectedChat pins the [enter] guard: with a
// chat selected, enter dispatches a switch to the attach view resuming that
// chat. Guards the `chat != nil` check.
func TestChatPicker_EnterKey_AttachesSelectedChat(t *testing.T) {
	m := seedChatPicker(&chatPickerStub{}, "")

	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected a switch-view command from enter on a selected chat")
	}
	msg, ok := cmd().(switchViewMsg)
	if !ok {
		t.Fatalf("enter cmd produced %T, want switchViewMsg", cmd())
	}
	if msg.view != ViewAttach {
		t.Errorf("enter should switch to ViewAttach, got %v", msg.view)
	}
	if msg.resumeID != "agent-1" {
		t.Errorf("enter should resume the selected chat agent-1, got %q", msg.resumeID)
	}
}

// TestChatPicker_AKey_ArchiveInErrorState pins the [a] handler reachable from
// the chat-listing error screen: with a session id present, pressing a arms
// the archive confirmation even though the chat list failed to load. Guards
// the `m.sessionID != ""` check in the error branch.
func TestChatPicker_AKey_ArchiveInErrorState(t *testing.T) {
	m := seedChatPicker(&chatPickerStub{}, "")
	m.err = errors.New("listing failed")

	updated, _ := m.Update(keyPress('a'))
	m = updated.(ChatPickerModel)

	if m.confirm != confirmArchive {
		t.Errorf("pressing a in the error state with a session id should arm confirmArchive, got %v", m.confirm)
	}
}
