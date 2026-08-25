package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/agenterr"
	"github.com/recurser/bossalib/safego"
	"github.com/recurser/bossd/internal/db"
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
	// RepairProxyPane routes a 401 the PROXY ITSELF minted — an unknown path
	// token — to a same-account respawn of the pane that owns that token,
	// skipping the account probe (BOS-982). The proxy never consulted the
	// account for such a 401, so probing it can only answer "healthy"; the pane,
	// not the credential, is what needs repairing.
	//
	// It is deliberately NOT reachable from an upstream 401: that one is a real
	// credential failure and must keep taking PrepareFailover's account-rotation
	// path, probe included.
	//
	// The implementation re-verifies attribution against the pane's own live
	// tmux state before dispatching anything, and returns false — no dispatch —
	// whenever the token cannot be tied to a live pane. token is a SECRET, taken
	// only so the pane's baked URL can be compared against it; it is never
	// logged.
	RepairProxyPane(ctx context.Context, sessionID, agentSessionID, token string) (bool, error)
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

	// inFlight counts authenticated proxied requests currently being served —
	// from just after token auth until the handler returns, which spans body
	// buffering, the upstream round trip, the SSE rate-limit peek, any replay,
	// and the response copy (BOS-888). Agents route every model request through
	// this proxy, so a non-zero count at shutdown is the number of agent turns a
	// restart would sever. Read via InFlightStreams for the drain's
	// start/progress/finish logs.
	inFlight atomic.Int64

	// repairJobs tracks the BOS-982 unknown-token pane repairs that are running
	// off the HTTP handler. The repair is dispatched AFTER the 401 is written and
	// deliberately outlives the handler (see beginUnknownTokenRepair), so this
	// is what stops a shutdown from cutting a repair mid-flight and what lets a
	// test join the work it triggered.
	repairJobs sync.WaitGroup
	// repairMu guards closingRepairs AND serialises it against repairJobs.Add. It
	// exists because http.Server.Shutdown does NOT stop in-flight handlers when its
	// ctx expires — it returns ctx.Err() and leaves them running — while
	// waitRepairJobs' inner goroutine stays blocked in repairJobs.Wait() after its
	// own ctx arm has fired. A surviving handler that reached the unknown-token
	// branch would then Add(1) from a possibly-zero counter concurrently with that
	// live Wait, which is the documented sync.WaitGroup misuse ("WaitGroup is
	// reused before previous Wait has returned").
	//
	// A flag read on its own — atomic or not — does not close that window: a
	// registration that observed "still open" can be preempted before its Add, and
	// Shutdown's Store plus its whole Wait can complete in the gap. The mutex is
	// what makes (observe-open, Add) one step that cannot interleave with setting
	// the flag, so the invariant Wait relies on is real: once closingRepairs is
	// true, every Add that will ever happen has already happened, and the counter
	// can only fall. Shutdown releases the mutex BEFORE waiting — holding it across
	// Wait would deadlock any repair still trying to register.
	//
	// Gating the Add costs nothing real: a daemon already past its drain budget
	// cannot finish a repair either, and the pane's next unresolved-token 401
	// re-enters the lane against the NEXT daemon, which is the one that can act on
	// it.
	repairMu sync.Mutex
	// closingRepairs is set the instant Shutdown begins and gates every later
	// repairJobs.Add. Guarded by repairMu — deliberately a plain bool rather than
	// an atomic, so nothing suggests it can be read safely on its own.
	closingRepairs bool

	// streams, when wired, durably records which chats are holding an in-flight
	// proxied stream so a hard-killed daemon leaves behind the set of streams
	// its death severed (BOS-890). Narrow interface, not the concrete recorder,
	// so this package keeps its one-way dependency direction. nil ⇒ the feature
	// is off and every call site is a no-op.
	streams StreamRecorder

	// drainProgressInterval pins how often Shutdown logs the falling in-flight
	// count while it drains, so a slow `boss daemon restart` reads as deliberate
	// rather than hung. Zero — the production value — means "derive it from the
	// drain budget" via drainProgressCadence; only tests pin it.
	drainProgressInterval time.Duration

	mu sync.RWMutex
	// hashToTarget is the RESOLUTION index: hex(sha256(token)) → proxy target
	// (a bare sessionID, or the chat-shaped string session.ProxyTargetForChat
	// builds). It is keyed by the DIGEST rather than the token because that is
	// the only key a restart can rebuild — the raw token is a secret and is
	// never persisted (BOS-979). See sessionForToken for why hashing away the
	// raw key keeps the timing property the old constant-time scan had.
	hashToTarget map[string]string
	// sessionHashes is sessionID → the set of digests that session owns, across
	// BOTH shapes (its own token and each of its chats'). It is the eviction
	// index: Deregister and ForgetBearer need to reach a rebuilt entry whose raw
	// token this process never saw, which the raw-token maps below cannot do.
	// A session can own several session-shaped digests — one per daemon
	// generation that minted for it — so this is a set, not a single value.
	sessionHashes map[string]map[string]struct{}
	// sessionToToken and chatToToken hold RAW tokens and therefore cover only
	// registrations THIS process minted or adopted. They are deliberately not
	// rebuildable: their job is to hand the same token back to a caller that is
	// about to bake it into a pane URL, which only ever happens at spawn time.
	// Resolution never reads them — that is hashToTarget's job.
	sessionToToken map[string]string
	chatToToken    map[string]string
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

	// proxyTokens, when wired, mirrors the token registry above into durable
	// storage so a daemon restart can rebuild it BEFORE serving (BOS-979). Only
	// hex(sha256(token)) is stored — never the token, which stays a secret held
	// in the maps and in the pane's own baked URL. Narrow db interface rather
	// than the concrete store, so a fake can drive the write-through paths.
	// nil ⇒ every persistence call here is a no-op and the daemon behaves
	// exactly as it did before, relying on the pane sweep alone.
	proxyTokens db.ProxyTokenStore

	// now is the clock used by the pass-through Warn rate-limiter and the
	// minute-bucket counters. Defaults to time.Now; tests override it (via
	// ProxyServerConfig.Now) to drive the one-minute window deterministically
	// without wall-clock sleeps.
	now func() time.Time

	// ptLogMu guards ptLogLast, the (displayID\x00class) → last-emit map that
	// collapses repeated pass-through Warns to one line per minute. Suppressed
	// events are still counted in ptStats — only the log line is rate-limited.
	ptLogMu   sync.Mutex
	ptLogLast map[string]time.Time

	// ptStats is the bounded minute-bucket tally of pass-through error events,
	// surfaced through RepairDoctor as an informational check. It holds no token,
	// body, or auth material — only display ids, class labels, and counts.
	ptStats *passthroughStats
}

