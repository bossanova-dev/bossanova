// Package upstream — stream.go houses the new reverse-stream client that
// replaces the heartbeat + SyncSessions loops in upstream.go. This file
// owns the outer reconnect loop and the per-connection orchestration
// (snapshot send, event forwarding, command dispatch, token refresh). The
// legacy Manager in upstream.go is preserved until P8 deletion; both can
// compile side-by-side so the switchover in T3.7 is a one-line swap.
package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/safego"
	"github.com/rs/zerolog"
)

// Backoff bounds for the stream outer loop. Start cheap, cap tight so a
// dead orchestrator doesn't leave the daemon silent for a full minute.
const (
	streamInitialBackoff = 1 * time.Second
	streamMaxBackoff     = 30 * time.Second
)

// bidirectionalStream abstracts the subset of *connect.BidiStreamForClient
// the StreamClient actually touches. The real ConnectRPC type already
// provides Send / Receive / CloseRequest, so this interface is satisfied
// without an adapter in production. Tests drop in a lightweight mock.
type bidirectionalStream interface {
	Send(*pb.DaemonEvent) error
	Receive() (*pb.OrchestratorCommand, error)
	CloseRequest() error
}

// streamOpener is the one bit of ConnectRPC glue the outer loop needs. The
// real OrchestratorServiceClient satisfies this via DaemonStream(ctx); the
// indirection keeps the test harness free of a full connect server.
type streamOpener interface {
	DaemonStream(ctx context.Context) bidirectionalStream
}

// connectOpener adapts a real bossanovav1connect.OrchestratorServiceClient
// to the streamOpener interface above. DaemonStream on that client returns
// a *connect.BidiStreamForClient which already implements bidirectionalStream
// structurally — the concrete return type is just wrapped here so the
// interface satisfaction is explicit and compile-checked.
type connectOpener struct {
	client    bossanovav1connect.OrchestratorServiceClient
	authToken string // fallback WorkOS JWT — used only when tokens is nil
	// tokens, when set, is consulted on every DaemonStream() call so that
	// reconnects after a bosso outage use a freshly-refreshed JWT rather
	// than the stale one bossd started with. The periodic in-band
	// refresher only runs while a stream is alive, so without this the
	// daemon tight-loops on "invalid credentials" once the initial JWT
	// expires during any reconnect gap > 5 min.
	tokens TokenProvider

	// sessionToken is the daemon session_token from RegisterDaemon — goes
	// in X-Daemon-Token. Mutable so StreamClient can rotate it after a
	// CodeUnauthenticated failure (e.g. another bossd with the same
	// daemon_id rotated it via UPSERT, or bosso's daemons table was
	// reset).
	tokenMu      sync.Mutex
	sessionToken string
}

// SetSessionToken swaps the daemon session_token used on subsequent
// DaemonStream opens. Called by StreamClient.Run after a successful
// re-register in response to a stale-token auth failure.
func (o *connectOpener) SetSessionToken(tok string) {
	o.tokenMu.Lock()
	o.sessionToken = tok
	o.tokenMu.Unlock()
}

// sessionTokenHolder is the capability StreamClient looks for on its
// opener to rotate the daemon session_token after a re-register. Real
// openers (connectOpener) satisfy this; bare test fakes that don't care
// about re-register simply don't implement it.
type sessionTokenHolder interface {
	SetSessionToken(tok string)
}

// DaemonStream opens a new bidi stream attaching the WorkOS JWT as a
// Bearer token and the daemon session_token as X-Daemon-Token. bosso
// requires both and cross-checks that the JWT's user owns the daemon
// identified by the session token (see services/bosso/internal/server/
// stream.go).
func (o *connectOpener) DaemonStream(ctx context.Context) bidirectionalStream {
	raw := o.client.DaemonStream(ctx)
	jwt := o.authToken
	if o.tokens != nil {
		// Refresh if the cached token is expired or within 60s of expiry
		// — the typical reconnect path. The 60s window matches the
		// refreshThreshold lower bound and gives bosso a comfortable
		// validity window to complete the register handshake.
		if exp := o.tokens.ExpiresAt(); !exp.IsZero() && time.Until(exp) < 60*time.Second {
			if _, err := o.tokens.Refresh(ctx); err != nil {
				// Fall through with whatever Token() holds — may be the
				// stale value. bosso will reject and the caller will log
				// + backoff; logging here too would be duplicative.
				_ = err
			}
		}
		if t := o.tokens.Token(); t != "" {
			jwt = t
		}
	}
	if jwt != "" {
		raw.RequestHeader().Set("Authorization", "Bearer "+jwt)
	}
	o.tokenMu.Lock()
	sessionToken := o.sessionToken
	o.tokenMu.Unlock()
	if sessionToken != "" {
		raw.RequestHeader().Set("X-Daemon-Token", sessionToken)
	}
	return connectBidiAdapter{stream: raw}
}

