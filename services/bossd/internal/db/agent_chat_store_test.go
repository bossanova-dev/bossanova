package db

import (
	"context"
	"errors"
	"strings"
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

// TestAgentChatStore_AccountIDRoundTrip pins the BOS-170 nullable account_id
// column on agent_chats: a chat created with an explicit account binding reads
// it back, and one created without a binding reads back nil.
func TestAgentChatStore_AccountIDRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	chatStore := NewAgentChatStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess, err := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "Chat account_id test",
		WorktreePath: "/tmp/wt/chat-acct",
		BranchName:   "feat/chat-acct",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Bound chat: account_id persists on the returned model and round-trips.
	acct := "a1"
	bound, err := chatStore.Create(ctx, CreateAgentChatParams{
		SessionID:      sess.ID,
		AgentSessionID: "agent-acct-bound",
		Title:          "bound chat",
		AccountID:      &acct,
	})
	if err != nil {
		t.Fatalf("create bound chat: %v", err)
	}
	if bound.AccountID == nil || *bound.AccountID != "a1" {
		t.Fatalf("create chat AccountID = %v, want a1", bound.AccountID)
	}
	got, err := chatStore.GetByAgentSessionID(ctx, "agent-acct-bound")
	if err != nil {
		t.Fatalf("get bound chat: %v", err)
	}
	if got.AccountID == nil || *got.AccountID != "a1" {
		t.Fatalf("get chat AccountID = %v, want a1", got.AccountID)
	}

	// Unbound chat: no account_id → nil.
	if _, err := chatStore.Create(ctx, CreateAgentChatParams{
		SessionID:      sess.ID,
		AgentSessionID: "agent-acct-unbound",
		Title:          "unbound chat",
	}); err != nil {
		t.Fatalf("create unbound chat: %v", err)
	}
	gotUnbound, err := chatStore.GetByAgentSessionID(ctx, "agent-acct-unbound")
	if err != nil {
		t.Fatalf("get unbound chat: %v", err)
	}
	if gotUnbound.AccountID != nil {
		t.Fatalf("unbound chat AccountID = %q, want nil", *gotUnbound.AccountID)
	}
}

func TestAgentChatStore_ModelRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	chatStore := NewAgentChatStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess, err := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "Chat model test",
		WorktreePath: "/tmp/wt/chat-model",
		BranchName:   "feat/chat-model",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// A chat created with a model persists it on the returned model and
	// round-trips through a re-read (BOS-381 per-chat model column).
	created, err := chatStore.Create(ctx, CreateAgentChatParams{
		SessionID:      sess.ID,
		AgentSessionID: "agent-model-set",
		Title:          "model chat",
		Model:          "opus",
	})
	if err != nil {
		t.Fatalf("create model chat: %v", err)
	}
	if created.Model != "opus" {
		t.Fatalf("create chat Model = %q, want opus", created.Model)
	}
	got, err := chatStore.GetByAgentSessionID(ctx, "agent-model-set")
	if err != nil {
		t.Fatalf("get model chat: %v", err)
	}
	if got.Model != "opus" {
		t.Fatalf("get chat Model = %q, want opus", got.Model)
	}

	// A chat created without a model backfills to the empty-string default
	// (empty → the plugin resolves the agent CLI default).
	if _, err := chatStore.Create(ctx, CreateAgentChatParams{
		SessionID:      sess.ID,
		AgentSessionID: "agent-model-empty",
		Title:          "no model chat",
	}); err != nil {
		t.Fatalf("create no-model chat: %v", err)
	}
	gotEmpty, err := chatStore.GetByAgentSessionID(ctx, "agent-model-empty")
	if err != nil {
		t.Fatalf("get no-model chat: %v", err)
	}
	if gotEmpty.Model != "" {
		t.Fatalf("unset chat Model = %q, want empty", gotEmpty.Model)
	}
}

