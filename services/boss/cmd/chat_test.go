package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type chatTargetClient struct {
	session *pb.Session
	err     error
}

func (c chatTargetClient) GetSession(context.Context, string) (*pb.Session, error) {
	return c.session, c.err
}

func TestResolveChatTarget(t *testing.T) {
	t.Run("session id resolves to primary chat", func(t *testing.T) {
		agentSessionID := "agent-123"
		got, err := resolveChatTarget(context.Background(), chatTargetClient{
			session: &pb.Session{Id: "sess-123", AgentSessionId: &agentSessionID},
		}, "sess-123")
		if err != nil {
			t.Fatalf("resolveChatTarget: %v", err)
		}
		if got.SessionID != "sess-123" || got.AgentSessionID != agentSessionID {
			t.Fatalf("target = %+v, want session/chat ids", got)
		}
	})

	t.Run("not found means target is already a chat id", func(t *testing.T) {
		got, err := resolveChatTarget(context.Background(), chatTargetClient{
			err: connect.NewError(connect.CodeNotFound, errors.New("session not found")),
		}, "agent-123")
		if err != nil {
			t.Fatalf("resolveChatTarget: %v", err)
		}
		if got.SessionID != "" || got.AgentSessionID != "agent-123" {
			t.Fatalf("target = %+v, want chat id passthrough", got)
		}
	})

	t.Run("session without primary chat is an error", func(t *testing.T) {
		_, err := resolveChatTarget(context.Background(), chatTargetClient{
			session: &pb.Session{Id: "sess-empty"},
		}, "sess-empty")
		if err == nil || !strings.Contains(err.Error(), "no primary chat id") {
			t.Fatalf("error = %v, want no primary chat id", err)
		}
	})

	t.Run("transport error is not mistaken for chat id", func(t *testing.T) {
		_, err := resolveChatTarget(context.Background(), chatTargetClient{
			err: errors.New("dial unix: connection refused"),
		}, "agent-123")
		if err == nil || !strings.Contains(err.Error(), "resolve chat target") {
			t.Fatalf("error = %v, want resolve chat target", err)
		}
	})
}

type chatWaitClient struct {
	statusSessionID string
	statuses        []*pb.ChatStatusEntry
	statusErr       error
	transcriptReqs  []*pb.GetChatTranscriptRequest
	transcript      *pb.GetChatTranscriptResponse
	transcriptErr   error
}

func (c *chatWaitClient) GetChatStatuses(_ context.Context, sessionID string) ([]*pb.ChatStatusEntry, error) {
	c.statusSessionID = sessionID
	return c.statuses, c.statusErr
}

func (c *chatWaitClient) GetChatTranscript(_ context.Context, req *pb.GetChatTranscriptRequest) (*pb.GetChatTranscriptResponse, error) {
	c.transcriptReqs = append(c.transcriptReqs, req)
	if c.transcript != nil {
		return c.transcript, c.transcriptErr
	}
	return &pb.GetChatTranscriptResponse{}, c.transcriptErr
}

func TestChatWaitTickUsesScopedStatusAndBaseline(t *testing.T) {
	t.Run("polls session scoped status", func(t *testing.T) {
		c := &chatWaitClient{
			statuses: []*pb.ChatStatusEntry{{AgentSessionId: "agent-123", Status: pb.ChatStatus_CHAT_STATUS_WORKING}},
		}
		done, _, err := chatWaitTick(context.Background(), c, chatTarget{SessionID: "sess-123", AgentSessionID: "agent-123"}, "old", 0)
		if err != nil {
			t.Fatalf("chatWaitTick: %v", err)
		}
		if done {
			t.Fatal("done = true, want false for working status")
		}
		if c.statusSessionID != "sess-123" {
			t.Fatalf("status session id = %q, want sess-123", c.statusSessionID)
		}
		if len(c.transcriptReqs) != 0 {
			t.Fatalf("transcript requests = %d, want 0 while working", len(c.transcriptReqs))
		}
	})

	t.Run("does not return stale baseline on first tick", func(t *testing.T) {
		c := &chatWaitClient{
			transcript: &pb.GetChatTranscriptResponse{Exists: true, FinalAssistantText: "old"},
		}
		done, _, err := chatWaitTick(context.Background(), c, chatTarget{AgentSessionID: "agent-123"}, "old", 0)
		if err != nil {
			t.Fatalf("chatWaitTick: %v", err)
		}
		if done {
			t.Fatal("done = true, want false for unchanged baseline on first tick")
		}
	})

	t.Run("changed final result completes", func(t *testing.T) {
		c := &chatWaitClient{
			statuses:   []*pb.ChatStatusEntry{{AgentSessionId: "agent-123", Status: pb.ChatStatus_CHAT_STATUS_IDLE}},
			transcript: &pb.GetChatTranscriptResponse{Exists: true, FinalAssistantText: "new"},
		}
		done, result, err := chatWaitTick(context.Background(), c, chatTarget{SessionID: "sess-123", AgentSessionID: "agent-123"}, "old", 0)
		if err != nil {
			t.Fatalf("chatWaitTick: %v", err)
		}
		if !done || result != "new" {
			t.Fatalf("done/result = %v/%q, want true/new", done, result)
		}
		if len(c.transcriptReqs) != 1 || c.transcriptReqs[0].GetSessionId() != "sess-123" || c.transcriptReqs[0].GetAgentSessionId() != "agent-123" {
			t.Fatalf("transcript request not scoped: %+v", c.transcriptReqs)
		}
	})
}
