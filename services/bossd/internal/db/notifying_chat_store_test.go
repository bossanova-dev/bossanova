package db

import (
	"context"
	"testing"

	"github.com/recurser/bossalib/models"
)

type chatChange struct {
	kind ChatChangeKind
	chat *models.AgentChat
}

func newSeededNotifyingStore(t *testing.T) (*NotifyingAgentChatStore, string, *[]chatChange) {
	t.Helper()
	d := setupTestDB(t)
	repoStore := NewRepoStore(d)
	sessionStore := NewSessionStore(d)
	repo := createTestRepo(t, repoStore)
	sess, err := sessionStore.Create(context.Background(), CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "Notifying chat store test",
		WorktreePath: "/tmp/wt/notifying-chat",
		BranchName:   "feat/notifying-chat",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	store := NewNotifyingAgentChatStore(NewAgentChatStore(d))
	var got []chatChange
	store.OnChange = func(kind ChatChangeKind, chat *models.AgentChat) {
		got = append(got, chatChange{kind: kind, chat: chat})
	}
	return store, sess.ID, &got
}

func TestNotifyingAgentChatStore_Create_FiresCreated(t *testing.T) {
	store, sessionID, got := newSeededNotifyingStore(t)

	chat, err := store.Create(context.Background(), CreateAgentChatParams{
		SessionID:      sessionID,
		AgentSessionID: "claude-create",
		Title:          "Created",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(*got) != 1 {
		t.Fatalf("hook fired %d times, want 1", len(*got))
	}
	if (*got)[0].kind != ChatChangeCreated {
		t.Errorf("kind = %v, want Created", (*got)[0].kind)
	}
	if (*got)[0].chat.ID != chat.ID {
		t.Errorf("chat.ID = %q, want %q", (*got)[0].chat.ID, chat.ID)
	}
	if (*got)[0].chat.SessionID != sessionID {
		t.Errorf("chat.SessionID = %q, want %q", (*got)[0].chat.SessionID, sessionID)
	}
}

func TestNotifyingAgentChatStore_UpdateTitleByAgentSessionID_FiresUpdated(t *testing.T) {
	store, sessionID, got := newSeededNotifyingStore(t)
	ctx := context.Background()

	if _, err := store.Create(ctx, CreateAgentChatParams{
		SessionID:      sessionID,
		AgentSessionID: "claude-update-title",
		Title:          "Original",
	}); err != nil {
		t.Fatalf("seed create: %v", err)
	}
	*got = nil // discard the create hook

	if err := store.UpdateTitleByAgentSessionID(ctx, "claude-update-title", "Renamed"); err != nil {
		t.Fatalf("update title: %v", err)
	}
	if len(*got) != 1 {
		t.Fatalf("hook fired %d times, want 1", len(*got))
	}
	if (*got)[0].kind != ChatChangeUpdated {
		t.Errorf("kind = %v, want Updated", (*got)[0].kind)
	}
	if (*got)[0].chat.Title != "Renamed" {
		t.Errorf("chat.Title = %q, want %q (post-update read)", (*got)[0].chat.Title, "Renamed")
	}
}

func TestNotifyingAgentChatStore_UpdateTmuxSessionName_FiresUpdated(t *testing.T) {
	store, sessionID, got := newSeededNotifyingStore(t)
	ctx := context.Background()

	if _, err := store.Create(ctx, CreateAgentChatParams{
		SessionID:      sessionID,
		AgentSessionID: "claude-update-tmux",
		Title:          "Tmux test",
	}); err != nil {
		t.Fatalf("seed create: %v", err)
	}
	*got = nil

	name := "tmux-session-1"
	if err := store.UpdateTmuxSessionName(ctx, "claude-update-tmux", &name); err != nil {
		t.Fatalf("update tmux: %v", err)
	}
	if len(*got) != 1 || (*got)[0].kind != ChatChangeUpdated {
		t.Fatalf("hook = %+v, want one Updated", *got)
	}
	if (*got)[0].chat.TmuxSessionName == nil || *(*got)[0].chat.TmuxSessionName != name {
		t.Errorf("chat.TmuxSessionName not propagated: %+v", (*got)[0].chat.TmuxSessionName)
	}
}

func TestNotifyingAgentChatStore_UpdateProviderSessionID_FiresUpdated(t *testing.T) {
	store, sessionID, got := newSeededNotifyingStore(t)
	ctx := context.Background()

	if _, err := store.Create(ctx, CreateAgentChatParams{
		SessionID:      sessionID,
		AgentSessionID: "agent-update-provider",
		Title:          "Provider test",
	}); err != nil {
		t.Fatalf("seed create: %v", err)
	}
	*got = nil

	providerID := "provider-resume-1"
	if err := store.UpdateProviderSessionID(ctx, "agent-update-provider", &providerID); err != nil {
		t.Fatalf("update provider session id: %v", err)
	}
	if len(*got) != 1 || (*got)[0].kind != ChatChangeUpdated {
		t.Fatalf("hook = %+v, want one Updated", *got)
	}
	if (*got)[0].chat.ProviderSessionID == nil || *(*got)[0].chat.ProviderSessionID != providerID {
		t.Errorf("chat.ProviderSessionID not propagated: %+v", (*got)[0].chat.ProviderSessionID)
	}
}

func TestNotifyingAgentChatStore_DeleteByAgentSessionID_FiresDeletedWithPreDeleteSnapshot(t *testing.T) {
	store, sessionID, got := newSeededNotifyingStore(t)
	ctx := context.Background()

	chat, err := store.Create(ctx, CreateAgentChatParams{
		SessionID:      sessionID,
		AgentSessionID: "claude-delete",
		Title:          "Doomed",
	})
	if err != nil {
		t.Fatalf("seed create: %v", err)
	}
	*got = nil

	if err := store.DeleteByAgentSessionID(ctx, "claude-delete"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(*got) != 1 || (*got)[0].kind != ChatChangeDeleted {
		t.Fatalf("hook = %+v, want one Deleted", *got)
	}
	// The hook must carry the chat as it existed before deletion so
	// downstream subscribers can scope the delete to the right session.
	if (*got)[0].chat.ID != chat.ID || (*got)[0].chat.SessionID != sessionID {
		t.Errorf("hook chat = %+v, want pre-delete snapshot of %+v", (*got)[0].chat, chat)
	}
}

func TestNotifyingAgentChatStore_DeleteUnknownClaudeID_NoHook(t *testing.T) {
	store, _, got := newSeededNotifyingStore(t)

	// Underlying SQL DELETE is idempotent on a missing row; the wrapper
	// must skip the hook in that case (no chat to report).
	if err := store.DeleteByAgentSessionID(context.Background(), "nonexistent"); err != nil {
		t.Fatalf("delete unknown: %v", err)
	}
	if len(*got) != 0 {
		t.Fatalf("hook fired %d times for unknown id, want 0", len(*got))
	}
}

func TestNotifyingAgentChatStore_NilOnChange_DoesNotPanic(t *testing.T) {
	d := setupTestDB(t)
	repoStore := NewRepoStore(d)
	sessionStore := NewSessionStore(d)
	repo := createTestRepo(t, repoStore)
	sess, err := sessionStore.Create(context.Background(), CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "Nil hook",
		WorktreePath: "/tmp/wt/nil-hook",
		BranchName:   "feat/nil-hook",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	store := NewNotifyingAgentChatStore(NewAgentChatStore(d))
	// OnChange deliberately not set.
	ctx := context.Background()

	if _, err := store.Create(ctx, CreateAgentChatParams{
		SessionID:      sess.ID,
		AgentSessionID: "claude-nilhook",
		Title:          "Nil hook test",
	}); err != nil {
		t.Fatalf("create with nil hook: %v", err)
	}
	if err := store.UpdateTitleByAgentSessionID(ctx, "claude-nilhook", "Updated"); err != nil {
		t.Fatalf("update with nil hook: %v", err)
	}
	if err := store.DeleteByAgentSessionID(ctx, "claude-nilhook"); err != nil {
		t.Fatalf("delete with nil hook: %v", err)
	}
}

func TestNotifyingAgentChatStore_CreateError_NoHook(t *testing.T) {
	store, _, got := newSeededNotifyingStore(t)

	// session_id is a NOT NULL FK; an empty value violates the foreign
	// key constraint and Create returns an error. The wrapper must NOT
	// fire a hook for a failed mutation.
	_, err := store.Create(context.Background(), CreateAgentChatParams{
		SessionID:      "missing-session",
		AgentSessionID: "claude-orphan",
		Title:          "Orphan",
	})
	if err == nil {
		t.Fatal("expected FK violation, got nil")
	}
	if len(*got) != 0 {
		t.Fatalf("hook fired %d times on failed Create, want 0", len(*got))
	}
}

// TestNotifyingAgentChatStore_RebindResumedChat_FiresUpdated pins BOS-1143
// acceptance criterion 7: the resume path's in-place update reaches the
// notifying decorator and emits ChatChangeUpdated, so a resumed chat still
// repaints in the UI. The old delete+create emitted Deleted then Created; a
// store method that skipped the decorator would emit nothing at all and the
// chat would silently go stale.
func TestNotifyingAgentChatStore_RebindResumedChat_FiresUpdated(t *testing.T) {
	store, sessionID, got := newSeededNotifyingStore(t)
	ctx := context.Background()

	provider := "codex-rollout-abc"
	if _, err := store.Create(ctx, CreateAgentChatParams{
		SessionID:         sessionID,
		AgentSessionID:    "agent-old",
		ProviderSessionID: &provider,
		AgentName:         "codex",
		Title:             "Original",
	}); err != nil {
		t.Fatalf("seed create: %v", err)
	}
	*got = nil // discard the create hook

	title := "Resumed"
	if err := store.RebindResumedChat(ctx, "agent-old", RebindResumedChatParams{Title: &title}); err != nil {
		t.Fatalf("rebind: %v", err)
	}
	if len(*got) != 1 {
		t.Fatalf("hook fired %d times, want 1", len(*got))
	}
	if (*got)[0].kind != ChatChangeUpdated {
		t.Errorf("kind = %v, want Updated", (*got)[0].kind)
	}
	if (*got)[0].chat.Title != title {
		t.Errorf("hook chat title = %q, want %q (post-update read)", (*got)[0].chat.Title, title)
	}
	if (*got)[0].chat.AgentName != "codex" {
		t.Errorf("hook chat agent_name = %q, want codex preserved", (*got)[0].chat.AgentName)
	}
}

// TestNotifyingAgentChatStore_RebindResumedChat_NotifiesUnderNewID pins the
// re-key case: when the rebind moves the row onto a new agent_session_id, the
// post-update read has to use the NEW id. Reading the old one would find
// nothing and the hook would either miss or carry a stale snapshot.
func TestNotifyingAgentChatStore_RebindResumedChat_NotifiesUnderNewID(t *testing.T) {
	store, sessionID, got := newSeededNotifyingStore(t)
	ctx := context.Background()

	if _, err := store.Create(ctx, CreateAgentChatParams{
		SessionID:      sessionID,
		AgentSessionID: "agent-old",
		Title:          "Original",
	}); err != nil {
		t.Fatalf("seed create: %v", err)
	}
	*got = nil

	newID := "agent-new"
	if err := store.RebindResumedChat(ctx, "agent-old", RebindResumedChatParams{NewAgentSessionID: &newID}); err != nil {
		t.Fatalf("rebind: %v", err)
	}
	if len(*got) != 1 || (*got)[0].kind != ChatChangeUpdated {
		t.Fatalf("hook = %+v, want one Updated", *got)
	}
	if (*got)[0].chat == nil {
		t.Fatal("hook carried a nil chat; the post-update read used the stale id")
	}
	if (*got)[0].chat.AgentSessionID != newID {
		t.Errorf("hook agent session id = %q, want %q", (*got)[0].chat.AgentSessionID, newID)
	}
}

// TestNotifyingAgentChatStore_RebindResumedChatError_NoHook asserts a failed
// rebind emits nothing: a refusal must not repaint the UI as though the resume
// succeeded.
func TestNotifyingAgentChatStore_RebindResumedChatError_NoHook(t *testing.T) {
	store, _, got := newSeededNotifyingStore(t)

	if err := store.RebindResumedChat(context.Background(), "agent-nonexistent", RebindResumedChatParams{}); err == nil {
		t.Fatal("expected an error rebinding an unknown agent_session_id")
	}
	if len(*got) != 0 {
		t.Errorf("hook fired %d times after a failed rebind, want 0", len(*got))
	}
}
