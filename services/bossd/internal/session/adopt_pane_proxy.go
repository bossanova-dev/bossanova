package session

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"

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

// anthropicBaseURLEnv is the tmux session environment variable the daemon bakes
// the failover-proxy URL into. Named once because three separate readers ask a
// pane for it — the adoption sweep, the stale-port sweep, and unknown-token
// attribution — and a typo in any one of them would read as "this pane has no
// proxy URL", which every one of those readers treats as a benign skip.
const anthropicBaseURLEnv = "ANTHROPIC_BASE_URL"

// persistedChatPaneName returns the chat's persisted tmux session name, or ""
// when the row does not carry one. An empty result is NOT "no pane": every
// caller falls back to deriving the name via tmux.ChatSessionName, because a
// chat whose tmux name was never persisted (or was cleared) still has a live
// pane holding a baked token. They differ only in how they obtain the owning
// session for that derivation, which is why the fallback itself is not shared.
func persistedChatPaneName(chat *models.AgentChat) string {
	if chat == nil || chat.TmuxSessionName == nil {
		return ""
	}
	return *chat.TmuxSessionName
}

// paneIsLive reports whether a named tmux pane exists right now. A pane that is
// gone can hold no baked token, so every reader below stops here rather than
// attributing anything to it.
func (l *Lifecycle) paneIsLive(ctx context.Context, tmuxName string) bool {
	return tmuxName != "" && l.tmux != nil && l.tmux.HasSession(ctx, tmuxName)
}

