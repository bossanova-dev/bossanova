package chatpicker

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/recurser/boss/api/tui/ui"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestChatPickerConfirmPromptsLeaveOneBlankLine(t *testing.T) {
	prNum := int32(42)
	press := func(key rune) func(Model) Model {
		return func(m Model) Model {
			updated, _ := m.Update(keyPress(key))
			return updated.(Model)
		}
	}
	tests := []struct {
		name   string
		model  func() Model
		prompt string
	}{
		{"steady action bar", func() Model { return chatPickerSized(&chatPickerStub{}) }, "[d]elete"},
		{"merge", func() Model {
			m := chatPickerSized(&chatPickerStub{})
			m.session = &pb.Session{Id: "session-1", PrNumber: &prNum}
			m.confirm = confirmMerge
			return m
		}, "Merge PR #42?"},
		{"delete", func() Model {
			m := chatPickerSized(&chatPickerStub{})
			m.confirm = confirmDelete
			return m
		}, `Delete "Test chat"?`},
		{"archive", func() Model {
			m := chatPickerSized(&chatPickerStub{})
			m.confirm = confirmArchive
			return m
		}, "Archive this session?"},
		{"switch account", func() Model {
			m := chatPickerSized(&chatPickerStub{})
			m.confirm = confirmSwitch
			return m
		}, "Switch account and interrupt it?"},
		{"attention merge", func() Model {
			return press('m')(attentionDetails(&attentionStub{}, parkSession("human-review:ui")))
		}, "Merge PR #42?"},
		{"attention dismiss", func() Model {
			return press('x')(attentionDetails(&attentionStub{}, attentionSession(pb.AttentionAction_ATTENTION_ACTION_DISMISS)))
		}, "Dismiss this alert?"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertOneBlankLineAbove(t, tt.model().View().Content, tt.prompt, "just now")
		})
	}
}

func TestAttentionConfirmKeepsTableOnScreen(t *testing.T) {
	for _, height := range []int{16, 20, 24, 30, 40} {
		m := attentionDetails(&attentionStub{}, parkSession("human-review:ui"))
		updated, _ := m.Update(listedChats(40))
		m = updated.(Model)
		updated, _ = m.Update(keyPress('m'))
		m = updated.(Model)
		updated, _ = m.Update(tea.WindowSizeMsg{Width: 160, Height: height})
		m = updated.(Model)
		screen := ansi.Strip(m.View().Content)
		if got := ui.BannerOverhead + len(strings.Split(screen, "\n")); got > height {
			t.Errorf("height %d: banner and view take %d rows:\n%s", height, got, screen)
		}
		if !strings.Contains(screen, "just now") {
			t.Errorf("height %d: no chat row on screen:\n%s", height, screen)
		}
		assertOneBlankLineAbove(t, screen, "Merge PR #42?", "just now")
	}
}

func TestChatPickerFooterStatusesLeaveOneBlankLine(t *testing.T) {
	prNum := int32(42)
	sized := func(mutate func(*Model)) Model {
		m := chatPickerSized(&chatPickerStub{})
		m.session = &pb.Session{Id: "session-1", PrNumber: &prNum}
		mutate(&m)
		return m
	}
	statuses := []struct {
		name   string
		mutate func(*Model)
		text   string
	}{
		{"archive wait", func(m *Model) { m.optimisticArchiveLatch = true }, "Archiving session once every chat is idle…"},
		{"failure", func(m *Model) { m.statusMsg = "Couldn't merge: boom" }, "Couldn't merge: boom"},
		{"attention result", func(m *Model) { m.statusNote = "Merged PR #42" }, "Merged PR #42"},
		{"switch notice", func(m *Model) { m.switchNotice = "switched to work" }, "switched to work"},
		{"merged label", func(m *Model) { m.merged = true }, "✓ PR #42 merged"},
	}
	for _, tt := range statuses {
		t.Run(tt.name, func(t *testing.T) {
			screen := sized(tt.mutate).View().Content
			assertOneBlankLineAbove(t, screen, tt.text, "just now")
			assertOneBlankLineAbove(t, screen, "[d]elete", tt.text)
		})
	}
	t.Run("stacked", func(t *testing.T) {
		m := sized(func(m *Model) {
			for _, tt := range statuses {
				tt.mutate(m)
			}
		})
		screen := m.View().Content
		marker := "just now"
		for _, tt := range statuses {
			assertOneBlankLineAbove(t, screen, tt.text, marker)
			marker = tt.text
		}
		assertOneBlankLineAbove(t, screen, "[d]elete", marker)
	})
	for _, tt := range []struct {
		name   string
		mutate func(*Model)
		text   string
	}{
		{"merging", func(m *Model) { m.merging = true }, "Merging PR #42…"},
		{"archiving", func(m *Model) { m.archiving = true }, "Archiving session…"},
		{"switching", func(m *Model) { m.switching = true }, "Switching account…"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assertOneBlankLineAbove(t, sized(tt.mutate).View().Content, tt.text, "just now")
		})
	}
}

