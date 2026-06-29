package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

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
		done, _, err := chatWaitTick(context.Background(), c, chatTarget{SessionID: "sess-123", AgentSessionID: "agent-123"}, "old", false)
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

	t.Run("does not return stale baseline while transcript is unchanged", func(t *testing.T) {
		c := &chatWaitClient{
			transcript: &pb.GetChatTranscriptResponse{Exists: true, FinalAssistantText: "old"},
		}
		// Every tick (not just the first) must keep waiting while the transcript
		// still shows the pre-wait answer AND the grace window has not expired: a
		// still-running follow-up leaves the final text equal to the baseline,
		// and returning it would hand back the stale previous result.
		for tick := range 3 {
			done, _, err := chatWaitTick(context.Background(), c, chatTarget{AgentSessionID: "agent-123"}, "old", false)
			if err != nil {
				t.Fatalf("chatWaitTick tick %d: %v", tick, err)
			}
			if done {
				t.Fatalf("done = true on tick %d, want false for unchanged baseline", tick)
			}
		}
	})

	t.Run("returns baseline result once grace expires", func(t *testing.T) {
		// A chat that was already finished when wait began keeps its final text
		// equal to the baseline forever. Once the grace window expires it must
		// return that result instead of sleeping until --timeout.
		c := &chatWaitClient{
			statuses:   []*pb.ChatStatusEntry{{AgentSessionId: "agent-123", Status: pb.ChatStatus_CHAT_STATUS_IDLE}},
			transcript: &pb.GetChatTranscriptResponse{Exists: true, FinalAssistantText: "old"},
		}
		done, result, err := chatWaitTick(context.Background(), c, chatTarget{SessionID: "sess-123", AgentSessionID: "agent-123"}, "old", true)
		if err != nil {
			t.Fatalf("chatWaitTick: %v", err)
		}
		if !done || result != "old" {
			t.Fatalf("done/result = %v/%q, want true/old once grace expired", done, result)
		}
	})

	t.Run("returns finished result when timeout is shorter than grace", func(t *testing.T) {
		// `boss chat wait --timeout 5s` on an already-finished chat whose final
		// text equals the baseline must surface that result, not report a timeout
		// just because the 12s baseline grace window never elapsed.
		c := &chatWaitClient{
			statuses:   []*pb.ChatStatusEntry{{AgentSessionId: "agent-123", Status: pb.ChatStatus_CHAT_STATUS_IDLE}},
			transcript: &pb.GetChatTranscriptResponse{Exists: true, FinalAssistantText: "old"},
		}
		cmd := &cobra.Command{}
		var out bytes.Buffer
		cmd.SetOut(&out)
		if err := chatWaitTimeout(cmd, c, chatTarget{SessionID: "sess-123", AgentSessionID: "agent-123"}, "old", "agent-123", 5*time.Second); err != nil {
			t.Fatalf("chatWaitTimeout: %v", err)
		}
		if strings.TrimSpace(out.String()) != "old" {
			t.Fatalf("output = %q, want old", out.String())
		}
	})

	t.Run("still reports timeout while a follow-up is working", func(t *testing.T) {
		c := &chatWaitClient{
			statuses: []*pb.ChatStatusEntry{{AgentSessionId: "agent-123", Status: pb.ChatStatus_CHAT_STATUS_WORKING}},
		}
		cmd := &cobra.Command{}
		var out bytes.Buffer
		cmd.SetOut(&out)
		err := chatWaitTimeout(cmd, c, chatTarget{SessionID: "sess-123", AgentSessionID: "agent-123"}, "old", "agent-123", 5*time.Second)
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("error = %v, want timed out", err)
		}
		if out.Len() != 0 {
			t.Fatalf("output = %q, want empty while working", out.String())
		}
	})

	t.Run("changed final result completes", func(t *testing.T) {
		c := &chatWaitClient{
			statuses:   []*pb.ChatStatusEntry{{AgentSessionId: "agent-123", Status: pb.ChatStatus_CHAT_STATUS_IDLE}},
			transcript: &pb.GetChatTranscriptResponse{Exists: true, FinalAssistantText: "new"},
		}
		done, result, err := chatWaitTick(context.Background(), c, chatTarget{SessionID: "sess-123", AgentSessionID: "agent-123"}, "old", false)
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
