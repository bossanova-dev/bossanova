package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/agenterr"
	"github.com/recurser/bossd/internal/rotation"
	"github.com/recurser/bossd/internal/session"
)

// *session.Lifecycle is the production Failover implementation.
var _ Failover = (*session.Lifecycle)(nil)

// defaultUpstream is the real Anthropic API the proxy forwards to over TLS.
const defaultUpstream = "https://api.anthropic.com"

// maxBufferedBody bounds how large a request body the proxy buffers to enable a
// replay after an account swap. A body larger than this is streamed straight
// through WITHOUT offering failover (buffering it would add unbounded memory +
// latency to the hot path). Claude request bodies (a prompt + context) sit far
// below this.
const maxBufferedBody = 8 << 20 // 8 MiB

// proxyReadHeaderTimeout bounds slowloris exposure on the loopback listener.
const proxyReadHeaderTimeout = 30 * time.Second

// commitTimeout bounds the detached post-replay rebind+audit persist so a hung
// store can't leak a goroutine after the client leg is done.
const commitTimeout = 10 * time.Second

// sseErrorPeekByteCap bounds how many leading bytes of a 200 text/event-stream
// response the proxy buffers while deciding whether the stream opens with a
// rate_limit_error (rotate + replay) or ships content (pass through). Anthropic's
// opening frames (message_start / content_block_start / ping) are tiny, so this
// small cap comfortably holds the whole pre-content window; if the cap is reached
// before a decisive frame the peek FAILS SAFE to today's byte-identical
// pass-through rather than holding the stream. Overridable per-server for tests.
const sseErrorPeekByteCap = 64 << 10 // 64 KiB

// sseErrorPeekDeadline bounds how long the peek keeps buffering opening frames
// before it gives up and fails safe to pass-through, so a slow-first-token stream
// (e.g. long extended thinking) or a schema drift degrades to today's behaviour
// rather than buffering the pre-content window indefinitely. This is a
// BETWEEN-READS budget: it is evaluated at the top of the peek loop and bounds
// how long we keep buffering across successive reads (Anthropic emits periodic
// `ping` keepalive frames during a pre-content gap, so reads keep returning and
// the budget is honoured at ping granularity). It does NOT preempt a single
// in-flight origBody.Read that stalls mid-frame — that read is bounded only by
// the request context (client cancellation) exactly as today's pass-through
// copy loop is, so this introduces no new hang. Overridable per-server for tests.
const sseErrorPeekDeadline = 5 * time.Second

// sseFrameBoundary delimits Server-Sent-Event frames (a blank line). The peek
// splits the buffered prefix on this boundary and inspects one frame at a time.
// LF-only ("\n\n"): this matches Anthropic's Messages API wire format. A
// CRLF-framed stream ("\r\n\r\n", spec-valid but not emitted by the target API)
// yields no boundary match, so the peek simply fails safe to byte-identical
// pass-through (today's behaviour) rather than misdetecting — never a false
// rotate.
var sseFrameBoundary = []byte("\n\n")

// hopByHopHeaders are connection-scoped headers that must not be forwarded
// across the proxy (RFC 7230 §6.1).
var hopByHopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Transfer-Encoding",
	"Upgrade", "TE", "Trailer", "Proxy-Authenticate", "Proxy-Authorization",
}

// Failover is the narrow slice of *session.Lifecycle the proxy depends on to
// perform an account swap on a 429/401 upstream response. Defined here (the
// consumer) so tests can substitute a fake without a full Lifecycle. The
// session.FailoverResult it exchanges carries the next account's bearer, which
// is a secret the proxy holds only long enough to rewrite one header and which
// is never logged.
type Failover interface {
	// CurrentBearer returns the OAuth bearer for the session's CURRENTLY bound
	// account, or "" when unbound / disabled / unresolvable. Used to translate the
	// interactive REPL's sentinel x-api-key into the bound account's subscription
	// token on the FIRST leg (the REPL ignores CLAUDE_CODE_OAUTH_TOKEN and sends
	// only x-api-key). "" ⇒ forward the client's own header unchanged (fail-safe).
	// The returned token is a SECRET and is never logged.
	CurrentBearer(ctx context.Context, sessionID string) (string, error)
	// PrepareFailover decides + materializes the next account's bearer for a
	// session whose upstream request returned status (429/401). Rotate=false ⇒
	// pass the original response through unchanged. It never persists the
	// rebind (that is CommitFailover's job, post-replay).
	PrepareFailover(ctx context.Context, sessionID string, status int) (session.FailoverResult, error)
	// PrepareFailoverKind is PrepareFailover for a signal not carried by a bare
	// HTTP status — used for an account suspension (a 403 whose body confirms an
	// org/billing block) mapped to rotation.AuthInvalidated. Same contract as
	// PrepareFailover otherwise.
	PrepareFailoverKind(ctx context.Context, sessionID string, kind rotation.SignalKind, trigger string) (session.FailoverResult, error)
	// CommitFailover persists the session→account rebind + audit AFTER a
	// successful replay, so the persisted account is the one that served the
	// request.
	CommitFailover(ctx context.Context, sessionID string, r session.FailoverResult) error
}

