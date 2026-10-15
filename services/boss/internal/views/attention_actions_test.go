package views

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func attentionSession(actions ...pb.AttentionAction) *pb.Session {
	n := int32(42)
	return &pb.Session{Id: "session-1", Title: "Needs review", PrNumber: &n, PrDisplayHeadSha: "rendered-head", Attention: &pb.SessionAttention{Actions: actions, ParkCodes: []string{"ledger-open"}, AlertKey: "resume:auth", TargetChatId: "target-chat"}}
}

func attentionHome(c *attentionStub, s *pb.Session) HomeModel {
	h := NewHomeModel(c, context.Background(), nil)
	h.loading = false
	h.repoCount = 1
	h.width = 120
	h.height = 30
	h.sessions = []*pb.Session{s}
	h.buildTableRows()
	return h
}

type attentionStub struct {
	noAgent         bool
	delivery        *pb.SendChatMessageResponse
	deliveryErr     error
	postedResult    *pb.SendBackSessionPRResponse
	recordErr       error
	recordedSession string
	recordedTitle   string
	recordedRole    pb.ChatRole
	recordCalls     int
	sentMessages    []*pb.SendChatMessageRequest
	chatPickerStub
	calls []string
	head  string
	note  string
}

func (s *attentionStub) ApproveSessionPR(_ context.Context, id string, _ bool) (*pb.VerifyApproveResult, error) {
	s.calls = append(s.calls, "approve:"+id)
	return &pb.VerifyApproveResult{IsApproved: true, Waives: "ledger-open"}, nil
}
func (s *attentionStub) SendBackSessionPR(_ context.Context, id, note string, _ bool) (*pb.SendBackSessionPRResponse, error) {
	s.calls = append(s.calls, "send-back:"+id)
	s.note = note
	if s.postedResult != nil {
		return s.postedResult, nil
	}
	return &pb.SendBackSessionPRResponse{IsPosted: true}, nil
}
func (s *attentionStub) MergeSession(_ context.Context, id, head string) (*pb.Session, string, error) {
	s.calls = append(s.calls, "merge:"+id)
	s.head = head
	return nil, "", nil
}
func (s *attentionStub) DismissSessionAlert(_ context.Context, id, key string) (*pb.Session, error) {
	s.calls = append(s.calls, "dismiss:"+id+":"+key)
	return nil, nil
}
func (s *attentionStub) ResumeSessionRun(_ context.Context, id string) (*pb.Session, error) {
	s.calls = append(s.calls, "resume:"+id)
	return nil, nil
}

func TestAttentionActionConfirm(t *testing.T) {
	for _, tc := range []struct {
		key    rune
		action pb.AttentionAction
		call   string
	}{
		{'y', pb.AttentionAction_ATTENTION_ACTION_APPROVE, "approve:session-1"},
		{'m', pb.AttentionAction_ATTENTION_ACTION_MERGE, "merge:session-1"},
		{'x', pb.AttentionAction_ATTENTION_ACTION_DISMISS, "dismiss:session-1:resume:auth"},
	} {
		t.Run(string(tc.key), func(t *testing.T) {
			c := &attentionStub{}
			h := attentionHome(c, attentionSession(tc.action))
			model, _ := h.Update(keyPress(tc.key))
			h = model.(HomeModel)
			if len(c.calls) != 0 {
				t.Fatal("write before confirmation")
			}
			if !strings.Contains(h.View().Content, "[y/enter] confirm") {
				t.Fatal("missing confirmation")
			}
			model, cmd := h.Update(keyPress('n'))
			h = model.(HomeModel)
			if cmd != nil || len(c.calls) != 0 {
				t.Fatal("cancel dispatched write")
			}
			model, _ = h.Update(keyPress(tc.key))
			h = model.(HomeModel)
			// Poll data may change while confirming; merge still binds the rendered head.
			h.sessions = []*pb.Session{attentionSession(tc.action)}
			h.sessions[0].PrDisplayHeadSha = "new-head"
			model, cmd = h.Update(keyPress('y'))
			h = model.(HomeModel)
			if cmd == nil {
				t.Fatal("confirmation did not send request")
			}
			model, _ = h.Update(cmd())
			h = model.(HomeModel)
			if len(c.calls) != 1 || c.calls[0] != tc.call {
				t.Fatalf("calls = %v", c.calls)
			}
			if tc.key == 'm' && c.head != "rendered-head" {
				t.Fatalf("merge head = %q", c.head)
			}
			if h.status == "" {
				t.Fatal("missing result status")
			}
		})
	}
}

