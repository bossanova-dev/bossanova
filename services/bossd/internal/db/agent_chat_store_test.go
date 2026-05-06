package db

import (
	"context"
	"testing"
	"time"
)

func TestAgentChatStore_CRUD(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	chatStore := NewAgentChatStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess, err := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "Chat CRUD test",
		WorktreePath: "/tmp/wt/chat",
		BranchName:   "feat/chat",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Create
	chat, err := chatStore.Create(ctx, CreateAgentChatParams{
		SessionID:      sess.ID,
		AgentSessionID: "claude-abc-123",
		Title:          "Initial chat",
	})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	if chat.ID == "" {
		t.Error("expected non-empty chat ID")
	}
	if chat.SessionID != sess.ID {
		t.Errorf("session_id = %q, want %q", chat.SessionID, sess.ID)
	}
	if chat.AgentSessionID != "claude-abc-123" {
		t.Errorf("claude_id = %q, want %q", chat.AgentSessionID, "claude-abc-123")
	}
	if chat.Title != "Initial chat" {
		t.Errorf("title = %q, want %q", chat.Title, "Initial chat")
	}

	// ListBySession
	chats, err := chatStore.ListBySession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("list by session: %v", err)
	}
	if len(chats) != 1 {
		t.Fatalf("expected 1 chat, got %d", len(chats))
	}
	if chats[0].AgentSessionID != "claude-abc-123" {
		t.Errorf("listed claude_id = %q, want %q", chats[0].AgentSessionID, "claude-abc-123")
	}

	// UpdateTitle
	if err := chatStore.UpdateTitle(ctx, chat.ID, "Updated title"); err != nil {
		t.Fatalf("update title: %v", err)
	}
	chats, _ = chatStore.ListBySession(ctx, sess.ID)
	if chats[0].Title != "Updated title" {
		t.Errorf("title after update = %q, want %q", chats[0].Title, "Updated title")
	}

	// UpdateTitleByAgentSessionID
	if err := chatStore.UpdateTitleByAgentSessionID(ctx, "claude-abc-123", "Title by claude ID"); err != nil {
		t.Fatalf("update title by claude ID: %v", err)
	}
	chats, _ = chatStore.ListBySession(ctx, sess.ID)
	if chats[0].Title != "Title by claude ID" {
		t.Errorf("title after update by claude ID = %q, want %q", chats[0].Title, "Title by claude ID")
	}
}

func TestAgentChatStore_ListBySession_Ordering(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	chatStore := NewAgentChatStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess, _ := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "Ordering test",
		WorktreePath: "/tmp/wt/order",
		BranchName:   "feat/order",
		BaseBranch:   "main",
	})

	// Create multiple chats with slight delay to ensure different timestamps.
	_, err := chatStore.Create(ctx, CreateAgentChatParams{
		SessionID: sess.ID, AgentSessionID: "first", Title: "First",
	})
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	_, err = chatStore.Create(ctx, CreateAgentChatParams{
		SessionID: sess.ID, AgentSessionID: "second", Title: "Second",
	})
	if err != nil {
		t.Fatalf("create second: %v", err)
	}

	chats, err := chatStore.ListBySession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(chats) != 2 {
		t.Fatalf("expected 2 chats, got %d", len(chats))
	}
	// Ordered by created_at DESC, so second should be first in list.
	if chats[0].AgentSessionID != "second" {
		t.Errorf("first result claude_id = %q, want %q (descending order)", chats[0].AgentSessionID, "second")
	}
	if chats[1].AgentSessionID != "first" {
		t.Errorf("second result claude_id = %q, want %q (descending order)", chats[1].AgentSessionID, "first")
	}
}

func TestAgentChatStore_FKCascade_DeleteSession(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	chatStore := NewAgentChatStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess, _ := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "FK cascade chat test",
		WorktreePath: "/tmp/wt/fk-chat",
		BranchName:   "feat/fk-chat",
		BaseBranch:   "main",
	})
	_, err := chatStore.Create(ctx, CreateAgentChatParams{
		SessionID: sess.ID, AgentSessionID: "cascade-test", Title: "Will be deleted",
	})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}

	// Delete session should cascade to chats.
	if err := sessionStore.Delete(ctx, sess.ID); err != nil {
		t.Fatalf("delete session: %v", err)
	}

	chats, _ := chatStore.ListBySession(ctx, sess.ID)
	if len(chats) != 0 {
		t.Errorf("chats should be deleted by cascade: got %d", len(chats))
	}
}

func TestAgentChatStore_DeleteByAgentSessionID(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	chatStore := NewAgentChatStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess, err := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "Delete by claude ID test",
		WorktreePath: "/tmp/wt/delete-claude",
		BranchName:   "feat/delete-claude",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Create two chats.
	_, err = chatStore.Create(ctx, CreateAgentChatParams{
		SessionID: sess.ID, AgentSessionID: "keep-me", Title: "Keeper",
	})
	if err != nil {
		t.Fatalf("create chat 1: %v", err)
	}
	_, err = chatStore.Create(ctx, CreateAgentChatParams{
		SessionID: sess.ID, AgentSessionID: "delete-me", Title: "Orphan",
	})
	if err != nil {
		t.Fatalf("create chat 2: %v", err)
	}

	// Delete one by claude_id.
	if err := chatStore.DeleteByAgentSessionID(ctx, "delete-me"); err != nil {
		t.Fatalf("delete by claude ID: %v", err)
	}

	// Only one should remain.
	chats, err := chatStore.ListBySession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(chats) != 1 {
		t.Fatalf("expected 1 chat after delete, got %d", len(chats))
	}
	if chats[0].AgentSessionID != "keep-me" {
		t.Errorf("remaining chat claude_id = %q, want %q", chats[0].AgentSessionID, "keep-me")
	}

	// Deleting a non-existent claude_id should not error.
	if err := chatStore.DeleteByAgentSessionID(ctx, "nonexistent"); err != nil {
		t.Errorf("delete non-existent should not error, got: %v", err)
	}
}

