package views

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"google.golang.org/protobuf/proto"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func listedChats(n int) chatsListedMsg {
	msg := chatsListedMsg{daemonStatuses: map[string]string{}}
	for i := range n {
		id := "agent-" + strings.Repeat("x", i)
		msg.chats = append(msg.chats, &pb.ClaudeChat{SessionId: "session-1", AgentSessionId: id, Title: id, CreatedAt: timestamppb.Now()})
		msg.daemonStatuses[id] = statusStopped
	}
	return msg
}

// assertOneBlankLineAbove pins the footer spacing every picker footer shares:
// the first line containing want is preceded by exactly one blank line, and the
// line above that is the table's last row (rowMarker).
func assertOneBlankLineAbove(t *testing.T, screen, want, rowMarker string) {
	t.Helper()
	lines := strings.Split(ansi.Strip(screen), "\n")
	at := -1
	for i, line := range lines {
		if strings.Contains(line, want) {
			at = i
			break
		}
	}
	if at < 2 {
		t.Fatalf("%q at line %d of the screen, want a table row and a blank line above it:\n%s", want, at, strings.Join(lines, "\n"))
	}
	if strings.TrimSpace(lines[at-1]) != "" {
		t.Fatalf("line above %q is %q, want a blank line:\n%s", want, lines[at-1], strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[at-2], rowMarker) {
		t.Fatalf("two lines above %q is %q, want the last table row (%q), i.e. exactly one blank line:\n%s", want, lines[at-2], rowMarker, strings.Join(lines, "\n"))
	}
}

// TestChatPickerConfirmPromptsLeaveOneBlankLine renders the chat picker in the
// steady state and in every confirmation and asserts each leaves exactly one
// blank line between the chat table's last row and the footer.
func TestChatPickerConfirmPromptsLeaveOneBlankLine(t *testing.T) {
	prNum := int32(42)
	press := func(key rune) func(ChatPickerModel) ChatPickerModel {
		return func(m ChatPickerModel) ChatPickerModel {
			updated, _ := m.Update(keyPress(key))
			return updated.(ChatPickerModel)
		}
	}
	tests := []struct {
		name   string
		model  func() ChatPickerModel
		prompt string
	}{
		{"steady action bar", func() ChatPickerModel { return chatPickerSized(&chatPickerStub{}) }, "[d]elete"},
		{"merge", func() ChatPickerModel {
			m := chatPickerSized(&chatPickerStub{})
			m.session = &pb.Session{Id: "session-1", PrNumber: &prNum}
			m.confirm = confirmMerge
			return m
		}, "Merge PR #42?"},
		{"delete", func() ChatPickerModel {
			m := chatPickerSized(&chatPickerStub{})
			m.confirm = confirmDelete
			return m
		}, `Delete "Test chat"?`},
		{"archive", func() ChatPickerModel {
			m := chatPickerSized(&chatPickerStub{})
			m.confirm = confirmArchive
			return m
		}, "Archive this session?"},
		{"switch account", func() ChatPickerModel {
			m := chatPickerSized(&chatPickerStub{})
			m.confirm = confirmSwitch
			return m
		}, "Switch account and interrupt it?"},
		{"attention merge", func() ChatPickerModel {
			return press('m')(attentionDetails(&attentionStub{}, parkSession("human-review:ui")))
		}, "Merge PR #42?"},
		{"attention dismiss", func() ChatPickerModel {
			return press('x')(attentionDetails(&attentionStub{}, attentionSession(pb.AttentionAction_ATTENTION_ACTION_DISMISS)))
		}, "Dismiss this alert?"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertOneBlankLineAbove(t, tt.model().View().Content, tt.prompt, "just now")
		})
	}
}

