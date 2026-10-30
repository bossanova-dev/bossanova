package domain

import (
	"errors"
	"strings"
)

// ErrBearerUnavailable marks a TRANSIENT failure to resolve the bound account's
// OAuth bearer — the account is bound, but its bearer could not be materialized
// right now (e.g. the claude plugin subprocess is restarting, so its
// MaterializeAgentAccount RPC is briefly unreachable). CurrentBearer returns it,
// wrapped, only after exhausting a short retry; the failover proxy maps it to a
// retryable 5xx so the request survives the blip instead of forwarding the
// sentinel x-api-key (a guaranteed upstream 401 that would kill the session).
// It is distinct from CurrentBearer's ("", nil) "genuinely unbound" result,
// whose sentinel 401 legitimately passes through. Never carries the token.
var ErrBearerUnavailable = errors.New("bound account bearer temporarily unavailable")

// SentinelAPIKey is a FIXED, non-credential marker injected as ANTHROPIC_API_KEY
// for proxied Claude sessions (BOS-326). The interactive Claude REPL ignores
// CLAUDE_CODE_OAUTH_TOKEN and authenticates only from the keychain unless an
// ANTHROPIC_API_KEY is present; setting this key overrides the keychain and
// forces every request through ANTHROPIC_BASE_URL (the loopback failover proxy)
// as an x-api-key request. The proxy then strips it and substitutes the bound
// account's OAuth bearer (CurrentBearer), so the sentinel never reaches upstream
// and billing stays on the subscription. Its last 20 characters must match the
// pre-approved entry seeded into ~/.claude.json (see apikey_approval.go). It is
// NOT a secret — it authorizes nothing upstream — but treat it as opaque config.
const SentinelAPIKey = "sk-ant-boss-managed-rotation-sentinel-00000000000000"

const proxyChatTargetPrefix = "chat\x00"

// ProxyTargetForChat returns the opaque failover-proxy target id for a tmux chat
// path token. fallbackAccountID is the account resolved at spawn time, used only
// before the chat binding has been durably persisted.
func ProxyTargetForChat(agentSessionID, fallbackAccountID string) string {
	return proxyChatTargetPrefix + agentSessionID + "\x00" + fallbackAccountID
}

// ParseProxyChatTarget decodes a chat target, or returns ok=false for a session target.
func ParseProxyChatTarget(targetID string) (agentSessionID, fallbackAccountID string, ok bool) {
	if !strings.HasPrefix(targetID, proxyChatTargetPrefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(targetID, proxyChatTargetPrefix)
	agentSessionID, fallbackAccountID, ok = strings.Cut(rest, "\x00")
	return agentSessionID, fallbackAccountID, ok && agentSessionID != ""
}

const proxyCompanionTargetPrefix = "companion\x00"

// ProxyTargetForCompanion returns a stable target for a session's Claude child processes.
// The account binding is stored separately, so failover never changes this target.
func ProxyTargetForCompanion(sessionID string) string {
	return proxyCompanionTargetPrefix + sessionID
}

// ParseProxyCompanionTarget decodes a companion target.
func ParseProxyCompanionTarget(targetID string) (sessionID string, ok bool) {
	if !strings.HasPrefix(targetID, proxyCompanionTargetPrefix) {
		return "", false
	}
	sessionID = strings.TrimPrefix(targetID, proxyCompanionTargetPrefix)
	return sessionID, sessionID != "" && !strings.ContainsRune(sessionID, '\x00')
}

// FailoverResult carries a proxy failover decision from PrepareFailover to the
// replay + CommitFailover. Token is the next account's bearer — a SECRET that
// must never be logged; it lives only long enough for the proxy to rewrite the
// Authorization header.
type FailoverResult struct {
	Rotate        bool   // false ⇒ pass the original upstream response through unchanged
	Token         string // next account's CLAUDE_CODE_OAUTH_TOKEN bearer (secret)
	NextAccountID string
	FromAccountID string
	Provider      string
	Trigger       string // ROTATION_TRIGGER_USAGE_LIMITED (429) | ROTATION_TRIGGER_AUTH_INVALIDATED (401)
}

// IsCanonicalProxyToken reports whether token is exactly 64 lowercase hex chars
// — the shape mintProxyToken emits (32 random bytes, hex-encoded).
//
// This is the SINGLE definition of that shape. The failover proxy uses it as a
// cheap pre-gate on its unknown-token branch (BOS-982), which is attacker
// reachable: an arbitrary garbage token costs this loop and nothing more — no
// durable read, no tmux exec. It is not a security boundary there either; the
// real check is the byte-exact constant-time comparison against the pane's own
// baked URL in RepairProxyPane. Keeping one definition is what stops the
// adoption sweep and that pre-gate from drifting into disagreeing about which
// tokens are ours.
func IsCanonicalProxyToken(token string) bool {
	if len(token) != 64 {
		return false
	}
	for i := 0; i < len(token); i++ {
		c := token[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return false
	}
	return true
}

// PassthroughClassCount is one (class, count) pair in a snapshot.
type PassthroughClassCount struct {
	Class string
	Count int
}

// PassthroughSessionSnapshot is one display session's pass-through error totals
// over the retained window, classes sorted by count (desc) then name.
type PassthroughSessionSnapshot struct {
	DisplayID string
	Total     int
	Classes   []PassthroughClassCount
}
