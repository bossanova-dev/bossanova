// Package hostclient provides a shared gRPC client for plugins to call back
// into the bossd host's HostService. This package is extracted from the
// autopilot plugin's host.go to avoid code duplication across plugins.
package hostclient

import (
	"context"
	"fmt"
	"io"
	"time"

	goplugin "github.com/hashicorp/go-plugin"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"
)

// DefaultRPCTimeout is the default per-call timeout applied to unary host
// service RPCs. It bounds how long a plugin will wait for a hung daemon
// before returning an error. Streaming RPCs (StreamAttemptOutput) are
// exempt — their lifetime is controlled by the caller's context.
const DefaultRPCTimeout = 30 * time.Second

// brokerDialTimeout bounds how long NewEagerClient waits for broker.Dial(1)
// to return before abandoning the wait with an error. The go-plugin broker's
// own connection-info dispatch uses a 5s TTL internally, so anything longer
// than ~5s almost certainly means the host never called AcceptAndServe. 10s
// is conservative enough to absorb slow hosts without stalling a plugin.
const brokerDialTimeout = 10 * time.Second

// ClientOption configures a hostclient Client at construction time.
type ClientOption func(*clientOptions)

type clientOptions struct {
	rpcTimeout time.Duration
}

// WithTimeout overrides the default per-call RPC timeout applied to unary
// methods. Callers that need a longer timeout for a specific call can
// construct a dedicated client with WithTimeout, or pass a context with a
// later deadline (which will still be clamped by the default).
func WithTimeout(d time.Duration) ClientOption {
	return func(o *clientOptions) { o.rpcTimeout = d }
}

func resolveOptions(opts []ClientOption) clientOptions {
	o := clientOptions{rpcTimeout: DefaultRPCTimeout}
	for _, fn := range opts {
		fn(&o)
	}
	return o
}

// Client defines the methods plugins can use to call back into the host service.
// Both DirectClient and EagerClient implement this interface.
type Client interface {
	// Workflow management
	CreateWorkflow(ctx context.Context, req *bossanovav1.CreateWorkflowRequest) (*bossanovav1.CreateWorkflowResponse, error)
	UpdateWorkflow(ctx context.Context, req *bossanovav1.UpdateWorkflowRequest) (*bossanovav1.UpdateWorkflowResponse, error)
	GetWorkflow(ctx context.Context, id string) (*bossanovav1.GetWorkflowResponse, error)

	// Attempt management
	CreateAttempt(ctx context.Context, req *bossanovav1.CreateAttemptRequest) (*bossanovav1.CreateAttemptResponse, error)
	GetAttemptStatus(ctx context.Context, attemptID string) (*bossanovav1.GetAttemptStatusResponse, error)
	StreamAttemptOutput(ctx context.Context, attemptID string) (AttemptOutputStream, error)

	// Workflow listing
	ListWorkflows(ctx context.Context, statusFilter string) (*bossanovav1.ListWorkflowsResponse, error)

	// Session management (new in auto-repair feature)
	ListSessions(ctx context.Context) (*bossanovav1.HostServiceListSessionsResponse, error)
	GetReviewComments(ctx context.Context, req *bossanovav1.GetReviewCommentsRequest) (*bossanovav1.GetReviewCommentsResponse, error)
	FireSessionEvent(ctx context.Context, req *bossanovav1.FireSessionEventRequest) (*bossanovav1.FireSessionEventResponse, error)

	// Repair status
	SetRepairStatus(ctx context.Context, req *bossanovav1.SetRepairStatusRequest) (*bossanovav1.SetRepairStatusResponse, error)
}

// AttemptOutputStream reads streamed output lines from a Claude attempt.
type AttemptOutputStream interface {
	Recv() (string, error)
}

// DirectClient wraps a gRPC connection to the daemon's HostService,
// providing typed methods for the plugin to call back into the host.
type DirectClient struct {
	conn              *grpc.ClientConn
	defaultRPCTimeout time.Duration
}

// NewDirectClient creates a new host service client from a gRPC connection.
// Pass WithTimeout(d) to override the default per-call RPC timeout.
func NewDirectClient(conn *grpc.ClientConn, opts ...ClientOption) *DirectClient {
	o := resolveOptions(opts)
	return &DirectClient{conn: conn, defaultRPCTimeout: o.rpcTimeout}
}

