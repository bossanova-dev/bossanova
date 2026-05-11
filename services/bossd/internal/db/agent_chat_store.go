package db

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sqlutil"
)

var _ AgentChatStore = (*SQLiteAgentChatStore)(nil)

// SQLiteAgentChatStore implements AgentChatStore using SQLite.
type SQLiteAgentChatStore struct {
	db *sql.DB
}

// NewAgentChatStore creates a new SQLite-backed AgentChatStore.
func NewAgentChatStore(db *sql.DB) *SQLiteAgentChatStore {
	return &SQLiteAgentChatStore{db: db}
}

func (s *SQLiteAgentChatStore) Create(ctx context.Context, params CreateAgentChatParams) (*models.AgentChat, error) {
	id, err := sqlutil.NewID()
	if err != nil {
		return nil, fmt.Errorf("new agent_chat id: %w", err)
	}
	now := sqlutil.TimeNow()
	agentName := params.AgentName
	if agentName == "" {
		agentName = "claude"
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO agent_chats (id, session_id, agent_session_id, provider_session_id, agent_name, title, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, params.SessionID, params.AgentSessionID, params.ProviderSessionID, agentName, params.Title, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert agent_chat: %w", err)
	}

	return &models.AgentChat{
		ID:                id,
		SessionID:         params.SessionID,
		AgentSessionID:    params.AgentSessionID,
		ProviderSessionID: params.ProviderSessionID,
		AgentName:         agentName,
		Title:             params.Title,
		CreatedAt:         sqlutil.ParseTime(now),
	}, nil
}

func (s *SQLiteAgentChatStore) GetByAgentSessionID(ctx context.Context, agentSessionID string) (*models.AgentChat, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id, agent_session_id, provider_session_id, agent_name, title, daemon_id, tmux_session_name, created_at
		 FROM agent_chats
		 WHERE agent_session_id = ?`,
		agentSessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("get agent_chat by agent_session_id: %w", err)
	}
	defer func() { _ = rows.Close() }()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("get agent_chat by agent_session_id: %w", err)
		}
		return nil, fmt.Errorf("agent_chat not found for agent_session_id %q", agentSessionID)
	}
	return scanAgentChat(rows)
}

func (s *SQLiteAgentChatStore) ListBySession(ctx context.Context, sessionID string) ([]*models.AgentChat, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id, agent_session_id, provider_session_id, agent_name, title, daemon_id, tmux_session_name, created_at
		 FROM agent_chats
		 WHERE session_id = ?
		 ORDER BY created_at DESC`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("list agent_chats: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var chats []*models.AgentChat
	for rows.Next() {
		c, err := scanAgentChat(rows)
		if err != nil {
			return nil, err
		}
		chats = append(chats, c)
	}
	return chats, rows.Err()
}

func (s *SQLiteAgentChatStore) UpdateTitle(ctx context.Context, id string, title string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE agent_chats SET title = ? WHERE id = ?`,
		title, id,
	)
	if err != nil {
		return fmt.Errorf("update agent_chat title: %w", err)
	}
	return nil
}

func (s *SQLiteAgentChatStore) UpdateTitleByAgentSessionID(ctx context.Context, agentSessionID string, title string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE agent_chats SET title = ? WHERE agent_session_id = ?`,
		title, agentSessionID,
	)
	if err != nil {
		return fmt.Errorf("update agent_chat title by agent_session_id: %w", err)
	}
	return nil
}

func (s *SQLiteAgentChatStore) UpdateTmuxSessionName(ctx context.Context, agentSessionID string, name *string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE agent_chats SET tmux_session_name = ? WHERE agent_session_id = ?`,
		name, agentSessionID,
	)
	if err != nil {
		return fmt.Errorf("update agent_chat tmux_session_name: %w", err)
	}
	return nil
}

func (s *SQLiteAgentChatStore) UpdateProviderSessionID(ctx context.Context, agentSessionID string, providerSessionID *string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE agent_chats SET provider_session_id = ? WHERE agent_session_id = ?`,
		providerSessionID, agentSessionID,
	)
	if err != nil {
		return fmt.Errorf("update agent_chat provider_session_id: %w", err)
	}
	return nil
}

func (s *SQLiteAgentChatStore) DeleteByAgentSessionID(ctx context.Context, agentSessionID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM agent_chats WHERE agent_session_id = ?`, agentSessionID)
	if err != nil {
		return fmt.Errorf("delete agent_chat by agent_session_id: %w", err)
	}
	return nil
}

func (s *SQLiteAgentChatStore) ListWithTmuxSession(ctx context.Context) ([]*models.AgentChat, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id, agent_session_id, provider_session_id, agent_name, title, daemon_id, tmux_session_name, created_at
		 FROM agent_chats
		 WHERE tmux_session_name IS NOT NULL AND tmux_session_name != ''
		 ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list agent_chats with tmux session: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var chats []*models.AgentChat
	for rows.Next() {
		c, err := scanAgentChat(rows)
		if err != nil {
			return nil, err
		}
		chats = append(chats, c)
	}
	return chats, rows.Err()
}

func scanAgentChat(rows *sql.Rows) (*models.AgentChat, error) {
	var c models.AgentChat
	var createdAt string
	if err := rows.Scan(&c.ID, &c.SessionID, &c.AgentSessionID, &c.ProviderSessionID, &c.AgentName, &c.Title, &c.DaemonID, &c.TmuxSessionName, &createdAt); err != nil {
		return nil, fmt.Errorf("scan agent_chat: %w", err)
	}
	c.CreatedAt = sqlutil.ParseTime(createdAt)
	return &c, nil
}