// ProxyServer is a loopback-only reverse proxy that the Claude Code subprocess
// is pointed at via ANTHROPIC_BASE_URL (BOS-320, S7). It forwards to
// https://api.anthropic.com over real TLS; on a 429/401 it consults the
// rotation engine, swaps the Authorization bearer to the next account, and
// replays the buffered request — account failover with no tmux pane respawn.
//
// Security model (mirrors HookServer):
//   - Binds 127.0.0.1 only; external traffic cannot reach it. The inbound leg
//     is plain HTTP (loopback), the outbound leg is real TLS.
//   - Per-session path token (/s/<token>/...), constant-time compared.
//   - It terminates the user's own OAuth traffic and re-signs it with another
//     account's bearer — a deliberate in-process auth MITM. Token values are
//     NEVER logged. It is default-on (gated on managed_accounts.enabled +
//     failover_proxy_enabled, both default true) and still liveness-gated
//     upstream: bossd only injects ANTHROPIC_BASE_URL when both flags are on
//     and this server has bound (a real listener + registrar wired).
type ProxyServer struct {
	failover  Failover
	logger    zerolog.Logger
	upstream  *url.URL
	transport http.RoundTripper

	// ssePeekByteCap and ssePeekDeadline bound the 200-SSE rate-limit peek. They
	// default to the sseErrorPeekByteCap / sseErrorPeekDeadline constants and are
	// exposed as fields only so tests can drive the byte-cap / deadline backstops
	// deterministically (no wall-clock sleeps).
	ssePeekByteCap  int
	ssePeekDeadline time.Duration

	// port is the configured FIXED loopback port to bind (BOS-409); 0 ⇒
	// ephemeral. Listen falls back to ephemeral if the fixed port is held.
	port int

	listener net.Listener
	srv      *http.Server

	mu             sync.RWMutex
	tokenToSession map[string]string
	sessionToToken map[string]string
	chatToToken    map[string]string
	sessionChats   map[string]map[string]string
	// sessionBearer caches the bearer a session was last failed-over TO, so the
	// swap is STICKY: after one 429/401 swap, every later request forwards with
	// the swapped account's bearer on its FIRST leg. The proxied subprocess sends
	// only the BOS-326 sentinel x-api-key (its CLAUDE_CODE_OAUTH_TOKEN env is
	// stripped and there is no pane respawn), which is translated per request via
	// CurrentBearer. Without this cache the swapped bearer would depend on
	// CurrentBearer observing the sessions.account_id rebind, so a request racing
	// just ahead of the commit would re-forward the original capped account's
	// bearer, re-trip the 429, re-enter failover, and cool the healthy account it
	// just rotated to — cascading through the pool and recording one audit per
	// request. The cached bearer is a SECRET, held only in memory and never
	// logged; it is dropped on Deregister.
	sessionBearer map[string]string
}

// ProxyServerConfig gathers the ProxyServer dependencies.
type ProxyServerConfig struct {
	Failover Failover
	Logger   zerolog.Logger
	// Upstream overrides the forward target (default https://api.anthropic.com).
	// Tests point it at an httptest.Server.
	Upstream string
	// Transport overrides the outbound RoundTripper (default
	// http.DefaultTransport, which speaks TLS to the real upstream).
	Transport http.RoundTripper
	// Port is the FIXED loopback port to bind (BOS-409). When > 0 Listen binds
	// 127.0.0.1:<Port> with SO_REUSEADDR so a frozen ANTHROPIC_BASE_URL baked
	// into a live tmux pane survives a daemon restart; on a bind collision it
	// falls back to an ephemeral port (non-fatal). 0 keeps today's ephemeral
	// (":0") behavior as an explicit opt-out.
	Port int
}

// NewProxyServer constructs a ProxyServer. Call Listen to bind, then Serve
// (typically in a goroutine). Shutdown is safe after either.
func NewProxyServer(cfg ProxyServerConfig) (*ProxyServer, error) {
	raw := cfg.Upstream
	if raw == "" {
		raw = defaultUpstream
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse upstream %q: %w", raw, err)
	}
	transport := cfg.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &ProxyServer{
		failover:        cfg.Failover,
		logger:          cfg.Logger,
		upstream:        u,
		transport:       transport,
		port:            cfg.Port,
		ssePeekByteCap:  sseErrorPeekByteCap,
		ssePeekDeadline: sseErrorPeekDeadline,
		tokenToSession:  map[string]string{},
		sessionToToken:  map[string]string{},
		chatToToken:     map[string]string{},
		sessionChats:    map[string]map[string]string{},
		sessionBearer:   map[string]string{},
	}, nil
}

