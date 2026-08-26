// Package hostclient provides a shared gRPC client for plugins to call back
// into the bossd host's HostService. This package is extracted from the
// autopilot plugin's host.go to avoid code duplication across plugins.
package hostclient

import (
	"context"
	"fmt"
	"time"

	goplugin "github.com/hashicorp/go-plugin"
	"github.com/recurser/bossalib/config"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"
)

// DefaultRPCTimeout is the default per-call timeout applied to unary host
// service RPCs. It bounds how long a plugin will wait for a hung daemon
// before returning an error.
//
// HostService is entirely unary — the proto declares no streaming RPC — and
// three of those unary RPCs are exempt from this value, in two different
// ways.
//
// The long-poll waits (WaitAgentRun, WaitChatRun) take no timeout at all:
// they block for as long as the run they are waiting on, which can be many
// minutes, so their lifetime is the caller's ctx. StartChatRun takes its own,
// looser ceiling instead (StartChatRunRPCTimeout below), because it spans a
// whole session start rather than a single daemon round-trip. Everything else
// funnels through invokeUnary and takes this value.
//
// A plugin that needs a different budget across its unary calls constructs
// its client with WithTimeout, which replaces this default for that client —
// but only for the calls that consult the default at all, i.e. not for the
// three named above. Note that neither lever ever extends a caller's context:
// both are ceilings, so the shorter of the two always wins.
const DefaultRPCTimeout = 30 * time.Second

// StartChatRunRPCTimeout is the default per-call ceiling for StartChatRun,
// derived from the default session-start readiness deadline. The repair plugin
// overrides this per client with the host's configured readiness deadline.
//
// StartChatRun spans a whole session start on the host side, not a single
// daemon round-trip. Its worst case is the repair path's replace branch: a
// cheap first StartTmuxChat that returns AlreadyExists, the replacement and
// cleanup of the blocking chat, then a full fresh launch — spawn, DB row,
// finalize-hook wiring, the composer-readiness wait
// (config.DefaultSessionStartReadyDeadline, 45s), the submit verifier (+2s) and
// the fresh-provider-session-ID resolve (+2s). The budget is
// readiness + max(readiness, 4s): 90s at the default, and 600s for a host
// configured at 300s. The shared 30s guard cannot cover this path — it would cut
// the call off mid-readiness, and the readiness wait would then run for whatever
// the ceiling had left rather than the deadline it was configured with. What is
// lost there is the CONFIGURED wait, not the diagnostic: session start is the
// multi-attempt path, so waitForReadyMarkerWithAttempts clamps the attempt to
// fit and the failure still arrives as the ready-marker error carrying its pane
// capture. Losing the diagnostic outright to a bare context error is the
// single-attempt established-send path's failure mode, which is handed its
// context unclamped.
//
// The relationship to the readiness deadline is pinned from the other end by
// TestSessionStartReadinessFitsStartChatRunBudget in services/bossd/internal/
// tmux/tmux_budget_test.go, so this default-derived value is not free to move
// downward on its own.
//
// StartChatRun has exactly one caller today: the repair plugin's
// (*repairMonitor).runRepairAttempt (plugins/bossd-plugin-repair/server.go,
// declared at :1523), which issues the RPC at :1631 and, if that returns
// AlreadyExists and the stale chat is reclaimed, again at :1661. Each call
// gets its own ceiling; the plugin wraps neither in an outer deadline.
var StartChatRunRPCTimeout = config.StartChatRunBudgetFor(config.DefaultSessionStartReadyDeadline)

// brokerDialTimeout bounds how long NewEagerClient waits for broker.Dial(1)
// to return before abandoning the wait with an error. The go-plugin broker's
// own connection-info dispatch uses a 5s TTL internally, so anything longer
// than ~5s almost certainly means the host never called AcceptAndServe. 10s
// is conservative enough to absorb slow hosts without stalling a plugin.
const brokerDialTimeout = 10 * time.Second