// TestHomeAttentionConfirmLeavesOneBlankLine is the Home counterpart for the
// confirmations Home shares with the details view.
func TestHomeAttentionConfirmLeavesOneBlankLine(t *testing.T) {
	tests := []struct {
		name    string
		session *pb.Session
		key     rune
		prompt  string
		row     string
	}{
		{"merge", parkSession("human-review:ui"), 'm', "Merge PR #42?", "evidence video"},
		{"dismiss", attentionSession(pb.AttentionAction_ATTENTION_ACTION_DISMISS), 'x', "Dismiss this alert?", "Needs review"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model, _ := attentionHome(&attentionStub{}, proto.CloneOf(tt.session)).Update(keyPress(tt.key))
			assertOneBlankLineAbove(t, model.View().Content, tt.prompt, tt.row)
		})
	}
}

// TestAttentionConfirmKeepsTableOnScreen pins the row budget the extra blank
// line costs: at every height the confirmation is shown, banner and view still
// fit the terminal and the chat table still shows a row.
func TestAttentionConfirmKeepsTableOnScreen(t *testing.T) {
	for _, height := range []int{16, 20, 24, 30, 40} {
		m := attentionDetails(&attentionStub{}, parkSession("human-review:ui"))
		updated, _ := m.Update(listedChats(40))
		m = updated.(ChatPickerModel)
		updated, _ = m.Update(keyPress('m'))
		m = updated.(ChatPickerModel)
		updated, _ = m.Update(tea.WindowSizeMsg{Width: 160, Height: height})
		m = updated.(ChatPickerModel)
		screen := ansi.Strip(m.View().Content)
		if got := bannerOverhead + len(strings.Split(screen, "\n")); got > height {
			t.Errorf("height %d: banner and view take %d rows:\n%s", height, got, screen)
		}
		if !strings.Contains(screen, "just now") {
			t.Errorf("height %d: no chat row on screen:\n%s", height, screen)
		}
		assertOneBlankLineAbove(t, screen, "Merge PR #42?", "just now")
	}
}

