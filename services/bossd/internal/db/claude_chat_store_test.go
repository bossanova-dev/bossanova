package db

import (
	"context"
	"testing"
	"time"
)

func TestClaudeChatStore_CRUD(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	chatStore := NewClaudeChatStore(db)
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
	chat, err := chatStore.Create(ctx, CreateClaudeChatParams{
		SessionID: sess.ID,
		ClaudeID:  "claude-abc-123",
		Title:     "Initial chat",
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
	if chat.ClaudeID != "claude-abc-123" {
		t.Errorf("claude_id = %q, want %q", chat.ClaudeID, "claude-abc-123")
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
	if chats[0].ClaudeID != "claude-abc-123" {
		t.Errorf("listed claude_id = %q, want %q", chats[0].ClaudeID, "claude-abc-123")
	}

	// UpdateTitle
	if err := chatStore.UpdateTitle(ctx, chat.ID, "Updated title"); err != nil {
		t.Fatalf("update title: %v", err)
	}
	chats, _ = chatStore.ListBySession(ctx, sess.ID)
	if chats[0].Title != "Updated title" {
		t.Errorf("title after update = %q, want %q", chats[0].Title, "Updated title")
	}

	// UpdateTitleByClaudeID
	if err := chatStore.UpdateTitleByClaudeID(ctx, "claude-abc-123", "Title by claude ID"); err != nil {
		t.Fatalf("update title by claude ID: %v", err)
	}
	chats, _ = chatStore.ListBySession(ctx, sess.ID)
	if chats[0].Title != "Title by claude ID" {
		t.Errorf("title after update by claude ID = %q, want %q", chats[0].Title, "Title by claude ID")
	}
}

func TestClaudeChatStore_ListBySession_Ordering(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	chatStore := NewClaudeChatStore(db)
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
	_, err := chatStore.Create(ctx, CreateClaudeChatParams{
		SessionID: sess.ID, ClaudeID: "first", Title: "First",
	})
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	_, err = chatStore.Create(ctx, CreateClaudeChatParams{
		SessionID: sess.ID, ClaudeID: "second", Title: "Second",
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
	if chats[0].ClaudeID != "second" {
		t.Errorf("first result claude_id = %q, want %q (descending order)", chats[0].ClaudeID, "second")
	}
	if chats[1].ClaudeID != "first" {
		t.Errorf("second result claude_id = %q, want %q (descending order)", chats[1].ClaudeID, "first")
	}
}

func TestClaudeChatStore_FKCascade_DeleteSession(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	chatStore := NewClaudeChatStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess, _ := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "FK cascade chat test",
		WorktreePath: "/tmp/wt/fk-chat",
		BranchName:   "feat/fk-chat",
		BaseBranch:   "main",
	})
	_, err := chatStore.Create(ctx, CreateClaudeChatParams{
		SessionID: sess.ID, ClaudeID: "cascade-test", Title: "Will be deleted",
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

func TestClaudeChatStore_DeleteByClaudeID(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	chatStore := NewClaudeChatStore(db)
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
	_, err = chatStore.Create(ctx, CreateClaudeChatParams{
		SessionID: sess.ID, ClaudeID: "keep-me", Title: "Keeper",
	})
	if err != nil {
		t.Fatalf("create chat 1: %v", err)
	}
	_, err = chatStore.Create(ctx, CreateClaudeChatParams{
		SessionID: sess.ID, ClaudeID: "delete-me", Title: "Orphan",
	})
	if err != nil {
		t.Fatalf("create chat 2: %v", err)
	}

	// Delete one by claude_id.
	if err := chatStore.DeleteByClaudeID(ctx, "delete-me"); err != nil {
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
	if chats[0].ClaudeID != "keep-me" {
		t.Errorf("remaining chat claude_id = %q, want %q", chats[0].ClaudeID, "keep-me")
	}

	// Deleting a non-existent claude_id should not error.
	if err := chatStore.DeleteByClaudeID(ctx, "nonexistent"); err != nil {
		t.Errorf("delete non-existent should not error, got: %v", err)
	}
}

func TestClaudeChatStore_ListBySession_Empty(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	chatStore := NewClaudeChatStore(db)
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
