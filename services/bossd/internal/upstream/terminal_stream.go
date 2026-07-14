// Package upstream — terminal_stream.go owns the bossd→bosso half of the
// new TerminalStream RPC introduced for the web /ws/attach feature
// (docs/plans/2026-04-26-web-tmux-attach.md). It is a sibling of stream.go
// rather than a section of it: the keystroke / data-chunk traffic on this
// stream must never starve Stop/Pause/Transfer commands on the control-plane
// DaemonStream, and isolating the writer goroutines + per-attach state is
// the cleanest way to enforce that.
//
// The client is the gRPC client; from its perspective the request stream
// carries TerminalServerMessage (data + exited frames it sends upward) and
// the response stream carries TerminalClientMessage (attach + input + resize
// + close commands from bosso). The "Client"/"Server" labels match the web
// feature's bosso-server / browser-client perspective, not the gRPC roles.
package upstream

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/safego"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/tmux"
	"github.com/rs/zerolog"
)

// isChatNotFound reports whether err signals that the chat row was missing
// (rather than a transient DB failure). The SQLite-backed store returns a
// wrapped error with the literal text "claude_chat not found"; we also
// match sql.ErrNoRows so a future store that surfaces it directly is
// handled too. Kept narrow so unrelated DB errors fall through to the
// generic "chat lookup failed" reason.
func isChatNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, sql.ErrNoRows) {
		return true
	}
	return strings.Contains(err.Error(), "claude_chat not found")
}

// terminalStreamInitialBackoff / terminalStreamMaxBackoff bound the outer
// reconnect loop. Match DaemonStream's bounds so a sustained bosso outage
// settles on the same retry cadence on both sides.
//
// terminalStreamHealthyDuration is the minimum stream lifetime that counts
// as a successful connection for backoff-reset purposes. A stream that
// lived this long is treated as evidence bosso is healthy, so a subsequent
// drop is a transient blip rather than the start of an outage. Mirrors
// the wasConnected() pattern in stream.go: that path uses application-
// level handshake completion as its signal; TerminalStream has no such
// handshake (Send(nil) only flushes headers — bosso may still reject
// later), so duration is the cleanest proxy.
const (
	terminalStreamInitialBackoff  = 1 * time.Second
	terminalStreamMaxBackoff      = 30 * time.Second
	terminalStreamHealthyDuration = terminalStreamMaxBackoff
)

// The BOS-376 terminal-liveness machinery (readiness/heartbeat signalling,
// tunable defaults, errTerminalNotConfirmed, runHeartbeat, escalateWedged,
// and the not-co-located matcher) lives in the sibling terminal_liveness.go.

// terminalBidiStream is the subset of *connect.BidiStreamForClient the
// TerminalStreamClient touches. The real connect type satisfies it
// structurally; tests pass a hand-rolled fake.
type terminalBidiStream interface {
	Send(*pb.TerminalServerMessage) error
	Receive() (*pb.TerminalClientMessage, error)
	CloseRequest() error
}

// terminalStreamOpener mirrors streamOpener for the TerminalStream RPC. The
// production opener wraps a connect client and stamps Authorization +
// X-Daemon-Token headers on every dial; tests inject a fake.
type terminalStreamOpener interface {
	TerminalStream(ctx context.Context) terminalBidiStream
}

// terminalConnectOpener adapts a real OrchestratorServiceClient. Mirrors
// connectOpener's structure so the auth-header logic is identical (decision
// #8 in the plan: reuse DaemonStream's daemon-token + WorkOS JWT
// cross-check). The session_token holder is shared with connectOpener so a
// re-register-driven rotation lands on both openers simultaneously —
// otherwise a token rotation would silently break TerminalStream auth
// until daemon restart.
type terminalConnectOpener struct {
	client       bossanovav1connect.OrchestratorServiceClient
	authToken    string // fallback WorkOS JWT — used only when tokens is nil
	tokens       TokenProvider
	sessionToken *SessionTokenHolder
	// authState mirrors the field on connectOpener — flipping NeedsLogin
	// here pauses the TerminalStreamClient.Run loop on the next iteration
	// so it stops dialling on a credential WorkOS already rejected. The
	// production wiring shares one AuthState with the DaemonStream opener.
	authState *AuthState
	logger    zerolog.Logger
}

// TerminalStream opens a new bidi stream with the same auth headers
// DaemonStream uses: Bearer JWT (refreshed on demand) + X-Daemon-Token.
// bosso cross-checks that the JWT's user owns the daemon identified by the
// session token (services/bosso/internal/server/stream.go).
func (o *terminalConnectOpener) TerminalStream(ctx context.Context) terminalBidiStream {
	raw := o.client.TerminalStream(ctx)
	jwt := o.authToken
	if o.tokens != nil {
		// Refresh proactively if the cached token is within 60s of expiry.
		// Matches connectOpener.DaemonStream's policy so a reconnect after
		// a long bosso outage doesn't tight-loop on stale credentials.
		if exp := o.tokens.ExpiresAt(); !exp.IsZero() && time.Until(exp) < 60*time.Second {
			if _, err := o.tokens.Refresh(ctx); err != nil {
				// invalid_grant means WorkOS has terminally killed the
				// refresh token. Mark the shared AuthState so the Run
				// loop pauses instead of tight-looping on a credential
				// that will never work. Other refresh failures are
				// logged by the DaemonStream opener (which runs more
				// often) — keep this branch quiet to avoid double-warns.
				if errors.Is(err, ErrAuthExpired) && o.authState != nil {
					if o.authState.MarkNeedsLogin() {
						o.logger.Warn().Err(err).Msg("terminal: token refresh rejected as invalid_grant; pausing stream until re-login")
					}
				}
			}
		}
		if t := o.tokens.Token(); t != "" {
			jwt = t
		}
	}
	if jwt != "" {
		raw.RequestHeader().Set("Authorization", "Bearer "+jwt)
	} else if o.authState != nil {
		// Mirror connectOpener.DaemonStream: no JWT to send means no point
		// dialling. Mark NeedsLogin so the Run loop pauses; log only on
		// the state change (the DaemonStream opener typically logs first).
		if o.authState.MarkNeedsLogin() {
			o.logger.Warn().Msg("terminal: no upstream credentials available; pausing stream until login")
		}
	}
	if o.sessionToken != nil {
		if tok := o.sessionToken.Get(); tok != "" {
			raw.RequestHeader().Set("X-Daemon-Token", tok)
		}
	}
	return terminalConnectBidiAdapter{stream: raw}
}