func attentionKey(s string) tea.KeyMsg {
	if s == "enter" {
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	}
	if s == "esc" {
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	}
	return keyPress([]rune(s)[0])
}

func TestAttentionActionSendBack(t *testing.T) {
	for _, details := range []bool{false, true} {
		t.Run(map[bool]string{false: "home", true: "details"}[details], func(t *testing.T) {
			c := &attentionStub{}
			s := attentionSession(pb.AttentionAction_ATTENTION_ACTION_SEND_BACK)
			var model tea.Model = attentionHome(c, s)
			if details {
				m := seedChatPicker(&c.chatPickerStub, statusStopped)
				m.client = c
				m.session = s
				model = m
			}
			model, _ = model.Update(keyPress('b'))
			model, cmd := model.Update(attentionKey("enter"))
			if cmd != nil || len(c.calls) != 0 {
				t.Fatal("empty note submitted")
			}
			if !strings.Contains(model.View().Content, "Note cannot be empty") {
				t.Fatal("empty note did not stay editable")
			}
			model, _ = model.Update(attentionKey("esc"))
			if len(c.calls) != 0 {
				t.Fatal("cancel sent request")
			}
			model, _ = model.Update(keyPress('b'))
			model, _ = model.Update(tea.PasteMsg{Content: "Fix the race"})
			model, cmd = model.Update(attentionKey("enter"))
			if cmd == nil {
				t.Fatal("note did not dispatch")
			}
			// A second confirm while the request is outstanding must not send again.
			model, duplicate := model.Update(attentionKey("enter"))
			if duplicate != nil {
				t.Fatal("duplicate send-back")
			}
			model, _ = model.Update(cmd())
			if len(c.calls) != 1 || c.calls[0] != "send-back:session-1" || c.note != "Fix the race" {
				t.Fatalf("calls=%v note=%q", c.calls, c.note)
			}
			if !strings.Contains(model.View().Content, "Sent back #42") {
				t.Fatal("missing result")
			}
		})
	}
}

func TestAttentionActionResumeAndRename(t *testing.T) {
	for _, details := range []bool{false, true} {
		t.Run(map[bool]string{false: "home", true: "details"}[details], func(t *testing.T) {
			c := &attentionStub{}
			s := attentionSession(pb.AttentionAction_ATTENTION_ACTION_RESUME)
			var model tea.Model = attentionHome(c, s)
			if details {
				m := seedChatPicker(&c.chatPickerStub, statusStopped)
				m.client = c
				m.session = s
				model = m
			}
			model, cmd := model.Update(keyPress('r'))
			if cmd == nil {
				t.Fatal("resume not dispatched")
			}
			model, duplicate := model.Update(keyPress('r'))
			if duplicate != nil {
				t.Fatal("duplicate resume")
			}
			model, _ = model.Update(cmd())
			if len(c.calls) != 1 || c.calls[0] != "resume:session-1" {
				t.Fatalf("calls=%v", c.calls)
			}
			if !strings.Contains(model.View().Content, "Run resumed") {
				t.Fatal("missing resume status")
			}
			s.Attention = nil
			if h, ok := model.(HomeModel); ok {
				h.buildTableRows()
				row, _ := h.tableCursorForSessionID(s.GetId())
				h.table.SetCursor(row)
				model = h
			}
			model, _ = model.Update(keyPress('r'))
			switch m := model.(type) {
			case HomeModel:
				if !m.rename.Active() {
					t.Fatal("home rename lost")
				}
			case ChatPickerModel:
				if !m.renaming {
					t.Fatal("chat rename lost")
				}
			}
		})
	}
}

