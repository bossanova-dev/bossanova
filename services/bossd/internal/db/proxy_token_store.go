package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/recurser/bossalib/contract/cloud/sqlutil"

	"github.com/recurser/bossd/internal/db/sqlcgen"
	"github.com/recurser/bossd/internal/domain"
)

var _ ProxyTokenStore = (*SQLiteProxyTokenStore)(nil)

// ProxyTokenRecord is the domain token registration.
type ProxyTokenRecord = domain.ProxyTokenRecord

// SQLiteProxyTokenStore implements ProxyTokenStore using SQLite.
type SQLiteProxyTokenStore struct {
	db *sql.DB
}

// NewProxyTokenStore creates a new SQLite-backed ProxyTokenStore.
func NewProxyTokenStore(db *sql.DB) *SQLiteProxyTokenStore {
	return &SQLiteProxyTokenStore{db: db}
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
	err := sqlcgen.New(s.db).UpsertProxyToken(ctx, sqlcgen.UpsertProxyTokenParams{
		TokenSha256: rec.TokenSHA256, SessionID: rec.SessionID,
		AgentSessionID: optionalText(rec.AgentSessionID), AccountID: optionalText(rec.AccountID),
		IsChatShaped: rec.IsChatShaped, CreatedAt: sqlutil.TimeNow(),
	})
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
	rows, err := sqlcgen.New(s.db).ListProxyTokens(ctx)
	if err != nil {
		return nil, fmt.Errorf("list proxy_tokens: %w", err)
	}
	return mapRows(rows, proxyTokenFromRow), nil
}

func proxyTokenFromRow(row sqlcgen.ProxyToken) domain.ProxyTokenRecord {
	return domain.ProxyTokenRecord{
		TokenSHA256: row.TokenSha256, SessionID: row.SessionID,
		AgentSessionID: textValue(row.AgentSessionID), AccountID: textValue(row.AccountID),
		IsChatShaped: row.IsChatShaped, CreatedAt: textTime(row.CreatedAt),
	}
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
	row, err := sqlcgen.New(s.db).GetProxyTokenByTokenHash(ctx, tokenSHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get proxy_token by hash: %w", err)
	}
	rec := proxyTokenFromRow(row)
	return &rec, nil
}

// DeleteBySessionID removes every token registered for a session — its
// session-shaped token and each of its chats' tokens, which all carry the same
// session_id. Idempotent: deleting an unknown session is a nil no-op.
func (s *SQLiteProxyTokenStore) DeleteBySessionID(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	if err := sqlcgen.New(s.db).DeleteProxyTokensBySessionID(ctx, sessionID); err != nil {
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
	if err := sqlcgen.New(s.db).DeleteProxyTokensByAgentSessionID(ctx, &agentSessionID); err != nil {
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
	res, err := sqlcgen.New(s.db).DeleteProxyTokensOlderThan(ctx, sqlutil.FormatTime(cutoff))
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