// terminalConnectBidiAdapter wraps the connect bidi type so the local
// interface is satisfied explicitly.
type terminalConnectBidiAdapter struct {
	stream *connect.BidiStreamForClient[pb.TerminalServerMessage, pb.TerminalClientMessage]
}

func (a terminalConnectBidiAdapter) Send(m *pb.TerminalServerMessage) error {
	return a.stream.Send(m)
}

func (a terminalConnectBidiAdapter) Receive() (*pb.TerminalClientMessage, error) {
	return a.stream.Receive()
}

func (a terminalConnectBidiAdapter) CloseRequest() error { return a.stream.CloseRequest() }

// terminalAttach is the subset of *tmux.TerminalAttach this client uses.
// Hoisted to an interface so tests can drop in a fake without spawning a
// PTY. The production path passes *tmux.TerminalAttach directly.
type terminalAttach interface {
	Output() <-chan *pb.TerminalDataChunk
	Exited() <-chan *pb.TerminalAttachExited
	Input(data []byte) error
	Resize(cols, rows uint32) error
	Close() error
}

// terminalAttachFactory builds the per-attach pipeline. The default builds
// a real *tmux.TerminalAttach; tests inject a fake-attach factory.
type terminalAttachFactory func(ctx context.Context, cfg tmux.AttachConfig) (terminalAttach, error)

func defaultTerminalAttachFactory(ctx context.Context, cfg tmux.AttachConfig) (terminalAttach, error) {
	return tmux.NewTerminalAttach(ctx, cfg)
}

// chatLookup returns the persisted tmux_session_name for the given
// claude_id. Constrained to the one method the client needs — keeps tests
// from having to implement the full AgentChatStore surface.
//
// MUST read the persisted `tmux_session_name` field on the chat row. Do
// NOT recompute via tmux.ChatSessionName(repoID, agentSessionID) — see plan
// Codex catch #5: the recomputed name is truncated to 8 chars per
// component and risks collisions across sessions.
type chatLookup interface {
	GetByAgentSessionID(ctx context.Context, agentSessionID string) (chatRow, error)
}

// chatRow is the thin projection of models.ClaudeChat the client cares
// about. Just the persisted tmux session name. Defined as a local struct
// so tests don't depend on bossalib/models.
type chatRow struct {
	TmuxSessionName *string
}

// chatStoreAdapter wraps the real db.AgentChatStore so it satisfies the
// minimal chatLookup interface above. Production wiring builds this; tests
// pass their own fake chatLookup directly.
type chatStoreAdapter struct {
	store db.AgentChatStore
}

// NewChatStoreLookup builds a chatLookup backed by the daemon's real
// AgentChatStore. Exported because Task A4's main.go wiring needs to
// construct a TerminalStreamClient with the live store.
func NewChatStoreLookup(store db.AgentChatStore) ChatLookup {
	return &chatStoreAdapter{store: store}
}

// ChatLookup is the public re-export of the unexported chatLookup
// interface. Task A4 wires a concrete adapter into the client config.
type ChatLookup = chatLookup

func (a *chatStoreAdapter) GetByAgentSessionID(ctx context.Context, agentSessionID string) (chatRow, error) {
	c, err := a.store.GetByAgentSessionID(ctx, agentSessionID)
	if err != nil {
		return chatRow{}, err
	}
	return chatRow{TmuxSessionName: c.TmuxSessionName}, nil
}

// TerminalStreamClient owns the bossd-side of the TerminalStream RPC.
// Multiplexes per-attach goroutines through a single bidi stream, keyed by
// attach_id.
//
// Lifecycle:
//   - Run(ctx) starts a connect/reconnect loop that opens a TerminalStream
//     and pumps inbound TerminalClientMessages → per-attach actions and
//     outbound per-attach output → TerminalServerMessages. Returns when
//     ctx is cancelled.
//   - On stream error with active attaches, all attaches are torn down
//     (their browser counterparts must reconnect to recover state) and
//     the loop reconnects with exponential backoff.
//   - On stream error with no active attaches, the same teardown happens
//     but the reconnect is still attempted — bosso may push a new attach
//     at any moment and starting the connect from cold would add latency
//     to the first attach.
type TerminalStreamClient struct {
	opener        terminalStreamOpener
	authState     *AuthState
	tmuxClient    *tmux.Client
	chats         chatLookup
	attachFactory terminalAttachFactory
	logger        zerolog.Logger
	clock         Clock

	// attaches is the per-stream multiplex map. Reset on every reconnect —
	// attach_ids are bosso-scoped and not portable across stream incarnations.
	mu            sync.Mutex
	attaches      map[string]*activeAttach
	streamClosing bool

	// cycleCancel cancels the Run loop's current stream attempt.
	// CycleStream fires it; Run installs it before each openStream and
	// clears it afterwards. Guarded by cycleMu (not mu — the attach map's
	// mutex is held across attach bookkeeping and must not couple to
	// lifecycle signalling).
	cycleMu     sync.Mutex
	cycleCancel context.CancelFunc

	// Terminal liveness handshake / watchdog (BOS-376). health is nil in
	// legacy tests; when nil the ready-gate, heartbeat, and escalation are
	// all skipped and the client behaves exactly as before. reRegister and
	// closeIdle are the paired-re-dial hooks the watchdog fires after K
	// consecutive ready-timeouts; readyTimeoutStreak counts them (only ever
	// touched from the single Run goroutine, so it needs no lock).
	health             *TerminalHealth
	reRegister         func(context.Context)
	closeIdle          func()
	readyTimeout       time.Duration
	pingInterval       time.Duration
	missedBeatsBudget  int
	readyTimeoutBudget int
	readyTimeoutStreak int
}

