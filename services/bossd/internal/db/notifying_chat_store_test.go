package db

import (
	"context"
	"testing"

	"github.com/recurser/bossalib/models"
)

type chatChange struct {
	kind ChatChangeKind
	chat *models.ClaudeChat
}

func newSeededNotifyingStore(t *testing.T) (*NotifyingClaudeChatStore, string, *[]chatChange) {
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

	store := NewNotifyingClaudeChatStore(NewClaudeChatStore(d))
	var got []chatChange
	store.OnChange = func(kind ChatChangeKind, chat *models.ClaudeChat) {
		got = append(got, chatChange{kind: kind, chat: chat})
	}
	return store, sess.ID, &got
}

func TestNotifyingClaudeChatStore_Create_FiresCreated(t *testing.T) {
	store, sessionID, got := newSeededNotifyingStore(t)

	chat, err := store.Create(context.Background(), CreateClaudeChatParams{
		SessionID: sessionID,
		ClaudeID:  "claude-create",
		Title:     "Created",
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

func TestNotifyingClaudeChatStore_UpdateTitleByClaudeID_FiresUpdated(t *testing.T) {
	store, sessionID, got := newSeededNotifyingStore(t)
	ctx := context.Background()

	if _, err := store.Create(ctx, CreateClaudeChatParams{
		SessionID: sessionID,
		ClaudeID:  "claude-update-title",
		Title:     "Original",
	}); err != nil {
		t.Fatalf("seed create: %v", err)
	}
	*got = nil // discard the create hook

	if err := store.UpdateTitleByClaudeID(ctx, "claude-update-title", "Renamed"); err != nil {
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

func TestNotifyingClaudeChatStore_UpdateTmuxSessionName_FiresUpdated(t *testing.T) {
	store, sessionID, got := newSeededNotifyingStore(t)
	ctx := context.Background()

	if _, err := store.Create(ctx, CreateClaudeChatParams{
		SessionID: sessionID,
		ClaudeID:  "claude-update-tmux",
		Title:     "Tmux test",
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

func TestNotifyingClaudeChatStore_DeleteByClaudeID_FiresDeletedWithPreDeleteSnapshot(t *testing.T) {
	store, sessionID, got := newSeededNotifyingStore(t)
	ctx := context.Background()

	chat, err := store.Create(ctx, CreateClaudeChatParams{
		SessionID: sessionID,
		ClaudeID:  "claude-delete",
		Title:     "Doomed",
	})
	if err != nil {
		t.Fatalf("seed create: %v", err)
	}
	*got = nil

	if err := store.DeleteByClaudeID(ctx, "claude-delete"); err != nil {
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

func TestNotifyingClaudeChatStore_DeleteUnknownClaudeID_NoHook(t *testing.T) {
	store, _, got := newSeededNotifyingStore(t)

	// Underlying SQL DELETE is idempotent on a missing row; the wrapper
	// must skip the hook in that case (no chat to report).
	if err := store.DeleteByClaudeID(context.Background(), "nonexistent"); err != nil {
		t.Fatalf("delete unknown: %v", err)
	}
	if len(*got) != 0 {
		t.Fatalf("hook fired %d times for unknown id, want 0", len(*got))
	}
}

func TestNotifyingClaudeChatStore_NilOnChange_DoesNotPanic(t *testing.T) {
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

	store := NewNotifyingClaudeChatStore(NewClaudeChatStore(d))
	// OnChange deliberately not set.
	ctx := context.Background()

	if _, err := store.Create(ctx, CreateClaudeChatParams{
		SessionID: sess.ID,
		ClaudeID:  "claude-nilhook",
		Title:     "Nil hook test",
	}); err != nil {
		t.Fatalf("create with nil hook: %v", err)
	}
	if err := store.UpdateTitleByClaudeID(ctx, "claude-nilhook", "Updated"); err != nil {
		t.Fatalf("update with nil hook: %v", err)
	}
	if err := store.DeleteByClaudeID(ctx, "claude-nilhook"); err != nil {
		t.Fatalf("delete with nil hook: %v", err)
	}
}

func TestNotifyingClaudeChatStore_CreateError_NoHook(t *testing.T) {
	store, _, got := newSeededNotifyingStore(t)

	// session_id is a NOT NULL FK; an empty value violates the foreign
	// key constraint and Create returns an error. The wrapper must NOT
	// fire a hook for a failed mutation.
	_, err := store.Create(context.Background(), CreateClaudeChatParams{
		SessionID: "missing-session",
		ClaudeID:  "claude-orphan",
		Title:     "Orphan",
	})
	if err == nil {
		t.Fatal("expected FK violation, got nil")
	}
	if len(*got) != 0 {
		t.Fatalf("hook fired %d times on failed Create, want 0", len(*got))
	}
}