// StreamRecorder is the durable in-flight stream record (BOS-890), satisfied by
// *inflight.Recorder in production. Declared here as a narrow interface rather
// than importing the package so the dependency arrow keeps pointing one way and
// tests can substitute a trivial fake.
//
// Every method must be safe on a nil receiver and safe for concurrent use: they
// are called from the proxy request path with no coordination.
type StreamRecorder interface {
	// Enter records that a chat has opened a proxied stream.
	Enter(displayID string)
	// Leave records that one of a chat's proxied streams has ended.
	Leave(displayID string)
	// Seal pins the current set and freezes further writes. Called only when a
	// shutdown is about to cut streams that have not finished; see Shutdown.
	Seal()
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
	// Now overrides the clock used by the pass-through Warn rate-limiter and the
	// minute-bucket counters. Defaults to time.Now; set by tests to advance the
	// one-minute window deterministically. Production leaves it nil.
	Now func() time.Time
	// Streams is the durable in-flight stream record (BOS-890). nil ⇒ the
	// daemon serves proxy traffic exactly as before and records nothing, so a
	// restart simply has no severed-stream evidence to recover from.
	Streams StreamRecorder
	// ProxyTokens is the durable path-token registry (BOS-979). nil ⇒ tokens
	// live only in memory, as before, and a restart depends entirely on the
	// pane sweep to re-adopt them.
	ProxyTokens db.ProxyTokenStore
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
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &ProxyServer{
		failover:        cfg.Failover,
		logger:          cfg.Logger,
		streams:         cfg.Streams,
		proxyTokens:     cfg.ProxyTokens,
		upstream:        u,
		transport:       transport,
		port:            cfg.Port,
		ssePeekByteCap:  sseErrorPeekByteCap,
		ssePeekDeadline: sseErrorPeekDeadline,
		hashToTarget:    map[string]string{},
		sessionHashes:   map[string]map[string]struct{}{},
		sessionToToken:  map[string]string{},
		chatToToken:     map[string]string{},
		sessionBearer:   map[string]string{},
		now:             now,
		ptLogLast:       map[string]time.Time{},
		ptStats:         newPassthroughStats(now),
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

// The drain-progress cadence is derived from the drain budget rather than
// fixed, because a fixed one silently stopped firing. defaultDrainProgressInterval
// was 15s, sized against the plan's original 120s budget; the shipped budget
// later became 15s (config.defaultProxyDrainTimeout) and a 15s ticker armed
// inside a 15s deadline can never fire — drainFailoverProxy builds the ctx
// BEFORE calling Shutdown, Shutdown starts the ticker after that, so the
// deadline always expires and stopProgress always closes the channel strictly
// before the first tick. The periodic line was unreachable in production and
// only the tests that pinned a short interval ever saw it.
const (
	// defaultDrainProgressInterval caps the derived cadence, so a generous
	// budget cannot make the drain log go quiet for minutes at a time. It is
	// also the fallback for a ctx carrying no deadline at all.
	defaultDrainProgressInterval = 15 * time.Second
	// drainProgressTicksPerDrain is how many progress lines a drain that spends
	// its entire budget should emit. Three is enough for the count to read as
	// falling — one line is a snapshot, two is a direction, three is a rate.
	drainProgressTicksPerDrain = 3
	// minDrainProgressInterval floors the derived cadence so an absurdly short
	// budget cannot turn the progress log into a hot loop.
	minDrainProgressInterval = 50 * time.Millisecond
)

// drainProgressCadence picks the interval for the periodic drain log: a
// fraction of the remaining budget, clamped at both ends. A pinned
// drainProgressInterval (tests only) wins outright.
func (p *ProxyServer) drainProgressCadence(ctx context.Context) time.Duration {
	if p.drainProgressInterval > 0 {
		return p.drainProgressInterval
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return defaultDrainProgressInterval
	}
	budget := time.Until(deadline)
	if budget <= 0 {
		return defaultDrainProgressInterval
	}
	interval := budget / drainProgressTicksPerDrain
	if interval < minDrainProgressInterval {
		interval = minDrainProgressInterval
	}
	if interval > defaultDrainProgressInterval {
		interval = defaultDrainProgressInterval
	}
	return interval
}

// DrainOutcome reports how a ProxyServer.Shutdown resolved (BOS-888). The
// caller logs it so a slow `boss daemon restart` is legible: whether in-flight
// agent turns finished or were cut, how many there were, and how long it took.
type DrainOutcome struct {
	// Drained is true when every in-flight proxied stream finished inside the
	// budget, false when the deadline expired and the remainder were cut.
	Drained bool
	// InFlightAtStart is how many authenticated proxied requests were being
	// served when the drain began — i.e. how many agent turns it risked. See
	// InFlightStreams: this counts whole requests, not just the body copy.
	InFlightAtStart int
	// InFlightAtEnd is how many were still running when the drain resolved.
	// Zero on a clean drain; non-zero names the turns that were severed.
	InFlightAtEnd int
	// Elapsed is how long the drain took.
	Elapsed time.Duration
}

// Shutdown drains in-flight proxied streams, then releases the listening socket.
//
// Draining matters because agents point ANTHROPIC_BASE_URL at this proxy, so
// its lifetime is their connection lifetime: tearing it down mid-turn severs
// the SSE stream and the agent reports "Connection lost mid-response"
// (BOS-888). http.Server.Shutdown returns as soon as connections go idle, so a
// generous ctx costs an idle restart nothing and only engages when streams
// genuinely are in flight. The returned DrainOutcome distinguishes a completed
// drain from an expired budget; a non-nil error is srv.Shutdown's (the ctx
// error on expiry).
//
// The listener is closed only AFTER that drain resolves. http.Server.Shutdown
// only closes listeners it is tracking — i.e. those registered via Serve. When
// Listen ran but Serve did not (or Shutdown races Serve's registration), the
// bound socket would otherwise stay open, so an immediate re-bind of the same
// fixed port fails with EADDRINUSE. Closing p.listener explicitly guarantees the
// port is fully released, which is what makes the fixed-port re-bind after a
// daemon exit deterministic (BOS-409). The close is idempotent: when Serve
// already ran, http.Server has closed the listener and this second Close returns
// a benign "use of closed" error we ignore.
func (p *ProxyServer) Shutdown(ctx context.Context) (DrainOutcome, error) {
	started := time.Now()
	outcome := DrainOutcome{InFlightAtStart: p.InFlightStreams()}

	// Close the repair-registration gate FIRST, before anything below can block.
	// Under repairMu, so this cannot land between a registration's gate check and
	// its repairJobs.Add: once this returns, every Add that will ever be taken has
	// already been taken and the counter can only fall — which is what makes the
	// waitRepairJobs join below safe even on the path where srv.Shutdown gives up
	// with handlers still live. The mutex is released here, not held across the
	// wait: a registration blocked on it would otherwise never reach the Done that
	// wait is waiting for.
	p.repairMu.Lock()
	p.closingRepairs = true
	p.repairMu.Unlock()

	var srvErr error
	if p.srv != nil {
		stopProgress := p.startDrainProgressLog(ctx, outcome.InFlightAtStart)
		srvErr = p.srv.Shutdown(ctx)
		stopProgress()
	}

	// Join any in-flight unknown-token pane repair (BOS-982). These run off the
	// handler, so the http.Server drain above does not see them; cutting one
	// between its durable read and its respawn dispatch would leave the pane
	// wedged with nothing scheduled to fix it.
	p.waitRepairJobs(ctx)

	outcome.InFlightAtEnd = p.InFlightStreams()
	outcome.Elapsed = time.Since(started)
	// Drained asks "did the streams finish?", which is NOT "did Shutdown return
	// nil". http.Server.Shutdown closes its listeners first and returns that
	// close error even when the subsequent wait drained every connection
	// cleanly, so `srvErr == nil` would report a fully drained shutdown as
	// drained=false whenever the listener close was noisy. Only an expired or
	// cancelled ctx means connections were actually cut.
	outcome.Drained = !errors.Is(srvErr, context.DeadlineExceeded) && !errors.Is(srvErr, context.Canceled)

	// A budget that expired has to actually cut what it reports as cut.
	// http.Server.Shutdown does NOT close active connections when its ctx
	// expires: it returns ctx.Err() and leaves every in-flight handler running.
	// Closing only the listener below would therefore leave those streams alive
	// while DrainOutcome said Drained=false and the daemon logged that they were
	// severed — a report contradicted by the process it describes. Close() is
	// what makes the timed-out branch mean what it says; it is deliberately NOT
	// called on the drained path, where there is nothing left to close and a
	// Close would only race the listener close below.
	//
	// InFlightAtEnd is deliberately NOT re-read afterwards. It is the count of
	// streams the expiry cut, which is the number the operator wants; re-reading
	// it after Close would race the cut handlers' own decrements and report
	// somewhere between that and zero depending on scheduling.
	if !outcome.Drained && p.srv != nil {
		// Seal the durable stream record FIRST (BOS-890). Close is about to cut
		// every surviving handler, and each cut handler runs its own deferred
		// Leave on the way out — so a record left live through the Close would be
		// emptied by exactly the streams the Close severed, leaving the next
		// startup a file that claims nothing was lost. Sealing pins the set as it
		// stands here, which is precisely the set this expiry is about to cut.
		//
		// Nothing is sealed on the drained path, and that asymmetry is the point:
		// there every handler finished on its own and removed itself, so the
		// record is already empty and a graceful restart correctly recovers
		// nothing.
		if p.streams != nil {
			p.streams.Seal()
		}
		_ = p.srv.Close()
	}

	if p.listener != nil {
		_ = p.listener.Close()
	}
	return outcome, srvErr
}

// InFlightStreams reports how many authenticated proxied requests are currently
// being served — the number of agent turns a shutdown right now would put at
// risk (BOS-888). It counts the whole handler, not just the body copy, because
// a request still awaiting upstream headers is just as much a turn the drain
// has to wait out.
func (p *ProxyServer) InFlightStreams() int {
	return int(p.inFlight.Load())
}

// startDrainProgressLog begins periodically logging the falling in-flight count
// and returns a stop function the caller must invoke once the drain resolves.
// When nothing is in flight it starts no goroutine and no ticker at all: the
// idle restart path must stay exactly as cheap as it was before BOS-888.
func (p *ProxyServer) startDrainProgressLog(ctx context.Context, inFlightAtStart int) func() {
	if inFlightAtStart == 0 {
		return func() {}
	}
	p.logger.Info().Int("in_flight_streams", inFlightAtStart).
		Msg("failover proxy: draining in-flight agent streams before shutdown")

	interval := p.drainProgressCadence(ctx)
	stop := make(chan struct{})
	done := safego.Go(p.logger, func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				p.logger.Info().Int("in_flight_streams", p.InFlightStreams()).
					Msg("failover proxy: still draining in-flight agent streams")
			}
		}
	})
	return func() {
		close(stop)
		<-done
	}
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
	p.registerTargetLocked(proxyTokenHash(tok), sessionID, sessionID)
	p.persistProxyTokenLocked(db.ProxyTokenRecord{
		TokenSHA256: proxyTokenHash(tok),
		SessionID:   sessionID,
	})
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
		p.registerTargetLocked(proxyTokenHash(tok), sessionID, target)
		// This branch does not mint — it REWRITES a live token's target to pick
		// up a changed fallback account. Persisting only on mint would leave the
		// row pinned to the account the chat had at spawn, so a rebuild would
		// resolve the pane to a different account than the running daemon does.
		p.persistProxyTokenLocked(chatProxyTokenRecord(tok, sessionID, agentSessionID, fallbackAccountID))
		return tok
	}
	tok := mintProxyToken()
	if tok == "" {
		return ""
	}
	p.chatToToken[agentSessionID] = tok
	p.registerTargetLocked(proxyTokenHash(tok), sessionID, target)
	p.persistProxyTokenLocked(chatProxyTokenRecord(tok, sessionID, agentSessionID, fallbackAccountID))
	return tok
}