// connectBidiAdapter bridges connect's BidiStreamForClient (which has all
// three methods already) to the local interface. Kept as a value type so
// nil-check pitfalls are obvious.
type connectBidiAdapter struct {
	stream *connect.BidiStreamForClient[pb.DaemonEvent, pb.OrchestratorCommand]
}

func (a connectBidiAdapter) Send(ev *pb.DaemonEvent) error { return a.stream.Send(ev) }

func (a connectBidiAdapter) Receive() (*pb.OrchestratorCommand, error) {
	return a.stream.Receive()
}

func (a connectBidiAdapter) CloseRequest() error { return a.stream.CloseRequest() }

// TokenProvider hands out WorkOS access tokens and refreshes them on
// demand. Mirrors the Manager's existing keychain-backed path but
// expressed as an interface so the stream client can be tested without
// a real keychain.
type TokenProvider interface {
	// Token returns the currently-cached access token. Empty when no
	// token is available (caller decides whether to proceed).
	Token() string
	// ExpiresAt returns the expiry timestamp for the cached token. Zero
	// value means "unknown" — the refresher should treat that as "do not
	// refresh proactively".
	ExpiresAt() time.Time
	// Refresh obtains a new access token from WorkOS. Implementations
	// must update the cached Token()/ExpiresAt() on success so the next
	// reconnect uses the fresh token.
	Refresh(ctx context.Context) (string, error)
}

// SessionCommandHandler encapsulates the daemon's existing stop/pause/resume
// paths behind a stream-shaped interface. The concrete implementation (wired
// in T3.7) delegates to *session.Lifecycle / *server.Server; the interface
// keeps command_dispatcher.go free of a dependency on the server package,
// avoiding an import cycle with upstream.
type SessionCommandHandler interface {
	Stop(ctx context.Context, sessionID string) (*pb.Session, error)
	Pause(ctx context.Context, sessionID string) (*pb.Session, error)
	Resume(ctx context.Context, sessionID string) (*pb.Session, error)
}

// WebhookDispatcher forwards a webhook payload to whatever in-daemon
// subscriber handles it. Returning (ok, err) keeps the dispatcher
// boilerplate uniform with the Stop/Pause/Resume paths.
type WebhookDispatcher interface {
	Dispatch(ctx context.Context, ev *pb.WebhookEvent) error
}

// TransferHandler encapsulates the daemon's participation in the
// coordinated transfer protocol (decision #14). One daemon can be either
// source or target in a given transfer; the handler figures out its role
// from the session_id and its own state.
//
// Transfer: bosso is initiating. If this daemon owns the session it takes
// the source role (pause, mark transferring_to) and returns (nil, nil) so
// the ACK carries no payload. If it does not own the session yet it takes
// the target role (create with transferring_from, resume) and returns a
// non-nil TransferConfirmed payload.
//
// Confirmed: bosso has seen the target's resume succeed. Source daemons
// emit SessionDelta{DELETED} for the session.
//
// Cancel: bosso is rolling back. Source daemons clear their
// transferring_to marker; target daemons delete any copy they created.
// Idempotent — safe to call when the daemon has no matching state.
//
// The interface keeps command_dispatcher.go free of a dependency on the
// session package (avoiding an import cycle), matching the
// SessionCommandHandler pattern.
type TransferHandler interface {
	Transfer(ctx context.Context, req *pb.TransferSessionCommand) (*pb.TransferConfirmed, error)
	Confirmed(ctx context.Context, req *pb.TransferConfirmed) error
	Cancel(ctx context.Context, req *pb.TransferCancel) error
}

// SessionAttacher kicks off a tmux reader for the given session and
// streams SessionAttachChunk events on the returned channel until the
// session ends or ctx is cancelled. Implementations are responsible for
// closing the channel when the attach ends.
type SessionAttacher interface {
	Attach(ctx context.Context, sessionID, commandID string) (<-chan *pb.SessionAttachChunk, error)
}

