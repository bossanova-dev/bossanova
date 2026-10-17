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

// detailsAppOnFullList is the App on the details view of a merge-ready session
// with more chats than any terminal here fits, sized to height, with the
// cursor moved down to the last row the table shows.
func detailsAppOnFullList(t *testing.T, s *pb.Session, height int) tea.Model {
	t.Helper()
	c := &attentionStub{}
	a := NewApp(c, nil)
	a.chatPicker = attentionDetails(c, s)
	a.activeView = ViewChatPicker
	var model tea.Model = a
	model, _ = model.Update(listedChats(40))
	model, _ = model.Update(tea.WindowSizeMsg{Width: 160, Height: height})
	a = model.(App)
	a.chatPicker.table.MoveDown(a.chatPicker.table.Height() - 1)
	updateCursorColumn(&a.chatPicker.table)
	model = a
	if screen := ansi.Strip(model.View().Content); !strings.Contains(screen, cursorChevron) {
		t.Fatalf("height %d: cursor row not on screen before any prompt opens:\n%s", height, screen)
	}
	return model
}

// TestChatPickerConfirmPromptsFitTheScreen opens each prompt that replaces the
// details view's action bar on a full chat list, at every height from 16 to 40,
// with the cursor on the last row the steady table shows. The whole frame,
// banner included, must fit the terminal, so the banner, the prompt, its key
// hints and the selected row are all on screen.
func TestChatPickerConfirmPromptsFitTheScreen(t *testing.T) {
	prNum := int32(42)
	ready := &pb.Session{Id: "session-1", Title: "Ship the widget", PrNumber: &prNum, DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING}
	prompts := []struct {
		name    string
		session *pb.Session
		open    func(tea.Model) tea.Model
		prompt  string
	}{
		{"delete", ready, pressKey('d'), "Delete "},
		{"archive", ready, pressKey('a'), "Archive this session?"},
		{"merge", ready, pressKey('m'), "Merge PR #42?"},
		{"rename", ready, pressKey('r'), "[enter] save"},
		{"switch account", ready, func(model tea.Model) tea.Model {
			a := model.(App)
			a.chatPicker.confirm = confirmSwitch
			a.chatPicker.syncTableHeight()
			return a
		}, "Switch account and interrupt it?"},
		{"attention merge", parkSession("human-review:ui"), pressKey('m'), "Merge PR #42?"},
	}
	for height := 16; height <= 40; height++ {
		steady := map[*pb.Session]tea.Model{}
		for _, tt := range prompts {
			if steady[tt.session] == nil {
				steady[tt.session] = detailsAppOnFullList(t, proto.CloneOf(tt.session), height)
			}
			label := fmt.Sprintf("%s, height %d", tt.name, height)
			model := tt.open(steady[tt.session])
			lines := strings.Split(ansi.Strip(model.View().Content), "\n")
			screen := strings.Join(lines, "\n")
			if len(lines) > height {
				t.Errorf("%s: frame is %d rows, %d past the terminal:\n%s", label, len(lines), len(lines)-height, screen)
				continue
			}
			if !strings.Contains(lines[1], tt.session.GetTitle()) {
				t.Errorf("%s: banner not at the top, line 1 is %q", label, lines[1])
			}
			for _, want := range []string{tt.prompt, "esc] cancel", cursorChevron} {
				if !strings.Contains(screen, want) {
					t.Errorf("%s: %q not on screen:\n%s", label, want, screen)
				}
			}
		}
	}
}

func pressKey(key rune) func(tea.Model) tea.Model {
	return func(model tea.Model) tea.Model {
		model, _ = model.Update(keyPress(key))
		return model
	}
}

// TestChatPickerErrorScreenFooterLeavesOneBlankLine covers the screen shown
// once the chat list failed: the archive wait, a failed archive and the archive
// confirmation each sit one blank line below the line above them.
func TestChatPickerErrorScreenFooterLeavesOneBlankLine(t *testing.T) {
	failed := func() ChatPickerModel {
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
