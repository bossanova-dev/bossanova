package main

import (
	"context"
	"fmt"
	"sync"

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

// lazyHostServiceClient defers the broker.Dial(1) until first use. This is
// necessary because GRPCServer runs before the host has called AcceptAndServe
// on the broker, so the connection cannot be established during plugin init.
type lazyHostServiceClient struct {
	broker *goplugin.GRPCBroker
	logger zerolog.Logger
	once   sync.Once
	inner  *hostServiceClient
	err    error
}

func newLazyHostServiceClient(broker *goplugin.GRPCBroker, logger zerolog.Logger) *lazyHostServiceClient {
	return &lazyHostServiceClient{broker: broker, logger: logger}
}

func (c *lazyHostServiceClient) connect() (*hostServiceClient, error) {
	c.once.Do(func() {
		conn, err := c.broker.Dial(1)
		if err != nil {
			c.err = fmt.Errorf("dial host service: %w", err)
			return
		}
		c.inner = newHostServiceClient(conn)
		c.logger.Info().Msg("connected to host service via broker")
	})
	return c.inner, c.err
}

func (c *lazyHostServiceClient) ListDependabotPRs(ctx context.Context, repoOriginURL string) ([]*bossanovav1.PRSummary, error) {
	client, err := c.connect()
	if err != nil {
		return nil, err
	}
	return client.ListDependabotPRs(ctx, repoOriginURL)
}

func (c *lazyHostServiceClient) GetCheckResults(ctx context.Context, repoOriginURL string, prNumber int32) ([]*bossanovav1.CheckResult, error) {
	client, err := c.connect()
	if err != nil {
		return nil, err
	}
	return client.GetCheckResults(ctx, repoOriginURL, prNumber)
}

func (c *lazyHostServiceClient) GetPRStatus(ctx context.Context, repoOriginURL string, prNumber int32) (*bossanovav1.PRStatus, error) {
	client, err := c.connect()
	if err != nil {
		return nil, err
	}
	return client.GetPRStatus(ctx, repoOriginURL, prNumber)
}

func (c *lazyHostServiceClient) ListClosedDependabotPRs(ctx context.Context, repoOriginURL string) ([]*bossanovav1.PRSummary, error) {
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