func TestAgentChatStore_UpdateAccountIDByAgentSessionID(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	chatStore := NewAgentChatStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess, err := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "Chat account update test",
		WorktreePath: "/tmp/wt/chat-acct-update",
		BranchName:   "feat/chat-acct-update",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := chatStore.Create(ctx, CreateAgentChatParams{
		SessionID:      sess.ID,
		AgentSessionID: "agent-acct-update",
		Title:          "account update",
	}); err != nil {
		t.Fatalf("create chat: %v", err)
	}

	accountID := "acct-123"
	if err := chatStore.UpdateAccountIDByAgentSessionID(ctx, "agent-acct-update", &accountID); err != nil {
		t.Fatalf("update account id: %v", err)
	}
	got, err := chatStore.GetByAgentSessionID(ctx, "agent-acct-update")
	if err != nil {
		t.Fatalf("get updated chat: %v", err)
	}
	if got.AccountID == nil || *got.AccountID != accountID {
		t.Fatalf("updated account_id = %v, want %q", got.AccountID, accountID)
	}

	accountZero := ""
	if err := chatStore.UpdateAccountIDByAgentSessionID(ctx, "agent-acct-update", &accountZero); err != nil {
		t.Fatalf("set account zero: %v", err)
	}
	got, err = chatStore.GetByAgentSessionID(ctx, "agent-acct-update")
	if err != nil {
		t.Fatalf("get account-zero chat: %v", err)
	}
	if got.AccountID == nil || *got.AccountID != "" {
		t.Fatalf("account-zero account_id = %v, want present-empty", got.AccountID)
	}
}

func TestAgentChatStore_MarkStartFailedSanitizesInvalidUTF8(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	chatStore := NewAgentChatStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess, err := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "Chat start-error test",
		WorktreePath: "/tmp/wt/chat-start-error",
		BranchName:   "feat/chat-start-error",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	_, err = chatStore.Create(ctx, CreateAgentChatParams{
		SessionID:      sess.ID,
		AgentSessionID: "agent-bad-start",
		Title:          "Repair chat",
	})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}

	reason := "send plan failed: " + string([]byte{0xff, 0xfe})
	if err := chatStore.MarkStartFailed(ctx, "agent-bad-start", reason); err != nil {
		t.Fatalf("mark start failed: %v", err)
	}

	chat, err := chatStore.GetByAgentSessionID(ctx, "agent-bad-start")
	if err != nil {
		t.Fatalf("get chat: %v", err)
	}
	if chat.StartError == nil {
		t.Fatal("StartError is nil")
	}
	if *chat.StartError != strings.ToValidUTF8(reason, "\uFFFD") {
		t.Fatalf("StartError = %q, want sanitized value", *chat.StartError)
	}
}