// StreamStores bundles the SQLite-backed readers the snapshot builder
// needs. Grouped into one struct so NewStreamClient stays readable when
// the caller site adds another store (repo store, workflow store) — new
// fields land here rather than widening the constructor signature.
type StreamStores struct {
	Sessions SessionSnapshotReader
	Chats    ChatSnapshotReader
	Repos    RepoSnapshotReader
	Statuses StatusSnapshotReader
}

// SessionSnapshotReader returns the slim projection of every active
// session the daemon currently knows about. Built to take the *pb.Session
// directly so the snapshot path never touches models.Session — that
// conversion happens once in the adapter, not per-field here.
type SessionSnapshotReader interface {
	SnapshotSessions(ctx context.Context) ([]*pb.Session, error)
}

// ChatSnapshotReader returns the ClaudeChat metadata projection (no
// transcripts — just the preview). Kept separate from SessionSnapshot so
// the two calls can run in parallel if that ever becomes a hotspot.
type ChatSnapshotReader interface {
	SnapshotChats(ctx context.Context) ([]*pb.ClaudeChatMetadata, error)
}

// RepoSnapshotReader lists the repos the daemon is currently managing.
// Snapshot uses the IDs only; the full Repo proto isn't sent up.
type RepoSnapshotReader interface {
	SnapshotRepoIDs(ctx context.Context) ([]string, error)
}

// StatusSnapshotReader returns the current ChatStatusEntry set from the
// in-memory chat-status tracker.
type StatusSnapshotReader interface {
	SnapshotStatuses(ctx context.Context) ([]*pb.ChatStatusEntry, error)
}

// StreamEvent is the union of session/chat/status events the daemon
// publishes internally for the reverse stream. It intentionally does not
// reuse the plugin-facing EventNotification (which has a disjoint oneof
// set) — keeping stream events in-package avoids enlarging the plugin
// proto for a purely internal pipeline.
type StreamEvent struct {
	// Exactly one of the following is non-nil.
	Session *SessionEvent
	Chat    *ChatEvent
	Status  *StatusEvent
}

// SessionEvent describes a session lifecycle change. Kind mirrors the
// SessionDelta proto one-to-one; Session is omitted on delete.
type SessionEvent struct {
	Kind    pb.SessionDelta_Kind
	Session *pb.Session
}

// ChatEvent describes a chat lifecycle change.
type ChatEvent struct {
	Kind pb.ChatDelta_Kind
	Chat *pb.ClaudeChatMetadata
}

// StatusEvent is a raw chat-status heartbeat, pre-coalescing. The
// coalescer (T3.4) dedupes bursts before they reach the wire.
type StatusEvent struct {
	Status *pb.ChatStatusDelta
}

// EventSource is the subscribe-side of the daemon's internal stream
// event bus. Implementations close the returned channel when ctx is
// cancelled or the source shuts down. A concrete eventbus adapter lands
// in T3.7 when the publishers (lifecycle, chat store, display computer)
// get wired to it.
type EventSource interface {
	Subscribe(ctx context.Context) <-chan StreamEvent
}

// Metrics is an optional sink for stream-level counters. The production
// wiring lands in a later task; callers that don't care can pass nil.
type Metrics interface {
	IncReconnect()
	IncStreamError(err error)
}

// noopMetrics is the default when the caller passes nil. Having a real
// zero-value receiver means every metric call site can skip nil checks.
type noopMetrics struct{}

func (noopMetrics) IncReconnect()          {}
func (noopMetrics) IncStreamError(_ error) {}

// StreamClient runs the bossd side of the reverse-stream protocol. It
// opens a long-lived DaemonStream, sends an initial DaemonSnapshot, then
// forwards session/chat/status deltas plus token refreshes until the
// stream terminates. The outer Run loop reconnects with exponential
// backoff on all errors other than context cancellation.
// ReRegisterFunc obtains a fresh daemon session_token by calling
// RegisterDaemon again. Invoked by the Run loop after bosso returns
// CodeUnauthenticated on DaemonStream — the common cause is that another
// bossd with the same daemon_id re-registered and rotated the token, or
// that bosso's daemons row for us was cleared. Callers wrap
// upstream.Register with the daemon's identity + current JWT.
type ReRegisterFunc func(ctx context.Context) (sessionToken string, err error)