func TestAttentionActionDetailsConfirm(t *testing.T) {
	for _, tc := range []struct {
		key    rune
		action pb.AttentionAction
		call   string
	}{
		{'y', pb.AttentionAction_ATTENTION_ACTION_APPROVE, "approve:session-1"},
		{'m', pb.AttentionAction_ATTENTION_ACTION_MERGE, "merge:session-1"},
		{'x', pb.AttentionAction_ATTENTION_ACTION_DISMISS, "dismiss:session-1:resume:auth"},
	} {
		t.Run(string(tc.key), func(t *testing.T) {
			c := &attentionStub{}
			m := seedChatPicker(&c.chatPickerStub, statusStopped)
			m.client = c
			m.session = attentionSession(tc.action)
			model, _ := m.Update(keyPress(tc.key))
			m = model.(ChatPickerModel)
			if !strings.Contains(m.View().Content, "[y/enter] confirm") {
				t.Fatal("missing confirmation")
			}
			model, cmd := m.Update(attentionKey("esc"))
			m = model.(ChatPickerModel)
			if cmd != nil || len(c.calls) != 0 {
				t.Fatal("cancel wrote")
			}
			model, _ = m.Update(keyPress(tc.key))
			m = model.(ChatPickerModel)
			model, cmd = m.Update(attentionKey("enter"))
			m = model.(ChatPickerModel)
			model, duplicate := m.Update(attentionKey("enter"))
			m = model.(ChatPickerModel)
			if duplicate != nil {
				t.Fatal("duplicate confirm")
			}
			if cmd == nil {
				t.Fatal("confirm did not dispatch")
			}
			model, _ = m.Update(cmd())
			m = model.(ChatPickerModel)
			if len(c.calls) != 1 || c.calls[0] != tc.call {
				t.Fatalf("calls=%v", c.calls)
			}
			if tc.key == 'm' && c.head != "rendered-head" {
				t.Fatalf("head=%q", c.head)
			}
			if m.statusNote == "" {
				t.Fatal("missing status")
			}
		})
	}
}

func TestAttentionActionNavigationAndAvailability(t *testing.T) {
	var opened []string
	original := openURLFunc
	openURLFunc = func(url string) error { opened = append(opened, url); return nil }
	t.Cleanup(func() { openURLFunc = original })
	for _, tc := range []struct {
		key    string
		action pb.AttentionAction
		url    string
	}{
		{"g", pb.AttentionAction_ATTENTION_ACTION_GITHUB, "https://github.com/org/repo/pull/42"},
		{"v", pb.AttentionAction_ATTENTION_ACTION_VIDEO, "https://example.com/proof"},
		{"l", pb.AttentionAction_ATTENTION_ACTION_TRACKER, "https://example.com/issue"},
		{"enter", pb.AttentionAction_ATTENTION_ACTION_OPEN_CHAT, ""},
		{"enter", pb.AttentionAction_ATTENTION_ACTION_ANSWER, ""},
	} {
		t.Run(tc.action.String(), func(t *testing.T) {
			s := attentionSession(tc.action)
			prURL := "https://github.com/org/repo/pull/42"
			s.PrUrl = &prURL
			s.Attention.EvidenceUrl = "https://example.com/proof"
			issueURL := "https://example.com/issue"
			s.TrackerUrl = &issueURL
			h := attentionHome(&attentionStub{}, s)
			_, cmd := h.Update(attentionKey(tc.key))
			if cmd == nil {
				t.Fatal("missing navigation")
			}
			msg := cmd()
			if tc.key == "enter" {
				nav, ok := msg.(switchViewMsg)
				if !ok || nav.sessionID != "session-1" || nav.resumeID != "target-chat" || nav.view != ViewAttach {
					t.Fatalf("navigation=%#v", msg)
				}
			} else if len(opened) == 0 || opened[len(opened)-1] != tc.url {
				t.Fatalf("opened=%v", opened)
			}
			// Details enter continues to attach the selected chat, not attention's target.
			m := seedChatPicker(&chatPickerStub{}, statusStopped)
			m.session = s
			_, cmd = m.Update(attentionKey(tc.key))
			if cmd == nil {
				t.Fatal("missing details navigation")
			}
			msg = cmd()
			if tc.key == "enter" {
				nav, ok := msg.(switchViewMsg)
				if !ok || nav.resumeID != m.selectedChat().GetAgentSessionId() {
					t.Fatalf("details navigation=%#v", msg)
				}
			}
		})
	}
	for _, key := range []rune{'y', 'b', 'x', 'v'} {
		c := &attentionStub{}
		h := attentionHome(c, attentionSession())
		_, cmd := h.Update(keyPress(key))
		if cmd != nil || len(c.calls) > 0 {
			t.Fatalf("unavailable %c dispatched", key)
		}
		m := seedChatPicker(&c.chatPickerStub, statusStopped)
		m.client = c
		m.session = attentionSession()
		_, cmd = m.Update(keyPress(key))
		if cmd != nil || len(c.calls) > 0 {
			t.Fatalf("unavailable details %c dispatched", key)
		}
	}
}