func TestChatPickerFooterStatusesKeepTableOnScreen(t *testing.T) {
	tests := []struct {
		name    string
		heights []int
		mutate  func(*Model)
	}{
		{"one status", []int{16, 20, 24, 30, 40}, func(m *Model) { m.statusMsg = "failed" }},
		{"every status", []int{30, 40}, func(m *Model) {
			m.optimisticArchiveLatch, m.merged = true, true
			m.statusMsg, m.statusNote, m.switchNotice = "failed", "Merged PR #42", "switched"
		}},
	}
	for _, tt := range tests {
		for _, height := range tt.heights {
			m := attentionDetails(&attentionStub{}, parkSession("human-review:ui"))
			updated, _ := m.Update(listedChats(40))
			m = updated.(Model)
			updated, _ = m.Update(tea.WindowSizeMsg{Width: 160, Height: height})
			m = updated.(Model)
			tt.mutate(&m)
			m.syncTableHeight()
			screen := ansi.Strip(m.View().Content)
			if got := ui.BannerOverhead + len(strings.Split(screen, "\n")); got > height {
				t.Errorf("%s at height %d: banner and view take %d rows:\n%s", tt.name, height, got, screen)
			}
			if !strings.Contains(screen, "just now") {
				t.Errorf("%s at height %d: no chat row on screen:\n%s", tt.name, height, screen)
			}
		}
	}
}

func TestChatPickerStatusLineKeepsCursorRowOnScreen(t *testing.T) {
	results := map[string]ui.AttentionActionResultMsg{
		"attention result": {SessionID: "session-1", Text: "Merged PR #42"},
		"failure":          {SessionID: "session-1", Err: errors.New("boom")},
	}
	cursors := map[string]func(Model) int{
		"last visible row": func(m Model) int { return m.table.Height() - 1 },
		"last row":         func(m Model) int { return len(m.chats) - 1 },
	}
	for name, result := range results {
		for cursorName, cursorAt := range cursors {
			for _, height := range []int{20, 24, 30} {
				label := fmt.Sprintf("%s, cursor on %s, height %d", name, cursorName, height)
				m := attentionDetails(&attentionStub{}, parkSession("human-review:ui"))
				updated, _ := m.Update(listedChats(40))
				m = updated.(Model)
				updated, _ = m.Update(tea.WindowSizeMsg{Width: 160, Height: height})
				m = updated.(Model)
				m.table.MoveDown(cursorAt(m))
				ui.UpdateCursorColumn(&m.table)
				if !strings.Contains(ansi.Strip(m.View().Content), ui.CursorChevron) {
					t.Fatalf("%s: cursor row not on screen before the status line:\n%s", label, ansi.Strip(m.View().Content))
				}

				updated, _ = m.Update(result)
				m = updated.(Model)

				screen := ansi.Strip(m.View().Content)
				if !strings.Contains(screen, ui.CursorChevron) {
					t.Errorf("%s: cursor row hidden once the status line is set:\n%s", label, screen)
				}
				if got := ui.BannerOverhead + len(strings.Split(screen, "\n")); got > height {
					t.Errorf("%s: banner and view take %d rows", label, got)
				}
			}
		}
	}
}

func TestChatPickerErrorScreenFooterLeavesOneBlankLine(t *testing.T) {
	failed := func() Model {
		m := chatPickerSized(&chatPickerStub{})
		m.err = errors.New("listing chats failed")
		return m
	}
	t.Run("stacked statuses", func(t *testing.T) {
		m := failed()
		m.optimisticArchiveLatch = true
		m.statusMsg = "Couldn't archive: boom"
		screen := m.View().Content
		assertOneBlankLineAbove(t, screen, "Archiving session once every chat is idle…", "listing chats failed")
		assertOneBlankLineAbove(t, screen, "Couldn't archive: boom", "Archiving session once every chat is idle…")
		assertOneBlankLineAbove(t, screen, "[a]rchive", "Couldn't archive: boom")
	})
	t.Run("archive confirmation", func(t *testing.T) {
		m := failed()
		m.confirm = confirmArchive
		assertOneBlankLineAbove(t, m.View().Content, "Archive this session?", "listing chats failed")
	})
}
