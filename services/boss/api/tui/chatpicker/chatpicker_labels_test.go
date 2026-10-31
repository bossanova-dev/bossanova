package chatpicker

import (
	"slices"
	"strings"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestChatPickerRoleLabels_ChatPicker_AC22(t *testing.T) {
	m := chatPickerSized(&chatPickerStub{})
	m.session = &pb.Session{Title: "Long session"}
	now := time.Now()
	m.chats = nil
	for i, tc := range []struct {
		title string
		role  pb.ChatRole
	}{{"build", pb.ChatRole_CHAT_ROLE_BUILD}, {"verify", pb.ChatRole_CHAT_ROLE_VERIFY}, {"repair: checks", pb.ChatRole_CHAT_ROLE_REPAIR}, {"verify 99", pb.ChatRole_CHAT_ROLE_VERIFY}, {"Repair: Long session", pb.ChatRole_CHAT_ROLE_REPAIR}} {
		m.chats = append(m.chats, &pb.AgentChat{AgentSessionId: string(rune('a' + i)), Title: tc.title, Role: tc.role, CreatedAt: timestamppb.New(now.Add(time.Duration(i) * time.Second))})
	}
	for _, width := range []int{140, 65} {
		m.width = width
		m.buildTableRows()
		view := m.View().Content
		for _, want := range []string{"build", "verify", "repair: checks", "verify 2", "repair 2"} {
			if !strings.Contains(view, want) {
				t.Errorf("width %d missing %q in %s", width, want, view)
			}
		}
		if strings.Contains(view, "Long session") && strings.Contains(view, "Repair:") {
			t.Errorf("legacy chat title still rendered: %s", view)
		}
	}
	m.table.SetCursor(4)
	renamed, _ := m.keyRename()
	m = renamed.(Model)
	if got := m.renameInput.Value(); got != "Repair: Long session" {
		t.Fatalf("rename prefill=%q", got)
	}
	m.renaming = false
	m.chats[1].Title = "my notes"
	m.buildTableRows()
	if !strings.Contains(m.View().Content, "my notes") || !strings.Contains(m.View().Content, "verify 2") {
		t.Fatal("rename lost explicit title or ordinal")
	}
	// A refresh may supply newest-first rows. Labels still follow creation time.
	slices.Reverse(m.chats)
	m.buildTableRows()
	if got := m.table.Rows()[1][1]; !strings.Contains(got, "verify 2") {
		t.Errorf("newest-first second verify row=%q", got)
	}
}