// ClientOption configures a hostclient Client at construction time.
type ClientOption func(*clientOptions)

type clientOptions struct {
	rpcTimeout          time.Duration
	startChatRunTimeout time.Duration
}

// WithTimeout overrides the default per-call RPC timeout applied to unary
// methods. Callers that need a longer timeout for a specific call can
// construct a dedicated client with WithTimeout, or pass a context with a
// later deadline (which will still be clamped by the default).
//
// It does not reach every RPC. StartChatRun ignores this shared unary value and
// takes StartChatRunRPCTimeout unless WithStartChatRunTimeout provides a
// per-client StartChatRun ceiling; WaitAgentRun and WaitChatRun take no
// client-side timeout at all. Tightening the budget here therefore leaves those
// three unaffected.
func WithTimeout(d time.Duration) ClientOption {
	return func(o *clientOptions) { o.rpcTimeout = d }
}

// WithStartChatRunTimeout overrides the StartChatRun ceiling for this client.
// Non-positive values are treated as unset and fall back to StartChatRunRPCTimeout.
func WithStartChatRunTimeout(d time.Duration) ClientOption {
	return func(o *clientOptions) { o.startChatRunTimeout = d }
}

func resolveOptions(opts []ClientOption) clientOptions {
	o := clientOptions{
		rpcTimeout:          DefaultRPCTimeout,
		startChatRunTimeout: StartChatRunRPCTimeout,
	}
	for _, fn := range opts {
		fn(&o)
	}
	if o.startChatRunTimeout <= 0 {
		o.startChatRunTimeout = StartChatRunRPCTimeout
	}
	return o
}

// Client defines the methods plugins can use to call back into the host service.
// Both DirectClient and EagerClient implement this interface.
type Client interface {
	// Session management
	ListSessions(ctx context.Context) (*bossanovav1.HostServiceListSessionsResponse, error)
	GetReviewComments(ctx context.Context, req *bossanovav1.GetReviewCommentsRequest) (*bossanovav1.GetReviewCommentsResponse, error)
	FireSessionEvent(ctx context.Context, req *bossanovav1.FireSessionEventRequest) (*bossanovav1.FireSessionEventResponse, error)

	// Repair status
	SetRepairStatus(ctx context.Context, req *bossanovav1.SetRepairStatusRequest) (*bossanovav1.SetRepairStatusResponse, error)

	// Agent execution. Forwards to the daemon, which delegates to the loaded
	// AgentRunner plugin (e.g. bossd-plugin-claude). Used by the repair
	// plugin to spawn a one-shot agent run on a session's worktree.
	StartAgentRun(ctx context.Context, req *bossanovav1.StartAgentRunHostRequest) (*bossanovav1.StartAgentRunHostResponse, error)
	WaitAgentRun(ctx context.Context, req *bossanovav1.WaitAgentRunHostRequest) (*bossanovav1.WaitAgentRunHostResponse, error)

	// Tmux-hosted chat runs. Like StartAgentRun / WaitAgentRun but the
	// daemon spawns the agent inside a tmux session and creates an
	// agent_chats row so the run is operator-attachable. Used by the
	// repair plugin (Task 5) so a /boss-repair invocation surfaces in
	// the chat list while it's running.
	StartChatRun(ctx context.Context, req *bossanovav1.StartChatRunHostRequest) (*bossanovav1.StartChatRunHostResponse, error)
	WaitChatRun(ctx context.Context, req *bossanovav1.WaitChatRunHostRequest) (*bossanovav1.WaitChatRunHostResponse, error)
	// ReclaimRepairChat clears a stale tmux-backed Repair chat that blocks a
	// new repair run. The daemon validates ownership before killing tmux.
	ReclaimRepairChat(ctx context.Context, req *bossanovav1.ReclaimRepairChatHostRequest) (*bossanovav1.ReclaimRepairChatHostResponse, error)

	// Repair diagnostics — persists the per-attempt outcome onto the session
	// row so the TUI can surface "failing ⚠ repair: claude not in PATH (3×)"
	// hints. Repair plugin calls this once per attempt in deferred cleanup.
	RecordRepairOutcome(ctx context.Context, req *bossanovav1.RecordRepairOutcomeRequest) (*bossanovav1.RecordRepairOutcomeResponse, error)

	// Run telemetry lets agent-runner plugins persist model/tool/subagent
	// counters against the run ID returned by StartAgentRun or StartChatRun.
	RecordRunTelemetry(ctx context.Context, req *bossanovav1.RecordRunTelemetryRequest) (*bossanovav1.RecordRunTelemetryResponse, error)
}

