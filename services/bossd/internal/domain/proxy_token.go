package domain

import "time"

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
// well as its token index, which needs the parts. ProxyTargetForChat
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
