package main

import (
	"context"
	"fmt"

	goplugin "github.com/hashicorp/go-plugin"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"
)

// dependabotAuthor is the GitHub login for dependabot PRs.
// The gh CLI returns "app/dependabot" (not "dependabot[bot]" as in the API).
const dependabotAuthor = "app/dependabot"

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

func (c *eagerHostServiceClient) ListDependabotPRs(ctx context.Context, repoOriginURL string) ([]*bossanovav1.PRSummary, error) {
	client, err := c.connect()
	if err != nil {
		return nil, err
	}
	return client.ListDependabotPRs(ctx, repoOriginURL)
}

func (c *eagerHostServiceClient) GetCheckResults(ctx context.Context, repoOriginURL string, prNumber int32) ([]*bossanovav1.CheckResult, error) {
	client, err := c.connect()
	if err != nil {
		return nil, err
	}
	return client.GetCheckResults(ctx, repoOriginURL, prNumber)
}

func (c *eagerHostServiceClient) GetPRStatus(ctx context.Context, repoOriginURL string, prNumber int32) (*bossanovav1.PRStatus, error) {
	client, err := c.connect()
	if err != nil {
		return nil, err
	}
	return client.GetPRStatus(ctx, repoOriginURL, prNumber)
}

func (c *eagerHostServiceClient) ListClosedDependabotPRs(ctx context.Context, repoOriginURL string) ([]*bossanovav1.PRSummary, error) {
	client, err := c.connect()
	if err != nil {
		return nil, err
	}
	return client.ListClosedDependabotPRs(ctx, repoOriginURL)
}

// ListDependabotPRs calls ListOpenPRs and filters for dependabot-authored PRs.
func (c *hostServiceClient) ListDependabotPRs(ctx context.Context, repoOriginURL string) ([]*bossanovav1.PRSummary, error) {
	prs, err := c.ListOpenPRs(ctx, repoOriginURL)
	if err != nil {
		return nil, err
	}

	var filtered []*bossanovav1.PRSummary
	for _, pr := range prs {
		if pr.GetAuthor() == dependabotAuthor {
			filtered = append(filtered, pr)
		}
	}
	return filtered, nil
}

func (c *hostServiceClient) ListOpenPRs(ctx context.Context, repoOriginURL string) ([]*bossanovav1.PRSummary, error) {
	req := &bossanovav1.ListOpenPRsRequest{RepoOriginUrl: repoOriginURL}
	resp := &bossanovav1.ListOpenPRsResponse{}
	err := c.conn.Invoke(ctx, "/bossanova.v1.HostService/ListOpenPRs", req, resp)
	if err != nil {
		return nil, err
	}
	return resp.GetPrs(), nil
}

func (c *hostServiceClient) GetCheckResults(ctx context.Context, repoOriginURL string, prNumber int32) ([]*bossanovav1.CheckResult, error) {
	req := &bossanovav1.GetCheckResultsRequest{
		RepoOriginUrl: repoOriginURL,
		PrNumber:      prNumber,
	}
	resp := &bossanovav1.GetCheckResultsResponse{}
	err := c.conn.Invoke(ctx, "/bossanova.v1.HostService/GetCheckResults", req, resp)
	if err != nil {
		return nil, err
	}
	return resp.GetChecks(), nil
}

// ListClosedDependabotPRs calls ListClosedPRs and filters for dependabot-authored PRs.
func (c *hostServiceClient) ListClosedDependabotPRs(ctx context.Context, repoOriginURL string) ([]*bossanovav1.PRSummary, error) {
	prs, err := c.ListClosedPRs(ctx, repoOriginURL)
	if err != nil {
		return nil, err
	}

	var filtered []*bossanovav1.PRSummary
	for _, pr := range prs {
		if pr.GetAuthor() == dependabotAuthor {
			filtered = append(filtered, pr)
		}
	}
	return filtered, nil
}

func (c *hostServiceClient) ListClosedPRs(ctx context.Context, repoOriginURL string) ([]*bossanovav1.PRSummary, error) {
	req := &bossanovav1.ListClosedPRsRequest{RepoOriginUrl: repoOriginURL}
	resp := &bossanovav1.ListClosedPRsResponse{}
	err := c.conn.Invoke(ctx, "/bossanova.v1.HostService/ListClosedPRs", req, resp)
	if err != nil {
		return nil, err
	}
	return resp.GetPrs(), nil
}

func (c *hostServiceClient) GetPRStatus(ctx context.Context, repoOriginURL string, prNumber int32) (*bossanovav1.PRStatus, error) {
	req := &bossanovav1.GetPRStatusRequest{
		RepoOriginUrl: repoOriginURL,
		PrNumber:      prNumber,
	}
	resp := &bossanovav1.GetPRStatusResponse{}
	err := c.conn.Invoke(ctx, "/bossanova.v1.HostService/GetPRStatus", req, resp)
	if err != nil {
		return nil, err
	}
	return resp.GetStatus(), nil
}