// DirectClient wraps a gRPC connection to the daemon's HostService,
// providing typed methods for the plugin to call back into the host.
type DirectClient struct {
	conn                *grpc.ClientConn
	defaultRPCTimeout   time.Duration
	startChatRunTimeout time.Duration
}

// NewDirectClient creates a new host service client from a gRPC connection.
// Pass WithTimeout(d) to override the default per-call RPC timeout.
func NewDirectClient(conn *grpc.ClientConn, opts ...ClientOption) *DirectClient {
	o := resolveOptions(opts)
	return &DirectClient{conn: conn, defaultRPCTimeout: o.rpcTimeout, startChatRunTimeout: o.startChatRunTimeout}
}

// EagerClient starts broker.Dial(1) in a background goroutine
// immediately upon construction. GRPCServer runs before the host has called
// AcceptAndServe on the broker, but the background goroutine blocks on the
// broker channel until ConnInfo arrives. The go-plugin broker cleans up
// pending connection info after 5 seconds, so we must start the Dial
// eagerly rather than deferring to the first RPC call.
type EagerClient struct {
	logger              zerolog.Logger
	inner               *DirectClient
	err                 error
	ready               chan struct{}
	defaultRPCTimeout   time.Duration
	startChatRunTimeout time.Duration
}

// NewEagerClient creates a new eager host service client that dials the host
// service in the background via the go-plugin broker using broker ID 1.
// This must be called immediately in GRPCServer to avoid the broker's
// 5-second timeout. Pass WithTimeout(d) to override the default per-call RPC
// timeout.
func NewEagerClient(broker *goplugin.GRPCBroker, logger zerolog.Logger, opts ...ClientOption) *EagerClient {
	return NewEagerClientWithBrokerID(broker, logger, 1, opts...)
}

// NewEagerClientWithBrokerID creates a new eager host service client that
// dials the host service in the background via the go-plugin broker on the
// given brokerID. This must be called immediately in GRPCServer to avoid the
// broker's 5-second timeout. Plugin types that share a daemon with other
// plugins must use a distinct broker ID (see BrokerID* constants in the
// plugin package). Pass WithTimeout(d) to override the default per-call RPC
// timeout.
func NewEagerClientWithBrokerID(broker *goplugin.GRPCBroker, logger zerolog.Logger, brokerID uint32, opts ...ClientOption) *EagerClient {
	return newEagerClientFromDialer(func() (*grpc.ClientConn, error) {
		return broker.Dial(brokerID)
	}, logger, opts...)
}

// newEagerClientFromDialer is the testable core of NewEagerClient: it accepts
// a dialer function rather than a *GRPCBroker so unit tests can inject a
// blocking dialer and verify the bounded-wait behavior without spinning up
// a real plugin subprocess.
func newEagerClientFromDialer(dial func() (*grpc.ClientConn, error), logger zerolog.Logger, opts ...ClientOption) *EagerClient {
	o := resolveOptions(opts)
	c := &EagerClient{
		logger:              logger,
		ready:               make(chan struct{}),
		defaultRPCTimeout:   o.rpcTimeout,
		startChatRunTimeout: o.startChatRunTimeout,
	}
	go func() {
		defer close(c.ready)
		conn, err := dialWithTimeout(dial, brokerDialTimeout)
		if err != nil {
			c.err = fmt.Errorf("dial host service: %w", err)
			c.logger.Error().Err(c.err).Dur("timeout", brokerDialTimeout).Msg("failed to connect to host service via broker")
			return
		}
		c.inner = NewDirectClient(conn, WithTimeout(c.defaultRPCTimeout), WithStartChatRunTimeout(c.startChatRunTimeout))
		c.logger.Info().Msg("connected to host service via broker")
	}()
	return c
}

