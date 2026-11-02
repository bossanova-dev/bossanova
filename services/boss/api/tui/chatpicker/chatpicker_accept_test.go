package chatpicker

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

const acceptHead = "abcdef0123456789abcdef0123456789abcdef01"

// The plain stub never reaches the accept flow, so an unexpected call fails.
func (s *chatPickerStub) ListSessionPRBlockers(context.Context, string) (*pb.ListSessionPRBlockersResponse, error) {
	panic("unused")
}

func (s *chatPickerStub) AcceptSessionPRBlocker(context.Context, *pb.AcceptSessionPRBlockerRequest) (*pb.AcceptSessionPRBlockerResponse, error) {
	panic("unused")
}

// acceptPickerClient answers the accept flow with canned responses and
// records what the picker sent.
type acceptPickerClient struct {
	*chatPickerStub
	blockers     *pb.ListSessionPRBlockersResponse
	acceptResp   *pb.AcceptSessionPRBlockerResponse
	blockerLists []string
	accepts      []*pb.AcceptSessionPRBlockerRequest
}

func (c *acceptPickerClient) ListSessionPRBlockers(_ context.Context, id string) (*pb.ListSessionPRBlockersResponse, error) {
	c.blockerLists = append(c.blockerLists, id)
	return c.blockers, nil
}

func (c *acceptPickerClient) AcceptSessionPRBlocker(_ context.Context, req *pb.AcceptSessionPRBlockerRequest) (*pb.AcceptSessionPRBlockerResponse, error) {
	c.accepts = append(c.accepts, req)
	return c.acceptResp, nil
}

func acceptBlockersStub() *acceptPickerClient {
	return &acceptPickerClient{
		chatPickerStub: &chatPickerStub{},
		blockers: &pb.ListSessionPRBlockersResponse{HeadSha: acceptHead, Blockers: []*pb.VerifyBlocker{
			{Id: "F-1", Severity: "P1", FilePath: "services/boss/a.go", LineNumber: 12, Summary: "nil map write"},
			{Id: "F-2", Severity: "P2", FilePath: "docs/b.md", Summary: "stale link"},
		}},
		acceptResp: &pb.AcceptSessionPRBlockerResponse{IsAccepted: true, HeadSha: acceptHead, BlockerId: "F-2", TicketIdentifier: "BOS-9001", TicketUrl: "https://linear.test/BOS-9001", CommentUrl: "https://github.test/c/1"},
	}
}

func blockedSession() *pb.Session {
	return &pb.Session{
		Id: "session-1", Title: "Blocked change", PrNumber: proto.Int32(42), PrDisplayHeadSha: acceptHead,
		DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN,
		Attention:     &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_FIX, ParkCodes: []string{"repair-exhausted"}},
	}
}

// acceptPicker is a sized picker with three chats on session s.
func acceptPicker(t *testing.T, stub *acceptPickerClient, s *pb.Session) Model {
	t.Helper()
	m := newTestModel(stub, context.Background(), "session-1", "")
	chats := make([]*pb.AgentChat, 3)
	for i := range chats {
		chats[i] = &pb.AgentChat{SessionId: "session-1", AgentSessionId: fmt.Sprintf("agent-%d", i), Title: fmt.Sprintf("Chat %d", i), CreatedAt: timestamppb.Now()}
	}
	updated, _ := m.Update(ChatsListedMsg{Chats: chats})
	updated, _ = updated.(Model).Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = updated.(Model)
	m.session = s
	return m
}

func step(t *testing.T, m Model, msg tea.Msg) (Model, tea.Cmd) {
	t.Helper()
	updated, cmd := m.Update(msg)
	return updated.(Model), cmd
}

