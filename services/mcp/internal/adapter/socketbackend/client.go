// Package socketbackend supplies a bossmcp.Backend backed by the local bossd
// daemon over its Unix socket. It speaks Connect RPC to bossanova.v1.DaemonService
// exactly like the boss CLI's local client, so the MCP tools see the same
// behaviour the CLI would.
package socketbackend

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/bossmcp"
	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/contract/daemon/socketauth"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

// Dial retry bounds. During a bossd restart the Unix socket file is briefly
// absent (ENOENT) or refusing connections (ECONNREFUSED). Retrying only the
// connection establishment (never a request body) bridges that window while
// keeping mutating RPCs safe.
const (
	maxDialAttempts = 4
	dialRetryDelay  = 250 * time.Millisecond
)

// Backend implements bossmcp.Backend over a Unix-socket Connect client.
type Backend struct {
	rpc        bossanovav1connect.DaemonServiceClient
	socketPath string

	// The host facts ProjectDaemonIdentity reads: the settings file, the
	// app data dir it resolves to (where bossd records its identity), and
	// the hostname the display name falls back to. Fields so tests point
	// them at temp dirs.
	settingsPath func() (string, error)
	appDataDir   func(config.Settings) (string, error)
	hostname     func() (string, error)
}

// Verify Backend satisfies the bossmcp interfaces at compile time.
var (
	_ bossmcp.Backend             = (*Backend)(nil)
	_ bossmcp.ProjectLocalBackend = (*Backend)(nil)
)

// dialWithRetry dials the bossd Unix socket, retrying a bounded number of times
// when the socket is transiently absent or refusing during a daemon restart. It
// retries ONLY connection-establishment errors (ENOENT / ECONNREFUSED); any
// other dial error, or a cancelled/expired context, returns immediately. Only
// the connection is retried — no request is ever re-sent, so mutating RPCs stay
// safe.
func dialWithRetry(ctx context.Context, socketPath string) (net.Conn, error) {
	var lastErr error
	for attempt := range maxDialAttempts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		var d net.Dialer
		conn, err := d.DialContext(ctx, "unix", socketPath)
		if err == nil {
			return conn, nil
		}
		lastErr = err

		// Retry only transient connection-establishment errors. errors.Is
		// unwraps through *net.OpError (it implements Unwrap).
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ECONNREFUSED) {
			return nil, err
		}

		// Back off before the next attempt, but abort promptly if the request
		// context is cancelled or times out during the delay. As of Go 1.23 a
		// receive from timer.C after Stop must not be drained — it is guaranteed
		// to block — so Stop() alone releases the timer here.
		if attempt < maxDialAttempts-1 {
			timer := time.NewTimer(dialRetryDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
	}

	return nil, fmt.Errorf("bossd socket %q unavailable after %d dial attempts (daemon may be restarting): %w", socketPath, maxDialAttempts, lastErr)
}

// New connects to the bossd daemon at socketPath. The base URL host is ignored
// because the Unix-socket dialer overrides it.
func New(socketPath string) (*Backend, error) {
	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialWithRetry(ctx, socketPath)
			},
		},
	}

	// Attach the daemon's socket auth token (co-located with the socket), exactly
	// as the boss CLI's local client does — bossd rejects every DaemonService RPC
	// lacking it with CodeUnauthenticated. If the token file is missing/malformed
	// we still build the client; the RPC then surfaces the daemon's clear
	// Unauthenticated error.
	var opts []connect.ClientOption
	if token, err := socketauth.ReadToken(socketPath); err == nil {
		opts = append(opts, connect.WithInterceptors(socketauth.NewClientInterceptor(token)))
	}

	rpc := bossanovav1connect.NewDaemonServiceClient(httpClient, "http://localhost", opts...)
	return &Backend{rpc: rpc, socketPath: socketPath, settingsPath: config.Path, appDataDir: appDataDirFor, hostname: os.Hostname}, nil
}

// DefaultSocketPath resolves the configured daemon socket path from settings,
// falling back to the default app-data location when none is configured.
func DefaultSocketPath() (string, error) {
	return config.ResolveSocketPath()
}

// SocketPath returns the path this backend dials.
func (b *Backend) SocketPath() string { return b.socketPath }

func (b *Backend) Ping(ctx context.Context) error {
	_, err := b.rpc.ListRepos(ctx, connect.NewRequest(&pb.ListReposRequest{}))
	return err
}

func (b *Backend) ListTrackerIssues(ctx context.Context, repoID, query, source string) ([]*pb.TrackerIssue, error) {
	req := &pb.ListTrackerIssuesRequest{RepoId: repoID, Query: query}
	if source != "" {
		req.Source = &source
	}
	resp, err := b.rpc.ListTrackerIssues(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetIssues(), nil
}

func (b *Backend) EmptyTrash(ctx context.Context, req *pb.EmptyTrashRequest) (int32, error) {
	resp, err := b.rpc.EmptyTrash(ctx, connect.NewRequest(req))
	if err != nil {
		return 0, err
	}
	return resp.Msg.GetDeletedCount(), nil
}

func stringPtrIfNotEmpty(s string) *string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	out := strings.TrimSpace(s)
	return &out
}
