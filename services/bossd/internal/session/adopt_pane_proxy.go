package session

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/tmux"
)

// SetDefaultAccountResolver injects the managed-default account resolver used by
// the startup adoption sweep (BOS-481) for panes whose chat and session both
// lack a persisted account binding. Wired in production to
// account.Resolver.DefaultAccountID. Optional: nil ⇒ both-nil rows are skipped.
func (l *Lifecycle) SetDefaultAccountResolver(r func(ctx context.Context, provider string, now time.Time) (string, error)) {
	l.defaultAccountResolver = r
}

// parseBakedProxyToken extracts the failover-proxy path token from a surviving
// pane's baked ANTHROPIC_BASE_URL, accepting ONLY the exact canonical shape this
// daemon itself bakes: scheme http, host EXACTLY 127.0.0.1, port EXACTLY the
// live proxy port, and path EXACTLY /s/<64-lowercase-hex>. No userinfo, query,
// fragment, or extra path segments. Anything else yields ("", false).
//
// This is deliberately STRICTER than stalePaneProxyPort (which tolerantly reads
// only the port to surface a mismatch): here the token is re-registered as a
// live proxy capability against a managed account, and the tmux session env is
// writable by any process that can run `tmux set-environment`. A permissive
// parse would let such a process register an arbitrary token, so the URL must
// match the canonical bake byte-for-byte. A port mismatch returns false too —
// those panes are the stale-port sweep's job, not ours.
func parseBakedProxyToken(bakedURL string, livePort int) (string, bool) {
	if bakedURL == "" || livePort == 0 {
		return "", false
	}
	u, err := url.Parse(bakedURL)
	if err != nil {
		return "", false
	}
	if u.Scheme != "http" || u.User != nil || u.Opaque != "" {
		return "", false
	}
	if u.Hostname() != "127.0.0.1" || u.Port() != strconv.Itoa(livePort) {
		return "", false
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" {
		return "", false
	}
	// Reject any percent-encoded path: the daemon bakes an unescaped
	// /s/<hex> path, so a canonical URL always has an empty RawPath. A
	// non-empty RawPath means the path required escaping (e.g. /s%2f<tok> or
	// /s/<tok-with-%61>), which decodes to a canonical-looking Path and would
	// otherwise slip past the byte-for-byte contract. tmux env is
	// attacker-influenceable, so enforce the exact bytes the daemon emits.
	if u.RawPath != "" {
		return "", false
	}
	const prefix = "/s/"
	if !strings.HasPrefix(u.Path, prefix) {
		return "", false
	}
	token := strings.TrimPrefix(u.Path, prefix)
	if !isCanonicalProxyToken(token) {
		return "", false
	}
	return token, true
}

