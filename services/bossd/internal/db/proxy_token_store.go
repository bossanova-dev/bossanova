package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/recurser/bossalib/sqlutil"
)

var _ ProxyTokenStore = (*SQLiteProxyTokenStore)(nil)

// ProxyTokenRecord is one durable failover-proxy path-token registration
// (BOS-979): the COMPONENTS of a proxy target, keyed by the hash of the token
// that resolves to it.
//
// TokenSHA256 is hex(sha256(token)) — never the token. A raw path token is a
// secret and must not reach the database, a log line, or a test fixture; the
// proxy hashes the presented token and looks the digest up.
//
// The assembled target string is deliberately absent. It embeds NUL bytes, and
// a boot-time rebuild has to repopulate the proxy's chat and session indexes as
// well as its token index, which needs the parts. session.ProxyTargetForChat
// stays the single author of the wire format.
type ProxyTokenRecord struct {
	// TokenSHA256 is hex(sha256(raw token)), the row's primary key.
	TokenSHA256 string
	// SessionID is the owning session. Its row cascades this one away.
	SessionID string
	// AgentSessionID is set only for a chat-shaped token; empty otherwise.
	AgentSessionID string
	// AccountID is the chat's fallback account at mint/refresh time. Empty is a
	// legitimate value (the system-default account) and round-trips as empty.
	AccountID string
	// IsChatShaped distinguishes a chat target from a bare session target.
	IsChatShaped bool
	// CreatedAt is when the row was first written; an Upsert of an existing
	// token preserves it.
	CreatedAt time.Time
}

// SQLiteProxyTokenStore implements ProxyTokenStore using SQLite.
type SQLiteProxyTokenStore struct {
	db *sql.DB
}

// NewProxyTokenStore creates a new SQLite-backed ProxyTokenStore.
func NewProxyTokenStore(db *sql.DB) *SQLiteProxyTokenStore {
	return &SQLiteProxyTokenStore{db: db}
}