// activeAttach is the per-attach bookkeeping the client maintains while a
// PTY is alive. The fan goroutine is owned by the client (not the
// TerminalAttach) so it can exit cleanly when the stream drops. `done` is
// the channel returned by safego.Go — closed once the pump goroutine has
// fully exited (including its panic-recovery log path).
type activeAttach struct {
	id          string
	sessionName string
	attach      terminalAttach
	cancel      context.CancelFunc
	done        <-chan struct{}
}

// TerminalStreamClientConfig bundles the constructor inputs.
type TerminalStreamClientConfig struct {
	// Opener is the live ConnectRPC client wrapper. If nil, an opener is
	// built from Client + AuthToken + SessionToken at construction time.
	Opener    terminalStreamOpener
	Client    bossanovav1connect.OrchestratorServiceClient
	AuthToken string // WorkOS JWT for Authorization header
	// SessionToken, when non-nil, is the shared holder for the daemon
	// session_token sent in X-Daemon-Token. Pass the same holder used by
	// the DaemonStream opener so a re-register-driven rotation reaches
	// both. If nil and Opener is nil, an empty holder is created — which
	// means rotations from the DaemonStream side will not propagate, so
	// any production wiring should provide a shared holder explicitly.
	SessionToken *SessionTokenHolder

	// TokenProvider, when set, is consulted on every TerminalStream open
	// so that reconnects after a bosso outage use a freshly-refreshed
	// JWT. Mirrors StreamClient's behaviour.
	TokenProvider TokenProvider

	// AuthState, when set, is the shared "needs re-login" flag used by
	// both stream clients. Production wiring should pass the same
	// instance as StreamClientConfig.AuthState so a rejection on either
	// bidi pauses both. Nil keeps the legacy reconnect-forever behaviour.
	AuthState *AuthState

	// TmuxClient is used for SetAttachOptions on every new attach (via
	// tmux.NewTerminalAttach) and for RefreshClient after a ring-buffer
	// overflow forces a RESYNC.
	TmuxClient *tmux.Client

	// Chats is the persisted-name lookup. The client MUST consult the
	// stored tmux_session_name on the chat row, not recompute one.
	Chats chatLookup

	// AttachFactory, when set, replaces the default tmux.NewTerminalAttach
	// constructor. Used by tests to drop in a fake-attach implementation.
	AttachFactory terminalAttachFactory

	// Health, when set, enables the BOS-376 positive-liveness path:
	// openStream waits for a TerminalReady frame before treating the
	// attempt as connected, runs the heartbeat watchdog, and the Run loop
	// escalates a run of ready-timeouts. Nil keeps the pre-BOS-376
	// behaviour (used by the legacy unit tests).
	Health *TerminalHealth

	// ReRegister is invoked by the watchdog after ReadyTimeoutBudget
	// consecutive ready-timeouts to force a fresh DaemonStream
	// registration (which rotates the session token and nudges the
	// DaemonStream to reconnect). CloseIdle drops pooled HTTP/2
	// connections so the paired re-dial lands both reverse streams on one
	// fresh connection, co-locating them. Both are optional.
	ReRegister func(context.Context)
	CloseIdle  func()

	// Liveness tunables. Zero values fall back to the terminal* constants.
	ReadyTimeout       time.Duration
	PingInterval       time.Duration
	MissedBeatsBudget  int
	ReadyTimeoutBudget int

	Logger zerolog.Logger
	Clock  Clock // nil → realClock
}

// NewTerminalStreamClient assembles a client. Pointer fields in the config
// are optional — defaults are filled in here. Panics when TmuxClient is
// nil (refresh-client cannot work without it, and a silent miswiring would
// leave overflow RESYNCs unrepainted with no log line).
func NewTerminalStreamClient(cfg TerminalStreamClientConfig) *TerminalStreamClient {
	if cfg.TmuxClient == nil {
		panic("upstream: NewTerminalStreamClient: TmuxClient is required")
	}
	opener := cfg.Opener
	if opener == nil && cfg.Client != nil {
		holder := cfg.SessionToken
		if holder == nil {
			holder = NewSessionTokenHolder("")
		}
		opener = &terminalConnectOpener{
			client:       cfg.Client,
			authToken:    cfg.AuthToken,
			sessionToken: holder,
			tokens:       cfg.TokenProvider,
			authState:    cfg.AuthState,
			logger:       cfg.Logger.With().Str("component", "terminal-stream-opener").Logger(),
		}
	}
	clock := cfg.Clock
	if clock == nil {
		clock = realClock{}
	}
	factory := cfg.AttachFactory
	if factory == nil {
		factory = defaultTerminalAttachFactory
	}
	readyTimeout := cfg.ReadyTimeout
	if readyTimeout <= 0 {
		readyTimeout = terminalReadyTimeout
	}
	pingInterval := cfg.PingInterval
	if pingInterval <= 0 {
		pingInterval = terminalPingInterval
	}
	missedBeatsBudget := cfg.MissedBeatsBudget
	if missedBeatsBudget <= 0 {
		missedBeatsBudget = terminalMissedBeatsBudget
	}
	readyTimeoutBudget := cfg.ReadyTimeoutBudget
	if readyTimeoutBudget <= 0 {
		readyTimeoutBudget = terminalReadyTimeoutBudget
	}
	return &TerminalStreamClient{
		opener:             opener,
		authState:          cfg.AuthState,
		tmuxClient:         cfg.TmuxClient,
		chats:              cfg.Chats,
		attachFactory:      factory,
		logger:             cfg.Logger.With().Str("component", "terminal-stream-client").Logger(),
		clock:              clock,
		attaches:           make(map[string]*activeAttach),
		health:             cfg.Health,
		reRegister:         cfg.ReRegister,
		closeIdle:          cfg.CloseIdle,
		readyTimeout:       readyTimeout,
		pingInterval:       pingInterval,
		missedBeatsBudget:  missedBeatsBudget,
		readyTimeoutBudget: readyTimeoutBudget,
	}
}

