package chatpicker

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/api/tui/ui"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func earlierChatPicker() Model {
	m := chatPickerSized(&chatPickerStub{})
	m.session = &pb.Session{Title: "Long session"}
	m.chats = nil
	now := time.Now()
	for i := range 8 {
		m.chats = append(m.chats, &pb.AgentChat{AgentSessionId: fmt.Sprintf("verify-%d", i), Role: pb.ChatRole_CHAT_ROLE_VERIFY, CreatedAt: timestamppb.New(now.Add(-time.Duration(i) * time.Minute))})
	}
	m.buildTableRows()
	return m
}

func TestEarlierToggle_ChatPicker_AC215501(t *testing.T) {
	m := earlierChatPicker()
	if got := len(m.table.Rows()); got != 2 {
		t.Fatalf("collapsed rows=%d, want 2", got)
	}
	if !strings.Contains(m.View().Content, "+7 earlier (7 verify)") {
		t.Fatal(m.View().Content)
	}
	m.table.SetCursor(1)
	if m.SelectedChatID() != "" {
		t.Fatal("toggle selected a chat")
	}
	for _, key := range []rune{'d', 'r', 'w', 'c'} {
		updated, cmd := m.Update(tea.KeyPressMsg{Code: key})
		m = updated.(Model)
		if cmd != nil || m.confirm != confirmNone || m.renaming || m.pickingAccount {
			t.Fatalf("%c acted on toggle", key)
		}
	}
	if !strings.Contains(m.View().Content, "[enter] show earlier") {
		t.Fatal("missing toggle action")
	}
	updated, _ := m.keyOpenChat()
	m = updated.(Model)
	if len(m.table.Rows()) != 9 || m.table.Cursor() != 8 || !strings.Contains(m.View().Content, "hide 7 earlier") {
		t.Fatal("expand lost rows or cursor")
	}
	updated, _ = m.keyOpenChat()
	m = updated.(Model)
	if len(m.table.Rows()) != 2 || m.table.Cursor() != 1 {
		t.Fatal("collapse lost toggle cursor")
	}
}

func TestEarlierProtectedChats_ChatPicker_AC215501(t *testing.T) {
	for _, status := range []string{ui.StatusWorking, ui.StatusQuestion, ui.StatusWaiting, ui.StatusLimited, "failed", "attention", "highlight", "operator", "legacy"} {
		t.Run(status, func(t *testing.T) {
			m := earlierChatPicker()
			chat := m.chats[4]
			m.daemonStatuses = map[string]string{chat.AgentSessionId: status}
			switch status {
			case "failed":
				chat.StartError = "could not start"
			case "attention":
				m.session.Attention = &pb.SessionAttention{TargetChatId: chat.AgentSessionId}
			case "highlight":
				m.highlightID = chat.AgentSessionId
			case "operator":
				chat.Role = pb.ChatRole_CHAT_ROLE_OPERATOR
			case "legacy":
				chat.Role = pb.ChatRole_CHAT_ROLE_UNSPECIFIED
			}
			m.buildTableRows()
			found := false
			for i := range m.table.Rows() {
				m.table.SetCursor(i)
				if m.SelectedChatID() == chat.AgentSessionId {
					found = true
				}
			}
			if !found {
				t.Fatal("protected chat hidden")
			}
			if status == "failed" && !strings.Contains(m.View().Content, "× failed") {
				t.Fatal("failed badge hidden")
			}
		})
	}
	m := earlierChatPicker()
	m.chats = m.chats[:2]
	m.buildTableRows()
	if len(m.table.Rows()) != 2 {
		t.Fatal("lone earlier chat collapsed")
	}
	m.table.SetCursor(1)
	if m.SelectedChatID() != m.chats[1].AgentSessionId {
		t.Fatal("lone earlier chat replaced by toggle")
	}
}

func TestEarlierProjectionRefresh_ChatPicker_AC215501(t *testing.T) {
	m := earlierChatPicker()
	protected := m.chats[5]
	m.highlightID = protected.AgentSessionId
	updated, _ := m.handleChatsListed(ChatsListedMsg{Chats: m.chats})
	m = updated.(Model)
	if m.SelectedChatID() != protected.AgentSessionId {
		t.Fatal("highlight did not map through visible rows")
	}
	m.daemonStatuses = map[string]string{protected.AgentSessionId: ui.StatusWorking}
	m.buildTableRows()
	before := m.table.Rows()[1][4]
	m = spinnerTick(m)
	if m.table.Rows()[1][4] == before {
		t.Fatal("visible protected chat spinner did not animate")
	}
	if got := m.table.Rows()[2][1]; !strings.Contains(got, "+6 earlier") {
		t.Fatalf("spinner changed toggle: %s", got)
	}
	updated, _ = m.handleRefresh(RefreshMsg{DaemonStatuses: map[string]string{protected.AgentSessionId: ui.StatusIdle}})
	m = updated.(Model)
	if m.SelectedChatID() != protected.AgentSessionId {
		t.Fatal("poll moved highlighted cursor")
	}
	m.table.SetCursor(len(m.table.Rows()) - 1)
	updated, _ = m.keyOpenChat()
	m = updated.(Model)
	updated, _ = m.handleRefresh(RefreshMsg{DaemonStatuses: map[string]string{}})
	m = updated.(Model)
	if len(m.table.Rows()) != 9 {
		t.Fatal("poll reset expansion")
	}
	m.table.SetCursor(7)
	updated, _ = m.handleChatDeleted(chatDeletedMsg{agentSessionID: m.chats[7].AgentSessionId})
	m = updated.(Model)
	if m.table.Cursor() >= len(m.table.Rows()) {
		t.Fatal("delete left cursor past projection")
	}
	if !strings.Contains(m.View().Content, "hide 5 earlier") {
		t.Fatal("delete did not rebuild count")
	}
}