// isCanonicalProxyToken reports whether token is exactly 64 lowercase hex chars
// — the shape mintProxyToken emits (32 random bytes, hex-encoded).
func isCanonicalProxyToken(token string) bool {
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

// sameAgentSessionChat reports whether a chat runs the same agent provider as
// its owning session, mirroring the server-side spawn discriminator
// (server/account_binding.go). When true and the chat has no account of its own,
// the chat inherits the session's account and takes the SESSION token shape.
func sameAgentSessionChat(sess *models.Session, chat *models.AgentChat) bool {
	return sess != nil && chat != nil && proxyAgentName(sess.AgentName) == proxyAgentName(chat.AgentName)
}

// resolveDefaultAccountID resolves the managed default account for a pane whose
// chat and session both lack a binding, mirroring the spawn's DefaultAccountID
// call. Returns "" (⇒ row skipped) when no resolver is wired or it errors — an
// empty account means the pane never received a proxy URL, so there is nothing
// to re-adopt.
func (l *Lifecycle) resolveDefaultAccountID(ctx context.Context, agentName string) string {
	if l.defaultAccountResolver == nil {
		return ""
	}
	id, err := l.defaultAccountResolver(ctx, proxyAgentName(agentName), time.Now())
	if err != nil {
		l.logger.Warn().Err(err).
			Str("agent", agentName).
			Msg("failover proxy adopt: resolve default account failed; skipping row")
		return ""
	}
	return id
}

// adoptSurvivingPaneProxyTokens is a best-effort startup sweep (BOS-481) that
// re-registers each surviving managed tmux pane's frozen failover-proxy token
// onto the fresh ProxyServer. After a daemon restart the fixed proxy port
// (BOS-409) lets a pane's baked ANTHROPIC_BASE_URL reach the new proxy, but the
// new proxy's token registry is empty, so the pane's /s/<token> path is unknown
// → 401 → the Claude REPL wedges on "Please run /login". Reconstructing the
// token from the pane env and re-registering it (never minting, never
// persisting) lets the pane reconnect in place with no respawn and no secret at
// rest.
//
// It mirrors detectStalePaneProxyPorts' iteration and per-row failure semantics:
// a ListWithTmuxSession error aborts the whole sweep with one Warn; a single bad
// row (session load miss, dead pane, non-canonical URL) is skipped without
// blocking the rest. The sweep is inert when the proxy is off/unbound, when no
// registrar is wired, or when there are no chats. For each live Claude pane it
// mirrors the spawn's account-shape discriminator exactly so the adopted token's
// target matches what the original spawn registered.
func (l *Lifecycle) adoptSurvivingPaneProxyTokens(ctx context.Context) {
	if l.agentChats == nil || l.proxyRegistrar == nil {
		return
	}
	if !l.failoverProxyEnabled() || l.proxyPort == 0 {
		return
	}
	chats, err := l.agentChats.ListWithTmuxSession(ctx)
	if err != nil {
		l.logger.Warn().Err(err).Msg("bootstrap adopt-proxy sweep: failed to list chats with tmux session")
		return
	}
	adopted := 0
	for _, chat := range chats {
		if chat == nil || chat.AgentSessionID == "" {
			continue
		}
		// Only Claude sessions ever get an injected ANTHROPIC_BASE_URL.
		if proxyAgentName(chat.AgentName) != string(models.AccountProviderClaude) {
			continue
		}
		// The session is always needed here (unlike the stale-port sweep) because
		// the token-shape discriminator reads sess.AgentName / sess.AccountID.
		sess, serr := l.sessions.Get(ctx, chat.SessionID)
		if serr != nil {
			l.logger.Warn().Err(serr).
				Str("agent_session", chat.AgentSessionID).
				Str("session", chat.SessionID).
				Msg("bootstrap adopt-proxy sweep: failed to load session; skipping")
			continue
		}

		tmuxName := ""
		if chat.TmuxSessionName != nil && *chat.TmuxSessionName != "" {
			tmuxName = *chat.TmuxSessionName
		} else {
			tmuxName = tmux.ChatSessionName(sess.RepoID, chat.AgentSessionID)
		}
		if tmuxName == "" || l.tmux == nil || !l.tmux.HasSession(ctx, tmuxName) {
			continue
		}
		baked, ok := l.tmux.ShowEnv(ctx, tmuxName, "ANTHROPIC_BASE_URL")
		if !ok || baked == "" {
			continue
		}
		token, ok := parseBakedProxyToken(baked, l.proxyPort)
		if !ok {
			// Port mismatch or non-canonical URL: left to detectStalePaneProxyPorts.
			continue
		}

		// Mirror the spawn's account resolution (server/account_binding.go): the
		// chat's own account wins; else same-agent chats inherit the session's
		// account (or the managed default when the session too is unbound); else a
		// cross-agent chat uses its provider's managed default.
		accountID := ""
		switch {
		case chat.AccountID != nil:
			accountID = *chat.AccountID
		case sameAgentSessionChat(sess, chat):
			if sess.AccountID != nil {
				accountID = *sess.AccountID
			} else {
				accountID = l.resolveDefaultAccountID(ctx, chat.AgentName)
			}
		default:
			accountID = l.resolveDefaultAccountID(ctx, chat.AgentName)
		}
		if accountID == "" {
			// A genuinely unmanaged pane never received a proxy URL, so there is
			// nothing to adopt.
			continue
		}

		// Mirror the spawn token-shape discriminator: a same-agent chat that
		// inherited the session's account takes the SESSION token shape; every
		// other case takes the CHAT shape.
		if sameAgentSessionChat(sess, chat) && chat.AccountID == nil && sess.AccountID != nil {
			l.proxyRegistrar.AdoptToken(token, sess.ID)
		} else {
			l.proxyRegistrar.AdoptTokenForChat(sess.ID, chat.AgentSessionID, accountID, token)
		}
		adopted++
	}
	if adopted > 0 {
		l.logger.Info().Int("adopted", adopted).
			Msg("failover proxy: re-adopted surviving tmux pane proxy tokens after daemon restart")
	}
}