// Listen binds a loopback-only TCP listener and wires the catch-all proxy
// handler. Split from Serve so Shutdown can race safely.
//
// When a FIXED port is configured (p.port > 0) it binds 127.0.0.1:<port> with
// SO_REUSEADDR (and SO_REUSEPORT on Darwin) set on the listening socket so a
// frozen ANTHROPIC_BASE_URL baked into a live tmux pane survives a daemon
// restart (BOS-409). SO_REUSEADDR lets the *listening socket* re-bind over
// connections from the prior listener lingering in TIME_WAIT; on macOS that is
// not enough for an immediate same-port re-bind, so SO_REUSEPORT is also set
// there (load-bearing on Darwin — see reusableSocketControl). On the common
// single-daemon restart this re-binds the same fixed port; only a genuinely
// conflicting live bind (a non-Darwin listener, or a foreign process holding the
// port) reaches the EADDRINUSE fallback below, which drops to an ephemeral ":0"
// bind so bossd never fails to start over a port collision (mirrors main.go's
// non-fatal proxy posture). (Go's net already sets SO_REUSEADDR by default on
// unix listeners; the explicit Control makes that intent testable and adds
// SO_REUSEPORT on Darwin.)
//
// p.port == 0 keeps today's OS-assigned ephemeral bind as an explicit opt-out.
func (p *ProxyServer) Listen() error {
	lc := net.ListenConfig{Control: reusableSocketControl}

	var ln net.Listener
	var err error
	if p.port > 0 {
		addr := fmt.Sprintf("127.0.0.1:%d", p.port)
		ln, err = lc.Listen(context.Background(), "tcp", addr)
		if err != nil {
			// Non-fatal: the fixed port is held (EADDRINUSE) or otherwise
			// unbindable. Fall back to an ephemeral port so bossd still starts;
			// the startup re-point sweep flags any panes baked to a now-stale
			// port. Port() reports whatever actually bound.
			p.logger.Warn().Err(err).Int("configured_port", p.port).
				Msg("failover proxy: fixed port unavailable, falling back to an ephemeral port")
			ln, err = lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
		}
	} else {
		ln, err = lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	}
	if err != nil {
		return fmt.Errorf("listen tcp 127.0.0.1: %w", err)
	}
	p.listener = ln

	mux := http.NewServeMux()
	mux.HandleFunc("/", p.handleProxy)

	p.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: proxyReadHeaderTimeout,
	}
	return nil
}

// Serve blocks serving requests on the loopback listener. Returns
// http.ErrServerClosed on clean Shutdown.
func (p *ProxyServer) Serve() error {
	return p.srv.Serve(p.listener)
}

// Shutdown gracefully stops the server and releases the listening socket.
//
// http.Server.Shutdown only closes listeners it is tracking — i.e. those
// registered via Serve. When Listen ran but Serve did not (or Shutdown races
// Serve's registration), the bound socket would otherwise stay open, so an
// immediate re-bind of the same fixed port fails with EADDRINUSE. Closing
// p.listener explicitly guarantees the port is fully released, which is what
// makes the fixed-port re-bind after a daemon exit deterministic (BOS-409). The
// close is idempotent: when Serve already ran, http.Server has closed the
// listener and this second Close returns a benign "use of closed" error we
// ignore.
func (p *ProxyServer) Shutdown(ctx context.Context) error {
	var srvErr error
	if p.srv != nil {
		srvErr = p.srv.Shutdown(ctx)
	}
	if p.listener != nil {
		_ = p.listener.Close()
	}
	return srvErr
}

// Port returns the bound port, or 0 before Listen.
func (p *ProxyServer) Port() int {
	if p.listener == nil {
		return 0
	}
	addr, ok := p.listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0
	}
	return addr.Port
}

// TokenForSession returns a stable per-session path token, minting + registering
// one on first call. Implements session.proxyTokenRegistrar. The token is a
// secret and is never logged.
func (p *ProxyServer) TokenForSession(sessionID string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if tok, ok := p.sessionToToken[sessionID]; ok {
		return tok
	}
	tok := mintProxyToken()
	if tok == "" {
		return ""
	}
	p.sessionToToken[sessionID] = tok
	p.tokenToSession[tok] = sessionID
	return tok
}

