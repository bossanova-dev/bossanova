// Package client provides a ConnectRPC client for the bossd daemon.
package client

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

// DefaultSocketPath returns the default Unix socket path for the daemon.
// On macOS: ~/Library/Application Support/bossanova/bossd.sock
func DefaultSocketPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	return filepath.Join(home, "Library", "Application Support", "bossanova", "bossd.sock"), nil
}

// Client wraps the generated ConnectRPC DaemonServiceClient with
// Unix socket transport for local daemon communication.
type Client struct {
	rpc        bossanovav1connect.DaemonServiceClient
	socketPath string
}

// New creates a Client connected to the daemon via the given Unix socket.
func New(socketPath string) *Client {
	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		},
	}

	// The base URL host is ignored; the Unix socket dialer overrides it.
	rpc := bossanovav1connect.NewDaemonServiceClient(
		httpClient,
		"http://localhost",
	)

	return &Client{
		rpc:        rpc,
		socketPath: socketPath,
	}
}

// Ping verifies the daemon is reachable by listing repos.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.rpc.ListRepos(ctx, connect.NewRequest(&pb.ListReposRequest{}))
	return err
}

// --- Context Resolution ---

func (c *Client) ResolveContext(ctx context.Context, workingDir string) (*pb.ResolveContextResponse, error) {
	resp, err := c.rpc.ResolveContext(ctx, connect.NewRequest(&pb.ResolveContextRequest{
		WorkingDirectory: workingDir,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// --- Repo Management ---

func (c *Client) RegisterRepo(ctx context.Context, req *pb.RegisterRepoRequest) (*pb.Repo, error) {
	resp, err := c.rpc.RegisterRepo(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Repo, nil
}

func (c *Client) ListRepos(ctx context.Context) ([]*pb.Repo, error) {
	resp, err := c.rpc.ListRepos(ctx, connect.NewRequest(&pb.ListReposRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Repos, nil
}

func (c *Client) RemoveRepo(ctx context.Context, id string) error {
	_, err := c.rpc.RemoveRepo(ctx, connect.NewRequest(&pb.RemoveRepoRequest{Id: id}))
	return err
}

func (c *Client) ListRepoPRs(ctx context.Context, repoID string) ([]*pb.PRSummary, error) {
	resp, err := c.rpc.ListRepoPRs(ctx, connect.NewRequest(&pb.ListRepoPRsRequest{RepoId: repoID}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.PullRequests, nil
}

// --- Session Lifecycle ---

func (c *Client) CreateSession(ctx context.Context, req *pb.CreateSessionRequest) (*pb.Session, error) {
	resp, err := c.rpc.CreateSession(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Session, nil
}

func (c *Client) GetSession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := c.rpc.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Session, nil
}

func (c *Client) ListSessions(ctx context.Context, req *pb.ListSessionsRequest) ([]*pb.Session, error) {
	resp, err := c.rpc.ListSessions(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Sessions, nil
}

func (c *Client) AttachSession(ctx context.Context, id string) (*connect.ServerStreamForClient[pb.AttachSessionResponse], error) {
	return c.rpc.AttachSession(ctx, connect.NewRequest(&pb.AttachSessionRequest{Id: id}))
}

func (c *Client) StopSession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := c.rpc.StopSession(ctx, connect.NewRequest(&pb.StopSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Session, nil
}

func (c *Client) PauseSession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := c.rpc.PauseSession(ctx, connect.NewRequest(&pb.PauseSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Session, nil
}

func (c *Client) ResumeSession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := c.rpc.ResumeSession(ctx, connect.NewRequest(&pb.ResumeSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Session, nil
}

func (c *Client) RetrySession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := c.rpc.RetrySession(ctx, connect.NewRequest(&pb.RetrySessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Session, nil
}

func (c *Client) CloseSession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := c.rpc.CloseSession(ctx, connect.NewRequest(&pb.CloseSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Session, nil
}

func (c *Client) RemoveSession(ctx context.Context, id string) error {
	_, err := c.rpc.RemoveSession(ctx, connect.NewRequest(&pb.RemoveSessionRequest{Id: id}))
	return err
}

// --- Archive / Resurrect ---

func (c *Client) ArchiveSession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := c.rpc.ArchiveSession(ctx, connect.NewRequest(&pb.ArchiveSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Session, nil
}

func (c *Client) ResurrectSession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := c.rpc.ResurrectSession(ctx, connect.NewRequest(&pb.ResurrectSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Session, nil
}

func (c *Client) EmptyTrash(ctx context.Context, req *pb.EmptyTrashRequest) (int32, error) {
	resp, err := c.rpc.EmptyTrash(ctx, connect.NewRequest(req))
	if err != nil {
		return 0, err
	}
	return resp.Msg.DeletedCount, nil
}