func TestEarlierRoleCounts_ChatPicker_AC215501(t *testing.T) {
	m := earlierChatPicker()
	m.chats = nil
	now := time.Now()
	for _, role := range []pb.ChatRole{pb.ChatRole_CHAT_ROLE_BUILD, pb.ChatRole_CHAT_ROLE_VERIFY, pb.ChatRole_CHAT_ROLE_REPAIR} {
		for i := range 3 {
			m.chats = append(m.chats, &pb.AgentChat{AgentSessionId: fmt.Sprintf("%d-%d", role, i), Role: role, CreatedAt: timestamppb.New(now.Add(-time.Duration(i) * time.Minute)), ReclaimedAt: timestamppb.Now()})
		}
	}
	m.chats[0].ReclaimedAt = nil
	m.chats[3].ReclaimedAt = nil
	m.chats[6].ReclaimedAt = nil
	m.chats[8].StartError = "failed to start"
	m.buildTableRows()
	view := m.View().Content
	if !strings.Contains(view, "+5 earlier (2 build, 2 verify, 1 repair)") || !strings.Contains(view, "× failed") || strings.Contains(view, "↺ replaced") {
		t.Fatal(view)
	}
	for i := range m.table.Rows() {
		m.table.SetCursor(i)
		if chat := m.selectedChat(); chat != nil && chat.GetAgentSessionId() == m.chats[8].AgentSessionId {
			updated, _ := m.handleChatDeleted(chatDeletedMsg{agentSessionID: chat.GetAgentSessionId()})
			m = updated.(Model)
			if m.table.Cursor() >= len(m.table.Rows()) {
				t.Fatal("collapsed delete cursor out of range")
			}
			return
		}
	}
	t.Fatal("failed chat missing")
}

func TestLongAutomationSession_ChatPicker_AC215501(t *testing.T) {
	m := earlierChatPicker()
	now := time.Now()
	for i := range 20 {
		m.chats = append(m.chats, &pb.AgentChat{AgentSessionId: fmt.Sprintf("repair-%d", i), Role: pb.ChatRole_CHAT_ROLE_REPAIR, CreatedAt: timestamppb.New(now.Add(-time.Duration(i) * time.Minute))})
	}
	m.chats = append(m.chats, &pb.AgentChat{AgentSessionId: "build", Role: pb.ChatRole_CHAT_ROLE_BUILD, CreatedAt: timestamppb.New(now.Add(-time.Hour))})
	for i, chat := range m.chats {
		if i != 0 && i != 8 && i != 28 {
			chat.ReclaimedAt = timestamppb.Now()
		}
	}
	m.chats[27].StartError = "start failed"
	m.buildTableRows()
	view := m.View().Content
	for _, want := range []string{"verify 8", "repair 20", "× failed", "+25 earlier (7 verify, 18 repair)"} {
		if !strings.Contains(view, want) {
			t.Fatalf("missing %q: %s", want, view)
		}
	}
	if strings.Contains(view, "↺ replaced") {
		t.Fatal("earlier replaced rows visible")
	}
	if len(m.chats) != 29 || len(m.table.Rows()) != 5 {
		t.Fatal("long session projection lost chats")
	}
	m.table.SetCursor(4)
	updated, _ := m.keyOpenChat()
	m = updated.(Model)
	if len(m.table.Rows()) != 30 || !strings.Contains(m.View().Content, "hide 25 earlier") {
		t.Fatal("long session did not expand")
	}
}

func TestEarlierFocusedChatBecomingIdle_ChatPicker_AC215501(t *testing.T) {
	for _, status := range []string{ui.StatusWorking, ui.StatusWaiting, ui.StatusQuestion, ui.StatusLimited} {
		t.Run(status, func(t *testing.T) {
			m := earlierChatPicker()
			focused := m.chats[3]
			m.chats = append(m.chats, &pb.AgentChat{AgentSessionId: "operator", Role: pb.ChatRole_CHAT_ROLE_OPERATOR, CreatedAt: timestamppb.Now()})
			m.daemonStatuses = map[string]string{focused.GetAgentSessionId(): status}
			m.buildTableRows()
			m.table.SetCursor(1)
			if m.SelectedChatID() != focused.GetAgentSessionId() {
				t.Fatal("fixture did not focus earlier live chat")
			}
			updated, _ := m.handleRefresh(RefreshMsg{DaemonStatuses: map[string]string{focused.GetAgentSessionId(): ui.StatusIdle}})
			m = updated.(Model)
			if m.SelectedChatID() != "" || !strings.Contains(m.View().Content, "[enter] show earlier") {
				t.Fatal("poll silently selected a different chat instead of history toggle")
			}
			updated, cmd := m.keyOpenChat()
			m = updated.(Model)
			if cmd != nil || !strings.Contains(m.View().Content, "hide 7 earlier") {
				t.Fatal("enter after poll attached another chat instead of expanding history")
			}
		})
	}
}

