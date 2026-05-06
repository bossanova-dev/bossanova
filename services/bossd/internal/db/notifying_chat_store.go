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

// NotifyingAgentChatStore wraps a AgentChatStore so every successful
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
type NotifyingAgentChatStore struct {
	inner AgentChatStore

	// OnChange is invoked synchronously on the calling goroutine after a
	// successful mutation. May be nil; the wrapper no-ops in that case,
	// matching the construct-then-wire pattern used elsewhere in main.go
	// (display computer, status tracker).
	OnChange func(kind ChatChangeKind, chat *models.AgentChat)
}

var _ AgentChatStore = (*NotifyingAgentChatStore)(nil)

// NewNotifyingAgentChatStore wraps inner. OnChange is unset by default;
// callers wire it after constructing dependent subsystems (e.g. the
// upstream stream bus) so the hook closure can capture them.
func NewNotifyingAgentChatStore(inner AgentChatStore) *NotifyingAgentChatStore {
	return &NotifyingAgentChatStore{inner: inner}
}

func (s *NotifyingAgentChatStore) Create(ctx context.Context, params CreateAgentChatParams) (*models.AgentChat, error) {
	chat, err := s.inner.Create(ctx, params)
	if err != nil {
		return chat, err
	}
	s.notify(ChatChangeCreated, chat)
	return chat, nil
}

func (s *NotifyingAgentChatStore) GetByAgentSessionID(ctx context.Context, agentSessionID string) (*models.AgentChat, error) {
	return s.inner.GetByAgentSessionID(ctx, agentSessionID)
}

func (s *NotifyingAgentChatStore) ListBySession(ctx context.Context, sessionID string) ([]*models.AgentChat, error) {
	return s.inner.ListBySession(ctx, sessionID)
}

func (s *NotifyingAgentChatStore) UpdateTitle(ctx context.Context, id string, title string) error {
	return s.inner.UpdateTitle(ctx, id, title)
}

func (s *NotifyingAgentChatStore) UpdateTitleByAgentSessionID(ctx context.Context, agentSessionID string, title string) error {
	if err := s.inner.UpdateTitleByAgentSessionID(ctx, agentSessionID, title); err != nil {
		return err
	}
	s.notifyAfterUpdate(ctx, agentSessionID)
	return nil
}

func (s *NotifyingAgentChatStore) UpdateTmuxSessionName(ctx context.Context, agentSessionID string, name *string) error {
	if err := s.inner.UpdateTmuxSessionName(ctx, agentSessionID, name); err != nil {
		return err
	}
	s.notifyAfterUpdate(ctx, agentSessionID)
	return nil
}

func (s *NotifyingAgentChatStore) DeleteByAgentSessionID(ctx context.Context, agentSessionID string) error {
	// Fetch first so the notification carries the chat that is about to
	// disappear — subscribers need session_id to scope the delete to a
	// per-session chat list.
	chat, getErr := s.inner.GetByAgentSessionID(ctx, agentSessionID)
	if err := s.inner.DeleteByAgentSessionID(ctx, agentSessionID); err != nil {
		return err
	}
	if getErr == nil {
		s.notify(ChatChangeDeleted, chat)
	}
	return nil
}

func (s *NotifyingAgentChatStore) ListWithTmuxSession(ctx context.Context) ([]*models.AgentChat, error) {
	return s.inner.ListWithTmuxSession(ctx)
}

func (s *NotifyingAgentChatStore) notify(kind ChatChangeKind, chat *models.AgentChat) {
	if s.OnChange == nil || chat == nil {
		return
	}
	s.OnChange(kind, chat)
}

// notifyAfterUpdate re-reads the chat post-mutation so the hook receives
// current state. A read failure silently skips the notification — the
// underlying write already succeeded, and surfacing a read error here
// would mask that.
func (s *NotifyingAgentChatStore) notifyAfterUpdate(ctx context.Context, agentSessionID string) {
	if s.OnChange == nil {
		return
	}
	chat, err := s.inner.GetByAgentSessionID(ctx, agentSessionID)
	if err != nil || chat == nil {
		return
	}
	s.OnChange(ChatChangeUpdated, chat)
}
