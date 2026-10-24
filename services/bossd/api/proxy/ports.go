package proxy

import (
	"context"
	"time"

	"github.com/recurser/bossd/internal/domain"
)

// TokenStore persists the proxy registry without exposing its storage implementation.
type TokenStore interface {
	// Upsert writes a registration through, updating an existing row for the
	// same token hash rather than inserting a second one. Callers write through
	// on every registry mutation, including the target refresh that rewrites a
	// live chat token's fallback account.
	Upsert(ctx context.Context, rec domain.ProxyTokenRecord) error
	// List returns every registration, for the boot-time rebuild.
	List(ctx context.Context) ([]domain.ProxyTokenRecord, error)
	// GetByTokenHash returns the single registration a token digest resolves
	// to, or (nil, nil) when no row holds that digest. This is the primary-key
	// read the proxy's unknown-token path uses to recover a rejected pane's
	// identity (BOS-982); List would be an unbounded scan on an
	// attacker-reachable branch.
	GetByTokenHash(ctx context.Context, tokenSHA256 string) (*domain.ProxyTokenRecord, error)
	// DeleteBySessionID removes a session's own token and all of its chats'
	// tokens. Idempotent.
	DeleteBySessionID(ctx context.Context, sessionID string) error
	// DeleteByAgentSessionID removes one chat's token. Required because
	// proxy_tokens carries no foreign key onto agent_chats.agent_session_id
	// (deliberately, even after 20260904000000 made that column UNIQUE), so no
	// cascade covers the chat-only delete path. Idempotent.
	DeleteByAgentSessionID(ctx context.Context, agentSessionID string) error
	// DeleteOlderThan drops every row created before cutoff and reports how many
	// went away. This is what bounds the table: no other delete path runs for a
	// merely idle or archived session, so the boot-time rebuild prunes by age
	// before it reads.
	DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error)
}