type StreamClient struct {
	opener           streamOpener
	stores           StreamStores
	events           EventSource
	tokenProvider    TokenProvider
	commandHandler   SessionCommandHandler
	transferHandler  TransferHandler
	webhooks         WebhookDispatcher
	attacher         SessionAttacher
	reRegister       ReRegisterFunc
	daemonID         string
	hostname         string
	logger           zerolog.Logger
	metrics          Metrics
	clock            Clock
	coalesceWindow   time.Duration
	refreshInterval  time.Duration
	refreshThreshold time.Duration

	// connected flips to true once Send(snapshot) succeeds on a stream.
	// Reset across reconnects so the backoff resets only on a fresh
	// successful handshake, not on dial-level flakes.
	mu        sync.Mutex
	connected bool
}

// StreamClientConfig bundles the constructor inputs. Pointer fields are
// optional; leaving them nil picks a sensible default.
type StreamClientConfig struct {
	// Opener is the live ConnectRPC client. If nil, the client will be
	// built from Client + AuthToken at construction time.
	Opener       streamOpener
	Client       bossanovav1connect.OrchestratorServiceClient
	AuthToken    string // WorkOS JWT for Authorization header
	SessionToken string // daemon session_token from RegisterDaemon for X-Daemon-Token header

	// Identity.
	DaemonID string
	Hostname string

	// Data sources.
	Stores        StreamStores
	Events        EventSource
	TokenProvider TokenProvider

	// Command-side collaborators.
	CommandHandler SessionCommandHandler
	// TransferHandler is optional. When nil, the dispatcher ACKs
	// TransferConfirmed and TransferCancel as idempotent no-ops and fails
	// the initial Transfer command with "not yet implemented" — preserving
	// the T3.6 behaviour. Real daemons wire a concrete implementation in
	// the task that lands the source/target session-lifecycle work.
	TransferHandler TransferHandler
	Webhooks        WebhookDispatcher
	Attacher        SessionAttacher

	// ReRegister, when set, is called by the Run loop after a stream
	// attempt fails with CodeUnauthenticated. On success the returned
	// session_token replaces the one in the opener so the next reconnect
	// authenticates cleanly. Nil is safe — callers who don't wire it
	// keep the previous tight-loop behaviour.
	ReRegister ReRegisterFunc

	// Observability / testing knobs.
	Logger  zerolog.Logger
	Metrics Metrics
	Clock   Clock // nil → realClock
	// CoalesceWindow is the ChatStatus flush interval (T3.4). Zero picks
	// the default 100ms.
	CoalesceWindow time.Duration
	// RefreshInterval is how often the token refresher wakes. Zero picks
	// 60s (decision #2).
	RefreshInterval time.Duration
	// RefreshThreshold is how much headroom we keep before expiry before
	// forcing a refresh. Zero picks 10 minutes (decision #2).
	RefreshThreshold time.Duration
}

// NewStreamClient assembles the client from the given config. Pointer
// fields in StreamClientConfig are optional — defaults are filled in
// here so the caller site stays compact.
func NewStreamClient(cfg StreamClientConfig) *StreamClient {
	opener := cfg.Opener
	if opener == nil && cfg.Client != nil {
		opener = &connectOpener{
			client:       cfg.Client,
			authToken:    cfg.AuthToken,
			sessionToken: cfg.SessionToken,
			tokens:       cfg.TokenProvider,
		}
	}
	metrics := cfg.Metrics
	if metrics == nil {
		metrics = noopMetrics{}
	}
	clock := cfg.Clock
	if clock == nil {
		clock = realClock{}
	}
	window := cfg.CoalesceWindow
	if window == 0 {
		window = 100 * time.Millisecond
	}
	refreshInterval := cfg.RefreshInterval
	if refreshInterval == 0 {
		refreshInterval = 60 * time.Second
	}
	refreshThreshold := cfg.RefreshThreshold
	if refreshThreshold == 0 {
		refreshThreshold = 10 * time.Minute
	}
	return &StreamClient{
		opener:           opener,
		stores:           cfg.Stores,
		events:           cfg.Events,
		tokenProvider:    cfg.TokenProvider,
		commandHandler:   cfg.CommandHandler,
		transferHandler:  cfg.TransferHandler,
		webhooks:         cfg.Webhooks,
		attacher:         cfg.Attacher,
		reRegister:       cfg.ReRegister,
		daemonID:         cfg.DaemonID,
		hostname:         cfg.Hostname,
		logger:           cfg.Logger.With().Str("component", "stream-client").Logger(),
		metrics:          metrics,
		clock:            clock,
		coalesceWindow:   window,
		refreshInterval:  refreshInterval,
		refreshThreshold: refreshThreshold,
	}
}