// Run blocks until ctx is cancelled. The reconnect loop opens a new
// TerminalStream on every iteration; on error all in-flight attaches are
// torn down before the next attempt.
func (c *TerminalStreamClient) Run(ctx context.Context) error {
	if c.opener == nil {
		return errors.New("terminal stream client: opener is required")
	}
	backoff := terminalStreamInitialBackoff
	for {
		if ctx.Err() != nil {
			c.closeAllAttaches()
			return nil
		}

		// Pause when the shared AuthState says the refresh token is
		// dead. Mirrors StreamClient.Run — keeps the daemon from
		// hammering bosso with a credential WorkOS already rejected.
		// Wait until NotifyLogin clears the flag (or ctx cancels).
		if c.authState != nil && c.authState.NeedsLogin() {
			c.logger.Warn().Msg("terminal stream paused: re-login required (waiting for boss login)")
			c.closeAllAttaches()
			select {
			case <-ctx.Done():
				return nil
			case <-c.authState.Wait():
				c.logger.Info().Msg("terminal stream resumed after re-login signal")
			}
			backoff = terminalStreamInitialBackoff
			continue
		}

		// Each attempt runs under its own cancellable context so
		// CycleStream can abandon a live stream without touching the
		// caller's ctx. attemptCtx cancelled while ctx is still alive is
		// therefore an unambiguous "cycled" signal.
		attemptCtx, attemptCancel := context.WithCancel(ctx)
		c.setCycleCancel(attemptCancel)

		startedAt := c.clock.Now()
		err := c.openStream(attemptCtx)
		c.setCycleCancel(nil)
		cycled := attemptCtx.Err() != nil && ctx.Err() == nil
		attemptCancel()
		if ctx.Err() != nil {
			c.closeAllAttaches()
			return nil
		}
		if cycled {
			// The DaemonStream completed a (re-)registration handshake:
			// bosso replaced our DaemonState, so whatever sender the old
			// stream had installed is stranded. Reconnect immediately —
			// this is an intentional rebind, not a failure, so no warn
			// and no backoff ramp.
			c.logger.Info().Msg("terminal stream cycled after daemon re-registration; rebinding")
			backoff = terminalStreamInitialBackoff
			continue
		}
		if c.authState != nil && c.authState.NeedsLogin() {
			// openStream returned because logout cancelled the inner
			// streamCtx. That is an intentional pause, not a stream
			// failure, so skip the reconnect warning/backoff and loop
			// straight to the NeedsLogin gate above.
			continue
		}
		// BOS-376 watchdog: a run of ready-timeouts means the terminal
		// stream keeps opening but never confirms readiness — the
		// alive-but-wrongly-bound (deploy cross-pod split) signature that
		// HTTP/2 keepalive cannot see. After K in a row, force a paired
		// DaemonStream re-register + fresh-connection re-dial so both
		// reverse streams co-locate again. Any other outcome (healthy
		// stream, transport error, ready confirmed) resets the streak.
		if c.health != nil && errors.Is(err, errTerminalNotConfirmed) {
			c.readyTimeoutStreak++
			if c.readyTimeoutStreak >= c.readyTimeoutBudget {
				c.escalateWedged(ctx)
				c.readyTimeoutStreak = 0
				// Reset backoff so the paired re-dial the watchdog just
				// forced happens promptly. Waiting out a ramped backoff here
				// would blunt the "instigate restart until they know they are
				// connected" responsiveness at exactly the moment we want the
				// fresh connection to re-dial and co-locate. The K-gate on the
				// streak still prevents tight-looping the escalation itself.
				backoff = terminalStreamInitialBackoff
			}
		} else {
			c.readyTimeoutStreak = 0
		}

		// Treat any stream that lived >= terminalStreamHealthyDuration as a
		// successful connection: any error after that is a transient drop,
		// not a sick bosso, so the next reconnect should start from the
		// initial backoff regardless of how high prior failures had ramped
		// it. Without this, a stream that runs healthily for hours then
		// blips inherits whatever backoff value the loop carried in.
		wasHealthy := c.clock.Now().Sub(startedAt) >= terminalStreamHealthyDuration
		if err == nil || wasHealthy {
			backoff = terminalStreamInitialBackoff
		}
		if err != nil {
			c.logger.Warn().Err(err).Dur("backoff", backoff).Msg("terminal stream closed, reconnecting")
		}

		select {
		case <-ctx.Done():
			c.closeAllAttaches()
			return nil
		case <-c.clock.After(backoff):
		}

		// Only ramp the backoff when this attempt failed quickly enough
		// that we have no evidence the stream was ever healthy.
		if err != nil && !wasHealthy {
			backoff *= 2
			if backoff > terminalStreamMaxBackoff {
				backoff = terminalStreamMaxBackoff
			}
		}
	}
}

