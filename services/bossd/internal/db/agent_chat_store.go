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
		`INSERT INTO agent_chats (id, session_id, agent_session_id, provider_session_id, agent_name, title, start_error, account_id, model, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, NULL, ?, ?, ?)`,
		id, params.SessionID, params.AgentSessionID, params.ProviderSessionID, agentName, params.Title, params.AccountID, params.Model, now,
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
		AccountID:         params.AccountID,
		Model:             params.Model,
		CreatedAt:         sqlutil.ParseTime(now),
	}, nil
}

func (s *SQLiteAgentChatStore) GetByAgentSessionID(ctx context.Context, agentSessionID string) (*models.AgentChat, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id, agent_session_id, provider_session_id, agent_name, title, daemon_id, tmux_session_name, start_error, account_id, model, created_at
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
		`SELECT id, session_id, agent_session_id, provider_session_id, agent_name, title, daemon_id, tmux_session_name, start_error, account_id, model, created_at
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
		fmt.Sprintf(`SELECT id, session_id, agent_session_id, provider_session_id, agent_name, title, daemon_id, tmux_session_name, start_error, account_id, model, created_at
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

func (s *SQLiteAgentChatStore) ClearTmuxSessionNameIf(ctx context.Context, agentSessionID, tmuxSessionName string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE agent_chats SET tmux_session_name = NULL WHERE agent_session_id = ? AND tmux_session_name = ?`,
		agentSessionID, tmuxSessionName,
	)
	if err != nil {
		return fmt.Errorf("clear agent_chat tmux_session_name if current: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("clear agent_chat tmux_session_name if current rows affected: %w", err)
	} else if n == 0 {
		return sql.ErrNoRows
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

func (s *SQLiteAgentChatStore) UpdateAccountIDByAgentSessionID(ctx context.Context, agentSessionID string, accountID *string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE agent_chats SET account_id = ? WHERE agent_session_id = ?`,
		accountID, agentSessionID,
	)
	if err != nil {
		return fmt.Errorf("update agent_chat account_id: %w", err)
	}
	return nil
}

// RebindResumedChat updates an existing chat row in place so a resume keeps
// the chat's identity instead of destroying and re-creating it (BOS-1143).
//
// Only the fields set on params are written; every other column keeps the
// value the row already carries. The statement is addressed by
// agent_session_id, which 20260904000000_agent_chats_unique_agent_session_id
// makes UNIQUE — so exactly one row can match. No matching row is an error
// (wrapped ErrAgentChatNotFound) rather than a silent no-op: the caller asked
// to resume a specific chat, and there is nothing to resume.
func (s *SQLiteAgentChatStore) RebindResumedChat(ctx context.Context, agentSessionID string, params RebindResumedChatParams) error {
	var sets []string
	var args []any
	if params.NewAgentSessionID != nil {
		sets = append(sets, "agent_session_id = ?")
		args = append(args, *params.NewAgentSessionID)
	}
	if params.SessionID != nil {
		sets = append(sets, "session_id = ?")
		args = append(args, *params.SessionID)
	}
	if params.Title != nil {
		sets = append(sets, "title = ?")
		args = append(args, *params.Title)
	}
	if params.AgentName != nil {
		sets = append(sets, "agent_name = ?")
		args = append(args, *params.AgentName)
	}
	if params.Model != nil {
		sets = append(sets, "model = ?")
		args = append(args, *params.Model)
	}
	if params.AccountID != nil {
		sets = append(sets, "account_id = ?")
		args = append(args, *params.AccountID)
	}
	if params.ProviderSessionID != nil {
		sets = append(sets, "provider_session_id = ?")
		args = append(args, *params.ProviderSessionID)
	}
	if params.ClearStartError {
		sets = append(sets, "start_error = NULL")
	}
	if len(sets) == 0 {
		// A no-field rebind still has to answer "does this chat exist?",
		// so write the column to itself rather than skipping the UPDATE:
		// SQLite counts matched rows, so RowsAffected stays meaningful.
		sets = append(sets, "agent_session_id = agent_session_id")
	}
	args = append(args, agentSessionID)

	res, err := s.db.ExecContext(ctx,
		`UPDATE agent_chats SET `+strings.Join(sets, ", ")+` WHERE agent_session_id = ?`,
		args...,
	)
	if err != nil {
		return fmt.Errorf("update agent_chat resume rebind: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("update agent_chat resume rebind rows affected: %w", err)
	} else if n == 0 {
		return fmt.Errorf("%w for agent_session_id %q", ErrAgentChatNotFound, agentSessionID)
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

// DeleteByAgentSessionID removes a chat AND the durable proxy-token row that
// pointed at it (BOS-979), in one transaction.
//
// The token delete is not a cascade: proxy_tokens.agent_session_id carries no
// foreign key, so nothing deletes the token row on our behalf. The two
// statements are issued together instead — a chat whose row is gone but whose
// token still resolves would let a rebuild point a live pane at a chat that no
// longer exists.
func (s *SQLiteAgentChatStore) DeleteByAgentSessionID(ctx context.Context, agentSessionID string) error {
	conn, err := beginImmediate(ctx, s.db, "agent_chat")
	if err != nil {
		return err
	}
	committed := false
	defer closeImmediate(ctx, conn, &committed)

	if _, err := conn.ExecContext(ctx,
		`DELETE FROM agent_chats WHERE agent_session_id = ?`, agentSessionID); err != nil {
		return fmt.Errorf("delete agent_chat by agent_session_id: %w", err)
	}
	if _, err := conn.ExecContext(ctx,
		`DELETE FROM proxy_tokens WHERE agent_session_id = ?`, agentSessionID); err != nil {
		return fmt.Errorf("delete proxy_tokens by agent_session_id: %w", err)
	}

	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("commit agent_chat delete: %w", err)
	}
	committed = true
	return nil
}

func (s *SQLiteAgentChatStore) ListWithTmuxSession(ctx context.Context) ([]*models.AgentChat, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id, agent_session_id, provider_session_id, agent_name, title, daemon_id, tmux_session_name, start_error, account_id, model, created_at
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
		`SELECT id, session_id, agent_session_id, provider_session_id, agent_name, title, daemon_id, tmux_session_name, start_error, account_id, model, created_at
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
	if err := rows.Scan(&c.ID, &c.SessionID, &c.AgentSessionID, &c.ProviderSessionID, &c.AgentName, &c.Title, &c.DaemonID, &c.TmuxSessionName, &c.StartError, &c.AccountID, &c.Model, &createdAt); err != nil {
		return nil, fmt.Errorf("scan agent_chat: %w", err)
	}
	c.CreatedAt = sqlutil.ParseTime(createdAt)
	return &c, nil
}