// dialWithTimeout runs dial in a background goroutine and returns whichever
// comes first: the dial result, or timeout. On timeout the background
// goroutine is left to drain — broker.Dial has no ctx, so there is no way
// to actively cancel it. It will unblock when the broker's internal channel
// closes (plugin shutdown) and its result is discarded via the buffered
// channel, so the leak is transient rather than permanent.
func dialWithTimeout(dial func() (*grpc.ClientConn, error), timeout time.Duration) (*grpc.ClientConn, error) {
	type dialResult struct {
		conn *grpc.ClientConn
		err  error
	}
	resultCh := make(chan dialResult, 1)
	go func() {
		conn, err := dial()
		resultCh <- dialResult{conn: conn, err: err}
	}()
	select {
	case r := <-resultCh:
		return r.conn, r.err
	case <-time.After(timeout):
		return nil, fmt.Errorf("broker dial timed out after %s", timeout)
	}
}

func (c *EagerClient) connect() (*DirectClient, error) {
	<-c.ready
	return c.inner, c.err
}

// --- EagerClient methods (delegate to inner after connect) ---

func (c *EagerClient) ListSessions(ctx context.Context) (*bossanovav1.HostServiceListSessionsResponse, error) {
	client, err := c.connect()
	if err != nil {
		return nil, err
	}
	return client.ListSessions(ctx)
}

func (c *EagerClient) GetReviewComments(ctx context.Context, req *bossanovav1.GetReviewCommentsRequest) (*bossanovav1.GetReviewCommentsResponse, error) {
	client, err := c.connect()
	if err != nil {
		return nil, err
	}
	return client.GetReviewComments(ctx, req)
}

func (c *EagerClient) FireSessionEvent(ctx context.Context, req *bossanovav1.FireSessionEventRequest) (*bossanovav1.FireSessionEventResponse, error) {
	client, err := c.connect()
	if err != nil {
		return nil, err
	}
	return client.FireSessionEvent(ctx, req)
}

func (c *EagerClient) SetRepairStatus(ctx context.Context, req *bossanovav1.SetRepairStatusRequest) (*bossanovav1.SetRepairStatusResponse, error) {
	client, err := c.connect()
	if err != nil {
		return nil, err
	}
	return client.SetRepairStatus(ctx, req)
}

func (c *EagerClient) StartAgentRun(ctx context.Context, req *bossanovav1.StartAgentRunHostRequest) (*bossanovav1.StartAgentRunHostResponse, error) {
	client, err := c.connect()
	if err != nil {
		return nil, err
	}
	return client.StartAgentRun(ctx, req)
}

func (c *EagerClient) WaitAgentRun(ctx context.Context, req *bossanovav1.WaitAgentRunHostRequest) (*bossanovav1.WaitAgentRunHostResponse, error) {
	client, err := c.connect()
	if err != nil {
		return nil, err
	}
	return client.WaitAgentRun(ctx, req)
}

func (c *EagerClient) StartChatRun(ctx context.Context, req *bossanovav1.StartChatRunHostRequest) (*bossanovav1.StartChatRunHostResponse, error) {
	client, err := c.connect()
	if err != nil {
		return nil, err
	}
	return client.StartChatRun(ctx, req)
}