func TestAttentionActionMissingMergeHeadRefuses(t *testing.T) {
	c := &attentionStub{}
	s := attentionSession(pb.AttentionAction_ATTENTION_ACTION_MERGE)
	s.PrDisplayHeadSha = ""
	h := attentionHome(c, s)
	model, _ := h.Update(keyPress('m'))
	h = model.(HomeModel)
	model, cmd := h.Update(keyPress('y'))
	h = model.(HomeModel)
	model, _ = h.Update(cmd())
	h = model.(HomeModel)
	if len(c.calls) != 0 || !h.statusErr || !strings.Contains(h.status, "head is unavailable") {
		t.Fatalf("calls=%v status=%s", c.calls, h.status)
	}
}

func TestAttentionActionNoteKeepsTypedHintText(t *testing.T) {
	h := attentionHome(&attentionStub{}, attentionSession(pb.AttentionAction_ATTENTION_ACTION_SEND_BACK))
	model, _ := h.Update(keyPress('b'))
	h = model.(HomeModel)
	model, _ = h.Update(tea.PasteMsg{Content: "Keep [enter] rename visible"})
	h = model.(HomeModel)
	screen := h.View().Content
	if !strings.Contains(screen, "Keep [enter] rename visible") || !strings.Contains(screen, "[enter] send") {
		t.Fatalf("note or hint altered:\n%s", screen)
	}
}

func TestAttentionActionNavigationResultDoesNotUnlockResume(t *testing.T) {
	original := openURLFunc
	openURLFunc = func(string) error { return nil }
	t.Cleanup(func() { openURLFunc = original })
	c := &attentionStub{}
	h := attentionHome(c, attentionSession(pb.AttentionAction_ATTENTION_ACTION_GITHUB, pb.AttentionAction_ATTENTION_ACTION_RESUME))
	model, open := h.Update(keyPress('g'))
	h = model.(HomeModel)
	model, resume := h.Update(keyPress('r'))
	h = model.(HomeModel)
	model, _ = h.Update(open())
	h = model.(HomeModel)
	_, duplicate := h.Update(keyPress('r'))
	if duplicate != nil {
		t.Fatal("navigation result unlocked in-flight resume")
	}
	if resume == nil {
		t.Fatal("resume not dispatched")
	}
}

func TestAttentionActionFooterShowsOneEnterDestination(t *testing.T) {
	h := attentionHome(&attentionStub{}, attentionSession(pb.AttentionAction_ATTENTION_ACTION_OPEN_CHAT))
	left, _, _ := h.sessionTableFooterActions()
	labels := strings.Join(left, "  ")
	if strings.Count(labels, "[enter]") != 1 || !strings.Contains(labels, "open the failed chat") {
		t.Fatalf("ambiguous enter footer: %s", labels)
	}
}

func (s *attentionStub) RecordChat(_ context.Context, sessionID, agentSessionID, title, agent string, role pb.ChatRole, resume bool) (*pb.ClaudeChat, error) {
	s.recordCalls++
	s.recordedSession = sessionID
	s.recordedTitle = title
	s.recordedRole = role
	if s.recordErr != nil {
		return nil, s.recordErr
	}
	if s.noAgent {
		return &pb.ClaudeChat{AgentSessionId: agentSessionID}, nil
	}
	return &pb.ClaudeChat{AgentSessionId: agentSessionID, TmuxSessionName: "repair-pane"}, nil
}
func (s *attentionStub) SendChatMessage(_ context.Context, req *pb.SendChatMessageRequest) (*pb.SendChatMessageResponse, error) {
	s.sentMessages = append(s.sentMessages, req)
	if s.deliveryErr != nil {
		return nil, s.deliveryErr
	}
	if s.delivery != nil {
		return s.delivery, nil
	}
	return &pb.SendChatMessageResponse{Delivered: true, DeliveryState: pb.SendChatMessageResponse_DELIVERY_STATE_SUBMITTED}, nil
}