// registerTargetLocked installs one digest → target resolution entry and files
// the digest under its owning session so eviction can find it later. Caller
// holds p.mu. Passing the digest rather than the token keeps every raw token
// confined to its own call frame.
func (p *ProxyServer) registerTargetLocked(hash, sessionID, target string) {
	if hash == "" || target == "" {
		return
	}
	p.hashToTarget[hash] = target
	if sessionID == "" {
		return
	}
	if p.sessionHashes[sessionID] == nil {
		p.sessionHashes[sessionID] = map[string]struct{}{}
	}
	p.sessionHashes[sessionID][hash] = struct{}{}
}

// Deregister drops a session's token (called when a session ends). Safe to call
// for an unknown session.
func (p *ProxyServer) Deregister(sessionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.sessionToToken, sessionID)
	delete(p.sessionBearer, sessionID)
	// Evict this session's pass-through counters too, so an ended session's
	// history does not linger until the sweep (bounds memory on churn) and its
	// display id is freed. The display id for a plain session is the session id;
	// for a chat it is the agentSessionID (see passthroughDisplayID).
	p.ptStats.forget(sessionID)
	// Walk the DIGEST set, not the raw-token maps: after a restart rebuild the
	// session's entries exist only as digests, and a Deregister that read
	// sessionToToken/chatToToken would leave every rebuilt row resolvable for
	// the life of the daemon.
	for hash := range p.sessionHashes[sessionID] {
		targetID := p.hashToTarget[hash]
		delete(p.hashToTarget, hash)
		if targetID == "" || targetID == sessionID {
			continue
		}
		delete(p.sessionBearer, targetID)
		// The agentSessionID is recovered from the target rather than tracked
		// separately, so the chat bookkeeping survives a rebuild that only ever
		// saw the target string.
		if agentSessionID, _, ok := session.ParseProxyChatTarget(targetID); ok {
			delete(p.chatToToken, agentSessionID)
			p.ptStats.forget(agentSessionID)
		}
	}
	delete(p.sessionHashes, sessionID)
	// One delete covers both arms: a chat row carries its owning session's
	// session_id, so the session's own token and every chat token beneath it go
	// together.
	p.forgetSessionProxyTokensLocked(sessionID)
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
	for hash := range p.sessionHashes[sessionID] {
		if target := p.hashToTarget[hash]; target != "" {
			delete(p.sessionBearer, target)
		}
	}
}

// ForgetAllBearers drops every session's sticky swapped bearer (but keeps all
// path tokens registered) so the next request on each session forwards the
// subprocess's own bearer. Called after an account credential refresh, where a
// stale swapped bearer for the refreshed credential must not silently override
// the freshly-saved secret. Implements session.proxyTokenRegistrar.
func (p *ProxyServer) ForgetAllBearers() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sessionBearer = map[string]string{}
}

// proxyTokenPersistTimeout bounds a single durable-registry write. The store is
// local SQLite, so this is a backstop against a wedged file lock rather than an
// expected wait: exceeding it degrades to the in-memory-only behavior we had
// before BOS-979 rather than stalling a session spawn.
//
// It is deliberately SHORT. These writes happen while the caller holds p.mu for
// writing, and sessionForToken takes p.mu for reading on every proxied request,
// so the timeout is the worst-case stall this imposes on ALL proxy resolution —
// not just on the mint that hit the wedged lock. A sub-millisecond write that
// has not completed in half a second is not going to complete usefully, and the
// failure path is already log-and-continue, so half a second buys everything a
// longer budget would while capping the blast radius.
const proxyTokenPersistTimeout = 500 * time.Millisecond

// proxyTokenTTL bounds how long a persisted path token stays resolvable.
//
// Nothing else bounds it. ProxyServer.Deregister has no production caller, and
// sessions.archived_at is a soft delete, so before BOS-979 a daemon restart was
// the de-facto revocation and after it a row would otherwise live forever —
// growing both the table and the rebuilt in-memory registry with every session
// ever spawned, and keeping a long-dead pane's token valid indefinitely.
//
// 30 days is deliberately far longer than the risk window suggests. The only
// thing a too-short TTL can break is a LIVE pane whose ANTHROPIC_BASE_URL was
// frozen at spawn and can never be reissued — the exact wedge BOS-979 exists to
// prevent — and a tmux pane can legitimately sit idle for a week or more. A
// month comfortably exceeds any plausible live-pane lifetime while still
// turning "forever" into a bound, and a pane that outlives it is recoverable
// through the pane sweep, which reconstructs the token from the pane's own env.
const proxyTokenTTL = 30 * 24 * time.Hour

// proxyTokenPruneTimeout bounds the boot-time age prune. It is separate from the
// per-write budget because it runs once, off the request path, before Serve.
const proxyTokenPruneTimeout = 10 * time.Second

// chatProxyTokenRecord builds the durable row for a chat-shaped token. The
// assembled target string is deliberately NOT stored — the rebuild reassembles
// it through session.ProxyTargetForChat from these components, so the wire
// format keeps exactly one author.
func chatProxyTokenRecord(token, sessionID, agentSessionID, accountID string) db.ProxyTokenRecord {
	return db.ProxyTokenRecord{
		TokenSHA256:    proxyTokenHash(token),
		SessionID:      sessionID,
		AgentSessionID: agentSessionID,
		AccountID:      accountID,
		IsChatShaped:   true,
	}
}

// persistProxyTokenLocked mirrors one registry entry into durable storage.
//
// The caller must already hold p.mu, and that is the point: the in-memory write
// and the durable write land inside the SAME critical section, so a concurrent
// reader can never observe a token registered in memory but absent from the
// table (the shape the restart rebuild and the pane sweep both act on). The
// store is local SQLite and a write is sub-millisecond, so holding the registry
// lock across it is cheaper than the reconciliation an async write would need.
//
// A failure here NEVER fails the caller. A session that cannot spawn is a worse
// outcome than a token that is merely not durable — the pane sweep remains the
// fallback for exactly that case — so the error is logged and the mint proceeds.
// Only the digest prefix is logged; the token itself never reaches a log line.
func (p *ProxyServer) persistProxyTokenLocked(rec db.ProxyTokenRecord) {
	if p.proxyTokens == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), proxyTokenPersistTimeout)
	defer cancel()
	if err := p.proxyTokens.Upsert(ctx, rec); err != nil {
		p.logger.Error().
			Err(err).
			Str("token_fingerprint", digestFingerprint(rec.TokenSHA256)).
			Str("session_id", rec.SessionID).
			Bool("chat_shaped", rec.IsChatShaped).
			Msg("failover proxy: persist path token failed; token works this run but a restart must fall back to the pane sweep")
	}
}