func TestAgentChatStore_GetByAgentSessionIDReturnsNotFoundSentinel(t *testing.T) {
	db := setupTestDB(t)
	chatStore := NewAgentChatStore(db)

	_, err := chatStore.GetByAgentSessionID(context.Background(), "missing-agent-session")
	if !errors.Is(err, ErrAgentChatNotFound) {
		t.Fatalf("GetByAgentSessionID error = %v, want ErrAgentChatNotFound", err)
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

func TestAgentChatStore_ProviderSessionID_CreateAndUpdate(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	chatStore := NewAgentChatStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess, err := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "provider session id",
		WorktreePath: "/tmp/wt/provider-session",
		BranchName:   "feat/provider-session",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	providerID := "provider-resume-1"
	chat, err := chatStore.Create(ctx, CreateAgentChatParams{
		SessionID:         sess.ID,
		AgentSessionID:    "agent-provider-create",
		ProviderSessionID: &providerID,
		Title:             "provider chat",
	})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	if chat.ProviderSessionID == nil || *chat.ProviderSessionID != providerID {
		t.Fatalf("returned provider_session_id = %v, want %q", chat.ProviderSessionID, providerID)
	}

	got, err := chatStore.GetByAgentSessionID(ctx, "agent-provider-create")
	if err != nil {
		t.Fatalf("get by agent_session_id: %v", err)
	}
	if got.ProviderSessionID == nil || *got.ProviderSessionID != providerID {
		t.Fatalf("get provider_session_id = %v, want %q", got.ProviderSessionID, providerID)
	}

	listed, err := chatStore.ListBySession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("list by session: %v", err)
	}
	if len(listed) != 1 || listed[0].ProviderSessionID == nil || *listed[0].ProviderSessionID != providerID {
		t.Fatalf("list provider_session_id = %v, want one chat with %q", listed, providerID)
	}

	if _, err := chatStore.Create(ctx, CreateAgentChatParams{
		SessionID:      sess.ID,
		AgentSessionID: "agent-provider-update",
		Title:          "provider update",
	}); err != nil {
		t.Fatalf("create chat without provider id: %v", err)
	}

	updatedID := "provider-resume-2"
	if err := chatStore.UpdateProviderSessionID(ctx, "agent-provider-update", &updatedID); err != nil {
		t.Fatalf("update provider session id: %v", err)
	}
	got, err = chatStore.GetByAgentSessionID(ctx, "agent-provider-update")
	if err != nil {
		t.Fatalf("get updated chat: %v", err)
	}
	if got.ProviderSessionID == nil || *got.ProviderSessionID != updatedID {
		t.Fatalf("updated provider_session_id = %v, want %q", got.ProviderSessionID, updatedID)
	}

	if err := chatStore.UpdateProviderSessionID(ctx, "agent-provider-update", nil); err != nil {
		t.Fatalf("clear provider session id: %v", err)
	}
	got, err = chatStore.GetByAgentSessionID(ctx, "agent-provider-update")
	if err != nil {
		t.Fatalf("get cleared chat: %v", err)
	}
	if got.ProviderSessionID != nil {
		t.Fatalf("cleared provider_session_id = %v, want nil", got.ProviderSessionID)
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

func TestAgentChatStore_ListRoutableChats(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	chatStore := NewAgentChatStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess, err := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "ListRoutableChats test",
		WorktreePath: "/tmp/wt/routable-list",
		BranchName:   "feat/routable-list",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Headless chat: no tmux session name, no start error — must be routable.
	if _, err := chatStore.Create(ctx, CreateAgentChatParams{
		SessionID: sess.ID, AgentSessionID: "headless", Title: "Headless",
	}); err != nil {
		t.Fatalf("create headless chat: %v", err)
	}

	// Tmux-hosted chat — must be routable.
	if _, err := chatStore.Create(ctx, CreateAgentChatParams{
		SessionID: sess.ID, AgentSessionID: "has-tmux", Title: "Has tmux",
	}); err != nil {
		t.Fatalf("create tmux chat: %v", err)
	}
	tmuxName := "boss-test-session"
	if err := chatStore.UpdateTmuxSessionName(ctx, "has-tmux", &tmuxName); err != nil {
		t.Fatalf("set tmux session name: %v", err)
	}

	// Failed-start chat: start_error set, tmux name cleared — must be excluded.
	if _, err := chatStore.Create(ctx, CreateAgentChatParams{
		SessionID: sess.ID, AgentSessionID: "failed", Title: "Failed",
	}); err != nil {
		t.Fatalf("create failed chat: %v", err)
	}
	if err := chatStore.MarkStartFailed(ctx, "failed", "tmux send timed out"); err != nil {
		t.Fatalf("mark start failed: %v", err)
	}

	chats, err := chatStore.ListRoutableChats(ctx)
	if err != nil {
		t.Fatalf("list routable chats: %v", err)
	}
	got := map[string]bool{}
	for _, c := range chats {
		got[c.AgentSessionID] = true
	}
	if !got["headless"] {
		t.Errorf("headless chat missing from routable snapshot; got %v", got)
	}
	if !got["has-tmux"] {
		t.Errorf("tmux chat missing from routable snapshot; got %v", got)
	}
	if got["failed"] {
		t.Errorf("failed-start chat should be excluded from routable snapshot; got %v", got)
	}
}