// Run is the reconnect outer loop. It returns only when ctx is cancelled.
// On every stream-close error the loop backs off (1s → 30s cap) before
// retrying. Successful stream completion (Send of snapshot + at least
// one round-trip) resets the backoff to 1s so a one-off flake doesn't
// escalate.
func (c *StreamClient) Run(ctx context.Context) {
	backoff := streamInitialBackoff
	for {
		if ctx.Err() != nil {
			return
		}

		c.markDisconnected()
		err := c.openStream(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err == nil:
			// Stream closed cleanly (server shutdown etc). Reset backoff
			// and try again immediately — this is usually a recycle, not
			// a sustained outage.
			backoff = streamInitialBackoff
		default:
			c.metrics.IncStreamError(err)
			// CodeUnauthenticated from the stream means bosso rejected
			// our credentials. The JWT is checked on open too, but the
			// typical cause is a stale session_token (another bossd
			// with the same daemon_id rotated it via UPSERT, or bosso's
			// daemons row for us was cleared). Without self-healing the
			// outer loop tight-loops forever presenting the same bad
			// token. We can't gate this on wasConnected() because
			// Send(snapshot) succeeds locally before the server's
			// header-only error arrives on Receive — by then
			// markConnected has already fired. Call ReRegister on any
			// Unauthenticated; if the JWT is also bad it'll fail and we
			// fall through to regular backoff.
			if connect.CodeOf(err) == connect.CodeUnauthenticated {
				if c.tryReRegister(ctx) {
					// Fresh token in hand — retry promptly.
					backoff = streamInitialBackoff
				}
			}
			c.logger.Warn().Err(err).Dur("backoff", backoff).Msg("stream closed, reconnecting")
			// Reset backoff to 1s if we had a successful handshake this
			// attempt — a handshake success means the connection worked
			// at least once, so we should retry promptly.
			if c.wasConnected() {
				backoff = streamInitialBackoff
			}
		}

		if ctx.Err() != nil {
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-c.clock.After(backoff):
		}

		// Exponential ramp, capped at streamMaxBackoff. Only grow when
		// the last attempt was a dead-on-arrival failure (no handshake).
		if !c.wasConnected() {
			backoff *= 2
			if backoff > streamMaxBackoff {
				backoff = streamMaxBackoff
			}
			c.metrics.IncReconnect()
		}
	}
}

// openStream runs a single stream attempt end-to-end: build snapshot,
// open the bidi stream, send snapshot, fan out forwarders, block on
// Receive() for commands. Returns when the stream dies for any reason.
func (c *StreamClient) openStream(ctx context.Context) error {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream := c.opener.DaemonStream(streamCtx)

	// 1. Build + send the snapshot. Any error here is fatal for this
	//    attempt — bosso rejects streams whose first event isn't a
	//    snapshot, so there's no point forwarding deltas afterwards.
	snap, err := c.buildSnapshot(streamCtx)
	if err != nil {
		return fmt.Errorf("build snapshot: %w", err)
	}
	if err := stream.Send(&pb.DaemonEvent{
		Event: &pb.DaemonEvent_Snapshot{Snapshot: snap},
	}); err != nil {
		return fmt.Errorf("send snapshot: %w", err)
	}

	// Mark connected only after a successful handshake. The outer loop
	// uses this to decide whether to reset backoff on the next error.
	c.markConnected()

	// 2. Spin up the outbound writer. A single goroutine owns the
	//    stream.Send side so snapshots/deltas/results don't race one
	//    another inside ConnectRPC's framer.
	outbound := make(chan *pb.DaemonEvent, 64)
	writerDone := safego.Go(c.logger, func() {
		c.runWriter(streamCtx, stream, outbound)
	})

	// 3. Delta forwarder.
	forwarderDone := safego.Go(c.logger, func() {
		c.subscribeDeltas(streamCtx, outbound)
	})

	// 4. Token refresher. Drives outbound on refresh; returning an error
	//    here closes the stream so the outer loop reconnects with a
	//    fresh token — matches decision #2 ("refresh failure → close
	//    stream → reconnect").
	refreshErrCh := make(chan error, 1)
	refresherDone := safego.Go(c.logger, func() {
		if err := c.runTokenRefresher(streamCtx, outbound); err != nil {
			refreshErrCh <- err
		}
		close(refreshErrCh)
	})

	// 5. Command reader — runs on this goroutine. Receive is blocking
	//    and must be owned by exactly one caller per connect semantics.
	readErr := c.runCommandReader(streamCtx, stream, outbound)

	// Tear down in the reverse order we started. Close the outbound
	// channel so the writer exits, then wait for all children so a
	// subsequent reconnect attempt sees a clean slate.
	cancel()

	// Drain the refresh error channel so its error takes precedence
	// over the generic EOF from Receive when a refresh forced the
	// close. Matches decision #2's "close stream so outer loop
	// reconnects" semantics.
	var refreshErr error
	select {
	case refreshErr = <-refreshErrCh:
	default:
	}

	close(outbound)
	<-writerDone
	<-forwarderDone
	<-refresherDone

	if refreshErr != nil {
		return refreshErr
	}
	return readErr
}