// TokenForChat returns a stable per-chat path token. The token target resolves
// via agent_chats.account_id instead of sessions.account_id, so cross-agent
// Claude chats use their own managed account.
func (p *ProxyServer) TokenForChat(sessionID, agentSessionID, fallbackAccountID string) string {
	if sessionID == "" || agentSessionID == "" {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	target := session.ProxyTargetForChat(agentSessionID, fallbackAccountID)
	if tok, ok := p.chatToToken[agentSessionID]; ok {
		p.tokenToSession[tok] = target
		return tok
	}
	tok := mintProxyToken()
	if tok == "" {
		return ""
	}
	p.chatToToken[agentSessionID] = tok
	if p.sessionChats[sessionID] == nil {
		p.sessionChats[sessionID] = map[string]string{}
	}
	p.sessionChats[sessionID][agentSessionID] = tok
	p.tokenToSession[tok] = target
	return tok
}

// Deregister drops a session's token (called when a session ends). Safe to call
// for an unknown session.
func (p *ProxyServer) Deregister(sessionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if tok, ok := p.sessionToToken[sessionID]; ok {
		delete(p.tokenToSession, tok)
		delete(p.sessionToToken, sessionID)
	}
	delete(p.sessionBearer, sessionID)
	for agentSessionID, tok := range p.sessionChats[sessionID] {
		targetID := p.tokenToSession[tok]
		delete(p.tokenToSession, tok)
		delete(p.chatToToken, agentSessionID)
		delete(p.sessionBearer, targetID)
	}
	delete(p.sessionChats, sessionID)
}

// ForgetBearer drops the session's sticky swapped bearer (but keeps its path
// token registered) so the next request forwards the subprocess's own bearer.
// Called after a manual/automatic SwitchAccount respawns the pane under a new
// account, so the sticky swap never silently overrides an explicit switch.
// Implements session.proxyTokenRegistrar. Safe for an unknown session.
func (p *ProxyServer) ForgetBearer(sessionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.sessionBearer, sessionID)
	for _, tok := range p.sessionChats[sessionID] {
		delete(p.sessionBearer, p.tokenToSession[tok])
	}
}

// bearerForSession returns the session's sticky swapped bearer, or "" before
// any failover has committed for it. The value is a secret and is never logged.
func (p *ProxyServer) bearerForSession(sessionID string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.sessionBearer[sessionID]
}

// rememberBearer records the bearer a session was failed over to, so the swap
// sticks for every later request without a pane respawn. Secret; never logged.
func (p *ProxyServer) rememberBearer(sessionID, token string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sessionBearer[sessionID] = token
}

// sessionForToken resolves a path token to its session using a constant-time
// compare against every registered token, so a matching token cannot be
// distinguished from a near-miss by timing.
func (p *ProxyServer) sessionForToken(token string) (string, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	tb := []byte(token)
	var sessionID string
	matched := 0
	for tok, sid := range p.tokenToSession {
		if subtle.ConstantTimeCompare(tb, []byte(tok)) == 1 {
			sessionID = sid
			matched = 1
		}
	}
	if matched == 1 {
		return sessionID, true
	}
	return "", false
}

// isManagedSentinel reports whether a request is the interactive REPL's managed
// shape: only the BOS-326 sentinel x-api-key, with no Authorization of its own.
// Such a request depends on the proxy to translate the sentinel into the bound
// account's bearer, so it must never be forwarded upstream unresolved — doing so
// is a guaranteed 401. A request carrying its own Authorization/x-api-key is
// self-authenticating and is forwarded unchanged.
func isManagedSentinel(r *http.Request) bool {
	return r.Header.Get("x-api-key") == session.SentinelAPIKey &&
		r.Header.Get("Authorization") == ""
}

func hasClientAuth(r *http.Request) bool {
	if r.Header.Get("Authorization") != "" {
		return true
	}
	apiKey := r.Header.Get("x-api-key")
	return apiKey != "" && apiKey != session.SentinelAPIKey
}

