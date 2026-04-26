package db

import (
	"context"

	"github.com/recurser/bossalib/models"
)

// ChatChangeKind classifies a chat-store mutation for downstream consumers.
type ChatChangeKind int

const (
	ChatChangeUnspecified ChatChangeKind = iota
	ChatChangeCreated
	ChatChangeUpdated
	ChatChangeDeleted
)

// NotifyingClaudeChatStore wraps a ClaudeChatStore so every successful
// mutation invokes OnChange with the affected chat. bossd uses it to fan
// ChatDelta events out on the upstream stream — without this, the
// orchestrator only ever sees chats from the initial DaemonSnapshot and
// the web UI's per-session chat list goes stale the moment a new chat
// is created.
//
// Notifications fire synchronously on the goroutine that performed the
// mutation, immediately after the inner store reports success. Hooks must
// be cheap; holding one up blocks the chat write path. Callers needing
// async behaviour should fan out inside the hook.
//
// UpdateTitle (by primary id) is intentionally not notified: the inner
// store has no GetByID accessor, and every production caller uses the
// claude-id variants. Adding a real caller later means extending the
// inner store with a GetByID and notifying here.
type NotifyingClaudeChatStore struct {
	inner ClaudeChatStore

	// OnChange is invoked synchronously on the calling goroutine after a
	// successful mutation. May be nil; the wrapper no-ops in that case,
	// matching the construct-then-wire pattern used elsewhere in main.go
	// (display computer, status tracker).
	OnChange func(kind ChatChangeKind, chat *models.ClaudeChat)
}

var _ ClaudeChatStore = (*NotifyingClaudeChatStore)(nil)

// NewNotifyingClaudeChatStore wraps inner. OnChange is unset by default;
// callers wire it after constructing dependent subsystems (e.g. the
// upstream stream bus) so the hook closure can capture them.
func NewNotifyingClaudeChatStore(inner ClaudeChatStore) *NotifyingClaudeChatStore {
	return &NotifyingClaudeChatStore{inner: inner}
}

func (s *NotifyingClaudeChatStore) Create(ctx context.Context, params CreateClaudeChatParams) (*models.ClaudeChat, error) {
	chat, err := s.inner.Create(ctx, params)
	if err != nil {
		return chat, err
	}
	s.notify(ChatChangeCreated, chat)
	return chat, nil
}

func (s *NotifyingClaudeChatStore) GetByClaudeID(ctx context.Context, claudeID string) (*models.ClaudeChat, error) {
	return s.inner.GetByClaudeID(ctx, claudeID)
}

func (s *NotifyingClaudeChatStore) ListBySession(ctx context.Context, sessionID string) ([]*models.ClaudeChat, error) {
	return s.inner.ListBySession(ctx, sessionID)
}

func (s *NotifyingClaudeChatStore) UpdateTitle(ctx context.Context, id string, title string) error {
	return s.inner.UpdateTitle(ctx, id, title)
}

func (s *NotifyingClaudeChatStore) UpdateTitleByClaudeID(ctx context.Context, claudeID string, title string) error {
	if err := s.inner.UpdateTitleByClaudeID(ctx, claudeID, title); err != nil {
		return err
	}
	s.notifyAfterUpdate(ctx, claudeID)
	return nil
}

func (s *NotifyingClaudeChatStore) UpdateTmuxSessionName(ctx context.Context, claudeID string, name *string) error {
	if err := s.inner.UpdateTmuxSessionName(ctx, claudeID, name); err != nil {
		return err
	}
	s.notifyAfterUpdate(ctx, claudeID)
	return nil
}

func (s *NotifyingClaudeChatStore) DeleteByClaudeID(ctx context.Context, claudeID string) error {
	// Fetch first so the notification carries the chat that is about to
	// disappear — subscribers need session_id to scope the delete to a
	// per-session chat list.
	chat, getErr := s.inner.GetByClaudeID(ctx, claudeID)
	if err := s.inner.DeleteByClaudeID(ctx, claudeID); err != nil {
		return err
	}
	if getErr == nil {
		s.notify(ChatChangeDeleted, chat)
	}
	return nil
}

func (s *NotifyingClaudeChatStore) ListWithTmuxSession(ctx context.Context) ([]*models.ClaudeChat, error) {
	return s.inner.ListWithTmuxSession(ctx)
}

func (s *NotifyingClaudeChatStore) notify(kind ChatChangeKind, chat *models.ClaudeChat) {
	if s.OnChange == nil || chat == nil {
		return
	}
	s.OnChange(kind, chat)
}

// notifyAfterUpdate re-reads the chat post-mutation so the hook receives
// current state. A read failure silently skips the notification — the
// underlying write already succeeded, and surfacing a read error here
// would mask that.
func (s *NotifyingClaudeChatStore) notifyAfterUpdate(ctx context.Context, claudeID string) {
	if s.OnChange == nil {
		return
	}
	chat, err := s.inner.GetByClaudeID(ctx, claudeID)
	if err != nil || chat == nil {
		return
	}
	s.OnChange(ChatChangeUpdated, chat)
}