func (c *EagerClient) WaitChatRun(ctx context.Context, req *bossanovav1.WaitChatRunHostRequest) (*bossanovav1.WaitChatRunHostResponse, error) {
	client, err := c.connect()
	if err != nil {
		return nil, err
	}
	return client.WaitChatRun(ctx, req)
}

func (c *EagerClient) ReclaimRepairChat(ctx context.Context, req *bossanovav1.ReclaimRepairChatHostRequest) (*bossanovav1.ReclaimRepairChatHostResponse, error) {
	client, err := c.connect()
	if err != nil {
		return nil, err
	}
	return client.ReclaimRepairChat(ctx, req)
}

func (c *EagerClient) RecordRepairOutcome(ctx context.Context, req *bossanovav1.RecordRepairOutcomeRequest) (*bossanovav1.RecordRepairOutcomeResponse, error) {
	client, err := c.connect()
	if err != nil {
		return nil, err
	}
	return client.RecordRepairOutcome(ctx, req)
}

func (c *EagerClient) RecordRunTelemetry(ctx context.Context, req *bossanovav1.RecordRunTelemetryRequest) (*bossanovav1.RecordRunTelemetryResponse, error) {
	client, err := c.connect()
	if err != nil {
		return nil, err
	}
	return client.RecordRunTelemetry(ctx, req)
}

// --- DirectClient methods (gRPC calls) ---

// invokeUnary applies the client's default RPC timeout and forwards to
// grpc.ClientConn.Invoke. Every unary HostService call funnels through here so
// the timeout is enforced in one place, except for StartChatRun — which calls
// invokeUnaryWithTimeout directly with StartChatRunRPCTimeout, because it spans
// a whole session start rather than a single daemon round-trip — and the
// long-poll waits (WaitAgentRun, WaitChatRun), which call conn.Invoke directly
// with no timeout at all. HostService has no streaming RPC today; one added
// later would bypass both helpers, its lifetime being the caller's context.
func (c *DirectClient) invokeUnary(ctx context.Context, method string, req, resp any) error {
	return c.invokeUnaryWithTimeout(ctx, c.defaultRPCTimeout, method, req, resp)
}

// invokeUnaryWithTimeout is invokeUnary with a caller-supplied ceiling, for the
// one RPC whose budget is a property of the RPC rather than of the client
// (StartChatRun, see StartChatRunRPCTimeout). Separated out so that exemption
// does not have to duplicate the invoke mechanics, and so tests can exercise a
// timeout path without waiting out a production value. Mirrors
// invokePluginUnary / invokePluginUnaryWithTimeout in
// services/bossd/internal/plugin/grpc_plugins.go, which solves the same problem
// in the daemon → plugin direction.
//
// The timeout is applied on top of ctx, so a caller with a shorter deadline
// still wins — context.WithTimeout never extends a parent.
func (c *DirectClient) invokeUnaryWithTimeout(ctx context.Context, timeout time.Duration, method string, req, resp any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return c.conn.Invoke(ctx, method, req, resp)
}

