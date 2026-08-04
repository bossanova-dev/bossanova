package server

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/status"
)

// waitingChatListFake serves a fixed, ordered chat list for one session. The
// order matters: the waiting-reason winner must be picked from an ordered slice,
// never from Go map iteration.
type waitingChatListFake struct {
	db.AgentChatStore
	chats []*models.AgentChat
}

func (f *waitingChatListFake) ListBySession(_ context.Context, _ string) ([]*models.AgentChat, error) {
	return f.chats, nil
}

const waitingTestReason = "awaiting checks_passed_ready on acme/widget#123"

// newWaitingStatusServer wires the minimum Server surface the chat/session
// status RPCs need, with the given chats all reported WORKING.
func newWaitingStatusServer(t *testing.T, agentSessionIDs ...string) (*Server, *status.Tracker) {
	t.Helper()
	chats := make([]*models.AgentChat, 0, len(agentSessionIDs))
	tracker := status.NewTracker()
	for _, id := range agentSessionIDs {
		chats = append(chats, &models.AgentChat{AgentSessionID: id, SessionID: "sess-1"})
		tracker.Update(id, pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())
	}
	return &Server{
		chatStatus: tracker,
		agentChats: &waitingChatListFake{chats: chats},
	}, tracker
}

func chatEntryByID(statuses []*pb.ChatStatusEntry, id string) *pb.ChatStatusEntry {
	for _, e := range statuses {
		if e.GetAgentSessionId() == id {
			return e
		}
	}
	return nil
}

// A chat the derivation layer marked as parked must be SERVED as waiting, with
// the canonical reason attached — the marker is useless if the RPC keeps
// reporting the raw WORKING heartbeat.
func TestGetChatStatuses_ArmedCallbackChatReportsWaiting(t *testing.T) {
	s, tracker := newWaitingStatusServer(t, "agent-a")
	tracker.SetWaiting("agent-a", waitingTestReason)

	resp, err := s.GetChatStatuses(context.Background(), connect.NewRequest(&pb.GetChatStatusesRequest{
		SessionId: "sess-1",
	}))
	if err != nil {
		t.Fatalf("GetChatStatuses: %v", err)
	}
	entry := chatEntryByID(resp.Msg.GetStatuses(), "agent-a")
	if entry == nil {
		t.Fatal("no status entry for agent-a")
	}
	if got := entry.GetStatus(); got != pb.ChatStatus_CHAT_STATUS_WAITING {
		t.Fatalf("status = %v, want WAITING", got)
	}
	if got := entry.GetWaitingReason(); got != waitingTestReason {
		t.Fatalf("waiting_reason = %q, want %q", got, waitingTestReason)
	}
}

// Over-reporting waiting is the failure mode to guard against: with no marker,
// a working chat stays working and carries no reason.
func TestGetChatStatuses_NoMarkerStaysWorking(t *testing.T) {
	s, _ := newWaitingStatusServer(t, "agent-a")

	resp, err := s.GetChatStatuses(context.Background(), connect.NewRequest(&pb.GetChatStatusesRequest{
		SessionId: "sess-1",
	}))
	if err != nil {
		t.Fatalf("GetChatStatuses: %v", err)
	}
	entry := chatEntryByID(resp.Msg.GetStatuses(), "agent-a")
	if entry == nil {
		t.Fatal("no status entry for agent-a")
	}
	if got := entry.GetStatus(); got != pb.ChatStatus_CHAT_STATUS_WORKING {
		t.Fatalf("status = %v, want WORKING", got)
	}
	if got := entry.GetWaitingReason(); got != "" {
		t.Fatalf("waiting_reason = %q, want empty", got)
	}
}

// A stale marker on a chat that is no longer working must not resurrect the
// waiting state: the reported status is the gate.
func TestGetChatStatuses_MarkerOnNonWorkingChatIsIgnored(t *testing.T) {
	s, tracker := newWaitingStatusServer(t, "agent-a")
	tracker.SetWaiting("agent-a", waitingTestReason)
	tracker.Update("agent-a", pb.ChatStatus_CHAT_STATUS_QUESTION, time.Now())

	resp, err := s.GetChatStatuses(context.Background(), connect.NewRequest(&pb.GetChatStatusesRequest{
		SessionId: "sess-1",
	}))
	if err != nil {
		t.Fatalf("GetChatStatuses: %v", err)
	}
	entry := chatEntryByID(resp.Msg.GetStatuses(), "agent-a")
	if entry == nil {
		t.Fatal("no status entry for agent-a")
	}
	if got := entry.GetStatus(); got != pb.ChatStatus_CHAT_STATUS_QUESTION {
		t.Fatalf("status = %v, want QUESTION", got)
	}
	if got := entry.GetWaitingReason(); got != "" {
		t.Fatalf("waiting_reason = %q, want empty", got)
	}
}