// nullableText maps Go's empty string onto SQL NULL, so a session-shaped row
// stores NULL for the chat-only columns rather than an empty string that reads as a
// real (empty) agent session. The read path inverts it.
func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Upsert writes a token registration through, replacing the target components
// of an existing row rather than inserting a second one for the same token.
//
// The update arm is load-bearing, not defensive: TokenForChat's existing-token
// branch rewrites a live token's target when the chat's fallback account
// changes, so the same digest legitimately arrives twice with a different
// account_id. Inserting instead of updating there would leave the rebuild
// resolving to the account the token had at MINT time. created_at is left
// alone so a refreshed row keeps its original age.
func (s *SQLiteProxyTokenStore) Upsert(ctx context.Context, rec ProxyTokenRecord) error {
	if rec.TokenSHA256 == "" {
		return fmt.Errorf("upsert proxy_token: token hash is required")
	}
	if rec.SessionID == "" {
		return fmt.Errorf("upsert proxy_token: session id is required")
	}
	if rec.IsChatShaped && rec.AgentSessionID == "" {
		return fmt.Errorf("upsert proxy_token: chat-shaped token requires an agent session id")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO proxy_tokens (token_sha256, session_id, agent_session_id, account_id, is_chat_shaped, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(token_sha256) DO UPDATE SET
		   session_id       = excluded.session_id,
		   agent_session_id = excluded.agent_session_id,
		   account_id       = excluded.account_id,
		   is_chat_shaped   = excluded.is_chat_shaped`,
		rec.TokenSHA256, rec.SessionID, nullableText(rec.AgentSessionID),
		nullableText(rec.AccountID), rec.IsChatShaped, sqlutil.TimeNow(),
	)
	if err != nil {
		return fmt.Errorf("upsert proxy_token: %w", err)
	}
	return nil
}

// List returns every persisted token registration. This is the boot-time
// rebuild's read: the daemon has no way to enumerate live panes, so it
// repopulates from the whole table and lets the FK cascade (plus the explicit
// chat delete) keep that table honest.
func (s *SQLiteProxyTokenStore) List(ctx context.Context) ([]ProxyTokenRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT token_sha256, session_id, agent_session_id, account_id, is_chat_shaped, created_at
		 FROM proxy_tokens
		 ORDER BY created_at, token_sha256`,
	)
	if err != nil {
		return nil, fmt.Errorf("list proxy_tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ProxyTokenRecord
	for rows.Next() {
		var (
			rec       ProxyTokenRecord
			agentID   sql.NullString
			accountID sql.NullString
			createdAt string
		)
		if err := rows.Scan(&rec.TokenSHA256, &rec.SessionID, &agentID, &accountID, &rec.IsChatShaped, &createdAt); err != nil {
			return nil, fmt.Errorf("scan proxy_token: %w", err)
		}
		rec.AgentSessionID = agentID.String
		rec.AccountID = accountID.String
		rec.CreatedAt = sqlutil.ParseTime(createdAt)
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list proxy_tokens: %w", err)
	}
	return out, nil
}

// GetByTokenHash resolves one token digest to its registration, or (nil, nil)
// when no row holds it.
//
// This is a primary-key read, deliberately not a List-and-scan: its caller is
// the proxy's unknown-token 401 branch (BOS-982), which is reachable by any
// local process presenting an arbitrary path token, so the work a miss costs
// must not grow with the size of the table.
func (s *SQLiteProxyTokenStore) GetByTokenHash(ctx context.Context, tokenSHA256 string) (*ProxyTokenRecord, error) {
	if tokenSHA256 == "" {
		return nil, nil
	}
	var (
		rec       ProxyTokenRecord
		agentID   sql.NullString
		accountID sql.NullString
		createdAt string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT token_sha256, session_id, agent_session_id, account_id, is_chat_shaped, created_at
		 FROM proxy_tokens
		 WHERE token_sha256 = ?`, tokenSHA256,
	).Scan(&rec.TokenSHA256, &rec.SessionID, &agentID, &accountID, &rec.IsChatShaped, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get proxy_token by hash: %w", err)
	}
	rec.AgentSessionID = agentID.String
	rec.AccountID = accountID.String
	rec.CreatedAt = sqlutil.ParseTime(createdAt)
	return &rec, nil
}

// DeleteBySessionID removes every token registered for a session — its
// session-shaped token and each of its chats' tokens, which all carry the same
// session_id. Idempotent: deleting an unknown session is a nil no-op.
func (s *SQLiteProxyTokenStore) DeleteBySessionID(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM proxy_tokens WHERE session_id = ?`, sessionID,
	); err != nil {
		return fmt.Errorf("delete proxy_tokens by session_id: %w", err)
	}
	return nil
}

// DeleteByAgentSessionID removes a single chat's token registration. This is
// the eviction the schema does not express as a cascade: proxy_tokens carries no
// foreign key onto agent_chats.agent_session_id, so deleting the chat row alone
// would leave the token behind. The column was not a legal parent key when this
// table was added; 20260904000000 made it UNIQUE without adding the FK, so the
// explicit delete below is now a deliberate choice rather than a workaround --
// and it is still the only thing that evicts the row. Idempotent.
func (s *SQLiteProxyTokenStore) DeleteByAgentSessionID(ctx context.Context, agentSessionID string) error {
	if agentSessionID == "" {
		return nil
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM proxy_tokens WHERE agent_session_id = ?`, agentSessionID,
	); err != nil {
		return fmt.Errorf("delete proxy_tokens by agent_session_id: %w", err)
	}
	return nil
}

// DeleteOlderThan removes every registration first written before cutoff and
// returns how many rows went away.
//
// This is the only bound on the table. `Deregister` has no production caller, a
// session's `archived_at` is a soft delete that keeps its rows, and a HARD
// session delete is rare — so without an age prune the table (and the registry
// rebuilt from it) grows monotonically with every session ever spawned, and a
// token minted for a long-dead pane resolves forever. The comparison is done in
// SQL against the canonical `sqlutil.FormatTime` layout, which is
// lexicographically ordered, so a string `<` is a chronological `<`.
//
// Idempotent, and safe to call when nothing is expired.
func (s *SQLiteProxyTokenStore) DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM proxy_tokens WHERE created_at < ?`, sqlutil.FormatTime(cutoff),
	)
	if err != nil {
		return 0, fmt.Errorf("delete proxy_tokens older than cutoff: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// The delete committed; only the count is unavailable. Report it as a
		// successful prune of an unknown size rather than failing the caller.
		return 0, nil
	}
	return n, nil
}
