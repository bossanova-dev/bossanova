package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sqlutil"
)

var ErrAgentChatNotFound = errors.New("agent_chat not found")

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
		`INSERT INTO agent_chats (id, session_id, agent_session_id, provider_session_id, agent_name, title, start_error, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, NULL, ?)`,
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
		`SELECT id, session_id, agent_session_id, provider_session_id, agent_name, title, daemon_id, tmux_session_name, start_error, created_at
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
		return nil, fmt.Errorf("%w for agent_session_id %q", ErrAgentChatNotFound, agentSessionID)
	}
	return scanAgentChat(rows)
}

func (s *SQLiteAgentChatStore) ListBySession(ctx context.Context, sessionID string) ([]*models.AgentChat, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id, agent_session_id, provider_session_id, agent_name, title, daemon_id, tmux_session_name, start_error, created_at
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

func (s *SQLiteAgentChatStore) ListBySessions(ctx context.Context, sessionIDs []string) (map[string][]*models.AgentChat, error) {
	chatsBySession := make(map[string][]*models.AgentChat, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return chatsBySession, nil
	}

	const chunkSize = 500
	for start := 0; start < len(sessionIDs); start += chunkSize {
		end := start + chunkSize
		if end > len(sessionIDs) {
			end = len(sessionIDs)
		}
		if err := s.listBySessionsChunk(ctx, sessionIDs[start:end], chatsBySession); err != nil {
			return nil, err
		}
	}
	return chatsBySession, nil
}

func (s *SQLiteAgentChatStore) listBySessionsChunk(ctx context.Context, sessionIDs []string, chatsBySession map[string][]*models.AgentChat) error {
	placeholders := strings.TrimRight(strings.Repeat("?,", len(sessionIDs)), ",")
	args := make([]any, len(sessionIDs))
	for i, id := range sessionIDs {
		args[i] = id
	}

	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT id, session_id, agent_session_id, provider_session_id, agent_name, title, daemon_id, tmux_session_name, start_error, created_at
		 FROM agent_chats
		 WHERE session_id IN (%s)
		 ORDER BY session_id, created_at DESC`, placeholders),
		args...,
	)
	if err != nil {
		return fmt.Errorf("list agent_chats by sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		c, err := scanAgentChat(rows)
		if err != nil {
			return err
		}
		chatsBySession[c.SessionID] = append(chatsBySession[c.SessionID], c)
	}
	return rows.Err()
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

// MarkStartFailed stamps a short reason on the row and clears
// tmux_session_name in a single statement, used by StartTmuxChat's
// failure paths. Mirrors UpdateTmuxSessionName(..., nil) for the
// idempotency side and adds the human-readable reason that the chat
// list view surfaces as a "(failed to start)" badge. reason="" is
// allowed (e.g. for tests) but reads like a happy-path row aside from
// the cleared tmux name, so callers should pass something diagnostic.
func (s *SQLiteAgentChatStore) MarkStartFailed(ctx context.Context, agentSessionID, reason string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE agent_chats SET start_error = ?, tmux_session_name = NULL WHERE agent_session_id = ?`,
		strings.ToValidUTF8(reason, "\uFFFD"), agentSessionID,
	)
	if err != nil {
		return fmt.Errorf("mark agent_chat start failed: %w", err)
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
		`SELECT id, session_id, agent_session_id, provider_session_id, agent_name, title, daemon_id, tmux_session_name, start_error, created_at
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

// ListRoutableChats returns every chat the daemon can still route to for the
// upstream reconnect snapshot: tmux-hosted chats (tmux_session_name set) AND
// headless runs (codex exec / claude --print), which never get a tmux session
// name. Headless chats would otherwise be invisible to bosso's snapshot path
// (it previously read ListWithTmuxSession), so if the create ChatDelta is
// missed or the daemon reconnects after the row already exists, FindDaemonForChat
// would keep 404ing remote send_chat_message / transcript reads that only carry
// agent_session_id. Rows whose tmux start failed (start_error set, tmux name
// cleared by MarkStartFailed) are excluded — they are not routable.
func (s *SQLiteAgentChatStore) ListRoutableChats(ctx context.Context) ([]*models.AgentChat, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id, agent_session_id, provider_session_id, agent_name, title, daemon_id, tmux_session_name, start_error, created_at
		 FROM agent_chats
		 WHERE (tmux_session_name IS NOT NULL AND tmux_session_name != '')
		    OR (start_error IS NULL OR start_error = '')
		 ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list routable agent_chats: %w", err)
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
	if err := rows.Scan(&c.ID, &c.SessionID, &c.AgentSessionID, &c.ProviderSessionID, &c.AgentName, &c.Title, &c.DaemonID, &c.TmuxSessionName, &c.StartError, &createdAt); err != nil {
		return nil, fmt.Errorf("scan agent_chat: %w", err)
	}
	c.CreatedAt = sqlutil.ParseTime(createdAt)
	return &c, nil
}
