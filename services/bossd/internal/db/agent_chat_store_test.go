package db

import (
	"context"
	"database/sql"
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

	// A resumed headless run keeps the same primary chat row but points it at
	// its newly started agent process.
	if err := chatStore.RebindResumedChat(ctx, "claude-abc-123", RebindResumedChatParams{
		NewAgentSessionID: strPtr("claude-resumed-456"),
	}); err != nil {
		t.Fatalf("rebind resumed chat: %v", err)
	}
	if _, err := chatStore.GetByAgentSessionID(ctx, "claude-abc-123"); !errors.Is(err, ErrAgentChatNotFound) {
		t.Fatalf("old agent session id lookup err = %v, want ErrAgentChatNotFound", err)
	}
	resumed, err := chatStore.GetByAgentSessionID(ctx, "claude-resumed-456")
	if err != nil {
		t.Fatalf("get resumed chat: %v", err)
	}
	if resumed.ID != chat.ID || resumed.Title != "Title by claude ID" {
		t.Errorf("resumed chat = %+v, want original row with updated agent id", resumed)
	}
}

// TestAgentChatStore_RebindResumedChatScopesUpdateToNamedRow pins that a
// rebind moves only the row whose agent_session_id the caller named. The
// UPDATE has no other predicate, so a WHERE clause that ever widened -- or a
// re-key that collided with a sibling -- would silently rewrite a chat
// belonging to a different session.
//
// This property was previously pinned on UpdateAgentSessionID, whose (id,
// old_agent_session_id) CAS pair has no analogue on RebindResumedChat; the
// half of that test which asserted an unmatched id is reported rather than
// applied now lives in TestAgentChatStore_RebindResumedChat's "missing row is
// reported, not silently created" subtest.
func TestAgentChatStore_RebindResumedChatScopesUpdateToNamedRow(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	chatStore := NewAgentChatStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	primarySession, err := sessionStore.Create(ctx, CreateSessionParams{
		RepoID: repo.ID, Title: "Primary", WorktreePath: "/tmp/wt/primary", BranchName: "feat/primary", BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("create primary session: %v", err)
	}
	siblingSession, err := sessionStore.Create(ctx, CreateSessionParams{
		RepoID: repo.ID, Title: "Sibling", WorktreePath: "/tmp/wt/sibling", BranchName: "feat/sibling", BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("create sibling session: %v", err)
	}
	primaryChat, err := chatStore.Create(ctx, CreateAgentChatParams{SessionID: primarySession.ID, AgentSessionID: "stale-agent", Title: "Primary chat"})
	if err != nil {
		t.Fatalf("create primary chat: %v", err)
	}
	siblingChat, err := chatStore.Create(ctx, CreateAgentChatParams{SessionID: siblingSession.ID, AgentSessionID: "sibling-agent", Title: "Sibling chat"})
	if err != nil {
		t.Fatalf("create sibling chat: %v", err)
	}

	resumedTitle := "Primary resumed"
	if err := chatStore.RebindResumedChat(ctx, "stale-agent", RebindResumedChatParams{
		NewAgentSessionID: strPtr("resumed-agent"),
		Title:             &resumedTitle,
	}); err != nil {
		t.Fatalf("rebind primary chat: %v", err)
	}

	primaryChats, err := chatStore.ListBySession(ctx, primarySession.ID)
	if err != nil {
		t.Fatalf("list primary chats: %v", err)
	}
	if got := primaryChats[0].ID; got != primaryChat.ID {
		t.Fatalf("primary chat id = %q, want %q (the rebind must update in place)", got, primaryChat.ID)
	}
	if got := primaryChats[0].AgentSessionID; got != "resumed-agent" {
		t.Errorf("primary chat agent session id = %q, want resumed-agent", got)
	}

	siblingChats, err := chatStore.ListBySession(ctx, siblingSession.ID)
	if err != nil {
		t.Fatalf("list sibling chats: %v", err)
	}
	if got := siblingChats[0].ID; got != siblingChat.ID {
		t.Fatalf("sibling chat id = %q, want %q", got, siblingChat.ID)
	}
	if got := siblingChats[0].AgentSessionID; got != "sibling-agent" {
		t.Errorf("sibling chat agent session id = %q, want sibling-agent (an unnamed row must not move)", got)
	}
	if got := siblingChats[0].Title; got != "Sibling chat" {
		t.Errorf("sibling chat title = %q, want %q (an unnamed row must not move)", got, "Sibling chat")
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

func TestAgentChatStore_ClearTmuxSessionNameIfRequiresObservedName(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()
	repo := createTestRepo(t, NewRepoStore(database))
	sess := createTestSession(t, NewSessionStore(database), repo.ID)
	chatStore := NewAgentChatStore(database)
	agentSessionID := "agent-clear-if"
	originalName := "boss-original-pane"
	newName := "boss-new-pane"

	if _, err := chatStore.Create(ctx, CreateAgentChatParams{
		SessionID:      sess.ID,
		AgentSessionID: agentSessionID,
		Title:          "conditional clear",
	}); err != nil {
		t.Fatalf("create chat: %v", err)
	}
	if err := chatStore.UpdateTmuxSessionName(ctx, agentSessionID, &originalName); err != nil {
		t.Fatalf("set tmux name: %v", err)
	}
	if err := chatStore.UpdateTmuxSessionName(ctx, agentSessionID, &newName); err != nil {
		t.Fatalf("update tmux name: %v", err)
	}
	if err := chatStore.ClearTmuxSessionNameIf(ctx, agentSessionID, originalName); err != sql.ErrNoRows {
		t.Fatalf("clear stale tmux name err = %v, want sql.ErrNoRows", err)
	}
	got, err := chatStore.GetByAgentSessionID(ctx, agentSessionID)
	if err != nil {
		t.Fatalf("get after stale clear: %v", err)
	}
	if got.TmuxSessionName == nil || *got.TmuxSessionName != newName {
		t.Fatalf("tmux_session_name after stale clear = %v, want %q", got.TmuxSessionName, newName)
	}
	if err := chatStore.ClearTmuxSessionNameIf(ctx, agentSessionID, newName); err != nil {
		t.Fatalf("clear current tmux name: %v", err)
	}
	got, err = chatStore.GetByAgentSessionID(ctx, agentSessionID)
	if err != nil {
		t.Fatalf("get after clear: %v", err)
	}
	if got.TmuxSessionName != nil {
		t.Fatalf("tmux_session_name after clear = %q, want nil", *got.TmuxSessionName)
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

// TestAgentChatStore_NullTmuxNameIsRoutableButNotTmuxListed pins the exact query
// gap the BOS-979 pane-adoption sweep depends on.
//
// The sweep carries a tmux.ChatSessionName(repoID, agentSessionID) fallback for a
// chat whose tmux_session_name is NULL, but it used to read its chat list through
// ListWithTmuxSession — whose WHERE clause excludes precisely those rows, making
// the fallback unreachable code. A pane whose name was never persisted, or was
// cleared by a partial write, held a live baked token that no restart could
// recover. This asserts the two predicates really do differ on that row, so the
// sweep's switch to ListRoutableChats is what closes the gap rather than a
// no-op rename.
func TestAgentChatStore_NullTmuxNameIsRoutableButNotTmuxListed(t *testing.T) {
	db := setupTestDB(t)
	chatStore := NewAgentChatStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, NewRepoStore(db))
	sess, err := NewSessionStore(db).Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "null tmux name",
		WorktreePath: "/tmp/wt/null-tmux",
		BranchName:   "feat/null-tmux",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// A chat that was tmux-hosted and whose name was then CLEARED — the
	// surviving pane is still alive and still holds its baked token.
	if _, err := chatStore.Create(ctx, CreateAgentChatParams{
		SessionID: sess.ID, AgentSessionID: "cleared-name", Title: "Cleared",
	}); err != nil {
		t.Fatalf("create chat: %v", err)
	}
	name := "boss-repo-cleared"
	if err := chatStore.UpdateTmuxSessionName(ctx, "cleared-name", &name); err != nil {
		t.Fatalf("set tmux name: %v", err)
	}
	if err := chatStore.UpdateTmuxSessionName(ctx, "cleared-name", nil); err != nil {
		t.Fatalf("clear tmux name: %v", err)
	}

	tmuxListed, err := chatStore.ListWithTmuxSession(ctx)
	if err != nil {
		t.Fatalf("list with tmux session: %v", err)
	}
	for _, c := range tmuxListed {
		if c.AgentSessionID == "cleared-name" {
			t.Fatalf("ListWithTmuxSession returned a NULL-name row; the gap this test pins does not exist")
		}
	}

	routable, err := chatStore.ListRoutableChats(ctx)
	if err != nil {
		t.Fatalf("list routable chats: %v", err)
	}
	found := false
	for _, c := range routable {
		if c.AgentSessionID == "cleared-name" {
			found = true
			if c.TmuxSessionName != nil {
				t.Fatalf("expected a NULL tmux_session_name, got %q", *c.TmuxSessionName)
			}
		}
	}
	if !found {
		t.Fatalf("ListRoutableChats dropped the NULL-name row the sweep needs to see")
	}
}

// TestAgentChatStore_DeleteByAgentSessionIDDropsProxyToken covers the chat half
// of the durable registry's eviction. proxy_tokens.agent_session_id carries no
// foreign key: agent_chats.agent_session_id was not a legal parent key when that
// table was added, and 20260904000000 made it UNIQUE without adding one. Nothing
// cascades here, so the chat delete has to issue the DELETE itself. Without it,
// deleting a chat would leave a row whose rebuild resolves a live pane's token
// to a chat that no longer exists.
func TestAgentChatStore_DeleteByAgentSessionIDDropsProxyToken(t *testing.T) {
	db := setupTestDB(t)
	chatStore := NewAgentChatStore(db)
	tokenStore := NewProxyTokenStore(db)
	ctx := context.Background()

	sessID := createProxyTokenTestSession(t, db, "chat delete drops proxy token")
	for _, agentSessionID := range []string{"keep-me", "delete-me"} {
		if _, err := chatStore.Create(ctx, CreateAgentChatParams{
			SessionID: sessID, AgentSessionID: agentSessionID, Title: agentSessionID,
		}); err != nil {
			t.Fatalf("create chat %s: %v", agentSessionID, err)
		}
		if err := tokenStore.Upsert(ctx, ProxyTokenRecord{
			TokenSHA256:    hexDigest(agentSessionID[0]),
			SessionID:      sessID,
			AgentSessionID: agentSessionID,
			AccountID:      "acct-1",
			IsChatShaped:   true,
		}); err != nil {
			t.Fatalf("persist token for %s: %v", agentSessionID, err)
		}
	}

	if err := chatStore.DeleteByAgentSessionID(ctx, "delete-me"); err != nil {
		t.Fatalf("delete by agent session id: %v", err)
	}

	rows, err := tokenStore.List(ctx)
	if err != nil {
		t.Fatalf("list tokens: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("proxy token rows = %d, want 1", len(rows))
	}
	if rows[0].AgentSessionID != "keep-me" {
		t.Errorf("surviving token agent_session_id = %q, want keep-me", rows[0].AgentSessionID)
	}

	// The chat row itself still goes away — the token delete must not have
	// replaced the original statement or rolled it back.
	chats, err := chatStore.ListBySession(ctx, sessID)
	if err != nil {
		t.Fatalf("list chats: %v", err)
	}
	if len(chats) != 1 || chats[0].AgentSessionID != "keep-me" {
		t.Fatalf("chats after delete = %+v, want only keep-me", chats)
	}

	// Deleting an agent session that never existed stays a no-op on both tables.
	if err := chatStore.DeleteByAgentSessionID(ctx, "nonexistent"); err != nil {
		t.Errorf("delete non-existent should not error, got: %v", err)
	}
	if rows, err = tokenStore.List(ctx); err != nil || len(rows) != 1 {
		t.Errorf("rows after no-op delete = %d (err %v), want 1", len(rows), err)
	}
}

// TestAgentChatStore_RebindResumedChat is the store-level regression test for
// BOS-1143. Resume used to delete the chat row and Create a replacement, which
// erased every column the caller did not re-supply -- turning an interrupted
// codex chat into a claude one. RebindResumedChat writes only the fields set on
// params; everything else keeps the value the row already carries.
func TestAgentChatStore_RebindResumedChat(t *testing.T) {
	db := setupTestDB(t)
	sessionStore := NewSessionStore(db)
	chatStore := NewAgentChatStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, NewRepoStore(db))
	sess, err := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "Rebind test",
		WorktreePath: "/tmp/wt/rebind",
		BranchName:   "feat/rebind",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	acct := "acct-1"
	provider := "codex-rollout-abc"
	chat, err := chatStore.Create(ctx, CreateAgentChatParams{
		SessionID:         sess.ID,
		AgentSessionID:    "agent-old",
		ProviderSessionID: &provider,
		AgentName:         "codex",
		Title:             "Original title",
		AccountID:         &acct,
		Model:             "gpt-5",
	})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	// MarkStartFailed addresses the row by agent_session_id and ignores
	// RowsAffected, so passing chat.ID here would match nothing and silently
	// seed nothing -- which would make the ClearStartError assertion below pass
	// vacuously. Assert the seed landed before relying on it.
	if err := chatStore.MarkStartFailed(ctx, "agent-old", "prior boot failure"); err != nil {
		t.Fatalf("mark start failed: %v", err)
	}
	if seeded, err := chatStore.GetByAgentSessionID(ctx, "agent-old"); err != nil {
		t.Fatalf("read back start_error seed: %v", err)
	} else if seeded.StartError == nil || *seeded.StartError != "prior boot failure" {
		t.Fatalf("start_error seed did not take: %v; ClearStartError could not be observed", seeded.StartError)
	}

	t.Run("absent fields are preserved", func(t *testing.T) {
		title := "Resumed title"
		if err := chatStore.RebindResumedChat(ctx, "agent-old", RebindResumedChatParams{
			Title:           &title,
			ClearStartError: true,
		}); err != nil {
			t.Fatalf("rebind: %v", err)
		}
		got, err := chatStore.GetByAgentSessionID(ctx, "agent-old")
		if err != nil {
			t.Fatalf("get after rebind: %v", err)
		}
		if got.Title != title {
			t.Errorf("title = %q, want %q", got.Title, title)
		}
		// The columns the defect erased.
		if got.AgentName != "codex" {
			t.Errorf("agent_name = %q, want codex preserved", got.AgentName)
		}
		if got.Model != "gpt-5" {
			t.Errorf("model = %q, want gpt-5 preserved", got.Model)
		}
		if got.AccountID == nil || *got.AccountID != acct {
			t.Errorf("account_id = %v, want %q preserved", got.AccountID, acct)
		}
		if got.ProviderSessionID == nil || *got.ProviderSessionID != provider {
			t.Errorf("provider_session_id = %v, want %q preserved", got.ProviderSessionID, provider)
		}
		if got.SessionID != sess.ID {
			t.Errorf("session_id = %q, want %q preserved", got.SessionID, sess.ID)
		}
		// ClearStartError replaces the implicit reset the old delete+create did:
		// without it a previously-failed chat stays badged as failed forever.
		if got.StartError != nil {
			t.Errorf("start_error = %v, want cleared by ClearStartError", got.StartError)
		}
	})

	t.Run("set fields are written", func(t *testing.T) {
		newID := "agent-new"
		newAgent := "claude"
		newModel := "sonnet"
		newAcct := "acct-2"
		newProvider := "claude-uuid-def"
		newAcctPtr, newProviderPtr := &newAcct, &newProvider
		if err := chatStore.RebindResumedChat(ctx, "agent-old", RebindResumedChatParams{
			NewAgentSessionID: &newID,
			AgentName:         &newAgent,
			Model:             &newModel,
			AccountID:         &newAcctPtr,
			ProviderSessionID: &newProviderPtr,
		}); err != nil {
			t.Fatalf("rebind: %v", err)
		}
		got, err := chatStore.GetByAgentSessionID(ctx, newID)
		if err != nil {
			t.Fatalf("get after re-key: %v", err)
		}
		if got.ID != chat.ID {
			t.Errorf("re-key created a new row: id = %q, want %q", got.ID, chat.ID)
		}
		if got.AgentName != newAgent || got.Model != newModel {
			t.Errorf("agent_name/model = %q/%q, want %q/%q", got.AgentName, got.Model, newAgent, newModel)
		}
		if got.AccountID == nil || *got.AccountID != newAcct {
			t.Errorf("account_id = %v, want %q", got.AccountID, newAcct)
		}
		if got.ProviderSessionID == nil || *got.ProviderSessionID != newProvider {
			t.Errorf("provider_session_id = %v, want %q", got.ProviderSessionID, newProvider)
		}
		if _, err := chatStore.GetByAgentSessionID(ctx, "agent-old"); !errors.Is(err, ErrAgentChatNotFound) {
			t.Errorf("old id still resolves after re-key: err = %v", err)
		}
	})

	t.Run("nullable columns can be cleared", func(t *testing.T) {
		var none *string
		if err := chatStore.RebindResumedChat(ctx, "agent-new", RebindResumedChatParams{
			AccountID:         &none,
			ProviderSessionID: &none,
		}); err != nil {
			t.Fatalf("rebind: %v", err)
		}
		got, err := chatStore.GetByAgentSessionID(ctx, "agent-new")
		if err != nil {
			t.Fatalf("get after clear: %v", err)
		}
		if got.AccountID != nil {
			t.Errorf("account_id = %v, want NULL", got.AccountID)
		}
		if got.ProviderSessionID != nil {
			t.Errorf("provider_session_id = %v, want NULL", got.ProviderSessionID)
		}
	})

	t.Run("missing row is reported, not silently created", func(t *testing.T) {
		err := chatStore.RebindResumedChat(ctx, "agent-nonexistent", RebindResumedChatParams{})
		if !errors.Is(err, ErrAgentChatNotFound) {
			t.Fatalf("err = %v, want ErrAgentChatNotFound", err)
		}
		if !strings.Contains(err.Error(), "agent-nonexistent") {
			t.Errorf("error %q does not name the agent_session_id", err.Error())
		}
		// A rebind that matched nothing must not have invented a row.
		chats, err := chatStore.ListBySession(ctx, sess.ID)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(chats) != 1 {
			t.Errorf("chats = %d, want 1 (the failed rebind created nothing)", len(chats))
		}
	})
}