func TestEarlierDefaultFocusUsesVisibleChat_ChatPicker_AC215501(t *testing.T) {
	m := chatPickerSized(&chatPickerStub{})
	now := time.Now()
	chats := []*pb.AgentChat{
		{AgentSessionId: "verify-new", Role: pb.ChatRole_CHAT_ROLE_VERIFY},
		{AgentSessionId: "verify-idle", Role: pb.ChatRole_CHAT_ROLE_VERIFY},
		{AgentSessionId: "build-working", Role: pb.ChatRole_CHAT_ROLE_BUILD},
		{AgentSessionId: "verify-old", Role: pb.ChatRole_CHAT_ROLE_VERIFY},
	}
	for i, chat := range chats {
		chat.CreatedAt = timestamppb.New(now.Add(-time.Duration(i) * time.Minute))
	}
	updated, _ := m.handleChatsListed(ChatsListedMsg{Chats: chats, DaemonStatuses: map[string]string{"verify-new": ui.StatusStopped, "verify-idle": ui.StatusIdle, "build-working": ui.StatusWorking, "verify-old": ui.StatusStopped}})
	m = updated.(Model)
	if got := m.SelectedChatID(); got != "build-working" {
		t.Fatalf("default focus=%q, want visible working build", got)
	}
}

func TestEarlierDeletePreservesNavigatedChat_ChatPicker_AC215501(t *testing.T) {
	for _, navigate := range []bool{true, false} {
		t.Run(fmt.Sprintf("navigate=%v", navigate), func(t *testing.T) {
			m := chatPickerSized(&chatPickerStub{})
			now := time.Now()
			chats := []*pb.AgentChat{
				{AgentSessionId: "verify-new", Role: pb.ChatRole_CHAT_ROLE_VERIFY},
				{AgentSessionId: "verify-old", Role: pb.ChatRole_CHAT_ROLE_VERIFY},
				{AgentSessionId: "repair-new", Role: pb.ChatRole_CHAT_ROLE_REPAIR},
				{AgentSessionId: "repair-old", Role: pb.ChatRole_CHAT_ROLE_REPAIR},
				{AgentSessionId: "repair-oldest", Role: pb.ChatRole_CHAT_ROLE_REPAIR},
			}
			for i, chat := range chats {
				chat.CreatedAt = timestamppb.New(now.Add(-time.Duration(i) * time.Minute))
			}
			updated, _ := m.handleChatsListed(ChatsListedMsg{Chats: chats})
			m = updated.(Model)
			m.table.SetCursor(0)
			updated, _ = m.startDelete()
			m = updated.(Model)
			if navigate {
				updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
				m = updated.(Model)
				if m.SelectedChatID() != "repair-new" {
					t.Fatal("fixture did not navigate while deletion was pending")
				}
			}
			updated, _ = m.handleChatDeleted(chatDeletedMsg{agentSessionID: "verify-new"})
			m = updated.(Model)
			if navigate {
				if got := m.SelectedChatID(); got != "repair-new" {
					t.Fatalf("delete moved navigated selection to %q", got)
				}
			} else if m.SelectedChatID() != "" || !strings.Contains(m.View().Content, "[enter] show earlier") {
				t.Fatal("deleting selected chat did not focus safe history toggle")
			}
		})
	}
}

func TestDeleteSelectedChatKeepsOrdinal_ChatPicker_AC215501(t *testing.T) {
	for _, cursor := range []int{0, 2, 4} {
		t.Run(fmt.Sprintf("cursor=%d", cursor), func(t *testing.T) {
			m := chatPickerSized(&chatPickerStub{})
			m.chats = nil
			for i := range 5 {
				m.chats = append(m.chats, &pb.AgentChat{
					AgentSessionId: fmt.Sprintf("chat-%d", i),
					CreatedAt:      timestamppb.New(time.Now()),
				})
			}
			m.buildTableRows()
			m.table.SetCursor(cursor)
			deleted := m.SelectedChatID()
			updated, _ := m.handleChatDeleted(chatDeletedMsg{agentSessionID: deleted})
			m = updated.(Model)
			wantCursor := min(cursor, 3)
			if got := m.table.Cursor(); got != wantCursor {
				t.Fatalf("cursor after deleting %q = %d, want %d", deleted, got, wantCursor)
			}
			if got := m.SelectedChatID(); got != m.chats[wantCursor].GetAgentSessionId() {
				t.Fatalf("selected chat = %q, want row %d", got, wantCursor)
			}
		})
	}
}