func TestGetSessionStatuses_SurfacesWaitingReason(t *testing.T) {
	s, tracker := newWaitingStatusServer(t, "agent-a")
	tracker.SetWaiting("agent-a", waitingTestReason)

	resp, err := s.GetSessionStatuses(context.Background(), connect.NewRequest(&pb.GetSessionStatusesRequest{
		SessionIds: []string{"sess-1"},
	}))
	if err != nil {
		t.Fatalf("GetSessionStatuses: %v", err)
	}
	if len(resp.Msg.GetStatuses()) != 1 {
		t.Fatalf("got %d session statuses, want 1", len(resp.Msg.GetStatuses()))
	}
	entry := resp.Msg.GetStatuses()[0]
	if got := entry.GetStatus(); got != pb.ChatStatus_CHAT_STATUS_WAITING {
		t.Fatalf("status = %v, want WAITING", got)
	}
	if got := entry.GetWaitingReason(); got != waitingTestReason {
		t.Fatalf("waiting_reason = %q, want %q", got, waitingTestReason)
	}
}

// The session aggregate keeps a session with real work in it honest: one parked
// chat alongside one genuinely working chat is a working session, and it must
// not inherit the parked chat's reason.
func TestChatStatusFromSessionChats_WorkingSiblingBeatsWaiting(t *testing.T) {
	s, tracker := newWaitingStatusServer(t, "agent-parked", "agent-busy")
	tracker.SetWaiting("agent-parked", waitingTestReason)

	got, reason, ok := s.chatStatusFromSessionChats(context.Background(),
		s.agentChats.(*waitingChatListFake).chats, true)
	if !ok {
		t.Fatal("chatStatusFromSessionChats reported not-loaded")
	}
	if got != pb.ChatStatus_CHAT_STATUS_WORKING {
		t.Fatalf("status = %v, want WORKING", got)
	}
	if reason != "" {
		t.Fatalf("reason = %q, want empty (the session is working, not waiting)", reason)
	}
}

// Waiting outranks idle: a parked chat is more informative than a sibling that
// is merely sitting at a prompt.
func TestChatStatusFromSessionChats_WaitingBeatsIdle(t *testing.T) {
	s, tracker := newWaitingStatusServer(t, "agent-parked", "agent-idle")
	tracker.SetWaiting("agent-parked", waitingTestReason)
	tracker.Update("agent-idle", pb.ChatStatus_CHAT_STATUS_IDLE, time.Now())

	got, reason, ok := s.chatStatusFromSessionChats(context.Background(),
		s.agentChats.(*waitingChatListFake).chats, true)
	if !ok {
		t.Fatal("chatStatusFromSessionChats reported not-loaded")
	}
	if got != pb.ChatStatus_CHAT_STATUS_WAITING {
		t.Fatalf("status = %v, want WAITING", got)
	}
	if reason != waitingTestReason {
		t.Fatalf("reason = %q, want %q", reason, waitingTestReason)
	}
}

// With two equally-parked chats the surfaced reason must be a function of the
// data, not of map iteration order: run it repeatedly and require one answer.
func TestChatStatusFromSessionChats_DeterministicReasonAcrossParkedChats(t *testing.T) {
	const otherReason = "awaiting merged on acme/widget#7"
	s, tracker := newWaitingStatusServer(t, "agent-b", "agent-a")
	tracker.SetWaiting("agent-b", otherReason)
	tracker.SetWaiting("agent-a", waitingTestReason)

	chats := s.agentChats.(*waitingChatListFake).chats
	for i := 0; i < 50; i++ {
		got, reason, ok := s.chatStatusFromSessionChats(context.Background(), chats, true)
		if !ok {
			t.Fatal("chatStatusFromSessionChats reported not-loaded")
		}
		if got != pb.ChatStatus_CHAT_STATUS_WAITING {
			t.Fatalf("status = %v, want WAITING", got)
		}
		// agent-a sorts first, so its reason is the deterministic winner even
		// though agent-b comes first in the chat list.
		if reason != waitingTestReason {
			t.Fatalf("iteration %d: reason = %q, want %q", i, reason, waitingTestReason)
		}
	}
}

// The pre-existing ladder is unchanged by the new rung.
func TestChatStatusFromSessionChats_QuestionAndLimitedStillBeatWaiting(t *testing.T) {
	for _, tc := range []struct {
		name  string
		other pb.ChatStatus
	}{
		{"question", pb.ChatStatus_CHAT_STATUS_QUESTION},
		{"limited", pb.ChatStatus_CHAT_STATUS_LIMITED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, tracker := newWaitingStatusServer(t, "agent-parked", "agent-other")
			tracker.SetWaiting("agent-parked", waitingTestReason)
			tracker.Update("agent-other", tc.other, time.Now())

			got, reason, ok := s.chatStatusFromSessionChats(context.Background(),
				s.agentChats.(*waitingChatListFake).chats, true)
			if !ok {
				t.Fatal("chatStatusFromSessionChats reported not-loaded")
			}
			if got != tc.other {
				t.Fatalf("status = %v, want %v", got, tc.other)
			}
			if reason != "" {
				t.Fatalf("reason = %q, want empty", reason)
			}
		})
	}
}
