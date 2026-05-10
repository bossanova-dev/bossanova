package server

import (
	"context"
	"database/sql"
	"sync"
	"testing"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
)

// recordChatStoreFake satisfies db.AgentChatStore for RecordChat's needs.
// GetByAgentSessionID always returns sql.ErrNoRows so the handler takes
// the Create branch (the path under test). Create stashes the params and
// synthesizes an AgentChat row that mirrors what SQLite would persist —
// crucially preserving the AgentName the handler passed through, since
// that's exactly what we're asserting on.
type recordChatStoreFake struct {
	db.AgentChatStore
	mu      sync.Mutex
	created *db.CreateAgentChatParams
}

func (f *recordChatStoreFake) GetByAgentSessionID(_ context.Context, _ string) (*models.AgentChat, error) {
	return nil, sql.ErrNoRows
}

func (f *recordChatStoreFake) Create(_ context.Context, params db.CreateAgentChatParams) (*models.AgentChat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := params
	f.created = &p
	return &models.AgentChat{
		ID:             "chat-1",
		SessionID:      params.SessionID,
		AgentSessionID: params.AgentSessionID,
		AgentName:      params.AgentName,
		Title:          params.Title,
	}, nil
}

func newRecordChatTestServer(sess *models.Session) (*Server, *recordChatStoreFake) {
	chats := &recordChatStoreFake{}
	return &Server{
		agentChats: chats,
		sessions:   &sessionStoreFake{sess: sess},
		// tmux left nil → ensureChatTmuxSession is a no-op, keeping the
		// test focused on the agent-name resolution rules.
	}, chats
}

// TestRecordChat_AgentNameOverridePreferred verifies that an explicit
// agent_name on RecordChatRequest takes precedence over the parent
// session's AgentName. Locks in the per-chat agent picker introduced
// for the codex routing work.
func TestRecordChat_AgentNameOverridePreferred(t *testing.T) {
	sess := &models.Session{ID: "s1", AgentName: "claude"}
	srv, chats := newRecordChatTestServer(sess)

	resp, err := srv.RecordChat(context.Background(), connect.NewRequest(&pb.RecordChatRequest{
		SessionId:      "s1",
		AgentSessionId: "agent-1",
		Title:          "hello",
		AgentName:      strPtr("codex"),
	}))
	if err != nil {
		t.Fatalf("RecordChat: %v", err)
	}
	if got := resp.Msg.GetChat().GetAgentName(); got != "codex" {
		t.Fatalf("response chat AgentName = %q, want %q", got, "codex")
	}
	if chats.created == nil {
		t.Fatalf("expected Create to be called")
	}
	if chats.created.AgentName != "codex" {
		t.Fatalf("Create params AgentName = %q, want %q (override should win over session)", chats.created.AgentName, "codex")
	}
}

// TestRecordChat_InheritsSessionAgentWhenUnset preserves the existing
// behavior: when the request omits AgentName, the chat row inherits
// the parent session's AgentName.
func TestRecordChat_InheritsSessionAgentWhenUnset(t *testing.T) {
	sess := &models.Session{ID: "s1", AgentName: "claude"}
	srv, chats := newRecordChatTestServer(sess)

	resp, err := srv.RecordChat(context.Background(), connect.NewRequest(&pb.RecordChatRequest{
		SessionId:      "s1",
		AgentSessionId: "agent-2",
		Title:          "hello",
		// AgentName intentionally unset.
	}))
	if err != nil {
		t.Fatalf("RecordChat: %v", err)
	}
	if got := resp.Msg.GetChat().GetAgentName(); got != "claude" {
		t.Fatalf("response chat AgentName = %q, want %q", got, "claude")
	}
	if chats.created.AgentName != "claude" {
		t.Fatalf("Create params AgentName = %q, want %q (should inherit from session)", chats.created.AgentName, "claude")
	}
}

// TestRecordChat_OverrideWinsEvenWhenSessionDiffers exercises the case
// where both the session and the override are non-empty but distinct.
// The override must win — this is the whole point of the field.
func TestRecordChat_OverrideWinsEvenWhenSessionDiffers(t *testing.T) {
	sess := &models.Session{ID: "s1", AgentName: "codex"}
	srv, chats := newRecordChatTestServer(sess)

	resp, err := srv.RecordChat(context.Background(), connect.NewRequest(&pb.RecordChatRequest{
		SessionId:      "s1",
		AgentSessionId: "agent-3",
		Title:          "hello",
		AgentName:      strPtr("claude"),
	}))
	if err != nil {
		t.Fatalf("RecordChat: %v", err)
	}
	if got := resp.Msg.GetChat().GetAgentName(); got != "claude" {
		t.Fatalf("response chat AgentName = %q, want %q (override should win)", got, "claude")
	}
	if chats.created.AgentName != "claude" {
		t.Fatalf("Create params AgentName = %q, want %q", chats.created.AgentName, "claude")
	}
}