// handleProxy is the single catch-all handler. It authenticates the per-session
// path token, buffers the request, forwards to the upstream, and — only when
// the status line is a 429/401 — attempts a transparent account swap + replay
// before any response body ships to the client.
func (p *ProxyServer) handleProxy(w http.ResponseWriter, r *http.Request) {
	token, upstreamPath, ok := parseProxyPath(r.URL.Path)
	if !ok {
		// Reachability probe (e.g. the HEAD / claude issues against the base
		// URL root): answer without hanging so the CLI treats us as reachable.
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	sessionID, ok := p.sessionForToken(token)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Buffer the body (bounded) so it can be replayed byte-for-byte after a
	// swap. A body over the cap streams through without failover.
	limited := io.LimitReader(r.Body, maxBufferedBody+1)
	buffered, err := io.ReadAll(limited)
	if err != nil {
		p.logger.Warn().Err(err).Str("session", sessionID).Msg("failover proxy: read request body failed")
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	canFailover := len(buffered) <= maxBufferedBody
	initialBody := newBodyReader(buffered)
	if !canFailover {
		initialBody = io.MultiReader(newBodyReader(buffered), r.Body)
	}

	// First-leg auth, in precedence order:
	//   1. a committed sticky swap (post-429/401 failover); else
	//   2. the bound account's bearer, translating the interactive REPL's sentinel
	//      x-api-key into the subscription token (BOS-326 — the REPL ignores
	//      CLAUDE_CODE_OAUTH_TOKEN and sends only x-api-key); else
	//   3. "" ⇒ forward the client's own header unchanged (fail-safe / today's
	//      direct behavior for headless runs whose env bearer already works).
	initialAuth := p.bearerForSession(sessionID)
	if initialAuth == "" && !hasClientAuth(r) {
		b, berr := p.failover.CurrentBearer(r.Context(), sessionID)
		switch {
		case errors.Is(berr, session.ErrBearerUnavailable):
			// The bound account's bearer is transiently unresolvable (typically the
			// claude plugin subprocess restarting, so MaterializeAccount is briefly
			// unreachable). For a session relying on us to translate its sentinel
			// x-api-key, forwarding now guarantees an upstream 401 that kills the
			// session — and the credentialless-sentinel failover suppression below
			// would pass that 401 straight through. Fail CLOSED with a retryable 503
			// so the CLI retries once the plugin recovers; never cool/rotate the
			// bound account for a self-inflicted 401. A client presenting its OWN
			// credential is forwarded unchanged (its header still works).
			// Redaction-safe: CurrentBearer's error never carries the token.
			if isManagedSentinel(r) {
				p.logger.Warn().Err(berr).Str("session", sessionID).
					Msg("failover proxy: bound bearer unavailable; returning retryable 503")
				w.Header().Set("Retry-After", "1")
				http.Error(w, "bound account bearer temporarily unavailable; retry", http.StatusServiceUnavailable)
				return
			}
			p.logger.Warn().Err(berr).Str("session", sessionID).
				Msg("failover proxy: current bearer resolve failed; forwarding client header")
		case berr != nil:
			p.logger.Warn().Err(berr).Str("session", sessionID).
				Msg("failover proxy: current bearer resolve failed; forwarding client header")
		default:
			initialAuth = b
		}
	}
	credentiallessManagedSentinel := initialAuth == "" && isManagedSentinel(r)
	resp, err := p.forward(r, upstreamPath, initialBody, initialAuth)
	if err != nil {
		p.logger.Warn().Err(err).Str("session", sessionID).Msg("failover proxy: upstream request failed")
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// Status-gated failover: decide BEFORE any body bytes ship to the client.
	canAttemptFailover := canFailover && (!credentiallessManagedSentinel || resp.StatusCode != http.StatusUnauthorized)

	// A 403 carrying the account-suspension signature (an org/billing block, e.g.
	// a credit-card limit disabling Claude subscription access) is handled like a
	// 401: fail the account and rotate. Unlike 429/401 the discriminator is the
	// BODY, so buffer the (small, error) response and inspect it. A benign 403
	// (e.g. a scope refusal) does not match and passes through unchanged.
	if canAttemptFailover && resp.StatusCode == http.StatusForbidden {
		// Read only a bounded PREFIX for suspension detection, but preserve the
		// FULL original body for pass-through by re-prepending the prefix via
		// io.MultiReader and keeping the original body as the closer. Anthropic 403
		// bodies are tiny, so the prefix holds the whole body in practice (the
		// SuspensionReason check is byte-identical to inspecting the full body),
		// but this avoids truncating a large benign 403 against the upstream
		// Content-Length on the pass-through path.
		prefix, _ := io.ReadAll(io.LimitReader(resp.Body, maxBufferedBody+1))
		origBody := resp.Body
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(prefix), origBody), origBody}
		if _, suspended := agenterr.SuspensionReason(string(prefix)); suspended {
			// Trigger mirrors the SignalKind: suspension is modeled as an
			// AuthInvalidated rotation, so it audits under the matching proto
			// enum ROTATION_TRIGGER_AUTH_INVALIDATED. Using a string without a
			// proto enum entry would hydrate back to ROTATION_TRIGGER_UNSPECIFIED
			// via pb.RotationTrigger_value in account_binding.go.
			res, ferr := p.failover.PrepareFailoverKind(r.Context(), sessionID, rotation.AuthInvalidated, "ROTATION_TRIGGER_AUTH_INVALIDATED")
			if ferr != nil {
				// Redaction-safe: the error never carries the token.
				p.logger.Warn().Err(ferr).Str("session", sessionID).Msg("failover proxy: prepare suspension failover failed; returning original response")
			}
			if res.Rotate {
				_ = resp.Body.Close()
				p.replayAndCommit(w, r, upstreamPath, buffered, sessionID, res)
				return
			}
		}
		// Not a suspension, or no rotation target: pass the buffered 403 through.
		p.copyResponse(w, resp)
		return
	}

	if canAttemptFailover && (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusUnauthorized) {
		res, ferr := p.failover.PrepareFailover(r.Context(), sessionID, resp.StatusCode)
		if ferr != nil {
			// Redaction-safe: PrepareFailover's error never carries the token.
			p.logger.Warn().Err(ferr).Str("session", sessionID).Msg("failover proxy: prepare failover failed; returning original response")
		}
		if res.Rotate {
			// Discard the rejected response; replay with the next bearer.
			_ = resp.Body.Close()
			p.replayAndCommit(w, r, upstreamPath, buffered, sessionID, res)
			return
		}
	}

	// A 200 text/event-stream response can still carry a rate_limit_error INSIDE
	// the stream (Anthropic returns HTTP 200 then emits `event: error` with
	// `error.type: rate_limit_error` before any content ships). The status-line
	// checks above are blind to that, so peek the opening SSE frames — buffering
	// only the pre-content prefix, bounded by a byte cap + deadline — and, if a
	// rate_limit_error arrives before the first content_block_delta, route it into
	// the same failover+replay machinery. Every other 200 (or a peek that reaches
	// content / a backstop) is reconstructed byte-for-byte and passes through, so
	// behaviour is unchanged outside this narrow, failover-capable case.
	if canAttemptFailover && resp.StatusCode == http.StatusOK && isSSEContentType(resp.Header.Get("Content-Type")) {
		rotate, reconstructed := p.peekSSEForRateLimit(resp)
		if rotate {
			res, ferr := p.failover.PrepareFailoverKind(r.Context(), sessionID, rotation.UsageLimited, "ROTATION_TRIGGER_USAGE_LIMITED")
			if ferr != nil {
				// Redaction-safe: the error never carries the token.
				p.logger.Warn().Err(ferr).Str("session", sessionID).Msg("failover proxy: prepare SSE rate-limit failover failed; returning original response")
			}
			if res.Rotate {
				// Discard the rejected stream (nothing shipped); replay with the next
				// bearer. reconstructed is the same *http.Response as resp with its body
				// re-prepended, so closing it releases the upstream connection.
				_ = reconstructed.Body.Close()
				p.replayAndCommit(w, r, upstreamPath, buffered, sessionID, res)
				return
			}
		}
		// No decisive rate_limit_error, or no rotation target: stream the
		// reconstructed (buffered prefix + remainder) response through unchanged.
		p.copyResponse(w, reconstructed)
		return
	}
	p.copyResponse(w, resp)
}

// isSSEContentType reports whether a Content-Type header names a Server-Sent-Event
// stream (e.g. "text/event-stream" or "text/event-stream; charset=utf-8").
func isSSEContentType(ct string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "text/event-stream")
}