// TestChatPickerFooterStatusesLeaveOneBlankLine covers the transient spinners
// and every status line the steady footer can stack: each has one blank line
// above it, and so does the action bar below the last one.
func TestChatPickerFooterStatusesLeaveOneBlankLine(t *testing.T) {
	prNum := int32(42)
	sized := func(mutate func(*ChatPickerModel)) ChatPickerModel {
		m := chatPickerSized(&chatPickerStub{})
		m.session = &pb.Session{Id: "session-1", PrNumber: &prNum}
		mutate(&m)
		return m
	}
	statuses := []struct {
		name   string
		mutate func(*ChatPickerModel)
		text   string
	}{
		{"archive wait", func(m *ChatPickerModel) { m.optimisticArchiveLatch = true }, "Archiving session once every chat is idle…"},
		{"failure", func(m *ChatPickerModel) { m.statusMsg = "Couldn't merge: boom" }, "Couldn't merge: boom"},
		{"attention result", func(m *ChatPickerModel) { m.statusNote = "Merged PR #42" }, "Merged PR #42"},
		{"switch notice", func(m *ChatPickerModel) { m.switchNotice = "switched to work" }, "switched to work"},
		{"merged label", func(m *ChatPickerModel) { m.merged = true }, "✓ PR #42 merged"},
	}
	for _, tt := range statuses {
		t.Run(tt.name, func(t *testing.T) {
			screen := sized(tt.mutate).View().Content
			assertOneBlankLineAbove(t, screen, tt.text, "just now")
			assertOneBlankLineAbove(t, screen, "[d]elete", tt.text)
		})
	}
	t.Run("stacked", func(t *testing.T) {
		m := sized(func(m *ChatPickerModel) {
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
		mutate func(*ChatPickerModel)
		text   string
	}{
		{"merging", func(m *ChatPickerModel) { m.merging = true }, "Merging PR #42…"},
		{"archiving", func(m *ChatPickerModel) { m.archiving = true }, "Archiving session…"},
		{"switching", func(m *ChatPickerModel) { m.switching = true }, "Switching account…"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assertOneBlankLineAbove(t, sized(tt.mutate).View().Content, tt.text, "just now")
		})
	}
}

// TestChatPickerFooterStatusesKeepTableOnScreen is the row budget for the
// status lines: whenever the footer's rows fit the terminal at all, banner and
// view still fit it and the chat table still shows a row.
func TestChatPickerFooterStatusesKeepTableOnScreen(t *testing.T) {
	tests := []struct {
		name    string
		heights []int
		mutate  func(*ChatPickerModel)
	}{
		{"one status", []int{16, 20, 24, 30, 40}, func(m *ChatPickerModel) { m.statusMsg = "failed" }},
		{"every status", []int{30, 40}, func(m *ChatPickerModel) {
			m.optimisticArchiveLatch, m.merged = true, true
			m.statusMsg, m.statusNote, m.switchNotice = "failed", "Merged PR #42", "switched"
		}},
	}
	for _, tt := range tests {
		for _, height := range tt.heights {
			m := attentionDetails(&attentionStub{}, parkSession("human-review:ui"))
			updated, _ := m.Update(listedChats(40))
			m = updated.(ChatPickerModel)
			updated, _ = m.Update(tea.WindowSizeMsg{Width: 160, Height: height})
			m = updated.(ChatPickerModel)
			tt.mutate(&m)
			m.syncTableHeight()
			screen := ansi.Strip(m.View().Content)
			if got := bannerOverhead + len(strings.Split(screen, "\n")); got > height {
				t.Errorf("%s at height %d: banner and view take %d rows:\n%s", tt.name, height, got, screen)
			}
			if !strings.Contains(screen, "just now") {
				t.Errorf("%s at height %d: no chat row on screen:\n%s", tt.name, height, screen)
			}
		}
	}
}

// TestChatPickerStatusLineKeepsCursorRowOnScreen sets a footer status line on a
// long list with the cursor on the last visible row of the first page, and on
// the list's last row. The status takes rows from the table, so the retained
// table has to shrink and re-anchor on the cursor in the same Update; a bare
// shrink, or sizing only the rendered copy, leaves the selected row outside
// the window.
func TestChatPickerStatusLineKeepsCursorRowOnScreen(t *testing.T) {
	results := map[string]attentionActionResultMsg{
		"attention result": {sessionID: "session-1", text: "Merged PR #42"},
		"failure":          {sessionID: "session-1", err: errors.New("boom")},
	}
	cursors := map[string]func(ChatPickerModel) int{
		"last visible row": func(m ChatPickerModel) int { return m.table.Height() - 1 },
		"last row":         func(m ChatPickerModel) int { return len(m.chats) - 1 },
	}
	for name, result := range results {
		for cursorName, cursorAt := range cursors {
			for _, height := range []int{20, 24, 30} {
				label := fmt.Sprintf("%s, cursor on %s, height %d", name, cursorName, height)
				m := attentionDetails(&attentionStub{}, parkSession("human-review:ui"))
				updated, _ := m.Update(listedChats(40))
				m = updated.(ChatPickerModel)
				updated, _ = m.Update(tea.WindowSizeMsg{Width: 160, Height: height})
				m = updated.(ChatPickerModel)
				m.table.MoveDown(cursorAt(m))
				updateCursorColumn(&m.table)
				if !strings.Contains(ansi.Strip(m.View().Content), cursorChevron) {
					t.Fatalf("%s: cursor row not on screen before the status line:\n%s", label, ansi.Strip(m.View().Content))
				}

				updated, _ = m.Update(result)
				m = updated.(ChatPickerModel)

				screen := ansi.Strip(m.View().Content)
				if !strings.Contains(screen, cursorChevron) {
					t.Errorf("%s: cursor row hidden once the status line is set:\n%s", label, screen)
				}
				if got := bannerOverhead + len(strings.Split(screen, "\n")); got > height {
					t.Errorf("%s: banner and view take %d rows", label, got)
				}
			}
		}
	}
}