// paneBakedProxyURL returns the failover-proxy URL baked into a live pane's tmux
// session environment, and whether it has one at all. An unset variable and an
// empty one are the same answer: nothing to parse.
//
// It deliberately does NOT interpret the value. The two readers want different
// things from it — the adoption sweep needs the strict canonical parse
// (parseBakedProxyToken), the stale-port sweep the deliberately tolerant one
// (stalePaneProxyPort) — and collapsing that difference is what this helper must
// not do.
func (l *Lifecycle) paneBakedProxyURL(ctx context.Context, tmuxName string) (string, bool) {
	if l.tmux == nil {
		return "", false
	}
	baked, ok := l.tmux.ShowEnv(ctx, tmuxName, anthropicBaseURLEnv)
	if !ok || baked == "" {
		return "", false
	}
	return baked, true
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
	if !IsCanonicalProxyToken(token) {
		return "", false
	}
	return token, true
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
// a list error aborts the whole sweep with one Warn; a single bad row (session
// load miss, dead pane, non-canonical URL) is skipped without blocking the rest.
// The sweep is inert when the proxy is off/unbound, when no registrar is wired,
// or when there are no chats. For each live Claude pane it mirrors the spawn's
// account-shape discriminator exactly so the adopted token's target matches what
// the original spawn registered.
//
// SINCE BOS-979 THIS IS A FALLBACK, NOT THE PRIMARY RECOVERY PATH. Bootstrap
// rebuilds the registry from durable rows first (rebuildProxyTokenRegistry), and
// a persisted row is authoritative because the daemon that minted the token
// wrote it. What is left for this sweep is exactly one population: panes spawned
// BEFORE the proxy_tokens table existed, which therefore have no row and can
// only be recovered by reading the token back out of tmux env. Every such pane
// dies with its daemon generation, so once no pre-migration pane can still be
// running — one full restart cycle past the BOS-979 release — this whole file
// and its registrar Adopt* entry points can be deleted. Tracked as the BOS-979
// follow-up "retire the tmux-env pane adoption sweep".
//
// FAILURE VISIBILITY: every early `continue` below increments a named counter,
// and the terminal log line emits the whole tally even when nothing was adopted.
// Six of these branches used to be silent, which made "there was nothing to
// adopt" and "the sweep could not evaluate a single row" produce byte-identical
// output — the exact ambiguity that made the original incident hard to read.
func (l *Lifecycle) adoptSurvivingPaneProxyTokens(ctx context.Context) {
	if l.agentChats == nil || l.proxyRegistrar == nil {
		return
	}
	if !l.failoverProxyEnabled() || l.proxyPort == 0 {
		return
	}
	// ListRoutableChats rather than ListWithTmuxSession: the latter filters out
	// every chat whose tmux_session_name is NULL, so the tmux.ChatSessionName
	// fallback below — written to recover exactly those rows — could never run.
	// A tmux chat whose name was never persisted (or was cleared) still has a
	// live pane holding a baked token, and that pane is precisely the one this
	// sweep exists for. The wider predicate also admits headless runs, which are
	// bounded and fall out cheaply at the has-session check.
	chats, err := l.agentChats.ListRoutableChats(ctx)
	if err != nil {
		l.logger.Warn().Err(err).Msg("bootstrap adopt-proxy sweep: failed to list routable chats")
		return
	}
	adopted := 0
	skips := adoptSweepSkips{}
	for _, chat := range chats {
		if chat == nil || chat.AgentSessionID == "" {
			skips.malformedRow++
			continue
		}
		// Only Claude sessions ever get an injected ANTHROPIC_BASE_URL.
		if proxyAgentName(chat.AgentName) != string(models.AccountProviderClaude) {
			skips.notClaude++
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
			skips.sessionLoadFailed++
			continue
		}

		tmuxName := persistedChatPaneName(chat)
		if tmuxName == "" {
			tmuxName = tmux.ChatSessionName(sess.RepoID, chat.AgentSessionID)
		}
		if !l.paneIsLive(ctx, tmuxName) {
			skips.noLivePane++
			continue
		}
		baked, ok := l.paneBakedProxyURL(ctx, tmuxName)
		if !ok {
			skips.noBakedURL++
			continue
		}
		token, ok := parseBakedProxyToken(baked, l.proxyPort)
		if !ok {
			// Port mismatch or non-canonical URL: left to detectStalePaneProxyPorts.
			skips.uncanonicalURL++
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
			skips.noAccount++
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
	// Emitted unconditionally, unlike the old `adopted > 0` guard: a sweep that
	// adopted nothing is the interesting case, and the tally is what separates
	// "no pane needed adopting" from "every row was skipped before it could be
	// evaluated". Held to Debug when there was nothing at all to look at, so a
	// daemon with no chats does not add a line to every start.
	ev := l.logger.Info()
	if adopted == 0 && skips.total() == 0 {
		ev = l.logger.Debug()
	}
	skips.attach(ev.Int("adopted", adopted).Int("rows", len(chats))).
		Msg("failover proxy: surviving-pane token adoption sweep complete (fallback path; the durable registry rebuild runs first)")
}

// adoptSweepSkips tallies why the sweep passed over a row. Counters only — no
// token, account id, or session id — so the whole tally is safe to log verbatim.
type adoptSweepSkips struct {
	// malformedRow: a nil row or one with no agent_session_id.
	malformedRow int
	// notClaude: a non-Claude agent, which never receives a proxy URL.
	notClaude int
	// sessionLoadFailed: the owning session could not be read, so the token
	// shape could not be decided. The only branch that ALSO logs per row.
	sessionLoadFailed int
	// noLivePane: no resolvable tmux name, no tmux client, or the pane is gone.
	noLivePane int
	// noBakedURL: a live pane with no ANTHROPIC_BASE_URL in its env.
	noBakedURL int
	// uncanonicalURL: a baked URL that is not this daemon's exact bake — a stale
	// port, or a shape that failed the byte-for-byte contract.
	uncanonicalURL int
	// noAccount: no account could be resolved, so the pane was never managed.
	noAccount int
}

func (s adoptSweepSkips) total() int {
	return s.malformedRow + s.notClaude + s.sessionLoadFailed + s.noLivePane +
		s.noBakedURL + s.uncanonicalURL + s.noAccount
}

// attach folds the tally onto a log event. Every reason is emitted, including
// zeros, so an operator diffing two boot logs sees a counter move rather than a
// field appear.
func (s adoptSweepSkips) attach(ev *zerolog.Event) *zerolog.Event {
	return ev.
		Int("skip_malformed_row", s.malformedRow).
		Int("skip_not_claude", s.notClaude).
		Int("skip_session_load_failed", s.sessionLoadFailed).
		Int("skip_no_live_pane", s.noLivePane).
		Int("skip_no_baked_url", s.noBakedURL).
		Int("skip_uncanonical_url", s.uncanonicalURL).
		Int("skip_no_account", s.noAccount)
}