// sseDecision is the outcome of classifying one SSE frame during the peek.
type sseDecision int

const (
	// sseUndecided is a pre-content opening frame (message_start, ping,
	// content_block_start, …) — keep peeking.
	sseUndecided sseDecision = iota
	// sseRotate is a terminal `error` frame whose error.type is rate_limit_error,
	// seen before any content shipped — discard the prefix and fail over.
	sseRotate
	// ssePassthrough is a decisive non-rotating frame: the first content_block_delta
	// (content is now on the wire) or any other error (e.g. overloaded_error). Flush
	// the buffered prefix and stream the remainder unchanged.
	ssePassthrough
)

// sseEventData is the minimal subset of an SSE `data:` JSON payload the peek
// inspects: the top-level event `type` and, for an error frame, `error.type`.
// Content deltas are deliberately NOT deserialized.
type sseEventData struct {
	Type  string `json:"type"`
	Error struct {
		Type string `json:"type"`
	} `json:"error"`
}

// peekSSEForRateLimit reads the leading frames of a 200 text/event-stream
// response, deciding whether the stream opens with a rate_limit_error (rotate)
// before any content ships. It ALWAYS returns a reconstructed response whose body
// re-prepends the buffered prefix (via io.MultiReader, keeping the original body
// as the io.Closer) so the caller can pass it through byte-for-byte when it does
// not rotate — mirroring the 403 suspension prefix pattern. Only event names and
// error types are inspected; no auth material is ever read or logged. The peek is
// bounded by ssePeekByteCap and by the ssePeekDeadline between-reads budget (see
// their doc comments) and fails safe to pass-through on any malformed/partial
// frame, EOF, read error, or backstop. Until it releases, the pre-content opening
// frames (message_start / content_block_start / ping) are held rather than
// flushed per-read; the first content_block_delta (or a backstop) flushes the
// whole buffered prefix, so visible tokens are never delayed beyond the deadline.
func (p *ProxyServer) peekSSEForRateLimit(resp *http.Response) (bool, *http.Response) {
	origBody := resp.Body
	byteCap := p.ssePeekByteCap
	if byteCap <= 0 {
		byteCap = sseErrorPeekByteCap
	}
	deadline := time.Now().Add(p.ssePeekDeadline)

	var prefix bytes.Buffer
	chunk := make([]byte, 4096)
	scanned := 0 // bytes of prefix already scanned for frame boundaries
	rotate := false

peekLoop:
	// Stop once the byte cap or the deadline is reached (fail safe to
	// pass-through); decisive frames break out early below.
	for prefix.Len() < byteCap && time.Now().Before(deadline) {
		n, rerr := origBody.Read(chunk)
		if n > 0 {
			prefix.Write(chunk[:n])
			buf := prefix.Bytes()
			for {
				rel := bytes.Index(buf[scanned:], sseFrameBoundary)
				if rel < 0 {
					break // no complete frame yet; read more
				}
				frameEnd := scanned + rel + len(sseFrameBoundary)
				if frameEnd > byteCap {
					// The next frame's boundary lies beyond the peek budget: give up
					// and fail safe to pass-through rather than buffer past the cap.
					break peekLoop
				}
				frame := buf[scanned : scanned+rel]
				scanned = frameEnd
				switch classifySSEFrame(frame) {
				case sseRotate:
					rotate = true
					break peekLoop
				case ssePassthrough:
					break peekLoop
				case sseUndecided:
					// keep scanning subsequent frames
				}
			}
		}
		if rerr != nil {
			break // EOF or read error: fail safe to pass-through
		}
	}

	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(prefix.Bytes()), origBody), origBody}
	return rotate, resp
}