// forgetSessionProxyTokensLocked drops every durable row a session owns — its
// own token and each of its chat tokens, which carry the same session_id — so an
// ended session's targets do not accumulate in the table. Caller holds p.mu.
// Like the write path, a failure is logged and swallowed: a stale row resolves
// to a session that no longer exists, which the rebuild discards anyway.
func (p *ProxyServer) forgetSessionProxyTokensLocked(sessionID string) {
	if p.proxyTokens == nil || sessionID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), proxyTokenPersistTimeout)
	defer cancel()
	if err := p.proxyTokens.DeleteBySessionID(ctx, sessionID); err != nil {
		p.logger.Error().
			Err(err).
			Str("session_id", sessionID).
			Msg("failover proxy: evict path tokens failed; stale rows will be discarded at rebuild")
	}
}

// pruneExpiredProxyTokens drops every persisted registration older than
// proxyTokenTTL. It is called once per daemon boot, from the rebuild path, and
// is deliberately NOT a background sweeper: the table only grows when a token is
// minted, and a boot-time prune is enough to keep it bounded.
//
// It takes no lock. The delete is a pure durable-store operation whose effect on
// memory is entirely "these rows are not read by the List that follows", and the
// caller has not taken p.mu yet.
//
// A failure is logged and swallowed. The consequence of a failed prune is an
// oversized table, which is exactly the state this branch inherited; the
// consequence of propagating it would be a daemon that cannot boot.
func (p *ProxyServer) pruneExpiredProxyTokens() {
	if p.proxyTokens == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), proxyTokenPruneTimeout)
	defer cancel()
	cutoff := time.Now().Add(-proxyTokenTTL)
	removed, err := p.proxyTokens.DeleteOlderThan(ctx, cutoff)
	if err != nil {
		p.logger.Error().
			Err(err).
			Time("cutoff", cutoff).
			Msg("failover proxy: pruning expired path tokens failed; the durable registry stays oversized this run")
		return
	}
	if removed > 0 {
		p.logger.Info().
			Int64("removed", removed).
			Time("cutoff", cutoff).
			Msg("failover proxy: pruned expired path tokens")
	}
}