func TestAttentionActionSendBackStartsRepair(t *testing.T) {
	for _, tc := range []struct {
		name      string
		refused   bool
		recordErr error
	}{
		{name: "posted"}, {name: "refused", refused: true}, {name: "repair failed", recordErr: errors.New("runner unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &attentionStub{recordErr: tc.recordErr}
			if tc.refused {
				c.postedResult = &pb.SendBackSessionPRResponse{Reason: "pr-not-open"}
			}
			h := attentionHome(c, attentionSession(pb.AttentionAction_ATTENTION_ACTION_SEND_BACK))
			model, _ := h.Update(keyPress('b'))
			h = model.(HomeModel)
			model, _ = h.Update(tea.PasteMsg{Content: "Fix the race"})
			h = model.(HomeModel)
			model, cmd := h.Update(attentionKey("enter"))
			h = model.(HomeModel)
			model, _ = h.Update(cmd())
			h = model.(HomeModel)
			if tc.refused {
				if c.recordCalls != 0 || len(c.sentMessages) != 0 || !h.statusErr {
					t.Fatal("refusal started repair or lost reason")
				}
				return
			}
			if c.recordCalls != 1 || c.recordedSession != "session-1" || c.recordedTitle != "send back" {
				t.Fatalf("repair calls=%d session=%s title=%s", c.recordCalls, c.recordedSession, c.recordedTitle)
			}
			if c.recordedRole != pb.ChatRole_CHAT_ROLE_OPERATOR {
				t.Fatalf("repair chat role=%v", c.recordedRole)
			}
			if tc.recordErr != nil {
				if len(c.sentMessages) != 0 || !h.statusErr || !strings.Contains(h.status, "note is posted") {
					t.Fatalf("false success after posted note: %s", h.status)
				}
				return
			}
			if len(c.sentMessages) != 1 {
				t.Fatalf("repair messages=%v", c.sentMessages)
			}
			req := c.sentMessages[0]
			want := "/boss-repair 42\n\nThe operator sent PR #42 back. Address this feedback before anything else:\n\nFix the race"
			if req.GetAgentSessionId() == "" || req.GetMessage() != want || !req.GetSubmit() || !req.GetWakeIfAsleep() {
				t.Fatalf("repair delivery=%v", req)
			}
		})
	}
}

func TestAttentionActionSendBackDeliveryStatus(t *testing.T) {
	for _, tc := range []struct {
		name    string
		client  *attentionStub
		want    string
		wantErr bool
	}{
		{"no live agent", &attentionStub{noAgent: true}, "has no live agent", true},
		{"delivery failed", &attentionStub{deliveryErr: errors.New("delivery failed")}, "note is posted", true},
		{"delivery unconfirmed", &attentionStub{delivery: &pb.SendChatMessageResponse{}}, "inspect it before retrying", false},
		{"queued", &attentionStub{delivery: &pb.SendChatMessageResponse{Delivered: true, DeliveryState: pb.SendChatMessageResponse_DELIVERY_STATE_QUEUED, NoticeText: "queued; do not resend"}}, "queued; do not resend", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := attentionHome(tc.client, attentionSession(pb.AttentionAction_ATTENTION_ACTION_SEND_BACK))
			model, _ := h.Update(keyPress('b'))
			h = model.(HomeModel)
			model, _ = h.Update(tea.PasteMsg{Content: "Fix the race"})
			h = model.(HomeModel)
			model, cmd := h.Update(attentionKey("enter"))
			h = model.(HomeModel)
			model, _ = h.Update(cmd())
			h = model.(HomeModel)
			if h.statusErr != tc.wantErr || !strings.Contains(h.status, tc.want) {
				t.Fatalf("status=%q error=%v", h.status, h.statusErr)
			}
			if tc.client.noAgent && len(tc.client.sentMessages) != 0 {
				t.Fatal("attempted delivery to dead agent")
			}
		})
	}
}