// CycleStream abandons the current TerminalStream attempt (if any) so the
// Run loop reconnects immediately. Production wiring invokes this from
// StreamClientConfig.OnHandshake: every DaemonStream registration builds a
// fresh DaemonState on bosso, and a terminal sender installed on the prior
// state is permanently stranded — bosso can close the orphaned handler
// (its half of the fix), but cycling here guarantees the rebind even
// against orchestrators that don't. Active attaches are torn down by the
// normal openStream teardown; their browsers reconnect via the WS layer,
// which bosso force-closed at the same replace. Nil-safe and non-blocking;
// a no-op when no attempt is in flight (the imminent attempt binds the
// fresh state anyway).
func (c *TerminalStreamClient) CycleStream() {
	if c == nil {
		return
	}
	c.cycleMu.Lock()
	cancel := c.cycleCancel
	c.cycleMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// setCycleCancel publishes (or clears) the cancel func for the Run loop's
// current stream attempt.
func (c *TerminalStreamClient) setCycleCancel(cancel context.CancelFunc) {
	c.cycleMu.Lock()
	c.cycleCancel = cancel
	c.cycleMu.Unlock()
}

// openStream runs a single connect attempt end-to-end. Returns when the
// stream errors out (so the outer loop can reconnect) or ctx is cancelled.
//
// Goroutines:
//   - reader (this goroutine): blocks on stream.Receive and dispatches
//     inbound commands (attach, input, resize, close).
//   - writer: drains the per-stream outbound channel (where per-attach
//     pumps fan their output).
//   - per-attach pump (one per active attach): forwards Output() and
//     Exited() onto the outbound channel, fires RefreshClient on
//     lost=true, and removes the attach from the map after Exited fires.
//
// Teardown ordering is load-bearing. closeAllAttaches MUST run before
// the streamCtx cancel: if cancel ran first, every per-attach pump
// would observe ctx.Done() before closeAllAttaches could call Close on
// the underlying TerminalAttach, leaving PTYs leaked on the daemon.
// closeAllAttaches calls Close() on each attach (which drains Output
// naturally), then cancels each attach's context, then joins the
// pumps' done channels — so by the time we return here, every PTY is
// torn down.
func (c *TerminalStreamClient) openStream(ctx context.Context) error {
	streamCtx, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	c.streamClosing = false
	c.mu.Unlock()
	authWatchDone := c.closeAttachesThenCancelOnNeedsLogin(streamCtx, cancel)
	defer func() {
		cancel()
		<-authWatchDone
	}()

	stream := c.opener.TerminalStream(streamCtx)

	// Connect bidi streams don't dispatch the HTTP request until the first
	// Send (or CloseRequest). Without this nudge, runReader below would
	// block on Receive() while the request sits unsent — bosso's
	// TerminalStream handler never runs, never calls StartTerminalSender,
	// and every ws_attach fails with "no active terminal sender" until the
	// daemon restarts. DaemonStream avoids this naturally because it
	// always sends a snapshot first; TerminalStream has nothing to send
	// until bosso pushes an attach, so flush the headers explicitly.
	// connect-go's Send(nil) is documented as a header-only frame.
	if err := stream.Send(nil); err != nil {
		return fmt.Errorf("flush terminal stream headers: %w", err)
	}

	outbound := make(chan *pb.TerminalServerMessage, 64)

	writerDone := safego.Go(c.logger, func() {
		c.runWriter(streamCtx, stream, outbound)
	})

	// The reader runs on its own goroutine (BOS-376) so openStream can wait
	// on the readiness gate concurrently with dispatch. readErrCh (buffered)
	// carries the reader's terminal error; readerDone closes after the
	// goroutine — including its panic-recovery path — has fully exited, so
	// it is safe to join before closing outbound.
	sess := newTerminalStreamSession()
	readErrCh := make(chan error, 1)
	readerDone := safego.Go(c.logger, func() {
		readErrCh <- c.runReader(streamCtx, stream, outbound, sess)
	})

	// hbDone is a closed channel until the ready gate passes and the
	// heartbeat watchdog is armed; teardown always joins it.
	closed := make(chan struct{})
	close(closed)
	hbDone := (<-chan struct{})(closed)

	// Tear down attaches in phases. TerminalAttach.Close must run before
	// streamCtx cancellation so PTYs are not leaked, but streamCtx cancellation
	// must happen before waiting on attach pumps so any pump blocked on outbound
	// can observe ctx.Done. Only close outbound after every attach producer
	// (the pumps AND the reader, which emits pongs) is done, then wait for
	// the single writer. MarkUnhealthy on every exit so the watchdog never
	// sees a stale healthy signal after teardown.
	teardown := func() {
		if c.health != nil {
			c.health.MarkUnhealthy()
		}
		states := c.closeActiveAttaches()
		cancel()
		c.cancelAndWaitAttaches(states)
		<-readerDone
		<-hbDone
		<-authWatchDone
		close(outbound)
		<-writerDone
	}

	finalErr := func(readErr, readCtxErr error) error {
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		if readCtxErr != nil {
			return readCtxErr
		}
		return nil
	}

	// BOS-376 readiness gate. Only active when a health signal is wired
	// (production); legacy tests without one skip straight to the reader
	// join and behave exactly as before.
	if c.health != nil {
		select {
		case <-sess.readyCh:
			if c.health.MarkHealthy() {
				c.logger.Info().Msg("terminal stream ready confirmed")
			}
		case <-c.clock.After(c.readyTimeout):
			c.health.NoteReadyTimeout()
			c.logger.Warn().Dur("timeout", c.readyTimeout).Msg("terminal stream: no TerminalReady within deadline; reconnecting")
			teardown()
			return errTerminalNotConfirmed
		case readErr := <-readErrCh:
			readCtxErr := streamCtx.Err()
			teardown()
			return finalErr(readErr, readCtxErr)
		case <-streamCtx.Done():
			teardown()
			return finalErr(nil, streamCtx.Err())
		}

		// Ready confirmed — arm the heartbeat watchdog. It resets on every
		// TerminalPing and tears the stream down after MissedBeatsBudget
		// silent intervals (an alive-but-wrongly-bound stream stops getting
		// pings). It writes nothing to outbound, so it need not be joined
		// before closing outbound — but teardown joins it anyway to avoid a
		// goroutine leak across reconnects.
		hbDone = safego.Go(c.logger, func() {
			c.runHeartbeat(streamCtx, sess, cancel)
		})
	}

	readErr := <-readErrCh
	readCtxErr := streamCtx.Err()
	teardown()
	return finalErr(readErr, readCtxErr)
}

// closeAttachesThenCancelOnNeedsLogin watches for logout and, when it
// fires, tears the active attaches down in the load-bearing order (Close
// each attach before cancelling streamCtx, then join the pumps) so PTYs
// are not leaked — see openStream's teardown comment. The bare cancel that
// the daemon stream uses would skip the attach Close and leak terminals.
func (c *TerminalStreamClient) closeAttachesThenCancelOnNeedsLogin(ctx context.Context, cancel context.CancelFunc) <-chan struct{} {
	return watchNeedsLogin(ctx, c.authState, func() {
		states := c.closeActiveAttaches()
		cancel()
		c.cancelAndWaitAttaches(states)
	})
}

// runWriter drains outbound onto the stream. Returns when outbound is
// closed (the openStream teardown path) or stream.Send fails.
func (c *TerminalStreamClient) runWriter(ctx context.Context, stream terminalBidiStream, outbound <-chan *pb.TerminalServerMessage) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-outbound:
			if !ok {
				return
			}
			if err := stream.Send(msg); err != nil {
				c.logger.Debug().Err(err).Msg("terminal stream send failed; writer exiting")
				return
			}
		}
	}
}