// EagerClient starts broker.Dial(1) in a background goroutine
// immediately upon construction. GRPCServer runs before the host has called
// AcceptAndServe on the broker, but the background goroutine blocks on the
// broker channel until ConnInfo arrives. The go-plugin broker cleans up
// pending connection info after 5 seconds, so we must start the Dial
// eagerly rather than deferring to the first RPC call.
type EagerClient struct {
	logger            zerolog.Logger
	inner             *DirectClient
	err               error
	ready             chan struct{}
	defaultRPCTimeout time.Duration
}

// NewEagerClient creates a new eager host service client that dials the host
// service in the background via the go-plugin broker. This must be called
// immediately in GRPCServer to avoid the broker's 5-second timeout.
// Pass WithTimeout(d) to override the default per-call RPC timeout.
func NewEagerClient(broker *goplugin.GRPCBroker, logger zerolog.Logger, opts ...ClientOption) *EagerClient {
	return newEagerClientFromDialer(func() (*grpc.ClientConn, error) {
		return broker.Dial(1)
	}, logger, opts...)
}

// newEagerClientFromDialer is the testable core of NewEagerClient: it accepts
// a dialer function rather than a *GRPCBroker so unit tests can inject a
// blocking dialer and verify the bounded-wait behavior without spinning up
// a real plugin subprocess.
func newEagerClientFromDialer(dial func() (*grpc.ClientConn, error), logger zerolog.Logger, opts ...ClientOption) *EagerClient {
	o := resolveOptions(opts)
	c := &EagerClient{
		logger:            logger,
		ready:             make(chan struct{}),
		defaultRPCTimeout: o.rpcTimeout,
	}
	go func() {
		defer close(c.ready)
		conn, err := dialWithTimeout(dial, brokerDialTimeout)
		if err != nil {
			c.err = fmt.Errorf("dial host service: %w", err)
			c.logger.Error().Err(c.err).Dur("timeout", brokerDialTimeout).Msg("failed to connect to host service via broker")
			return
		}
		c.inner = NewDirectClient(conn, WithTimeout(c.defaultRPCTimeout))
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

func (c *EagerClient) CreateWorkflow(ctx context.Context, req *bossanovav1.CreateWorkflowRequest) (*bossanovav1.CreateWorkflowResponse, error) {
	client, err := c.connect()
	if err != nil {
		return nil, err
	}
	return client.CreateWorkflow(ctx, req)
}

func (c *EagerClient) UpdateWorkflow(ctx context.Context, req *bossanovav1.UpdateWorkflowRequest) (*bossanovav1.UpdateWorkflowResponse, error) {
	client, err := c.connect()
	if err != nil {
		return nil, err
	}
	return client.UpdateWorkflow(ctx, req)
}

func (c *EagerClient) GetWorkflow(ctx context.Context, id string) (*bossanovav1.GetWorkflowResponse, error) {
	client, err := c.connect()
	if err != nil {
		return nil, err
	}
	return client.GetWorkflow(ctx, id)
}

func (c *EagerClient) CreateAttempt(ctx context.Context, req *bossanovav1.CreateAttemptRequest) (*bossanovav1.CreateAttemptResponse, error) {
	client, err := c.connect()
	if err != nil {
		return nil, err
	}
	return client.CreateAttempt(ctx, req)
}

func (c *EagerClient) GetAttemptStatus(ctx context.Context, attemptID string) (*bossanovav1.GetAttemptStatusResponse, error) {
	client, err := c.connect()
	if err != nil {
		return nil, err
	}
	return client.GetAttemptStatus(ctx, attemptID)
}

func (c *EagerClient) StreamAttemptOutput(ctx context.Context, attemptID string) (AttemptOutputStream, error) {
	client, err := c.connect()
	if err != nil {
		return nil, err
	}
	return client.StreamAttemptOutput(ctx, attemptID)
}

func (c *EagerClient) ListWorkflows(ctx context.Context, statusFilter string) (*bossanovav1.ListWorkflowsResponse, error) {
	client, err := c.connect()
	if err != nil {
		return nil, err
	}
	return client.ListWorkflows(ctx, statusFilter)
}

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

// --- DirectClient methods (gRPC calls) ---

// invokeUnary applies the client's default RPC timeout and forwards to
// grpc.ClientConn.Invoke. All unary HostService calls funnel through here so
// the timeout is enforced in one place. Streaming RPCs do not use this helper
// because their lifetime is controlled by the caller's context.
func (c *DirectClient) invokeUnary(ctx context.Context, method string, req, resp any) error {
	ctx, cancel := context.WithTimeout(ctx, c.defaultRPCTimeout)
	defer cancel()
	return c.conn.Invoke(ctx, method, req, resp)
}

func (c *DirectClient) CreateWorkflow(ctx context.Context, req *bossanovav1.CreateWorkflowRequest) (*bossanovav1.CreateWorkflowResponse, error) {
	resp := &bossanovav1.CreateWorkflowResponse{}
	if err := c.invokeUnary(ctx, "/bossanova.v1.HostService/CreateWorkflow", req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *DirectClient) UpdateWorkflow(ctx context.Context, req *bossanovav1.UpdateWorkflowRequest) (*bossanovav1.UpdateWorkflowResponse, error) {
	resp := &bossanovav1.UpdateWorkflowResponse{}
	if err := c.invokeUnary(ctx, "/bossanova.v1.HostService/UpdateWorkflow", req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *DirectClient) GetWorkflow(ctx context.Context, id string) (*bossanovav1.GetWorkflowResponse, error) {
	req := &bossanovav1.GetWorkflowRequest{Id: id}
	resp := &bossanovav1.GetWorkflowResponse{}
	if err := c.invokeUnary(ctx, "/bossanova.v1.HostService/GetWorkflow", req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *DirectClient) CreateAttempt(ctx context.Context, req *bossanovav1.CreateAttemptRequest) (*bossanovav1.CreateAttemptResponse, error) {
	resp := &bossanovav1.CreateAttemptResponse{}
	if err := c.invokeUnary(ctx, "/bossanova.v1.HostService/CreateAttempt", req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *DirectClient) GetAttemptStatus(ctx context.Context, attemptID string) (*bossanovav1.GetAttemptStatusResponse, error) {
	req := &bossanovav1.GetAttemptStatusRequest{AttemptId: attemptID}
	resp := &bossanovav1.GetAttemptStatusResponse{}
	if err := c.invokeUnary(ctx, "/bossanova.v1.HostService/GetAttemptStatus", req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// StreamAttemptOutput opens a server-streaming RPC to receive output lines
// from a running Claude attempt. The stream is intentionally exempt from
// DefaultRPCTimeout because an attempt can run for many minutes — cancel
// via the caller-supplied ctx instead.
func (c *DirectClient) StreamAttemptOutput(ctx context.Context, attemptID string) (AttemptOutputStream, error) {
	req := &bossanovav1.StreamAttemptOutputRequest{AttemptId: attemptID}
	streamDesc := &grpc.StreamDesc{
		StreamName:    "StreamAttemptOutput",
		ServerStreams: true,
	}
	stream, err := c.conn.NewStream(ctx, streamDesc, "/bossanova.v1.HostService/StreamAttemptOutput")
	if err != nil {
		return nil, fmt.Errorf("open stream: %w", err)
	}
	if err := stream.SendMsg(req); err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	if err := stream.CloseSend(); err != nil {
		return nil, fmt.Errorf("close send: %w", err)
	}
	return &attemptOutputStream{stream: stream}, nil
}

func (c *DirectClient) ListWorkflows(ctx context.Context, statusFilter string) (*bossanovav1.ListWorkflowsResponse, error) {
	req := &bossanovav1.ListWorkflowsRequest{StatusFilter: statusFilter}
	resp := &bossanovav1.ListWorkflowsResponse{}
	if err := c.invokeUnary(ctx, "/bossanova.v1.HostService/ListWorkflows", req, resp); err != nil {
		return nil, err
	}
	return resp, nil
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

// attemptOutputStream wraps a gRPC ClientStream to implement AttemptOutputStream.
type attemptOutputStream struct {
	stream grpc.ClientStream
}

func (s *attemptOutputStream) Recv() (string, error) {
	resp := &bossanovav1.StreamAttemptOutputResponse{}
	if err := s.stream.RecvMsg(resp); err != nil {
		if err == io.EOF {
			return "", io.EOF
		}
		return "", err
	}
	return resp.GetLine(), nil
}

// Compile-time interface checks.
var (
	_ Client              = (*EagerClient)(nil)
	_ Client              = (*DirectClient)(nil)
	_ AttemptOutputStream = (*attemptOutputStream)(nil)
)
