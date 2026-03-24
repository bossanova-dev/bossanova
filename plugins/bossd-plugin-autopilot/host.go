package main

import (
	"context"
	"fmt"
	"io"

	goplugin "github.com/hashicorp/go-plugin"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"
)

// hostClient defines the methods the orchestrator uses from the host service.
// Both hostServiceClient and eagerHostServiceClient implement this interface.
type hostClient interface {
	CreateWorkflow(ctx context.Context, req *bossanovav1.CreateWorkflowRequest) (*bossanovav1.CreateWorkflowResponse, error)
	UpdateWorkflow(ctx context.Context, req *bossanovav1.UpdateWorkflowRequest) (*bossanovav1.UpdateWorkflowResponse, error)
	GetWorkflow(ctx context.Context, id string) (*bossanovav1.GetWorkflowResponse, error)
	CreateAttempt(ctx context.Context, req *bossanovav1.CreateAttemptRequest) (*bossanovav1.CreateAttemptResponse, error)
	GetAttemptStatus(ctx context.Context, attemptID string) (*bossanovav1.GetAttemptStatusResponse, error)
	StreamAttemptOutput(ctx context.Context, attemptID string) (AttemptOutputStream, error)
}

// AttemptOutputStream reads streamed output lines from a Claude attempt.
type AttemptOutputStream interface {
	Recv() (string, error)
}

// hostServiceClient wraps a gRPC connection to the daemon's HostService,
// providing typed methods for the plugin to call back into the host.
type hostServiceClient struct {
	conn *grpc.ClientConn
}

func newHostServiceClient(conn *grpc.ClientConn) *hostServiceClient {
	return &hostServiceClient{conn: conn}
}

// eagerHostServiceClient starts broker.Dial(1) in a background goroutine
// immediately upon construction. GRPCServer runs before the host has called
// AcceptAndServe on the broker, but the background goroutine blocks on the
// broker channel until ConnInfo arrives. The go-plugin broker cleans up
// pending connection info after 5 seconds, so we must start the Dial
// eagerly rather than deferring to the first RPC call.
type eagerHostServiceClient struct {
	logger zerolog.Logger
	inner  *hostServiceClient
	err    error
	ready  chan struct{}
}

func newEagerHostServiceClient(broker *goplugin.GRPCBroker, logger zerolog.Logger) *eagerHostServiceClient {
	c := &eagerHostServiceClient{
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
		c.inner = newHostServiceClient(conn)
		c.logger.Info().Msg("connected to host service via broker")
	}()
	return c
}

func (c *eagerHostServiceClient) connect() (*hostServiceClient, error) {
	<-c.ready
	return c.inner, c.err
}

// --- Lazy client methods (delegate to inner after connect) ---

func (c *eagerHostServiceClient) CreateWorkflow(ctx context.Context, req *bossanovav1.CreateWorkflowRequest) (*bossanovav1.CreateWorkflowResponse, error) {
	client, err := c.connect()
	if err != nil {
		return nil, err
	}
	return client.CreateWorkflow(ctx, req)
}

func (c *eagerHostServiceClient) UpdateWorkflow(ctx context.Context, req *bossanovav1.UpdateWorkflowRequest) (*bossanovav1.UpdateWorkflowResponse, error) {
	client, err := c.connect()
	if err != nil {
		return nil, err
	}
	return client.UpdateWorkflow(ctx, req)
}

func (c *eagerHostServiceClient) GetWorkflow(ctx context.Context, id string) (*bossanovav1.GetWorkflowResponse, error) {
	client, err := c.connect()
	if err != nil {
		return nil, err
	}
	return client.GetWorkflow(ctx, id)
}

func (c *eagerHostServiceClient) CreateAttempt(ctx context.Context, req *bossanovav1.CreateAttemptRequest) (*bossanovav1.CreateAttemptResponse, error) {
	client, err := c.connect()
	if err != nil {
		return nil, err
	}
	return client.CreateAttempt(ctx, req)
}

func (c *eagerHostServiceClient) GetAttemptStatus(ctx context.Context, attemptID string) (*bossanovav1.GetAttemptStatusResponse, error) {
	client, err := c.connect()
	if err != nil {
		return nil, err
	}
	return client.GetAttemptStatus(ctx, attemptID)
}

func (c *eagerHostServiceClient) StreamAttemptOutput(ctx context.Context, attemptID string) (AttemptOutputStream, error) {
	client, err := c.connect()
	if err != nil {
		return nil, err
	}
	return client.StreamAttemptOutput(ctx, attemptID)
}

// --- Direct client methods (gRPC calls) ---

func (c *hostServiceClient) CreateWorkflow(ctx context.Context, req *bossanovav1.CreateWorkflowRequest) (*bossanovav1.CreateWorkflowResponse, error) {
	resp := &bossanovav1.CreateWorkflowResponse{}
	err := c.conn.Invoke(ctx, "/bossanova.v1.HostService/CreateWorkflow", req, resp)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *hostServiceClient) UpdateWorkflow(ctx context.Context, req *bossanovav1.UpdateWorkflowRequest) (*bossanovav1.UpdateWorkflowResponse, error) {
	resp := &bossanovav1.UpdateWorkflowResponse{}
	err := c.conn.Invoke(ctx, "/bossanova.v1.HostService/UpdateWorkflow", req, resp)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *hostServiceClient) GetWorkflow(ctx context.Context, id string) (*bossanovav1.GetWorkflowResponse, error) {
	req := &bossanovav1.GetWorkflowRequest{Id: id}
	resp := &bossanovav1.GetWorkflowResponse{}
	err := c.conn.Invoke(ctx, "/bossanova.v1.HostService/GetWorkflow", req, resp)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *hostServiceClient) CreateAttempt(ctx context.Context, req *bossanovav1.CreateAttemptRequest) (*bossanovav1.CreateAttemptResponse, error) {
	resp := &bossanovav1.CreateAttemptResponse{}
	err := c.conn.Invoke(ctx, "/bossanova.v1.HostService/CreateAttempt", req, resp)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *hostServiceClient) GetAttemptStatus(ctx context.Context, attemptID string) (*bossanovav1.GetAttemptStatusResponse, error) {
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
func (c *hostServiceClient) StreamAttemptOutput(ctx context.Context, attemptID string) (AttemptOutputStream, error) {
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
	_ hostClient          = (*eagerHostServiceClient)(nil)
	_ hostClient          = (*hostServiceClient)(nil)
	_ AttemptOutputStream = (*attemptOutputStream)(nil)
)
