package db

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/recurser/bossalib/models"
)

var _ ClaudeChatStore = (*SQLiteClaudeChatStore)(nil)

// SQLiteClaudeChatStore implements ClaudeChatStore using SQLite.
type SQLiteClaudeChatStore struct {
	db *sql.DB
}

// NewClaudeChatStore creates a new SQLite-backed ClaudeChatStore.
func NewClaudeChatStore(db *sql.DB) *SQLiteClaudeChatStore {
	return &SQLiteClaudeChatStore{db: db}
}

func (s *SQLiteClaudeChatStore) Create(ctx context.Context, params CreateClaudeChatParams) (*models.ClaudeChat, error) {
	id := newID()
	now := timeNow()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO claude_chats (id, session_id, claude_id, title, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		id, params.SessionID, params.ClaudeID, params.Title, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert claude_chat: %w", err)
	}

	return &models.ClaudeChat{
		ID:        id,
		SessionID: params.SessionID,
		ClaudeID:  params.ClaudeID,
		Title:     params.Title,
		CreatedAt: parseTime(now),
	}, nil
}

func (s *SQLiteClaudeChatStore) ListBySession(ctx context.Context, sessionID string) ([]*models.ClaudeChat, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id, claude_id, title, daemon_id, created_at
		 FROM claude_chats
		 WHERE session_id = ?
		 ORDER BY created_at DESC`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("list claude_chats: %w", err)
	}
	defer rows.Close()

	var chats []*models.ClaudeChat
	for rows.Next() {
		c, err := scanClaudeChat(rows)
		if err != nil {
			return nil, err
		}
		chats = append(chats, c)
	}
	return chats, rows.Err()
}

func (s *SQLiteClaudeChatStore) UpdateTitle(ctx context.Context, id string, title string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE claude_chats SET title = ? WHERE id = ?`,
		title, id,
	)
	if err != nil {
		return fmt.Errorf("update claude_chat title: %w", err)
	}
	return nil
}

func (s *SQLiteClaudeChatStore) UpdateTitleByClaudeID(ctx context.Context, claudeID string, title string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE claude_chats SET title = ? WHERE claude_id = ?`,
		title, claudeID,
	)
	if err != nil {
		return fmt.Errorf("update claude_chat title by claude_id: %w", err)
	}
	return nil
}

func scanClaudeChat(rows *sql.Rows) (*models.ClaudeChat, error) {
	var c models.ClaudeChat
	var createdAt string
	if err := rows.Scan(&c.ID, &c.SessionID, &c.ClaudeID, &c.Title, &c.DaemonID, &createdAt); err != nil {
		return nil, fmt.Errorf("scan claude_chat: %w", err)
	}
	c.CreatedAt = parseTime(createdAt)
	return &c, nil
}
