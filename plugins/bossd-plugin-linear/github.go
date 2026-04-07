package main

import (
	"context"
	"fmt"
	"strings"

	goplugin "github.com/hashicorp/go-plugin"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"
)

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

func (c *eagerHostServiceClient) ListOpenPRs(ctx context.Context, repoOriginURL string) ([]*bossanovav1.PRSummary, error) {
	client, err := c.connect()
	if err != nil {
		return nil, err
	}
	return client.ListOpenPRs(ctx, repoOriginURL)
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

// matchPR attempts to match a Linear issue to an existing PR.
// It first tries to match by branch name (exact match), then falls back
// to matching by title (contains [ENG-123]). Returns (prNumber, branch)
// if a match is found, or (0, "") if no match.
func matchPR(issue linearIssue, prs []*bossanovav1.PRSummary) (prNumber int32, branch string) {
	// Primary: branch name match
	if issue.BranchName != "" {
		for _, pr := range prs {
			if pr.HeadBranch == issue.BranchName {
				return pr.Number, pr.HeadBranch
			}
		}
	}

	// Fallback: title contains [ENG-123]
	tag := "[" + issue.Identifier + "]"
	for _, pr := range prs {
		if strings.Contains(pr.Title, tag) {
			return pr.Number, pr.HeadBranch
		}
	}

	return 0, ""
}