// runReader blocks on stream.Receive and dispatches each inbound command.
// Returns when Receive errors so the outer loop can reconnect.
func (c *TerminalStreamClient) runReader(ctx context.Context, stream terminalBidiStream, outbound chan<- *pb.TerminalServerMessage, sess *terminalStreamSession) error {
	for {
		msg, err := stream.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if ctx.Err() != nil {
				return nil
			}
			// BOS-376: bosso tags a not-co-located precondition rejection
			// (the TerminalStream landed on a pod whose DaemonStream is not
			// local-and-Ready — the deploy cross-pod split). Map it to
			// errTerminalNotConfirmed so the Run-loop watchdog counts it
			// toward the forced-re-register streak, exactly like a
			// ready-timeout. A plain transport error stays transient and
			// does NOT feed the streak.
			if isTerminalNotColocated(err) {
				return fmt.Errorf("%w: %v", errTerminalNotConfirmed, err)
			}
			return fmt.Errorf("terminal stream receive: %w", err)
		}
		c.handleClientMessage(ctx, msg, outbound, sess)
	}
}

// handleClientMessage routes one inbound TerminalClientMessage. Unknown
// attach_ids on input/resize/close are logged and dropped (forward-compat
// with future commands).
func (c *TerminalStreamClient) handleClientMessage(ctx context.Context, msg *pb.TerminalClientMessage, outbound chan<- *pb.TerminalServerMessage, sess *terminalStreamSession) {
	switch m := msg.GetMsg().(type) {
	case *pb.TerminalClientMessage_Attach:
		c.handleAttach(ctx, m.Attach, outbound)
	case *pb.TerminalClientMessage_Input:
		c.handleInput(m.Input)
	case *pb.TerminalClientMessage_Resize:
		c.handleResize(m.Resize)
	case *pb.TerminalClientMessage_Close:
		c.handleClose(m.Close)
	case *pb.TerminalClientMessage_Ready:
		// BOS-376: positive proof this stream is bound to a co-located,
		// Ready bosso pod. Unblocks openStream's readiness gate.
		sess.signalReady()
	case *pb.TerminalClientMessage_Ping:
		// BOS-376: application-level liveness probe. Reset the heartbeat
		// watchdog and echo a matching pong so bosso can measure the RTT.
		sess.signalPing()
		c.sendServerMessage(ctx, outbound, &pb.TerminalServerMessage{
			Msg: &pb.TerminalServerMessage_Pong{Pong: &pb.TerminalPong{Seq: m.Ping.GetSeq()}},
		})
	default:
		c.logger.Warn().Msg("terminal stream: unknown client message; dropping")
	}
}

