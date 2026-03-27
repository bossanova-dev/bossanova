// Package hostclient provides a shared gRPC client for plugins to call back
// into the bossd host's HostService. This package is extracted from the
// autopilot plugin's host.go to avoid code duplication across plugins.
package hostclient

import (
	"context"
	"fmt"
	"io"

	goplugin "github.com/hashicorp/go-plugin"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"
)

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
	conn *grpc.ClientConn
}

// NewDirectClient creates a new host service client from a gRPC connection.
func NewDirectClient(conn *grpc.ClientConn) *DirectClient {
	return &DirectClient{conn: conn}
}

// EagerClient starts broker.Dial(1) in a background goroutine
// immediately upon construction. GRPCServer runs before the host has called
// AcceptAndServe on the broker, but the background goroutine blocks on the
// broker channel until ConnInfo arrives. The go-plugin broker cleans up
// pending connection info after 5 seconds, so we must start the Dial
// eagerly rather than deferring to the first RPC call.
type EagerClient struct {
	logger zerolog.Logger
	inner  *DirectClient
	err    error
	ready  chan struct{}
}

// NewEagerClient creates a new eager host service client that dials the host
// service in the background via the go-plugin broker. This must be called
// immediately in GRPCServer to avoid the broker's 5-second timeout.
func NewEagerClient(broker *goplugin.GRPCBroker, logger zerolog.Logger) *EagerClient {
	c := &EagerClient{
		logger: logger,
		ready:  make(chan struct{}),
	}
	go func() {
		defer close(c.ready)
		conn, err := broker.Dial(1)
		if err != nil {
			c.err = fmt.Errorf("dial host service: %w", err)
			c.logger.Error().Err(c.err).Msg("failed to connect to host service via broker")
			return
		}
		c.inner = NewDirectClient(conn)
		c.logger.Info().Msg("connected to host service via broker")
	}()
	return c
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

func (c *DirectClient) CreateWorkflow(ctx context.Context, req *bossanovav1.CreateWorkflowRequest) (*bossanovav1.CreateWorkflowResponse, error) {
	resp := &bossanovav1.CreateWorkflowResponse{}
	err := c.conn.Invoke(ctx, "/bossanova.v1.HostService/CreateWorkflow", req, resp)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *DirectClient) UpdateWorkflow(ctx context.Context, req *bossanovav1.UpdateWorkflowRequest) (*bossanovav1.UpdateWorkflowResponse, error) {
	resp := &bossanovav1.UpdateWorkflowResponse{}
	err := c.conn.Invoke(ctx, "/bossanova.v1.HostService/UpdateWorkflow", req, resp)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *DirectClient) GetWorkflow(ctx context.Context, id string) (*bossanovav1.GetWorkflowResponse, error) {
	req := &bossanovav1.GetWorkflowRequest{Id: id}
	resp := &bossanovav1.GetWorkflowResponse{}
	err := c.conn.Invoke(ctx, "/bossanova.v1.HostService/GetWorkflow", req, resp)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *DirectClient) CreateAttempt(ctx context.Context, req *bossanovav1.CreateAttemptRequest) (*bossanovav1.CreateAttemptResponse, error) {
	resp := &bossanovav1.CreateAttemptResponse{}
	err := c.conn.Invoke(ctx, "/bossanova.v1.HostService/CreateAttempt", req, resp)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *DirectClient) GetAttemptStatus(ctx context.Context, attemptID string) (*bossanovav1.GetAttemptStatusResponse, error) {
	req := &bossanovav1.GetAttemptStatusRequest{AttemptId: attemptID}
	resp := &bossanovav1.GetAttemptStatusResponse{}
	err := c.conn.Invoke(ctx, "/bossanova.v1.HostService/GetAttemptStatus", req, resp)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// StreamAttemptOutput opens a server-streaming RPC to receive output lines
// from a running Claude attempt.
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
	err := c.conn.Invoke(ctx, "/bossanova.v1.HostService/ListWorkflows", req, resp)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *DirectClient) ListSessions(ctx context.Context) (*bossanovav1.HostServiceListSessionsResponse, error) {
	req := &bossanovav1.HostServiceListSessionsRequest{}
	resp := &bossanovav1.HostServiceListSessionsResponse{}
	err := c.conn.Invoke(ctx, "/bossanova.v1.HostService/ListSessions", req, resp)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *DirectClient) GetReviewComments(ctx context.Context, req *bossanovav1.GetReviewCommentsRequest) (*bossanovav1.GetReviewCommentsResponse, error) {
	resp := &bossanovav1.GetReviewCommentsResponse{}
	err := c.conn.Invoke(ctx, "/bossanova.v1.HostService/GetReviewComments", req, resp)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *DirectClient) FireSessionEvent(ctx context.Context, req *bossanovav1.FireSessionEventRequest) (*bossanovav1.FireSessionEventResponse, error) {
	resp := &bossanovav1.FireSessionEventResponse{}
	err := c.conn.Invoke(ctx, "/bossanova.v1.HostService/FireSessionEvent", req, resp)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *DirectClient) SetRepairStatus(ctx context.Context, req *bossanovav1.SetRepairStatusRequest) (*bossanovav1.SetRepairStatusResponse, error) {
	resp := &bossanovav1.SetRepairStatusResponse{}
	err := c.conn.Invoke(ctx, "/bossanova.v1.HostService/SetRepairStatus", req, resp)
	if err != nil {
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