// classifySSEFrame decides whether a single SSE frame is a rate_limit_error
// (rotate), a decisive non-rotating frame (content_block_delta or another error →
// pass through), or a pre-content opening frame (undecided → keep peeking). It is
// tolerant: a malformed/partial `data:` payload falls back to the `event:` name
// and never rotates unless it POSITIVELY reads error.type == "rate_limit_error".
func classifySSEFrame(frame []byte) sseDecision {
	eventName, dataJSON := parseSSEFrame(frame)
	var payload sseEventData
	_ = json.Unmarshal([]byte(dataJSON), &payload) // tolerant: err ⇒ empty payload
	typ := payload.Type
	if typ == "" {
		typ = eventName
	}
	switch typ {
	case "content_block_delta":
		return ssePassthrough
	case "error":
		if payload.Error.Type == "rate_limit_error" {
			return sseRotate
		}
		return ssePassthrough
	default:
		return sseUndecided
	}
}

// parseSSEFrame extracts the `event:` name and the concatenated `data:` payload
// from one SSE frame. Multiple data lines are joined with "\n" per the SSE spec
// (valid inter-token whitespace for a JSON value split across lines). Only these
// two fields are read — never any auth header or token.
func parseSSEFrame(frame []byte) (eventName, data string) {
	var dataParts []string
	for _, raw := range bytes.Split(frame, []byte("\n")) {
		line := bytes.TrimRight(raw, "\r")
		switch {
		case bytes.HasPrefix(line, []byte("event:")):
			eventName = strings.TrimSpace(string(line[len("event:"):]))
		case bytes.HasPrefix(line, []byte("data:")):
			dataParts = append(dataParts, strings.TrimSpace(string(line[len("data:"):])))
		}
	}
	return eventName, strings.Join(dataParts, "\n")
}