// handleAttach validates the attach request, looks up the persisted tmux
// session name on the chat row, spawns a TerminalAttach, and starts the
// per-attach fan goroutine. On any failure the attach is reported back to
// bosso as TerminalAttachExited so the browser can surface a clean error.
func (c *TerminalStreamClient) handleAttach(ctx context.Context, cmd *pb.TerminalAttachCommand, outbound chan<- *pb.TerminalServerMessage) {
	attachID := cmd.GetAttachId()
	if attachID == "" {
		c.logger.Warn().Msg("terminal stream: attach with empty attach_id; dropping")
		return
	}
	chatID := cmd.GetChatId()
	if chatID == "" {
		c.sendExited(ctx, outbound, attachID, "attach: empty chat_id")
		return
	}

	c.mu.Lock()
	if _, exists := c.attaches[attachID]; exists {
		c.mu.Unlock()
		c.logger.Warn().Str("attach_id", attachID).Msg("terminal stream: duplicate attach_id; dropping")
		return
	}
	c.mu.Unlock()

	row, err := c.chats.GetByAgentSessionID(ctx, chatID)
	if err != nil {
		// Wire-side `Reason` MUST be a fixed string. The full error stays
		// in the daemon log for triage, but the browser never sees driver
		// text like "sql: no rows in result set" — that's both noisy and a
		// (mild) information leak. The chat store reports not-found via a
		// wrapped sentinel-style "claude_chat not found" message; sql.ErrNoRows
		// is also matched in case a future store passes the raw error
		// through.
		c.logger.Warn().Err(err).Str("chat_id", chatID).Msg("terminal stream: chat lookup failed")
		reason := "chat lookup failed"
		if isChatNotFound(err) {
			reason = "chat not found"
		}
		c.sendExited(ctx, outbound, attachID, reason)
		return
	}
	if row.TmuxSessionName == nil || *row.TmuxSessionName == "" {
		// Plan Codex catch #3: empty/nil tmux_session_name → clean error
		// rather than silently using ChatSessionName(repoID, agentSessionID).
		c.sendExited(ctx, outbound, attachID, "tmux session not ready")
		return
	}
	sessionName := *row.TmuxSessionName

	attachCtx, cancel := context.WithCancel(ctx)
	attach, err := c.attachFactory(attachCtx, tmux.AttachConfig{
		AttachID:    attachID,
		SessionName: sessionName,
		Cols:        cmd.GetCols(),
		Rows:        cmd.GetRows(),
		TmuxClient:  c.tmuxClient,
		Logger:      c.logger,
	})
	if err != nil {
		cancel()
		c.logger.Warn().Err(err).Str("attach_id", attachID).Msg("terminal stream: attach spawn failed")
		c.sendExited(ctx, outbound, attachID, fmt.Sprintf("attach spawn: %v", err))
		return
	}

	state := &activeAttach{
		id:          attachID,
		sessionName: sessionName,
		attach:      attach,
		cancel:      cancel,
	}

	pumpStart := make(chan struct{})
	// safego.Go gives us panic recovery + a done channel for the teardown
	// path; assign done before publishing state so logout teardown can never
	// snapshot a partially initialized activeAttach. Gate the pump until after
	// the attach is in c.attaches so a fast-exiting attach can still remove
	// itself from the map.
	state.done = safego.Go(c.logger, func() {
		<-pumpStart
		c.runAttachPump(attachCtx, state, outbound)
	})

	c.mu.Lock()
	if c.streamClosing || attachCtx.Err() != nil {
		c.mu.Unlock()
		_ = attach.Close()
		cancel()
		close(pumpStart)
		<-state.done
		return
	}
	if _, exists := c.attaches[attachID]; exists {
		// Race: a concurrent attach with the same id beat us here. Tear
		// down our newly-spawned PTY and bail.
		c.mu.Unlock()
		_ = attach.Close()
		cancel()
		close(pumpStart)
		<-state.done
		c.logger.Warn().Str("attach_id", attachID).Msg("terminal stream: lost duplicate-attach race; dropping")
		return
	}
	c.attaches[attachID] = state
	c.mu.Unlock()
	close(pumpStart)
}

// runAttachPump fans Output() and Exited() onto the per-stream outbound
// channel. Owns the per-attach lifecycle: it removes the attach from the
// map before returning so the writer can quiesce. The done channel
// (closed by safego.Go after this function returns) is what
// closeAllAttaches joins on for shutdown.
func (c *TerminalStreamClient) runAttachPump(ctx context.Context, state *activeAttach, outbound chan<- *pb.TerminalServerMessage) {
	defer func() {
		c.mu.Lock()
		// Only remove if still ours — handleClose may have already swapped
		// us out and closed the attach.
		if existing, ok := c.attaches[state.id]; ok && existing == state {
			delete(c.attaches, state.id)
		}
		c.mu.Unlock()
	}()

	output := state.attach.Output()
	exited := state.attach.Exited()

	for {
		select {
		case <-ctx.Done():
			return
		case chunk, ok := <-output:
			if !ok {
				// Output closed → wait for Exited (which the
				// TerminalAttach guarantees fires after Output is
				// closed) and forward it.
				select {
				case ev, ok := <-exited:
					if !ok {
						return
					}
					c.sendServerMessage(ctx, outbound, &pb.TerminalServerMessage{
						Msg: &pb.TerminalServerMessage_Exited{Exited: ev},
					})
				case <-ctx.Done():
				}
				return
			}
			if chunk.GetLost() {
				// Fire and forget — RefreshClient is best-effort. A
				// failure here just means the outer terminals don't
				// repaint immediately; the next user keypress will
				// likely trigger a redraw.
				c.fireRefreshClient(ctx, state.sessionName)
			}
			c.sendServerMessage(ctx, outbound, &pb.TerminalServerMessage{
				Msg: &pb.TerminalServerMessage_Data{Data: chunk},
			})
		}
	}
}

// fireRefreshClient invokes tmux.Client.RefreshClient in a separate
// goroutine so the per-attach pump never blocks on a slow tmux invocation.
// Logs failures but doesn't propagate — the pump's correctness doesn't
// depend on the repaint succeeding. safego.Go provides panic recovery; we
// discard the returned done channel because this is fire-and-forget.
func (c *TerminalStreamClient) fireRefreshClient(ctx context.Context, sessionName string) {
	if c.tmuxClient == nil {
		// tmuxClient is required by NewTerminalStreamClient; this branch
		// is defensive only (e.g. tests that swap it out).
		return
	}
	_ = safego.Go(c.logger, func() {
		if err := c.tmuxClient.RefreshClient(ctx, sessionName); err != nil {
			c.logger.Warn().Err(err).Str("session", sessionName).Msg("tmux refresh-client failed after lost-bytes RESYNC")
		}
	})
}