// runWriter is the single-writer goroutine that drains the outbound
// channel onto the stream. Exits when outbound is closed or Send fails
// (the failure propagates up via the command reader's next Receive).
func (c *StreamClient) runWriter(ctx context.Context, stream bidirectionalStream, outbound <-chan *pb.DaemonEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-outbound:
			if !ok {
				return
			}
			if err := stream.Send(ev); err != nil {
				c.logger.Debug().Err(err).Msg("stream send failed, writer exiting")
				return
			}
		}
	}
}

// runCommandReader blocks on stream.Receive and dispatches each inbound
// command. Returns when Receive returns an error (EOF, reset, ctx) so
// the outer loop can decide whether to reconnect.
func (c *StreamClient) runCommandReader(ctx context.Context, stream bidirectionalStream, outbound chan<- *pb.DaemonEvent) error {
	for {
		cmd, err := stream.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("receive: %w", err)
		}
		c.handleCommand(ctx, cmd, outbound)
	}
}

// handleCommand dispatches a single inbound command. Kept as a separate
// method (rather than inlined into runCommandReader) so command_dispatcher.go
// can hang the per-oneof logic off it without widening the reader.
func (c *StreamClient) handleCommand(ctx context.Context, cmd *pb.OrchestratorCommand, outbound chan<- *pb.DaemonEvent) {
	result := c.dispatchCommand(ctx, cmd, outbound)
	if result == nil {
		// Attach + unknown commands return nil because they either stream
		// chunks asynchronously (attach) or emit nothing (unknown).
		return
	}
	select {
	case outbound <- result:
	case <-ctx.Done():
	}
}

// tryReRegister invokes the configured ReRegister callback (if any) to
// obtain a fresh daemon session_token and, on success, rotates the
// opener's token so the next DaemonStream open authenticates with it.
// Returns true iff the opener was updated. Safe to call when
// ReRegister is nil (returns false without work) or when the opener
// does not implement sessionTokenHolder (logs + returns false).
func (c *StreamClient) tryReRegister(ctx context.Context) bool {
	if c.reRegister == nil {
		return false
	}
	holder, ok := c.opener.(sessionTokenHolder)
	if !ok {
		c.logger.Warn().Msg("stream: opener does not support session token rotation; skipping re-register")
		return false
	}
	tok, err := c.reRegister(ctx)
	if err != nil {
		c.logger.Warn().Err(err).Msg("stream: re-register failed after auth rejection")
		return false
	}
	if tok == "" {
		c.logger.Warn().Msg("stream: re-register returned empty session token; skipping rotation")
		return false
	}
	holder.SetSessionToken(tok)
	c.logger.Info().Msg("stream: rotated session_token after auth rejection")
	return true
}

// markConnected / markDisconnected / wasConnected are the tiny state
// machine that tells Run whether the last attempt reached the "snapshot
// accepted" point. Backoff resets only when this is true.
func (c *StreamClient) markConnected() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = true
}

func (c *StreamClient) markDisconnected() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = false
}

func (c *StreamClient) wasConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}