// Proves chat-picker.AC213401.
func TestChatPickerAcceptBlockerKey_ChatPicker_AC213401(t *testing.T) {
	stub := acceptBlockersStub()
	m := acceptPicker(t, stub, blockedSession())
	if view := m.View().Content; !strings.Contains(view, "acc[e]pt blocker") {
		t.Fatalf("a blocked session's bar must offer acc[e]pt blocker:\n%s", view)
	}

	m, cmd := step(t, m, keyPress('e'))
	if cmd == nil || !m.acceptBlocker.Active() {
		t.Fatal("e must open the accept-blocker flow")
	}
	m, _ = step(t, m, cmd())
	view := stripANSI(m.View().Content)
	for _, want := range []string{"Accept a verify blocker · PR #42 at abcdef01", "services/boss/a.go:12", "nil map write", "F-1", "stale link", "F-2"} {
		if !strings.Contains(view, want) {
			t.Fatalf("picker missing %q:\n%s", want, view)
		}
	}
	if len(stub.blockerLists) != 1 || stub.blockerLists[0] != "session-1" {
		t.Fatalf("list calls = %v", stub.blockerLists)
	}

	m, _ = step(t, m, specialKeyPress(tea.KeyDown))
	m, _ = step(t, m, specialKeyPress(tea.KeyEnter))
	if !m.textEntryActive() {
		t.Fatal("the reason step must count as text entry")
	}
	m = typeIntoPicker(m, "am deferred")
	if m.confirm != confirmNone || m.merging || m.archiving || m.Cancelled() || !strings.Contains(stripANSI(m.View().Content), "am deferred") {
		t.Fatalf("typed letters must fill the reason, never act: confirm=%v", m.confirm)
	}
	m, _ = step(t, m, specialKeyPress(tea.KeyEnter))
	if view := stripANSI(m.View().Content); !strings.Contains(view, "Accept F-2 on PR #42 at abcdef01?") {
		t.Fatalf("missing confirmation:\n%s", view)
	}
	m, cmd = step(t, m, keyPress('y'))
	if cmd == nil || len(stub.accepts) != 0 {
		t.Fatal("y must dispatch exactly the accept command")
	}
	m, _ = step(t, m, cmd())
	if len(stub.accepts) != 1 {
		t.Fatalf("accept calls = %d, want 1", len(stub.accepts))
	}
	if got := stub.accepts[0]; got.GetId() != "session-1" || got.GetBlockerId() != "F-2" || got.GetReason() != "am deferred" || got.GetExpectedHeadSha() != acceptHead || got.GetIsDryRun() {
		t.Fatalf("accept request = %v", got)
	}
	view = stripANSI(m.View().Content)
	if m.acceptBlocker.Active() || strings.Contains(view, "Accept a verify blocker") || !strings.Contains(view, "Follow-up: BOS-9001") {
		t.Fatalf("the status line must name the follow-up ticket:\n%s", view)
	}

	refused := acceptBlockersStub()
	refused.acceptResp = &pb.AcceptSessionPRBlockerResponse{Reason: "head-moved"}
	m = acceptPicker(t, refused, blockedSession())
	m, cmd = step(t, m, keyPress('e'))
	m, _ = step(t, m, cmd())
	m, _ = step(t, m, specialKeyPress(tea.KeyEnter))
	m = typeIntoPicker(m, "later")
	m, _ = step(t, m, specialKeyPress(tea.KeyEnter))
	m, cmd = step(t, m, keyPress('y'))
	m, _ = step(t, m, cmd())
	if !strings.Contains(m.statusMsg, "PR #42 blocker F-1 not accepted: the PR's head changed") {
		t.Fatalf("refusal status = %q", m.statusMsg)
	}
}

// Proves chat-picker.AC213402.
func TestChatPickerAcceptBlockerHiddenKeyDoesNothing_ChatPicker_AC213402(t *testing.T) {
	passing := blockedSession()
	passing.DisplayStatus, passing.Attention = pb.DisplayStatus_DISPLAY_STATUS_PASSING, nil
	repairing := blockedSession()
	repairing.RepairActive = true
	for name, s := range map[string]*pb.Session{"passing": passing, "repair active": repairing} {
		t.Run(name, func(t *testing.T) {
			stub := acceptBlockersStub()
			m := acceptPicker(t, stub, s)
			m.table.SetCursor(1)
			if view := m.View().Content; strings.Contains(view, "acc[e]pt") {
				t.Fatalf("the bar must not offer acc[e]pt blocker:\n%s", view)
			}
			m, cmd := step(t, m, keyPress('e'))
			if cmd != nil || m.acceptBlocker.Active() || m.table.Cursor() != 1 || len(stub.blockerLists) != 0 || len(stub.accepts) != 0 {
				t.Fatalf("a hidden e acted: cmd=%v active=%v cursor=%d lists=%v", cmd != nil, m.acceptBlocker.Active(), m.table.Cursor(), stub.blockerLists)
			}
		})
	}

	// Esc in the open picker closes it and leaves the view without sending.
	stub := acceptBlockersStub()
	m := acceptPicker(t, stub, blockedSession())
	m, cmd := step(t, m, keyPress('e'))
	m, _ = step(t, m, cmd())
	m, _ = step(t, m, specialKeyPress(tea.KeyEscape))
	if m.acceptBlocker.Active() || m.Cancelled() || len(stub.accepts) != 0 {
		t.Fatal("esc in the picker must close the flow only")
	}
}