// handleInput routes a TerminalInputCommand to the matching attach. Drops
// silently if the attach_id is unknown — bosso may have reordered messages
// past a Close, or the attach exited on the daemon side before the input
// landed.
func (c *TerminalStreamClient) handleInput(cmd *pb.TerminalInputCommand) {
	state := c.lookupAttach(cmd.GetAttachId())
	if state == nil {
		c.logger.Debug().Str("attach_id", cmd.GetAttachId()).Msg("terminal stream: input for unknown attach; dropping")
		return
	}
	if err := state.attach.Input(cmd.GetData()); err != nil {
		c.logger.Debug().Err(err).Str("attach_id", cmd.GetAttachId()).Msg("terminal stream: input write failed")
	}
}

// handleResize routes a TerminalResizeCommand to the matching attach.
func (c *TerminalStreamClient) handleResize(cmd *pb.TerminalResizeCommand) {
	state := c.lookupAttach(cmd.GetAttachId())
	if state == nil {
		c.logger.Debug().Str("attach_id", cmd.GetAttachId()).Msg("terminal stream: resize for unknown attach; dropping")
		return
	}
	if err := state.attach.Resize(cmd.GetCols(), cmd.GetRows()); err != nil {
		c.logger.Debug().Err(err).Str("attach_id", cmd.GetAttachId()).Msg("terminal stream: resize failed")
	}
}

// handleClose tears down the matching attach. The TerminalAttach's own
// Exited() will surface via runAttachPump, so we don't double-emit
// TerminalAttachExited here.
//
// Notably we do NOT cancel state.cancel() here: doing so would race the
// pump's `<-ctx.Done()` against the Output/Exited drain and frequently
// truncate the synthetic exit frame. The pump exits naturally once the
// attach's Output channel closes (which Close() guarantees) and the
// Exited frame is forwarded.
func (c *TerminalStreamClient) handleClose(cmd *pb.TerminalCloseCommand) {
	state := c.lookupAttach(cmd.GetAttachId())
	if state == nil {
		c.logger.Debug().Str("attach_id", cmd.GetAttachId()).Msg("terminal stream: close for unknown attach; dropping")
		return
	}
	if err := state.attach.Close(); err != nil {
		c.logger.Debug().Err(err).Str("attach_id", cmd.GetAttachId()).Msg("terminal stream: attach close returned error")
	}
}

// lookupAttach returns the active attach state for an attach_id, or nil.
func (c *TerminalStreamClient) lookupAttach(attachID string) *activeAttach {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.attaches[attachID]
}

// closeActiveAttaches snapshots and removes every active attach, then calls
// TerminalAttach.Close on each one before any caller-triggered context
// cancellation. Snapshotting first prevents attach pumps that observe
// cancellation from removing themselves before teardown can Close the
// underlying PTY. Marking the stream as closing under the same lock prevents
// handleAttach from publishing a just-spawned attach after this snapshot.
func (c *TerminalStreamClient) closeActiveAttaches() []*activeAttach {
	c.mu.Lock()
	c.streamClosing = true
	states := make([]*activeAttach, 0, len(c.attaches))
	for _, s := range c.attaches {
		states = append(states, s)
	}
	c.attaches = make(map[string]*activeAttach)
	c.mu.Unlock()

	// Close the attach FIRST: that closes Output() which lets the pump
	// drain naturally rather than racing ctx.Done. The cancel afterwards
	// is a belt-and-braces guarantee for any pump still blocked on a send.
	for _, s := range states {
		if err := s.attach.Close(); err != nil {
			c.logger.Debug().Err(err).Str("attach_id", s.id).Msg("terminal stream: attach close returned error during teardown")
		}
	}
	return states
}

func (c *TerminalStreamClient) cancelAndWaitAttaches(states []*activeAttach) {
	for _, s := range states {
		s.cancel()
	}
	for _, s := range states {
		<-s.done
	}
}

// closeAllAttaches tears down every active attach. Called on stream error
// and on context cancellation. Waiting on done at the end serialises
// goroutine shutdown so a subsequent reconnect starts from a clean slate.
func (c *TerminalStreamClient) closeAllAttaches() {
	c.cancelAndWaitAttaches(c.closeActiveAttaches())
}

// sendServerMessage pushes a message onto the outbound channel, respecting
// ctx cancellation. Used by the per-attach pumps so they don't deadlock if
// the writer has already exited.
func (c *TerminalStreamClient) sendServerMessage(ctx context.Context, outbound chan<- *pb.TerminalServerMessage, msg *pb.TerminalServerMessage) {
	select {
	case outbound <- msg:
	case <-ctx.Done():
	}
}

// sendExited synthesises a TerminalAttachExited frame for an attach that
// failed before its TerminalAttach was spawned (or whose chat lookup
// failed). Used so bosso can surface a clean error on the browser side
// rather than waiting indefinitely for a real exit frame. Synthetic
// exits always carry exit_code=-1 (no real PTY ever ran).
func (c *TerminalStreamClient) sendExited(ctx context.Context, outbound chan<- *pb.TerminalServerMessage, attachID, reason string) {
	c.sendServerMessage(ctx, outbound, &pb.TerminalServerMessage{
		Msg: &pb.TerminalServerMessage_Exited{
			Exited: &pb.TerminalAttachExited{
				AttachId: attachID,
				ExitCode: -1,
				Reason:   reason,
			},
		},
	})
}
