package server

import (
	"context"
	"database/sql"
	"testing"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/agent"
)

// transcriptAgentClient is a fake AgentRunnerClient for GetChatTranscript tests.
// Only ReadTranscript is overridden; everything else panics via the embedded nil.
type transcriptAgentClient struct {
	agent.AgentRunnerClient
	resp    *pb.ReadTranscriptResponse
	err     error
	lastReq *pb.ReadTranscriptRequest
}

func (c *transcriptAgentClient) ReadTranscript(_ context.Context, req *pb.ReadTranscriptRequest) (*pb.ReadTranscriptResponse, error) {
	c.lastReq = req
	return c.resp, c.err
}

func TestGetChatTranscript_NotFound_UnknownChat(t *testing.T) {
	s := &Server{
		agentChats: &chatStoreFake{chat: nil},
		sessions:   &sessionStoreFake{sess: nil},
	}
	_, err := s.GetChatTranscript(context.Background(), connect.NewRequest(&pb.GetChatTranscriptRequest{
		AgentSessionId: "missing",
	}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("expected CodeNotFound, got %v", connect.CodeOf(err))
	}
}

func TestGetChatTranscript_NotFound_SessionMismatch(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1", AgentName: "claude"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	s := &Server{
		agentChats: &chatStoreFake{chat: chat},
		sessions:   &sessionStoreFake{sess: sess},
	}
	_, err := s.GetChatTranscript(context.Background(), connect.NewRequest(&pb.GetChatTranscriptRequest{
		AgentSessionId: "agent-1",
		SessionId:      "wrong-session",
	}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("expected CodeNotFound, got %v", connect.CodeOf(err))
	}
}

func TestGetChatTranscript_ForwardsAndReturnsPayload(t *testing.T) {
	workDir := t.TempDir()
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1", AgentName: "claude"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: workDir}

	wantMessages := []*pb.ChatMessage{
		{Role: "user", Text: "hello"},
		{Role: "assistant", Text: "hi there"},
	}
	fakeClient := &transcriptAgentClient{
		resp: &pb.ReadTranscriptResponse{
			Messages:           wantMessages,
			FinalAssistantText: "hi there",
			Exists:             true,
		},
	}

	s := &Server{
		agentChats:   &chatStoreFake{chat: chat},
		sessions:     &sessionStoreFake{sess: sess},
		agentClients: map[string]agent.AgentRunnerClient{"claude": fakeClient},
	}

	resp, err := s.GetChatTranscript(context.Background(), connect.NewRequest(&pb.GetChatTranscriptRequest{
		AgentSessionId: "agent-1",
		MaxMessages:    10,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify forwarded request fields.
	if fakeClient.lastReq == nil {
		t.Fatal("ReadTranscript was not called")
	}
	if fakeClient.lastReq.WorkDir != workDir {
		t.Errorf("WorkDir = %q, want %q", fakeClient.lastReq.WorkDir, workDir)
	}
	if fakeClient.lastReq.AgentSessionId != "agent-1" {
		t.Errorf("AgentSessionId = %q, want agent-1", fakeClient.lastReq.AgentSessionId)
	}
	if fakeClient.lastReq.MaxMessages != 10 {
		t.Errorf("MaxMessages = %d, want 10", fakeClient.lastReq.MaxMessages)
	}

	// Verify returned payload.
	if !resp.Msg.Exists {
		t.Error("Exists should be true")
	}
	if resp.Msg.FinalAssistantText != "hi there" {
		t.Errorf("FinalAssistantText = %q, want %q", resp.Msg.FinalAssistantText, "hi there")
	}
	if len(resp.Msg.Messages) != 2 {
		t.Fatalf("Messages len = %d, want 2", len(resp.Msg.Messages))
	}
	if resp.Msg.Messages[0].Text != "hello" || resp.Msg.Messages[1].Text != "hi there" {
		t.Errorf("Messages = %v", resp.Msg.Messages)
	}
}

func TestGetChatTranscript_ExistsFalseIsNotError(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1", AgentName: "claude"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	fakeClient := &transcriptAgentClient{
		resp: &pb.ReadTranscriptResponse{Exists: false},
	}
	s := &Server{
		agentChats:   &chatStoreFake{chat: chat},
		sessions:     &sessionStoreFake{sess: sess},
		agentClients: map[string]agent.AgentRunnerClient{"claude": fakeClient},
	}

	resp, err := s.GetChatTranscript(context.Background(), connect.NewRequest(&pb.GetChatTranscriptRequest{
		AgentSessionId: "agent-1",
	}))
	if err != nil {
		t.Fatalf("exists=false must not be an error, got: %v", err)
	}
	if resp.Msg.Exists {
		t.Error("Exists should be false")
	}
}

func TestGetChatTranscript_SessionScopeCheckPassesWhenMatch(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1", AgentName: "claude"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	fakeClient := &transcriptAgentClient{
		resp: &pb.ReadTranscriptResponse{Exists: true},
	}
	s := &Server{
		agentChats:   &chatStoreFake{chat: chat},
		sessions:     &sessionStoreFake{sess: sess},
		agentClients: map[string]agent.AgentRunnerClient{"claude": fakeClient},
	}

	_, err := s.GetChatTranscript(context.Background(), connect.NewRequest(&pb.GetChatTranscriptRequest{
		AgentSessionId: "agent-1",
		SessionId:      "s1", // matches
	}))
	if err != nil {
		t.Fatalf("matching session_id should succeed, got: %v", err)
	}
}

func TestGetChatTranscript_NotFound_SessionMissing(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1", AgentName: "claude"}
	s := &Server{
		agentChats: &chatStoreFake{chat: chat},
		sessions:   &sessionStoreFake{sess: nil}, // session missing
	}
	_, err := s.GetChatTranscript(context.Background(), connect.NewRequest(&pb.GetChatTranscriptRequest{
		AgentSessionId: "agent-1",
	}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("expected CodeNotFound, got %v", connect.CodeOf(err))
	}
}

// sessionStoreFake.Get returns sql.ErrNoRows when sess is nil.
// Verify it also handles this correctly for GetChatTranscript.
func TestGetChatTranscript_SessionStoreFakeReturnsNoRows(t *testing.T) {
	f := &sessionStoreFake{sess: nil}
	s, err := f.Get(context.Background(), "anything")
	if err != sql.ErrNoRows {
		t.Fatalf("expected sql.ErrNoRows, got %v", err)
	}
	if s != nil {
		t.Fatal("expected nil session")
	}
}
