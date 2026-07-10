package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
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
		failover:       cfg.Failover,
		logger:         cfg.Logger,
		upstream:       u,
		transport:      transport,
		tokenToSession: map[string]string{},
		sessionToToken: map[string]string{},
		chatToToken:    map[string]string{},
		sessionChats:   map[string]map[string]string{},
		sessionBearer:  map[string]string{},
	}, nil
}

// Listen binds a loopback-only TCP listener on an ephemeral port and wires the
// catch-all proxy handler. Split from Serve so Shutdown can race safely.
func (p *ProxyServer) Listen() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
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

// Shutdown gracefully stops the server.
func (p *ProxyServer) Shutdown(ctx context.Context) error {
	if p.srv == nil {
		return nil
	}
	return p.srv.Shutdown(ctx)
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
	if canAttemptFailover && (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusUnauthorized) {
		res, ferr := p.failover.PrepareFailover(r.Context(), sessionID, resp.StatusCode)
		if ferr != nil {
			// Redaction-safe: PrepareFailover's error never carries the token.
			p.logger.Warn().Err(ferr).Str("session", sessionID).Msg("failover proxy: prepare failover failed; returning original response")
		}
		if res.Rotate {
			// Discard the rejected response; replay with the next bearer.
			_ = resp.Body.Close()
			replay, rerr := p.forward(r, upstreamPath, newBodyReader(buffered), res.Token)
			if rerr != nil {
				p.logger.Warn().Err(rerr).Str("session", sessionID).Msg("failover proxy: replay request failed")
				http.Error(w, "bad gateway", http.StatusBadGateway)
				return
			}
			defer func() { _ = replay.Body.Close() }()
			if replay.StatusCode < http.StatusBadRequest {
				// Persist the rebind + audit for the account that actually served
				// the successful request. Only make the swap STICKY once the
				// rebind is durably persisted: caching the bearer while
				// account_id stayed on the old account would desync the forwarded
				// account from the persisted one. On a commit failure we still
				// serve this successful replay (the request already went through)
				// but leave the swap unstuck, so the next request re-forwards the
				// old bearer, cleanly re-tries the failover, and re-attempts the
				// commit — keeping forwarded and persisted accounts in sync.
				//
				// Commit on a DETACHED context: the replay already consumed the
				// next account's quota, so the durable rebind must survive a client
				// (subprocess) cancellation between the replay returning and this
				// persist — otherwise the rebind is lost and the next request cools
				// the wrong account. PrepareFailover + the replay legitimately stay
				// on r.Context() (abort if the client goes away before success).
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
			return
		}
	}
	p.copyResponse(w, resp)
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
