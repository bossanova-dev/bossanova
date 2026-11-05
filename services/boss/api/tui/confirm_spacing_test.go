package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/recurser/boss/api/tui/chatpicker"

	"github.com/recurser/boss/api/tui/ui"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"google.golang.org/protobuf/proto"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func listedChats(n int) chatpicker.ChatsListedMsg {
	msg := chatpicker.ChatsListedMsg{DaemonStatuses: map[string]string{}}
	for i := range n {
		id := "agent-" + strings.Repeat("x", i)
		msg.Chats = append(msg.Chats, &pb.AgentChat{SessionId: "session-1", AgentSessionId: id, Title: id, CreatedAt: timestamppb.Now()})
		msg.DaemonStatuses[id] = ui.StatusStopped
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
		{"merge", parkSession("human-review:ui"), 'm', "Merge PR #42?", "Parked change"},
		{"dismiss", attentionSession(pb.AttentionAction_ATTENTION_ACTION_DISMISS), 'x', "Dismiss this alert?", "Needs review"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model, _ := attentionHome(&attentionStub{}, proto.CloneOf(tt.session)).Update(keyPress(tt.key))
			assertOneBlankLineAbove(t, model.View().Content, tt.prompt, tt.row)
		})
	}
}

// TestHomeMergeResultLeavesOneBlankLine is the result line a landed merge
// leaves on Home: one blank line under the board's last row, and one between it
// and the action bar, the spacing the confirmation before it already has.
func TestHomeMergeResultLeavesOneBlankLine(t *testing.T) {
	stub := &attentionStub{mergeResp: &pb.OperatorMergeSessionResponse{IsMerged: true, TrackerWrites: "retrying", TrackerWhy: "pr-unreadable"}}
	model, _ := attentionHome(stub, parkSession("human-review:ui")).Update(keyPress('m'))
	model, merge := model.Update(keyPress('y'))
	model, _ = model.Update(merge())
	screen := model.View().Content
	assertOneBlankLineAbove(t, screen, "Merged PR #42", "Parked change")
	assertOneBlankLineAbove(t, screen, "[n]ew session", "Merged PR #42")
}

// TestAttentionConfirmKeepsTableOnScreen pins the row budget the extra blank
// line costs: at every height the confirmation is shown, banner and view still
// fit the terminal and the chat table still shows a row.

// TestChatPickerFooterStatusesLeaveOneBlankLine covers the transient spinners
// and every status line the steady footer can stack: each has one blank line
// above it, and so does the action bar below the last one.

// TestChatPickerFooterStatusesKeepTableOnScreen is the row budget for the
// status lines: whenever the footer's rows fit the terminal at all, banner and
// view still fit it and the chat table still shows a row.

// TestChatPickerStatusLineKeepsCursorRowOnScreen sets a footer status line on a
// long list with the cursor on the last visible row of the first page, and on
// the list's last row. The status takes rows from the table, so the retained
// table must shrink in the same Update while keeping the selected row visible.

// detailsAppOnFullList is the App on the details view of a merge-ready session
// with more chats than any terminal here fits, sized to height, with the
// cursor moved down to the last row the table shows.
func detailsAppOnFullList(t *testing.T, s *pb.Session, height int) tea.Model {
	t.Helper()
	c := &attentionStub{}
	a := NewApp(c, nil)
	a.chatPicker = attentionDetails(c, s)
	a.activeView = ui.ViewChatPicker
	var model tea.Model = a
	model, _ = model.Update(listedChats(40))
	model, _ = model.Update(tea.WindowSizeMsg{Width: 160, Height: height})
	a = model.(App)
	a.chatPicker.Table().MoveDown(a.chatPicker.Table().Height() - 1)
	model = a
	if screen := ansi.Strip(model.View().Content); !strings.Contains(screen, ui.CursorChevron) {
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
			model, _ = a.Update(chatpicker.RefreshMsg{
				DaemonStatuses: map[string]string{a.chatPicker.SelectedChatID(): ui.StatusWorking},
			})
			model, cmd := model.Update(keyPress('c'))
			if cmd == nil {
				t.Fatal("switch account did not load accounts")
			}
			model, _ = model.Update(cmd())
			model, _ = model.Update(attentionKey("enter"))
			return model
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
			for _, want := range []string{tt.prompt, "esc] cancel", ui.CursorChevron} {
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
