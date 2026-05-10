package main

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
)

// fakeSessionStore satisfies db.SessionStore narrowly — only Get is wired.
// Returning sql.ErrNoRows when the requested ID is not the seeded session
// mirrors the production sqlite store's behavior on miss.
type fakeSessionStore struct {
	db.SessionStore
	byID map[string]*models.Session
}

func (f *fakeSessionStore) Get(_ context.Context, id string) (*models.Session, error) {
	if sess, ok := f.byID[id]; ok {
		return sess, nil
	}
	return nil, sql.ErrNoRows
}

// fakeAgentChatStore satisfies db.AgentChatStore narrowly — only
// GetByAgentSessionID is wired. The lookup closure under test only
// reaches for that method.
type fakeAgentChatStore struct {
	db.AgentChatStore
	byAgentSessionID map[string]*models.AgentChat
}

func (f *fakeAgentChatStore) GetByAgentSessionID(_ context.Context, id string) (*models.AgentChat, error) {
	if chat, ok := f.byAgentSessionID[id]; ok {
		return chat, nil
	}
	return nil, sql.ErrNoRows
}

func TestNewDispatcherLookup_PrefersSessionByBossID(t *testing.T) {
	sessions := &fakeSessionStore{byID: map[string]*models.Session{
		"sess-1": {ID: "sess-1", AgentName: "codex"},
	}}
	chats := &fakeAgentChatStore{}

	lookup := newDispatcherLookup(sessions, chats)
	got, err := lookup("sess-1")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got != "codex" {
		t.Errorf("lookup(sess-1) = %q, want %q", got, "codex")
	}
}

func TestNewDispatcherLookup_FallsBackToChatsByAgentSessionID(t *testing.T) {
	// Session miss → falls through to chats reverse index. This is the
	// regression guard for the I1 routing bug: liveness checker and the
	// interactive attach adapter pass agent-session-IDs (not bossd IDs)
	// to dispatcher.IsRunning/Subscribe/History.
	sessions := &fakeSessionStore{byID: map[string]*models.Session{}}
	chats := &fakeAgentChatStore{byAgentSessionID: map[string]*models.AgentChat{
		"agent-uuid-codex-7": {AgentSessionID: "agent-uuid-codex-7", AgentName: "codex"},
	}}

	lookup := newDispatcherLookup(sessions, chats)
	got, err := lookup("agent-uuid-codex-7")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got != "codex" {
		t.Errorf("lookup(agent-uuid-codex-7) = %q, want %q", got, "codex")
	}
}

func TestNewDispatcherLookup_BothMissReturnsError(t *testing.T) {
	lookup := newDispatcherLookup(&fakeSessionStore{}, &fakeAgentChatStore{})
	_, err := lookup("ghost-id")
	if err == nil {
		t.Fatalf("expected error for unknown id")
	}
	// Sanity-check the message references the input id so dispatcher
	// logs are debuggable.
	if !strings.Contains(err.Error(), "ghost-id") {
		t.Errorf("error %q should mention the missing id", err)
	}
}

func TestNewDispatcherLookup_NilChatsStoreSurvives(t *testing.T) {
	// Defensive: nil chat store must not panic — falls through to error.
	lookup := newDispatcherLookup(&fakeSessionStore{}, nil)
	if _, err := lookup("anything"); err == nil {
		t.Fatalf("expected error when both stores miss / nil")
	}
}