// RebuildTokenRegistry repopulates the resolution index from the durable rows
// written at mint/adopt time, so a tmux pane whose ANTHROPIC_BASE_URL was baked
// by a PREVIOUS daemon keeps resolving after a restart (BOS-979). Implements
// session.proxyTokenRegistrar; called from Lifecycle.Bootstrap, which runs
// before Serve, so no request can observe a half-rebuilt registry.
//
// Only the two DIGEST-keyed indexes are rebuilt. sessionToToken and chatToToken
// hold raw tokens, which are secrets that were deliberately never persisted, so
// they stay empty for a pane this process did not itself spawn — the pane still
// resolves, because resolution only ever reads hashToTarget, and the pane sweep
// (or a re-adoption) is what refills the raw maps when a token is recoverable
// from the pane's own env.
//
// It runs BEFORE the pane sweep so a persisted row wins over a tmux-env
// reconstruction, and it never clobbers a registration already in memory: a
// live spawn's token always outranks a stored row for the same digest.
//
// A read failure is returned, not fatal — the caller logs and continues, which
// degrades to exactly the pre-BOS-979 behavior of depending on the pane sweep.
func (p *ProxyServer) RebuildTokenRegistry(ctx context.Context) error {
	if p.proxyTokens == nil {
		return nil
	}
	// Prune BEFORE the read, so an expired row is never resurrected into memory
	// for the lifetime of this process. Failure degrades to log-and-continue,
	// like every other durable-registry write: an unbounded table is a worse
	// outcome than a failed prune, but a failed prune must never wedge a boot.
	p.pruneExpiredProxyTokens()
	recs, err := p.proxyTokens.List(ctx)
	if err != nil {
		return fmt.Errorf("list persisted proxy tokens: %w", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	restored, skipped := 0, 0
	for _, rec := range recs {
		if rec.TokenSHA256 == "" || rec.SessionID == "" {
			skipped++
			continue
		}
		target := rec.SessionID
		if rec.IsChatShaped {
			if rec.AgentSessionID == "" {
				skipped++
				continue
			}
			// Reassembled through the single author of the wire format. The
			// target embeds NUL bytes and is deliberately not a stored column,
			// so this call — not the row — is what defines the shape.
			target = session.ProxyTargetForChat(rec.AgentSessionID, rec.AccountID)
		}
		if existing, ok := p.hashToTarget[rec.TokenSHA256]; ok {
			if existing != target {
				// Something registered this digest between process start and
				// Bootstrap; the live registration wins, same precedence as
				// AdoptToken. Log-safe: the digest is not the token, but only
				// its fingerprint is emitted for symmetry with the write path.
				p.logger.Warn().
					Str("token_fingerprint", digestFingerprint(rec.TokenSHA256)).
					Str("session_id", rec.SessionID).
					Msg("failover proxy: rebuilt token conflicts with a live registration; keeping the live one")
				skipped++
			}
			continue
		}
		p.registerTargetLocked(rec.TokenSHA256, rec.SessionID, target)
		restored++
	}
	p.logger.Info().
		Int("restored", restored).
		Int("skipped", skipped).
		Int("rows", len(recs)).
		Msg("failover proxy: rebuilt path-token registry from durable rows")
	return nil
}

// digestFingerprint abbreviates an ALREADY-hashed token for a log line. It takes
// the digest rather than the token so a caller holding only the durable row can
// log a correlatable id without the raw secret ever being in scope.
func digestFingerprint(digest string) string {
	if len(digest) > 8 {
		return digest[:8]
	}
	return digest
}

// tokenPrefix returns a short, non-reversible prefix of a proxy token (at most 8
// hex chars) safe to put in a diagnostic log line. Full proxy tokens are secrets
// and must never be logged; a conflict can be diagnosed from the prefix alone.
func tokenPrefix(token string) string {
	if len(token) > 8 {
		return token[:8]
	}
	return token
}

// --- pass-through observability (BOS-483) -----------------------------------
//
// The proxy intercepts only a narrow set of upstream errors (429/401, a
// suspension 403, a pre-content SSE rate_limit_error) for account failover;
// EVERYTHING else it forwards byte-for-byte and, historically, SILENTLY. That
// silence made an operator blind to the upstream error rate a session actually
// saw. The helpers below add PURELY ADDITIVE observability: they never change a
// rotation decision or an upstream response — they emit one structured Warn per
// un-rotated error response (with a TRUTHFUL, locally-derivable decline reason),
// rate-limit repeats, and keep a bounded in-memory tally for RepairDoctor. No
// token, body, or auth header is ever read or logged here.

// Pass-through decline reasons. Each is something the proxy can prove LOCALLY
// about why it did not rotate a given error response — never a guess about the
// upstream's intent. A PrepareFailover* that simply returned Rotate=false
// collapses to the coarse passthroughReasonFailoverDeclined: the proxy cannot
// truthfully attribute a finer cause, so it does not fabricate one.
const (
	// passthroughReasonStatusNotIntercepted: an error status the failover logic
	// never attempts (e.g. 410/500, or a benign 4xx that is not 401/403/429).
	passthroughReasonStatusNotIntercepted = "status_not_intercepted"
	// passthroughReasonBodyTooLarge: the request body exceeded maxBufferedBody, so
	// the proxy streamed through without buffering and could not offer failover.
	passthroughReasonBodyTooLarge = "body_too_large"
	// passthroughReasonCredentiallessSentinel: a managed-sentinel session whose
	// bearer was unresolved got a 401 the proxy deliberately does NOT rotate on
	// (rotating a self-inflicted credentialless 401 would cool a healthy account).
	passthroughReasonCredentiallessSentinel = "credentialless_sentinel"
	// passthroughReasonSuspensionNotMatched: a 403 whose body did NOT carry the
	// account-suspension signature — a benign scope refusal passed through.
	passthroughReasonSuspensionNotMatched = "suspension_not_matched"
	// passthroughReasonReplayStillErroring: a post-rotation replay leg itself
	// returned ≥400 (the rotated account did not clear the error).
	passthroughReasonReplayStillErroring = "replay_still_erroring"
	// passthroughReasonSSEErrorPassthrough: a 200 SSE stream that opened with a
	// non-rate-limit `error` frame (e.g. overloaded_error) before any content.
	passthroughReasonSSEErrorPassthrough = "sse_error_passthrough"
	// passthroughReasonFailoverDeclined: a 401/429 where PrepareFailover returned
	// Rotate=false (no eligible next account / rotation disabled). Deliberately
	// coarse — the proxy has no truthful sub-reason to report.
	passthroughReasonFailoverDeclined = "failover_declined"
)

// unknownTokenBody is the self-identifying 401 body the proxy returns for a path
// token it does not recognise (BOS-483). The old opaque "unauthorized" gave an
// operator staring at a pane's failed request no clue the proxy itself — not the
// upstream — rejected it; this names the component and the most likely cause.
//
// Since BOS-979 the registry is rebuilt from durable rows before the daemon
// serves, so "the pane predates a restart" is no longer the expected cause and
// the body no longer points operators at that ticket. A token that reaches here
// now means the registration is genuinely gone — its session was deregistered,
// its row was evicted, or the pane belongs to a different daemon's registry.
// Tests compare against this constant plus the trailing newline http.Error
// appends.
const unknownTokenBody = "bossd failover proxy: unknown session token (no live registration for this pane; its session has ended or its token was never registered with this daemon)"

// passthroughWarnWindow is the minimum spacing between repeated pass-through
// Warns for the same (displayID, class): repeats within a minute collapse to one
// line while the counters still record every event.
const passthroughWarnWindow = time.Minute

// passthroughWarnMapCap bounds the rate-limit dedupe map before an opportunistic
// sweep drops entries older than the window. It is a soft cap: the map is
// naturally bounded by the live (displayID, class) set, and the sweep keeps a
// burst of distinct unknown-token fingerprints from growing it without bound.
const passthroughWarnMapCap = 1024

// passthroughDisplayID derives a log-SAFE display id from an internal proxy
// target. A chat target is chat\x00<agentSessionID>\x00<fallbackAccountID>: the
// raw form carries NUL control bytes and an account id, so only the
// agentSessionID is surfaced. A plain session target is already safe and is
// returned unchanged.
func passthroughDisplayID(targetID string) string {
	if agentSessionID, _, ok := session.ParseProxyChatTarget(targetID); ok {
		return agentSessionID
	}
	return targetID
}

// passthroughClass labels an un-rotated error response for counting/logging. An
// HTTP error is its status code ("410"); a 200 SSE error pass-through is
// "sse_<type>" (e.g. "sse_overloaded_error") so the 200 status and the in-stream
// error are not conflated in the tally.
func passthroughClass(status int, sseErrorType string) string {
	if sseErrorType != "" {
		return "sse_" + sseErrorType
	}
	return strconv.Itoa(status)
}

// tokenFingerprint returns the first 8 hex chars of SHA-256(token) — a stable,
// non-reversible fingerprint safe to log so repeated unknown-token rejections
// from the SAME baked pane can be correlated WITHOUT ever logging token bytes.
// (tokenPrefix logs a raw 8-char slice, fine for a token WE minted this run and
// hold; an unknown token is attacker-influenceable input, so it is hashed.)
//
// The return type is named, not a bare string, because logUnknownToken now takes
// the fingerprint rather than the token: a caller handing it the raw token would
// log the secret verbatim, and the only thing that caught that was a whole-log
// security scan running much later. It is a compile error instead.
type tokenFP string

func tokenFingerprint(token string) tokenFP {
	return tokenFP(proxyTokenHash(token)[:8])
}

// proxyTokenHash returns the full hex-encoded SHA-256 of a path token — the
// durable registry's primary key, and the only form of a token that is ever
// written to disk or a log. It is deliberately the single author of that digest
// so the value a row is keyed by and the value tokenFingerprint abbreviates for
// a log line can never drift apart; a rebuild that looked up a differently
// derived digest would silently resolve nothing.
func proxyTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// allowPassthroughWarn reports whether a Warn keyed by (displayID, class) should
// emit now, collapsing repeats to one line per passthroughWarnWindow. Suppressed
// events are still counted by the caller — only the log line is rate-limited.
// The map is swept opportunistically past passthroughWarnMapCap so a burst of
// distinct keys (e.g. rotating unknown-token fingerprints) cannot grow it
// without bound.
func (p *ProxyServer) allowPassthroughWarn(displayID, class string) bool {
	key := displayID + "\x00" + class
	now := p.now()
	p.ptLogMu.Lock()
	defer p.ptLogMu.Unlock()
	if last, ok := p.ptLogLast[key]; ok && now.Sub(last) < passthroughWarnWindow {
		return false
	}
	p.ptLogLast[key] = now
	if len(p.ptLogLast) > passthroughWarnMapCap {
		// Cheap common case: drop entries past the window.
		for k, t := range p.ptLogLast {
			if now.Sub(t) >= passthroughWarnWindow {
				delete(p.ptLogLast, k)
			}
		}
		// Hard backstop: a burst of > cap DISTINCT keys inside one window ages out
		// nothing above, so clear wholesale to keep the map truly bounded. The only
		// cost is that in-flight (displayID,class) rate-limits reset, so at most a
		// few extra Warn lines may print — far cheaper than unbounded growth.
		if len(p.ptLogLast) > passthroughWarnMapCap {
			clear(p.ptLogLast)
		}
	}
	return true
}

// logPassthrough records and (rate-limited) logs one un-rotated error response.
// It ALWAYS increments the bounded counters (so a suppressed Warn is still
// counted), then emits at most one Warn per (displayID, class) per minute. All
// fields are log-safe: a derived display id, the class, method, the upstream API
// path (never the token-bearing request path), a truthful reason, and — for a
// 200 SSE error pass-through — the classified error type.
func (p *ProxyServer) logPassthrough(targetID, method, upstreamPath string, status int, reason, sseErrorType string) {
	displayID := passthroughDisplayID(targetID)
	class := passthroughClass(status, sseErrorType)
	p.ptStats.record(displayID, class)
	if !p.allowPassthroughWarn(displayID, class) {
		return
	}
	ev := p.logger.Warn().
		Str("event", "failover_proxy_passthrough").
		Str("session", displayID).
		Int("status", status).
		Str("method", method).
		Str("path", upstreamPath).
		Str("reason", reason)
	if sseErrorType != "" {
		ev = ev.Str("sse_error", sseErrorType)
	}
	ev.Msg("failover proxy: passed upstream error through un-rotated")
}

// logUnknownToken logs a rejected request whose path token the proxy does not
// recognise. It logs a SHA-256 fingerprint (never token bytes) and the upstream
// API path (never the token-bearing request path), rate-limited per fingerprint
// because the volume is unauthenticated and attacker-influenceable.
//
// The rate-limit charge is NOT taken here. The limiter is stateful, one charge
// gates both the warn and the pane-repair attempt (BOS-982), and a helper that
// both charged and reported its charge through the return value made a logging
// function the control-flow authority for a repair dispatch. The single charge
// lives at the one call site instead, which is also the only place that can see
// both consequences of it; this function just writes the line for a charge that
// was already taken, and takes the fingerprint rather than the token so the
// secret does not travel any further than it must.
func (p *ProxyServer) logUnknownToken(fp tokenFP, method, upstreamPath string) {
	p.logger.Warn().
		Str("event", "failover_proxy_unknown_token").
		Str("token_fingerprint", string(fp)).
		Str("method", method).
		Str("path", upstreamPath).
		Msg("failover proxy: rejected request with unknown session token")
}

// proxyTokenRepairTimeout bounds the CONTEXT-AWARE part of one unknown-token
// repair attempt: the durable primary-key read plus the two tmux calls the
// attribution check makes against a single named pane. It is generous relative
// to the persist timeout because a tmux exec is a subprocess.
//
// It does NOT bound the whole attempt. The dispatch it ends in reaches the
// rotator, whose shared reservation loads config from disk through an API that
// takes no context — so this timeout could never have been the thing that kept
// a wedged repair from holding the 401 open. Running the repair off the handler,
// after the 401 is already written, is what does (see beginUnknownTokenRepair).
const proxyTokenRepairTimeout = 3 * time.Second

// repairUnknownTokenPane tries to attribute a token the in-memory registry could
// not resolve, and — when it belongs to a live pane — routes it to a
// probe-skipping respawn-in-place (BOS-982).
//
// This is the whole point of the change: the shape it recovers from is a token
// whose DURABLE row still exists while the in-memory map has lost it, which is
// exactly what a daemon restart or a Deregister-while-the-pane-lives produces.
// Before this, that pane's only route back to health was Claude Code rendering
// the 401, the status poller scraping a login banner off it, and the rotator
// probing an account that was never involved.
//
// The gates are ordered cheapest-first and every one of them fails closed:
//
//	no failover seam / no durable store  → nothing to attribute with
//	token is not 64-hex                  → cannot be one of ours; no DB read
//	no row for the digest                → unattributable; the 401 stands
//	row is session-shaped                → no chat to respawn
//	RepairProxyPane says no              → pane not live, or not this pane's token
//
// Whatever happens, the 401 body the handler already wrote stands unchanged — a
// repair is a side effect of the rejection, never a substitute for it. token is a
// secret and is never logged; only its digest prefix appears.
func (p *ProxyServer) repairUnknownTokenPane(token string) bool {
	if p.failover == nil || p.proxyTokens == nil {
		return false
	}
	// The canonical-shape pre-gate. It is the SAME predicate the adoption sweep
	// applies to a pane's baked URL, deliberately shared rather than re-spelled:
	// a second copy could drift into calling a token ours that attribution would
	// then refuse (or the reverse). Cheap and attacker-facing — garbage costs this
	// loop and nothing else, no durable read and no tmux exec.
	if !session.IsCanonicalProxyToken(token) {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), proxyTokenRepairTimeout)
	defer cancel()

	digest := proxyTokenHash(token)
	rec, err := p.proxyTokens.GetByTokenHash(ctx, digest)
	if err != nil {
		p.logger.Warn().
			Err(err).
			Str("token_fingerprint", digestFingerprint(digest)).
			Msg("failover proxy: durable lookup for an unknown path token failed; leaving the pane to the status-scrape path")
		return false
	}
	if rec == nil || !rec.IsChatShaped || rec.AgentSessionID == "" {
		return false
	}
	repaired, err := p.failover.RepairProxyPane(ctx, rec.SessionID, rec.AgentSessionID, token)
	if err != nil {
		p.logger.Warn().
			Err(err).
			Str("token_fingerprint", digestFingerprint(digest)).
			Str("session_id", rec.SessionID).
			Msg("failover proxy: pane repair for an unknown path token failed; leaving the pane to the status-scrape path")
		return false
	}
	return repaired
}

// beginUnknownTokenRepair runs an unknown-token pane repair OFF the HTTP
// handler that observed the token.
//
// The 401 is written first and does not wait for this. The repair is not a
// substitute for the rejection — Claude Code sees the same body either way — and
// the work behind it is not something an HTTP handler should be holding a
// response open for: a durable read, two tmux subprocess calls, and then a
// rotator dispatch whose shared reservation loads config from disk through an
// API that takes no context. proxyTokenRepairTimeout bounds the ctx-aware part
// only, so on the handler's goroutine a wedged tmux or a slow disk would delay
// the 401 for a pane that is already retrying.
//
// The goroutine is joined, not fire-and-forget: repairJobs is what Shutdown
// waits on, so a restart cannot cut a repair between its durable read and its
// respawn dispatch.
//
// Registration is split from execution on purpose. The caller registers the job
// BEFORE it writes the 401 and runs it after, so anything that has observed the
// response — Shutdown's join, a test — is guaranteed to see the repair as
// outstanding rather than racing the handler to the Add.
//
// That split is what makes the Add/Done pairing non-local, so the returned run
// func is ALWAYS deferred by its caller rather than called at a chosen point: a
// panic on the 401 write path (http.ErrAbortHandler from a wrapping
// ResponseWriter, a middleware panic) would otherwise skip it, strand the
// counter, and make every later Shutdown burn its whole drain budget on a job
// that will never run — a hang nowhere near the code that caused it. run is
// never nil, so the caller needs no nil check for its defer.
//
// Once Shutdown has begun no repair is registered at all: the returned run is a
// no-op and no Add is taken. The gate check and the Add happen together under
// repairMu, which is what makes them one step against Shutdown's flag write —
// checking a bare flag and then adding leaves a window in which Shutdown sets the
// flag and its repairJobs.Wait both complete, so the Add lands on a WaitGroup
// whose Wait has already returned. See closingRepairs.
func (p *ProxyServer) beginUnknownTokenRepair(token string) (run func()) {
	p.repairMu.Lock()
	if p.closingRepairs {
		p.repairMu.Unlock()
		return func() {}
	}
	p.repairJobs.Add(1)
	p.repairMu.Unlock()
	return func() {
		safego.Go(p.logger, func() {
			defer p.repairJobs.Done()
			p.repairUnknownTokenPane(token)
		})
	}
}

// waitRepairJobs blocks until every dispatched unknown-token repair has
// finished, or ctx expires. A repair is short and bounded, so the usual outcome
// is an immediate return; the ctx guard exists so a shutdown budget that has
// already expired is not extended by this.
func (p *ProxyServer) waitRepairJobs(ctx context.Context) {
	done := make(chan struct{})
	safego.Go(p.logger, func() {
		defer close(done)
		p.repairJobs.Wait()
	})
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// passthroughStatsWindow is how many one-minute buckets (→ last hour) of
// pass-through error history the counters retain per display session.
const passthroughStatsWindow = 60

// passthroughStatsMaxClasses caps the distinct error classes tracked per session
// in-window; a further new class folds into passthroughStatsOtherClass so a
// pathological status/error-type spread cannot grow a session's map without
// bound.
const passthroughStatsMaxClasses = 16

// passthroughStatsMaxSessions caps how many display sessions are tracked at once;
// past it the least-recently-active session is evicted (LRU).
const passthroughStatsMaxSessions = 256

// passthroughStatsOtherClass is the fold-in label for classes beyond the cap.
const passthroughStatsOtherClass = "other"

// passthroughStats is a bounded, in-memory tally of pass-through error events
// per display session, surfaced through RepairDoctor. It records EVERY
// qualifying event (including Warn-suppressed repeats) and is bounded three
// ways: at most passthroughStatsWindow one-minute buckets per session (older
// pruned lazily), at most passthroughStatsMaxClasses distinct classes per
// session (further classes fold into "other"), and at most
// passthroughStatsMaxSessions sessions (LRU-evicted). It holds only display ids,
// class labels, and counts — never a token, body, or auth header.
type passthroughStats struct {
	now      func() time.Time
	mu       sync.Mutex
	sessions map[string]*ptSessionCounter
}

// ptSessionCounter holds one display session's minute buckets.
type ptSessionCounter struct {
	buckets    map[int64]map[string]int // unix-minute → class → count
	lastMinute int64                    // most recent event minute (LRU anchor)
}

func newPassthroughStats(now func() time.Time) *passthroughStats {
	if now == nil {
		now = time.Now
	}
	return &passthroughStats{now: now, sessions: map[string]*ptSessionCounter{}}
}

// record increments the current-minute bucket for (displayID, class), pruning
// stale buckets, capping distinct classes, and LRU-evicting sessions past the
// global cap. Never blocks the request path meaningfully (O(buckets×classes)
// with both ≤ their small caps).
func (s *passthroughStats) record(displayID, class string) {
	if displayID == "" {
		return
	}
	minute := s.now().Unix() / 60
	s.mu.Lock()
	defer s.mu.Unlock()

	sc := s.sessions[displayID]
	if sc == nil {
		if len(s.sessions) >= passthroughStatsMaxSessions {
			s.evictLRULocked()
		}
		sc = &ptSessionCounter{buckets: map[int64]map[string]int{}}
		s.sessions[displayID] = sc
	}
	sc.lastMinute = minute

	// Prune buckets older than the window.
	for m := range sc.buckets {
		if minute-m >= passthroughStatsWindow {
			delete(sc.buckets, m)
		}
	}

	// Cap distinct classes per session (across the retained window). Recomputed
	// from the surviving buckets so a class that aged out frees a slot.
	distinct := map[string]struct{}{}
	for _, b := range sc.buckets {
		for c := range b {
			distinct[c] = struct{}{}
		}
	}
	if _, seen := distinct[class]; !seen && len(distinct) >= passthroughStatsMaxClasses {
		class = passthroughStatsOtherClass
	}

	b := sc.buckets[minute]
	if b == nil {
		b = map[string]int{}
		sc.buckets[minute] = b
	}
	b[class]++
}

// forget drops a display session's counters (called from Deregister so an ended
// session's history does not linger until the sweep).
func (s *passthroughStats) forget(displayID string) {
	s.mu.Lock()
	delete(s.sessions, displayID)
	s.mu.Unlock()
}

// evictLRULocked removes the least-recently-active session. Caller holds s.mu.
func (s *passthroughStats) evictLRULocked() {
	var oldestID string
	var oldest int64
	first := true
	for id, sc := range s.sessions {
		if first || sc.lastMinute < oldest {
			oldest, oldestID, first = sc.lastMinute, id, false
		}
	}
	if oldestID != "" {
		delete(s.sessions, oldestID)
	}
}

// passthroughClassCount is one (class, count) pair in a snapshot.
type passthroughClassCount struct {
	Class string
	Count int
}

// passthroughSessionSnapshot is one display session's pass-through error totals
// over the retained window, classes sorted by count (desc) then name.
type passthroughSessionSnapshot struct {
	DisplayID string
	Total     int
	Classes   []passthroughClassCount
}

// snapshot returns a deterministic, per-session view of the in-window counters,
// sorted by total (desc) then display id. Sessions whose buckets have all aged
// out are omitted (they are pruned lazily on the next record).
func (s *passthroughStats) snapshot() []passthroughSessionSnapshot {
	minute := s.now().Unix() / 60
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []passthroughSessionSnapshot
	for id, sc := range s.sessions {
		classTotals := map[string]int{}
		for m, b := range sc.buckets {
			if minute-m >= passthroughStatsWindow {
				continue
			}
			for c, n := range b {
				classTotals[c] += n
			}
		}
		if len(classTotals) == 0 {
			continue
		}
		entry := passthroughSessionSnapshot{DisplayID: id}
		for c, n := range classTotals {
			entry.Classes = append(entry.Classes, passthroughClassCount{Class: c, Count: n})
			entry.Total += n
		}
		sort.Slice(entry.Classes, func(i, j int) bool {
			if entry.Classes[i].Count != entry.Classes[j].Count {
				return entry.Classes[i].Count > entry.Classes[j].Count
			}
			return entry.Classes[i].Class < entry.Classes[j].Class
		})
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Total != out[j].Total {
			return out[i].Total > out[j].Total
		}
		return out[i].DisplayID < out[j].DisplayID
	})
	return out
}

// PassthroughStatsSnapshot exposes the bounded pass-through error tally for the
// RepairDoctor informational check. It satisfies the unexported
// passthroughStatsProvider seam consumed in repair_doctor.go.
func (p *ProxyServer) PassthroughStatsSnapshot() []passthroughSessionSnapshot {
	return p.ptStats.snapshot()
}

// AdoptToken re-registers an EXISTING session-shaped path token reconstructed
// from a surviving tmux pane's baked ANTHROPIC_BASE_URL after a daemon restart
// (BOS-481), so the pane's frozen /s/<token> keeps resolving to sessionID with
// no respawn. It NEVER mints — the token is supplied by the caller from the pane
// env, so the pane's own already-frozen URL is what starts routing again.
// Implements session.proxyTokenRegistrar. Idempotent and conflict-safe:
//   - session already holds a DIFFERENT (fresh-spawn) token ⇒ the live spawn
//     wins; adoption is skipped SILENTLY (a newer pane already re-registered).
//   - token already registered to the SAME target ⇒ no-op (re-adopt / sibling).
//   - token already registered to a DIFFERENT target ⇒ first registration wins;
//     adoption is skipped with a single Warn (token shown only as an ≤8-hex
//     prefix). The full token is never logged.
func (p *ProxyServer) AdoptToken(token, sessionID string) {
	if token == "" || sessionID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if existing, ok := p.sessionToToken[sessionID]; ok && existing != token {
		// A live fresh spawn already minted a different token for this session:
		// defer to it silently, leaving the spawn's registration intact.
		return
	}
	hash := proxyTokenHash(token)
	if existingTarget, ok := p.hashToTarget[hash]; ok && existingTarget != sessionID {
		p.logger.Warn().
			Str("token_prefix", tokenPrefix(token)).
			Msg("failover proxy: adopt token already registered to a different target; keeping first registration")
		return
	}
	// Falling through on an already-matching target is deliberate, not a
	// redundant rewrite: after the boot rebuild the digest resolves but the RAW
	// token is not in sessionToToken, and only the sweep is holding it. Filling
	// it in here means a later TokenForSession hands the live pane back its own
	// baked token instead of minting a second one the pane can never learn.
	p.sessionToToken[sessionID] = token
	p.registerTargetLocked(hash, sessionID, sessionID)
	// An adopted token must become durable too, or the NEXT restart loses it
	// again and recovery depends on the pane sweep succeeding every time.
	p.persistProxyTokenLocked(db.ProxyTokenRecord{
		TokenSHA256: hash,
		SessionID:   sessionID,
	})
}

// AdoptTokenForChat re-registers an EXISTING chat-shaped path token reconstructed
// from a surviving tmux pane after a daemon restart (BOS-481), filling the
// chat/session bookkeeping so Deregister and ForgetBearer keep working exactly
// as for a freshly-minted chat token. It NEVER mints. accountID is the account
// resolved at adoption time; it seeds the target's fallbackAccountID for the
// small window before the durable chat binding is observed. Idempotent and
// conflict-safe with the same precedence as AdoptToken (spawn wins silently;
// same target no-op; different target keeps first + single ≤8-hex-prefix Warn).
func (p *ProxyServer) AdoptTokenForChat(sessionID, agentSessionID, accountID, token string) {
	if token == "" || sessionID == "" || agentSessionID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	target := session.ProxyTargetForChat(agentSessionID, accountID)
	if existing, ok := p.chatToToken[agentSessionID]; ok && existing != token {
		// A live fresh spawn already minted a different token for this chat.
		return
	}
	hash := proxyTokenHash(token)
	if existingTarget, ok := p.hashToTarget[hash]; ok && existingTarget != target {
		p.logger.Warn().
			Str("token_prefix", tokenPrefix(token)).
			Msg("failover proxy: adopt chat token already registered to a different target; keeping first registration")
		return
	}
	// Same reasoning as AdoptToken: a matching target after a rebuild still
	// needs the raw-token bookkeeping filled in, so a later TokenForChat
	// refreshes the LIVE pane's token rather than minting an unreachable one.
	p.chatToToken[agentSessionID] = token
	p.registerTargetLocked(hash, sessionID, target)
	p.persistProxyTokenLocked(chatProxyTokenRecord(token, sessionID, agentSessionID, accountID))
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

// sessionForToken resolves a path token to its proxy target through a single
// hash-keyed map lookup on hex(sha256(token)).
//
// This replaced an O(n) subtle.ConstantTimeCompare scan over every registered
// raw token, and the timing property that scan existed to provide is PRESERVED,
// not traded away. The attacker supplies the token, so they already know it and
// can compute its SHA-256 themselves; hashing it leaks nothing they did not
// bring with them. What must never happen is a comparison that short-circuits
// against an unknown secret, revealing it prefix by prefix — and no such
// comparison remains here. The raw token is consumed only by SHA-256, which is
// data-independent in time, and the resulting digest is used as a map key.
// Go's map lookup does compare key bytes non-constant-time, but only against
// digests, so the most a timing oracle could recover is how far a digest the
// attacker already possesses matches a stored digest of a token they already
// possess — and sha256 preimage resistance means a near-miss digest cannot be
// walked back into a near-miss token. Meanwhile the old scan's real cost was
// unbounded: it hashed nothing but touched every live registration on every
// request, including 401 probes.
func (p *ProxyServer) sessionForToken(token string) (string, bool) {
	hash := proxyTokenHash(token)
	p.mu.RLock()
	defer p.mu.RUnlock()
	target, ok := p.hashToTarget[hash]
	if !ok || target == "" {
		return "", false
	}
	return target, true
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
		// Self-identifying 401 (BOS-483): name the proxy and the most likely cause
		// (a pane whose baked token predates a daemon restart) instead of an opaque
		// "unauthorized". Log a SHA-256 fingerprint (never the token) and the
		// upstream path (never r.URL.Path, which carries the token).
		// ONE rate-limiter charge gates BOTH the warn and the repair attempt
		// (BOS-982), so a retrying pane cannot drive a respawn per request. The
		// limiter is stateful, so it is charged exactly once, here, where both
		// consequences of the answer are visible.
		fp := tokenFingerprint(token)
		if p.allowPassthroughWarn("unknown-token", string(fp)) {
			p.logUnknownToken(fp, r.Method, upstreamPath)
			// Deferred, not called below the write: the Add is already taken, so the
			// matching Done must not depend on control reaching a later statement.
			// The deferred call still runs AFTER http.Error, which is the ordering
			// the registration/execution split exists to give.
			runRepair := p.beginUnknownTokenRepair(token)
			defer runRepair()
		}
		// The rejection is written FIRST and never waits on the repair. This 401
		// is self-inflicted: the proxy minted it without ever consulting an
		// account. When the token still resolves durably to a live pane, that
		// pane is sent straight to a same-account respawn instead of travelling
		// through an account-invalidation diagnosis it can never satisfy — but
		// the response is unchanged either way, so Claude Code sees the same 401
		// and retries at the same moment it always did; the repair just means
		// something is now fixing the pane while it does.
		http.Error(w, unknownTokenBody, http.StatusUnauthorized)
		return
	}

	// Count this request for its WHOLE lifetime, not just the body copy
	// (BOS-888). A request buffering its body, awaiting upstream headers, or
	// sitting in the SSE rate-limit peek is an at-risk agent turn that
	// srv.Shutdown will wait out exactly like a streaming one; counting only the
	// copy would report zero in flight while the drain genuinely waited, and
	// send the shutdown log down its "nothing in flight" quiet path. It is
	// placed AFTER token auth so unauthenticated probes and 401s — which return
	// immediately and are never agent turns — do not inflate the count.
	p.inFlight.Add(1)
	defer p.inFlight.Add(-1)

	// Mirror the same span durably (BOS-890). The in-flight counter above lives
	// only in this process's memory, so it tells the NEXT daemon nothing about
	// the turns this one was serving when it died; the recorder is that memory
	// written down.
	//
	// Only CHAT-scoped targets are recorded. The agent session id is the
	// identifier the resume lane's gates and delivery seam speak, so a
	// session-scoped token has nothing the next daemon could act on — recording
	// it would arm a cycle that every gate below is guaranteed to abandon.
	if p.streams != nil {
		if agentSessionID, _, ok := session.ParseProxyChatTarget(sessionID); ok {
			p.streams.Enter(agentSessionID)
			defer p.streams.Leave(agentSessionID)
		}
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
		_, suspended := agenterr.SuspensionReason(string(prefix))
		if suspended {
			// Trigger mirrors the SignalKind: suspension is modeled as an
			// AuthInvalidated rotation, so it audits under the matching proto
			// enum ROTATION_TRIGGER_AUTH_INVALIDATED. Using a string without a
			// proto enum entry would hydrate back to ROTATION_TRIGGER_UNSPECIFIED
			// via pb.RotationTrigger_value in account_binding.go.
			res, ferr := p.failover.PrepareFailoverKind(r.Context(), sessionID, rotation.AuthInvalidated, "ROTATION_TRIGGER_AUTH_INVALIDATED")
			if ferr != nil {
				// Redaction-safe: the error never carries the token. Not double-logged:
				// with Rotate=false this response falls through to the single
				// logPassthrough(failover_declined) line below.
				_ = ferr
			}
			if res.Rotate {
				_ = resp.Body.Close()
				p.replayAndCommit(w, r, upstreamPath, buffered, sessionID, res)
				return
			}
		}
		// A matched suspension that could not rotate (no target / prepare errored) is
		// a coarse failover_declined; an unmatched 403 is a benign suspension_not_matched.
		reason := passthroughReasonSuspensionNotMatched
		if suspended {
			reason = passthroughReasonFailoverDeclined
		}
		p.logPassthrough(sessionID, r.Method, upstreamPath, resp.StatusCode, reason, "")
		p.copyResponse(w, resp)
		return
	}

	if canAttemptFailover && (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusUnauthorized) {
		res, ferr := p.failover.PrepareFailover(r.Context(), sessionID, resp.StatusCode)
		if ferr != nil {
			// Redaction-safe: PrepareFailover's error never carries the token. Not
			// double-logged: with Rotate=false this response falls through to the
			// single logPassthrough(failover_declined) line at the tail of handleProxy.
			_ = ferr
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
		rotate, sseErrType, reconstructed := p.peekSSEForRateLimit(resp)
		if rotate {
			res, ferr := p.failover.PrepareFailoverKind(r.Context(), sessionID, rotation.UsageLimited, "ROTATION_TRIGGER_USAGE_LIMITED")
			if ferr != nil {
				// Redaction-safe: the error never carries the token. Not double-logged:
				// with Rotate=false this stream falls through to the single
				// logPassthrough(sse_error_passthrough) line below.
				_ = ferr
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
		// reconstructed (buffered prefix + remainder) response through unchanged. A
		// pre-content error frame we could not rotate on (sseErrType != "") is a
		// 200-status pass-through worth surfacing; a normal content stream stays silent.
		if sseErrType != "" {
			p.logPassthrough(sessionID, r.Method, upstreamPath, resp.StatusCode, passthroughReasonSSEErrorPassthrough, sseErrType)
		}
		p.copyResponse(w, reconstructed)
		return
	}
	// Final pass-through for any other response. Only ERROR statuses (≥400) are
	// logged (the silence guard keeps successful responses quiet); the reason is
	// whatever the proxy can prove locally about why it did not rotate.
	if resp.StatusCode >= http.StatusBadRequest {
		reason := passthroughReasonStatusNotIntercepted
		switch {
		case !canFailover:
			// The oversized body was streamed through, so no failover was possible.
			reason = passthroughReasonBodyTooLarge
		case credentiallessManagedSentinel && resp.StatusCode == http.StatusUnauthorized:
			// A managed sentinel whose bearer was unresolved: a 401 we deliberately
			// do not rotate on (would cool a healthy account for a self-inflicted 401).
			reason = passthroughReasonCredentiallessSentinel
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusUnauthorized:
			// A 401/429 the failover machinery saw but declined to rotate (no eligible
			// next account / prepare errored). Coarse by design — no truthful sub-reason.
			reason = passthroughReasonFailoverDeclined
		}
		p.logPassthrough(sessionID, r.Method, upstreamPath, resp.StatusCode, reason, "")
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
//
// It also returns the classified SSE error type (e.g. "rate_limit_error" when
// rotating, or "overloaded_error" for a non-rotating pre-content error frame)
// so a 200-status stream that opened with an error the proxy passes through can
// be logged as status=200 sse_error=<type> (BOS-483). The type is "" when the
// peek reached content or a backstop without a decisive error frame.
func (p *ProxyServer) peekSSEForRateLimit(resp *http.Response) (bool, string, *http.Response) {
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
	sseErrType := ""

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
				dec, errType := classifySSEFrame(frame)
				switch dec {
				case sseRotate:
					rotate = true
					sseErrType = errType
					break peekLoop
				case ssePassthrough:
					// errType is "" for a content_block_delta (normal stream), or the
					// non-rotating error type for a pre-content error frame.
					sseErrType = errType
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
	return rotate, sseErrType, resp
}

// classifySSEFrame decides whether a single SSE frame is a rate_limit_error
// (rotate), a decisive non-rotating frame (content_block_delta or another error →
// pass through), or a pre-content opening frame (undecided → keep peeking). It is
// tolerant: a malformed/partial `data:` payload falls back to the `event:` name
// and never rotates unless it POSITIVELY reads error.type == "rate_limit_error".
// It also returns the classified error type: for an `error` frame this is
// payload.Error.Type (e.g. "rate_limit_error" or "overloaded_error"); for a
// content or undecided frame it is "".
func classifySSEFrame(frame []byte) (sseDecision, string) {
	eventName, dataJSON := parseSSEFrame(frame)
	var payload sseEventData
	_ = json.Unmarshal([]byte(dataJSON), &payload) // tolerant: err ⇒ empty payload
	typ := payload.Type
	if typ == "" {
		typ = eventName
	}
	switch typ {
	case "content_block_delta":
		return ssePassthrough, ""
	case "error":
		if payload.Error.Type == "rate_limit_error" {
			return sseRotate, payload.Error.Type
		}
		return ssePassthrough, payload.Error.Type
	default:
		return sseUndecided, ""
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
	replaySSEErrType := ""
	if replay.StatusCode == http.StatusOK && isSSEContentType(replay.Header.Get("Content-Type")) {
		replayRateLimited, replaySSEErrType, replay = p.peekSSEForRateLimit(replay)
	}

	// The rotated account did not clear the error: the replay leg itself returned
	// ≥400, or opened with a pre-content SSE error. Surface it as replay_still_erroring
	// (BOS-483) so a rotation that fails to help is visible, not silently swallowed.
	if replay.StatusCode >= http.StatusBadRequest {
		p.logPassthrough(sessionID, r.Method, upstreamPath, replay.StatusCode, passthroughReasonReplayStillErroring, "")
	} else if replaySSEErrType != "" {
		p.logPassthrough(sessionID, r.Method, upstreamPath, replay.StatusCode, passthroughReasonReplayStillErroring, replaySSEErrType)
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
