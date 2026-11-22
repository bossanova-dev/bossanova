package server

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/domain"
)

// menuAgentClient reads a transcript and the question a pane's menu shows.
type menuAgentClient struct {
	transcriptAgentClient
	menu *pb.ChatQuestion
	read []string
}

func (c *menuAgentClient) ReadQuestionMenu(_ context.Context, req *pb.ReadQuestionMenuRequest) (*pb.ReadQuestionMenuResponse, error) {
	c.read = append(c.read, string(req.GetPaneContent()))
	return &pb.ReadQuestionMenuResponse{Question: c.menu}, nil
}

var pickColour = &pb.ChatQuestion{Id: "menu-colour", Text: "Pick a colour", Options: []string{"Red", "Green"}, IsAnswerable: true}

func readWithMenu(t *testing.T, recorded []*pb.ChatMessage, menu *pb.ChatQuestion, pane *fakeChatPane, activity bool) ([]*pb.ChatMessage, *menuAgentClient) {
	t.Helper()
	client := &menuAgentClient{
		transcriptAgentClient: transcriptAgentClient{resp: &pb.ReadTranscriptResponse{Messages: recorded, Exists: true}},
		menu:                  menu,
	}
	s := &Server{
		agentChats:   &chatStoreFake{chat: &domain.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1", AgentName: "claude"}},
		sessions:     &sessionStoreFake{sess: &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}},
		agentClients: map[string]agent.RunnerClient{"claude": client},
		chatTmux:     pane,
	}
	resp, err := s.GetChatTranscript(t.Context(), connect.NewRequest(&pb.GetChatTranscriptRequest{AgentSessionId: "agent-1", ShouldIncludeActivity: activity}))
	if err != nil {
		t.Fatalf("GetChatTranscript: %v", err)
	}
	return resp.Msg.GetMessages(), client
}

func TestGetChatTranscript_EndsOnTheQuestionTheMenuShows(t *testing.T) {
	recorded := []*pb.ChatMessage{{Role: "user", Kind: "text", Text: "pick one"}}
	got, client := readWithMenu(t, recorded, pickColour, &fakeChatPane{live: true, pane: "the menu"}, true)
	if len(got) != 2 {
		t.Fatalf("%d messages, want the transcript then the question", len(got))
	}
	last := got[1]
	if last.GetKind() != "question" || last.GetRole() != "assistant" || last.GetQuestion().GetId() != "menu-colour" {
		t.Fatalf("last message = %v, want the menu's question", last)
	}
	if len(client.read) != 1 || client.read[0] != "the menu" {
		t.Fatalf("read panes %q, want the chat's captured pane", client.read)
	}
}

func TestGetChatTranscript_LeavesTheTranscriptAloneWithoutAWaitingMenu(t *testing.T) {
	recorded := []*pb.ChatMessage{{Role: "user", Kind: "text", Text: "pick one"}}
	asked := []*pb.ChatMessage{recorded[0], {Role: "assistant", Kind: "question", Text: "Pick a colour", Question: pickColour}}
	cases := []struct {
		name     string
		recorded []*pb.ChatMessage
		menu     *pb.ChatQuestion
		pane     *fakeChatPane
		activity bool
		want     int
	}{
		{"no menu on screen", recorded, nil, &fakeChatPane{live: true}, true, 1},
		{"the chat is not running", recorded, pickColour, &fakeChatPane{live: false}, true, 1},
		{"a flat read", recorded, pickColour, &fakeChatPane{live: true}, false, 1},
		{"the transcript already ends on it", asked, pickColour, &fakeChatPane{live: true}, true, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := readWithMenu(t, tc.recorded, tc.menu, tc.pane, tc.activity)
			if len(got) != tc.want {
				t.Fatalf("%d messages, want %d", len(got), tc.want)
			}
		})
	}
}