func TestAgentChatStore_ListBySession_Empty(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	chatStore := NewAgentChatStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess, _ := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "Empty list test",
		WorktreePath: "/tmp/wt/empty",
		BranchName:   "feat/empty",
		BaseBranch:   "main",
	})

	chats, err := chatStore.ListBySession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}
	if len(chats) != 0 {
		t.Errorf("expected 0 chats, got %d", len(chats))
	}
}

// TestAgentChatStore_AgentNameDefault verifies that creating a chat without
// specifying AgentName persists and reads back the "claude" default — the
// migration's NOT NULL DEFAULT 'claude' is the safety net for legacy callers
// that haven't been updated to pass the field yet.
func TestAgentChatStore_AgentNameDefault(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	chatStore := NewAgentChatStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess, err := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "agent_name default",
		WorktreePath: "/tmp/wt/agent-default",
		BranchName:   "feat/agent-default",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	chat, err := chatStore.Create(ctx, CreateAgentChatParams{
		SessionID:      sess.ID,
		AgentSessionID: "agent-default-1",
		Title:          "default agent",
	})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	if chat.AgentName != "claude" {
		t.Errorf("returned agent_name = %q, want %q", chat.AgentName, "claude")
	}

	// Read back via Get and List paths to confirm SELECT/scan is wired.
	got, err := chatStore.GetByAgentSessionID(ctx, "agent-default-1")
	if err != nil {
		t.Fatalf("get by agent_session_id: %v", err)
	}
	if got.AgentName != "claude" {
		t.Errorf("get agent_name = %q, want %q", got.AgentName, "claude")
	}

	listed, err := chatStore.ListBySession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("list by session: %v", err)
	}
	if len(listed) != 1 || listed[0].AgentName != "claude" {
		t.Errorf("list agent_name = %v, want one chat with %q", listed, "claude")
	}
}

// TestAgentChatStore_AgentNameExplicit verifies that an explicit AgentName
// round-trips through INSERT and SELECT unchanged.
func TestAgentChatStore_AgentNameExplicit(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	chatStore := NewAgentChatStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess, err := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "agent_name explicit",
		WorktreePath: "/tmp/wt/agent-explicit",
		BranchName:   "feat/agent-explicit",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	chat, err := chatStore.Create(ctx, CreateAgentChatParams{
		SessionID:      sess.ID,
		AgentSessionID: "agent-explicit-1",
		AgentName:      "opencode",
		Title:          "opencode chat",
	})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	if chat.AgentName != "opencode" {
		t.Errorf("returned agent_name = %q, want %q", chat.AgentName, "opencode")
	}

	got, err := chatStore.GetByAgentSessionID(ctx, "agent-explicit-1")
	if err != nil {
		t.Fatalf("get by agent_session_id: %v", err)
	}
	if got.AgentName != "opencode" {
		t.Errorf("get agent_name = %q, want %q", got.AgentName, "opencode")
	}
}

func TestAgentChatStore_ListWithTmuxSession(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	chatStore := NewAgentChatStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess, err := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "ListWithTmuxSession test",
		WorktreePath: "/tmp/wt/tmux-list",
		BranchName:   "feat/tmux-list",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Create chat without tmux session.
	_, err = chatStore.Create(ctx, CreateAgentChatParams{
		SessionID: sess.ID, AgentSessionID: "no-tmux", Title: "No tmux",
	})
	if err != nil {
		t.Fatalf("create chat without tmux: %v", err)
	}

	// Create chat with tmux session.
	_, err = chatStore.Create(ctx, CreateAgentChatParams{
		SessionID: sess.ID, AgentSessionID: "has-tmux", Title: "Has tmux",
	})
	if err != nil {
		t.Fatalf("create chat with tmux: %v", err)
	}
	tmuxName := "boss-test-session"
	if err := chatStore.UpdateTmuxSessionName(ctx, "has-tmux", &tmuxName); err != nil {
		t.Fatalf("set tmux session name: %v", err)
	}

	// ListWithTmuxSession should return only the chat with tmux session.
	chats, err := chatStore.ListWithTmuxSession(ctx)
	if err != nil {
		t.Fatalf("list with tmux session: %v", err)
	}
	if len(chats) != 1 {
		t.Fatalf("expected 1 chat with tmux session, got %d", len(chats))
	}
	if chats[0].AgentSessionID != "has-tmux" {
		t.Errorf("claude_id = %q, want %q", chats[0].AgentSessionID, "has-tmux")
	}
	if chats[0].TmuxSessionName == nil || *chats[0].TmuxSessionName != tmuxName {
		t.Errorf("tmux_session_name = %v, want %q", chats[0].TmuxSessionName, tmuxName)
	}
}