// replayAndCommit replays the buffered request with the rotated bearer
// (res.Token) and streams the result to the client, persisting the sticky rebind
// on a successful replay. A replay is "successful" only when its status line is
// <400 AND it did not itself open with a pre-content SSE rate_limit_error, so a
// rotated account that is also rate-limited does not get audited/rebound as if
// it served the request. It assumes res.Rotate is true and the original rejected
// response has already been discarded by the caller.
func (p *ProxyServer) replayAndCommit(w http.ResponseWriter, r *http.Request, upstreamPath string, buffered []byte, sessionID string, res session.FailoverResult) {
	replay, rerr := p.forward(r, upstreamPath, newBodyReader(buffered), res.Token)
	if rerr != nil {
		p.logger.Warn().Err(rerr).Str("session", sessionID).Msg("failover proxy: replay request failed")
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	defer func() { _ = replay.Body.Close() }()

	// A replay can succeed at the status line (HTTP 200) yet still open with an
	// SSE rate_limit_error — Anthropic ships 200, then an `error` frame whose
	// error.type is rate_limit_error before any content. Committing the sticky
	// rebind on that would rebind + audit the rotated account as if it served the
	// request, while the client still receives a rate limit. Peek the replay's
	// opening frames exactly as the first leg does; when the replay is itself
	// pre-content rate-limited, skip the commit and stream the reconstructed
	// stream through unchanged, leaving the swap unstuck so the next request
	// cleanly re-tries failover (mirroring the commit-failure fallback below).
	replayRateLimited := false
	if replay.StatusCode == http.StatusOK && isSSEContentType(replay.Header.Get("Content-Type")) {
		replayRateLimited, replay = p.peekSSEForRateLimit(replay)
	}

	if replay.StatusCode < http.StatusBadRequest && !replayRateLimited {
		// Persist the rebind + audit for the account that actually served the
		// successful request. Only make the swap STICKY once the rebind is durably
		// persisted: caching the bearer while account_id stayed on the old account
		// would desync the forwarded account from the persisted one. On a commit
		// failure we still serve this successful replay (the request already went
		// through) but leave the swap unstuck, so the next request re-forwards the
		// old bearer, cleanly re-tries the failover, and re-attempts the commit —
		// keeping forwarded and persisted accounts in sync.
		//
		// Commit on a DETACHED context: the replay already consumed the next
		// account's quota, so the durable rebind must survive a client (subprocess)
		// cancellation between the replay returning and this persist — otherwise
		// the rebind is lost and the next request cools the wrong account. The
		// prepare + replay legitimately stay on r.Context() (abort if the client
		// goes away before success).
		commitCtx, cancelCommit := context.WithTimeout(context.WithoutCancel(r.Context()), commitTimeout)
		cerr := p.failover.CommitFailover(commitCtx, sessionID, res)
		cancelCommit()
		if cerr != nil {
			p.logger.Error().Err(cerr).Str("session", sessionID).Msg("failover proxy: commit failover failed; swap not made sticky")
		} else {
			p.rememberBearer(sessionID, res.Token)
			p.logger.Info().Str("session", sessionID).Int("replayed_status", replay.StatusCode).
				Msg("failover proxy: replayed with next account, no pane respawn")
		}
	}
	p.copyResponse(w, replay)
}

// forward builds and issues one upstream request. overrideAuth, when non-empty,
// replaces the Authorization header with a fresh bearer (the replay path).
func (p *ProxyServer) forward(r *http.Request, upstreamPath string, body io.Reader, overrideAuth string) (*http.Response, error) {
	u := *p.upstream
	u.Path = strings.TrimSuffix(p.upstream.Path, "/") + upstreamPath
	u.RawQuery = r.URL.RawQuery

	req, err := http.NewRequestWithContext(r.Context(), r.Method, u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}
	// Preserve all client headers (anthropic-beta / anthropic-version /
	// User-Agent / Content-Type / Authorization), minus hop-by-hop headers.
	req.Header = sanitizedHeader(r.Header)
	if overrideAuth != "" {
		req.Header.Set("Authorization", "Bearer "+overrideAuth)
		// When we supply the account's OAuth bearer, the client's sentinel
		// x-api-key (BOS-326) must never reach upstream — otherwise upstream sees
		// an api-key request and bills console/pay-per-use instead of the
		// subscription. Dropping it leaves only the OAuth bearer.
		req.Header.Del("x-api-key")
	} else if req.Header.Get("x-api-key") == session.SentinelAPIKey {
		// If CurrentBearer is temporarily unavailable, fail closed for the
		// managed sentinel: never forward it as an upstream API key.
		req.Header.Del("x-api-key")
	}
	return p.transport.RoundTrip(req)
}

// copyResponse streams an upstream response back to the client: headers, status
// line, then body. Hop-by-hop headers are dropped. The caller owns closing
// resp.Body.
func (p *ProxyServer) copyResponse(w http.ResponseWriter, resp *http.Response) {
	dst := w.Header()
	for k, vv := range resp.Header {
		if isHopByHop(k) {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	// Flush each chunk as it arrives. Claude responses are Server-Sent-Event
	// streams; a plain io.Copy into the ResponseWriter batches them behind the
	// server's ~2KB buffer, delivering tokens to the CLI in bursts. Flushing per
	// read keeps the stream promptly transparent. Flush is best-effort — a
	// ResponseWriter without http.Flusher support just no-ops.
	rc := http.NewResponseController(w)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				p.logger.Warn().Err(werr).Msg("failover proxy: copy response body failed")
				return
			}
			_ = rc.Flush()
		}
		if rerr == io.EOF {
			return
		}
		if rerr != nil {
			p.logger.Warn().Err(rerr).Msg("failover proxy: read upstream response body failed")
			return
		}
	}
}

// sanitizedHeader clones h and removes hop-by-hop headers.
func sanitizedHeader(h http.Header) http.Header {
	out := h.Clone()
	if out == nil {
		out = http.Header{}
	}
	for _, hop := range hopByHopHeaders {
		out.Del(hop)
	}
	return out
}

func isHopByHop(key string) bool {
	for _, hop := range hopByHopHeaders {
		if strings.EqualFold(key, hop) {
			return true
		}
	}
	return false
}

// parseProxyPath splits "/s/<token>/<upstream...>" into the token and the
// upstream path (with leading slash). ok=false for any path not under /s/.
func parseProxyPath(p string) (token, upstream string, ok bool) {
	const prefix = "/s/"
	if !strings.HasPrefix(p, prefix) {
		return "", "", false
	}
	rest := p[len(prefix):]
	if rest == "" {
		return "", "", false
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		token = rest[:i]
		upstream = rest[i:]
	} else {
		token = rest
		upstream = "/"
	}
	if token == "" {
		return "", "", false
	}
	return token, upstream, true
}

// newBodyReader returns a fresh reader over buffered so http.NewRequest can
// detect its length and set ContentLength + GetBody (enabling clean replay).
func newBodyReader(buffered []byte) io.Reader {
	return bytes.NewReader(buffered)
}

// mintProxyToken returns a 64-hex-char cryptographically random path token, or
// "" if the system RNG fails (the caller then skips injection).
func mintProxyToken() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return hex.EncodeToString(buf)
}
