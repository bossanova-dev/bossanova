package scenario

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/adapter/realstackrpc"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestCompileChatRoles(t *testing.T) {
	for _, role := range []string{"", "build", "verify", "repair", "operator", "invalid"} {
		t.Run(role, func(t *testing.T) {
			doc := Document{Version: 1, Name: "roles", Repos: []Repo{{Key: "r", DisplayName: "Repo"}}, Sessions: []Session{{Key: "s", Repo: "r", Title: "Session", State: "READY_FOR_REVIEW", Chats: []Chat{{Key: "c", Title: "chat", Role: role}}}}}
			plan, problems := Compile(doc)
			if role == "invalid" {
				if len(problems) == 0 {
					t.Fatal("invalid role accepted")
				}
				return
			}
			if len(problems) > 0 {
				t.Fatal(problems)
			}
			want := role
			if want == "" {
				want = "operator"
			}
			if got := plan.Spec.Sessions[0].Chats[0].Role; got != want {
				t.Errorf("role=%q, want %q", got, want)
			}
		})
	}
}

func TestRecordChatsPreservesRoles(t *testing.T) {
	for _, tc := range []struct {
		role string
		want pb.ChatRole
	}{
		{"build", pb.ChatRole_CHAT_ROLE_BUILD},
		{"verify", pb.ChatRole_CHAT_ROLE_VERIFY},
		{"repair", pb.ChatRole_CHAT_ROLE_REPAIR},
		{"operator", pb.ChatRole_CHAT_ROLE_OPERATOR},
		{"", pb.ChatRole_CHAT_ROLE_OPERATOR},
	} {
		t.Run(tc.role, func(t *testing.T) {
			client := &scenarioChatRecorder{}
			doc := Document{Sessions: []Session{{Key: "s", Chats: []Chat{{Key: "c", Title: "stored title", Role: tc.role}}}}}
			st := &applyState{Keys: Keys{Sessions: map[string]string{"s": "session-id"}}}
			if err := recordChats(context.Background(), doc, client, st); err != nil {
				t.Fatal(err)
			}
			if len(client.requests) != 1 {
				t.Fatalf("recorded %d chats, want 1", len(client.requests))
			}
			got := client.requests[0]
			if got.Role != tc.want || got.SessionId != "session-id" || got.AgentSessionId != "session-id-c" || got.Title != "stored title" || got.GetAgentName() != "stub" {
				t.Fatalf("recorded request = %v, want role %v and original chat identity/title/agent", got, tc.want)
			}
		})
	}
}

type scenarioChatRecorder struct {
	realstackrpc.Client
	requests []*pb.RecordChatRequest
}

func (c *scenarioChatRecorder) RecordChat(_ context.Context, request *connect.Request[pb.RecordChatRequest]) (*connect.Response[pb.RecordChatResponse], error) {
	c.requests = append(c.requests, request.Msg)
	return connect.NewResponse(&pb.RecordChatResponse{}), nil
}
