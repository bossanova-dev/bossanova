package views

import (
	"context"
	"testing"

	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// perSessionPickerStub answers each read for the session it names, so a reply
// carries the identity of the picker that asked for it.
type perSessionPickerStub struct{ chatPickerStub }

func (s *perSessionPickerStub) GetSession(_ context.Context, id string, _ client.SessionReadOptions) (*pb.Session, error) {
	return &pb.Session{Id: id, Title: "title of " + id}, nil
}

func (s *perSessionPickerStub) ListChats(_ context.Context, id string) ([]*pb.AgentChat, error) {
	return []*pb.AgentChat{{SessionId: id, AgentSessionId: "chat-of-" + id, CreatedAt: timestamppb.Now()}}, nil
}

// TestChatPicker_DropsRepliesFetchedForAnotherSession pins the cross-session
// paint the 2026-10-16 delta incident raised. Every navigation builds a fresh
// picker, but the session, chat list and refresh replies still in flight from
// the previous picker used to land on the next one unconditionally, which put
// one repo's title, attention and chat rows on another repo's session, with
// the attention keys then acting on the wrong PR. Under --host every reply
// crosses ssh, which widens that window.
func TestChatPicker_DropsRepliesFetchedForAnotherSession(t *testing.T) {
	c := &perSessionPickerStub{}
	previous := NewChatPickerModel(c, context.Background(), "fresh-claim", "")
	current := NewChatPickerModel(c, context.Background(), "madverts", "")

	for name, reply := range map[string]any{
		"session": previous.fetchSession()(),
		"chats":   previous.listChats()(),
		"refresh": previous.refreshStatuses()(),
	} {
		updated, _ := current.Update(reply)
		got := updated.(ChatPickerModel)
		if got.session.GetId() == "fresh-claim" {
			t.Fatalf("%s reply from the fresh-claim picker installed its session on the madverts picker", name)
		}
		for _, chat := range got.chats {
			if chat.GetSessionId() == "fresh-claim" {
				t.Fatalf("%s reply from the fresh-claim picker listed chat %s on the madverts picker", name, chat.GetAgentSessionId())
			}
		}
	}

	updated, _ := current.Update(current.fetchSession()())
	if got := updated.(ChatPickerModel).session.GetId(); got != "madverts" {
		t.Fatalf("own session reply installed %q, want madverts", got)
	}
	updated, _ = current.Update(current.listChats()())
	chats := updated.(ChatPickerModel).chats
	if len(chats) != 1 || chats[0].GetAgentSessionId() != "chat-of-madverts" {
		t.Fatalf("own chats reply listed %v, want chat-of-madverts", chats)
	}
}