func (c *DirectClient) ListSessions(ctx context.Context) (*bossanovav1.HostServiceListSessionsResponse, error) {
	req := &bossanovav1.HostServiceListSessionsRequest{}
	resp := &bossanovav1.HostServiceListSessionsResponse{}
	if err := c.invokeUnary(ctx, "/bossanova.v1.HostService/ListSessions", req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *DirectClient) GetReviewComments(ctx context.Context, req *bossanovav1.GetReviewCommentsRequest) (*bossanovav1.GetReviewCommentsResponse, error) {
	resp := &bossanovav1.GetReviewCommentsResponse{}
	if err := c.invokeUnary(ctx, "/bossanova.v1.HostService/GetReviewComments", req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *DirectClient) FireSessionEvent(ctx context.Context, req *bossanovav1.FireSessionEventRequest) (*bossanovav1.FireSessionEventResponse, error) {
	resp := &bossanovav1.FireSessionEventResponse{}
	if err := c.invokeUnary(ctx, "/bossanova.v1.HostService/FireSessionEvent", req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *DirectClient) SetRepairStatus(ctx context.Context, req *bossanovav1.SetRepairStatusRequest) (*bossanovav1.SetRepairStatusResponse, error) {
	resp := &bossanovav1.SetRepairStatusResponse{}
	if err := c.invokeUnary(ctx, "/bossanova.v1.HostService/SetRepairStatus", req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *DirectClient) StartAgentRun(ctx context.Context, req *bossanovav1.StartAgentRunHostRequest) (*bossanovav1.StartAgentRunHostResponse, error) {
	resp := &bossanovav1.StartAgentRunHostResponse{}
	if err := c.invokeUnary(ctx, "/bossanova.v1.HostService/StartAgentRun", req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// WaitAgentRun bypasses the default RPC timeout because agent runs can
// legitimately take many minutes (or longer). Lifetime is controlled by the
// caller's ctx — the repair plugin cancels it on shutdown to drain.
func (c *DirectClient) WaitAgentRun(ctx context.Context, req *bossanovav1.WaitAgentRunHostRequest) (*bossanovav1.WaitAgentRunHostResponse, error) {
	resp := &bossanovav1.WaitAgentRunHostResponse{}
	if err := c.conn.Invoke(ctx, "/bossanova.v1.HostService/WaitAgentRun", req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// StartChatRun takes the client's StartChatRun ceiling rather than the shared
// unary default: it spans a whole session start on the host side, so the shared
// hung-daemon guard is too tight for it. See StartChatRunRPCTimeout for the
// default sizing and the guard test that pins it against the readiness deadline.
func (c *DirectClient) StartChatRun(ctx context.Context, req *bossanovav1.StartChatRunHostRequest) (*bossanovav1.StartChatRunHostResponse, error) {
	resp := &bossanovav1.StartChatRunHostResponse{}
	if err := c.invokeUnaryWithTimeout(ctx, c.startChatRunTimeout, "/bossanova.v1.HostService/StartChatRun", req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// WaitChatRun bypasses the default RPC timeout because the daemon-side
// WaitChatRun blocks for up to 30 minutes waiting for the Stop hook.
// Lifetime is controlled by the caller's ctx — the repair plugin cancels
// it on shutdown to drain.
func (c *DirectClient) WaitChatRun(ctx context.Context, req *bossanovav1.WaitChatRunHostRequest) (*bossanovav1.WaitChatRunHostResponse, error) {
	resp := &bossanovav1.WaitChatRunHostResponse{}
	if err := c.conn.Invoke(ctx, "/bossanova.v1.HostService/WaitChatRun", req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *DirectClient) ReclaimRepairChat(ctx context.Context, req *bossanovav1.ReclaimRepairChatHostRequest) (*bossanovav1.ReclaimRepairChatHostResponse, error) {
	resp := &bossanovav1.ReclaimRepairChatHostResponse{}
	if err := c.invokeUnary(ctx, "/bossanova.v1.HostService/ReclaimRepairChat", req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *DirectClient) RecordRepairOutcome(ctx context.Context, req *bossanovav1.RecordRepairOutcomeRequest) (*bossanovav1.RecordRepairOutcomeResponse, error) {
	resp := &bossanovav1.RecordRepairOutcomeResponse{}
	if err := c.invokeUnary(ctx, "/bossanova.v1.HostService/RecordRepairOutcome", req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *DirectClient) RecordRunTelemetry(ctx context.Context, req *bossanovav1.RecordRunTelemetryRequest) (*bossanovav1.RecordRunTelemetryResponse, error) {
	resp := &bossanovav1.RecordRunTelemetryResponse{}
	if err := c.invokeUnary(ctx, "/bossanova.v1.HostService/RecordRunTelemetry", req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// Compile-time interface checks.
var (
	_ Client = (*EagerClient)(nil)
	_ Client = (*DirectClient)(nil)
)
