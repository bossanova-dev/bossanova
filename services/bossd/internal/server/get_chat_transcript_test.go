package server

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/agent"
)

// transcriptAgentClient is a fake AgentRunnerClient for GetChatTranscript tests.
// Only ReadTranscript is overridden; everything else panics via the embedded nil.
// When respByID is non-nil, ReadTranscript returns the response keyed by the
// request's AgentSessionId (a missing key yields Exists:false), enabling a
// miss-on-primary / hit-on-provider case. Otherwise it returns resp for every
// call. calls counts invocations so tests can assert the claude path takes
// exactly one ReadTranscript call (no provider retry).
type transcriptAgentClient struct {
	agent.AgentRunnerClient
	resp     *pb.ReadTranscriptResponse
	respByID map[string]*pb.ReadTranscriptResponse
	err      error
	lastReq  *pb.ReadTranscriptRequest
	calls    int
}

func (c *transcriptAgentClient) ReadTranscript(_ context.Context, req *pb.ReadTranscriptRequest) (*pb.ReadTranscriptResponse, error) {
	c.lastReq = req
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	if c.respByID != nil {
		if r, ok := c.respByID[req.GetAgentSessionId()]; ok {
			return r, nil
		}
		return &pb.ReadTranscriptResponse{Exists: false}, nil
	}
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

func TestGetChatTranscript_FallsBackToSessionWhenNoChatRow(t *testing.T) {
	workDir := t.TempDir()
	sess := &models.Session{
		ID:             "s1",
		RepoID:         "r1",
		WorktreePath:   workDir,
		AgentName:      "codex",
		AgentSessionID: strPtr("agent-1"),
	}
	fakeClient := &transcriptAgentClient{
		resp: &pb.ReadTranscriptResponse{
			Messages:           []*pb.ChatMessage{{Role: "assistant", Text: "GPT-5"}},
			FinalAssistantText: "GPT-5",
			Exists:             true,
		},
	}
	s := &Server{
		agentChats:   &chatStoreFake{chat: nil}, // GetByAgentSessionID -> sql.ErrNoRows
		sessions:     &sessionStoreFake{sess: sess},
		agentClients: map[string]agent.AgentRunnerClient{"codex": fakeClient},
	}

	resp, err := s.GetChatTranscript(context.Background(), connect.NewRequest(&pb.GetChatTranscriptRequest{
		SessionId:      "s1",
		AgentSessionId: "agent-1",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Msg.GetExists() || resp.Msg.GetFinalAssistantText() != "GPT-5" {
		t.Fatalf("unexpected response: %+v", resp.Msg)
	}
	if fakeClient.lastReq.GetWorkDir() != workDir {
		t.Errorf("WorkDir = %q, want %q", fakeClient.lastReq.GetWorkDir(), workDir)
	}
	if fakeClient.lastReq.GetAgentSessionId() != "agent-1" {
		t.Errorf("AgentSessionId = %q, want agent-1", fakeClient.lastReq.GetAgentSessionId())
	}
}

func TestGetChatTranscript_NotFoundWithoutChatRowOrSessionID(t *testing.T) {
	s := &Server{
		agentChats:   &chatStoreFake{chat: nil},
		sessions:     &sessionStoreFake{sess: nil},
		agentClients: map[string]agent.AgentRunnerClient{},
	}
	_, err := s.GetChatTranscript(context.Background(), connect.NewRequest(&pb.GetChatTranscriptRequest{
		AgentSessionId: "agent-1", // no session_id
	}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("want CodeNotFound, got %v", err)
	}
}

func TestGetChatTranscript_ProviderSessionFallback(t *testing.T) {
	const (
		agentSessionID    = "agent-1"    // caller-supplied (record_chat) id
		providerSessionID = "rollout-99" // codex's own rollout UUID
	)
	msgs := []*pb.ChatMessage{{Role: "assistant", Text: "GPT-5"}}

	tests := []struct {
		name          string
		agentName     string // defaults to "codex" when empty
		providerID    *string
		respByID      map[string]*pb.ReadTranscriptResponse
		wantExists    bool
		wantFinal     string
		wantReasonSet bool
		wantReasonNo  string // reason must NOT contain this substring when set
		wantCalls     int
	}{
		{
			// miss_on_agent_session_id, hit_on_provider_session_id
			name:       "miss_on_agent_session_id then hit_on_provider_session_id",
			providerID: strPtr(providerSessionID),
			respByID: map[string]*pb.ReadTranscriptResponse{
				agentSessionID:    {Exists: false},
				providerSessionID: {Exists: true, FinalAssistantText: "GPT-5", Messages: msgs},
			},
			wantExists: true,
			wantFinal:  "GPT-5",
			wantCalls:  2, // primary miss + provider retry hit
		},
		{
			// claude: ids coincide (provider nil) => exactly one call, no retry.
			name:       "claude single call no retry",
			providerID: nil,
			respByID: map[string]*pb.ReadTranscriptResponse{
				agentSessionID: {Exists: true, FinalAssistantText: "hi", Messages: msgs},
			},
			wantExists: true,
			wantFinal:  "hi",
			wantCalls:  1,
		},
		{
			// Provider id present but rollout still absent on disk => reason set.
			name:       "provider retry still misses sets reason",
			providerID: strPtr(providerSessionID),
			respByID: map[string]*pb.ReadTranscriptResponse{
				agentSessionID:    {Exists: false},
				providerSessionID: {Exists: false},
			},
			wantExists:    false,
			wantReasonSet: true,
			wantCalls:     2,
		},
		{
			// Chat row exists but no provider id discovered yet => reason set,
			// single call (nothing to retry).
			name:       "no provider id yet sets reason",
			providerID: nil,
			respByID: map[string]*pb.ReadTranscriptResponse{
				agentSessionID: {Exists: false},
			},
			wantExists:    false,
			wantReasonSet: true,
			wantCalls:     1,
		},
		{
			// A claude chat (ids coincide, no provider id) that genuinely misses
			// must NOT be told about a "codex rollout" — the reason must stay
			// agent-accurate for non-codex agents.
			name:      "claude miss reason is agent-neutral",
			agentName: "claude",
			respByID: map[string]*pb.ReadTranscriptResponse{
				agentSessionID: {Exists: false},
			},
			wantExists:    false,
			wantReasonSet: true,
			wantReasonNo:  "codex",
			wantCalls:     1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workDir := t.TempDir()
			agentName := tt.agentName
			if agentName == "" {
				agentName = "codex"
			}
			chat := &models.AgentChat{
				ID:                "c1",
				AgentSessionID:    agentSessionID,
				ProviderSessionID: tt.providerID,
				SessionID:         "s1",
				AgentName:         agentName,
			}
			sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: workDir}
			fakeClient := &transcriptAgentClient{respByID: tt.respByID}
			s := &Server{
				agentChats:   &chatStoreFake{chat: chat},
				sessions:     &sessionStoreFake{sess: sess},
				agentClients: map[string]agent.AgentRunnerClient{agentName: fakeClient},
			}

			resp, err := s.GetChatTranscript(context.Background(), connect.NewRequest(&pb.GetChatTranscriptRequest{
				AgentSessionId: agentSessionID,
				MaxMessages:    5,
			}))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if resp.Msg.GetExists() != tt.wantExists {
				t.Errorf("Exists = %v, want %v", resp.Msg.GetExists(), tt.wantExists)
			}
			if tt.wantFinal != "" && resp.Msg.GetFinalAssistantText() != tt.wantFinal {
				t.Errorf("FinalAssistantText = %q, want %q", resp.Msg.GetFinalAssistantText(), tt.wantFinal)
			}
			if tt.wantReasonSet && resp.Msg.GetReason() == "" {
				t.Errorf("expected non-empty reason on miss, got empty")
			}
			if tt.wantReasonNo != "" && strings.Contains(resp.Msg.GetReason(), tt.wantReasonNo) {
				t.Errorf("reason %q must not contain %q", resp.Msg.GetReason(), tt.wantReasonNo)
			}
			if !tt.wantReasonSet && tt.wantExists && resp.Msg.GetReason() != "" {
				t.Errorf("expected empty reason on hit, got %q", resp.Msg.GetReason())
			}
			if fakeClient.calls != tt.wantCalls {
				t.Errorf("ReadTranscript calls = %d, want %d", fakeClient.calls, tt.wantCalls)
			}
		})
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
