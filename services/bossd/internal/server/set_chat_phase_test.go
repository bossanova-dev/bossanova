package server

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
)

type phaseChatStore struct{ waitingChatListFake }

func (f *phaseChatStore) GetByAgentSessionID(_ context.Context, id string) (*models.AgentChat, error) {
	for _, chat := range f.chats {
		if chat.AgentSessionID == id {
			return chat, nil
		}
	}
	return nil, db.ErrAgentChatNotFound
}

func TestSetChatPhase(t *testing.T) {
	s, tracker := newWaitingStatusServer(t, "agent-a", "agent-b")
	s.agentChats = &phaseChatStore{*s.agentChats.(*waitingChatListFake)}
	ctx := context.Background()
	set := func(chat, session, phase string) error {
		_, err := s.SetChatPhase(ctx, connect.NewRequest(&pb.SetChatPhaseRequest{AgentSessionId: chat, SessionId: session, Phase: phase}))
		return err
	}
	assertRead := func(want string) {
		t.Helper()
		chats, err := s.GetChatStatuses(ctx, connect.NewRequest(&pb.GetChatStatusesRequest{SessionId: "sess-1"}))
		if err != nil {
			t.Fatal(err)
		}
		if got := chatEntryByID(chats.Msg.Statuses, "agent-b").GetPhase(); got != want {
			t.Fatalf("chat phase = %q, want %q", got, want)
		}
		sessions, err := s.GetSessionStatuses(ctx, connect.NewRequest(&pb.GetSessionStatusesRequest{SessionIds: []string{"sess-1"}}))
		if err != nil {
			t.Fatal(err)
		}
		if got := sessions.Msg.Statuses[0].GetPhase(); got != want {
			t.Fatalf("session phase = %q, want %q", got, want)
		}
	}
	if err := set("agent-b", "sess-1", " reviewing "); err != nil {
		t.Fatal(err)
	}
	assertRead("reviewing") // Higher-id chat is the only one with a phase.
	tracker.Update("agent-b", pb.ChatStatus_CHAT_STATUS_QUESTION, time.Now())
	assertRead("")
	tracker.Update("agent-b", pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())
	assertRead("reviewing")
	tracker.SetWaiting("agent-b", waitingTestReason)
	assertRead("")
	tracker.SetWaiting("agent-b", "")
	assertRead("reviewing")
	tracker.Update("agent-b", pb.ChatStatus_CHAT_STATUS_IDLE, time.Now())
	assertRead("")
	tracker.Update("agent-b", pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())
	if err := set("agent-b", "", "reviewing"); err != nil {
		t.Fatal(err)
	}
	if err := set("agent-b", "", ""); err != nil {
		t.Fatal(err)
	}
	assertRead("")
	for _, tc := range []struct {
		chat, session, phase string
		code                 connect.Code
	}{
		{"", "", "reviewing", connect.CodeInvalidArgument},
		{"missing", "", "reviewing", connect.CodeNotFound},
		{"agent-b", "other", "reviewing", connect.CodeNotFound},
		{"agent-b", "", "bad\x1b[31m", connect.CodeInvalidArgument},
		{"agent-b", "", "   ", connect.CodeInvalidArgument},
	} {
		if err := set(tc.chat, tc.session, tc.phase); connect.CodeOf(err) != tc.code {
			t.Errorf("set %+v: %v", tc, err)
		}
		if tracker.Phase("agent-b") != "" {
			t.Fatal("rejected phase stored")
		}
	}
	if err := set("agent-a", "", "building"); err != nil {
		t.Fatal(err)
	}
	if err := set("agent-b", "", "reviewing"); err != nil {
		t.Fatal(err)
	}
	sessions, err := s.GetSessionStatuses(ctx, connect.NewRequest(&pb.GetSessionStatusesRequest{SessionIds: []string{"sess-1"}}))
	if err != nil || sessions.Msg.Statuses[0].GetPhase() != "building" {
		t.Fatalf("deterministic winner: %v, %v", sessions, err)
	}
}
