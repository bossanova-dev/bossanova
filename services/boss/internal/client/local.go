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

// LocalClient communicates with the daemon via Unix socket.
type LocalClient struct {
	rpc        bossanovav1connect.DaemonServiceClient
	socketPath string
}

// Verify LocalClient implements BossClient at compile time.
var _ BossClient = (*LocalClient)(nil)

// NewLocal creates a LocalClient connected to the daemon via the given Unix socket.
func NewLocal(socketPath string) *LocalClient {
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

	return &LocalClient{
		rpc:        rpc,
		socketPath: socketPath,
	}
}

func (c *LocalClient) Ping(ctx context.Context) error {
	_, err := c.rpc.ListRepos(ctx, connect.NewRequest(&pb.ListReposRequest{}))
	return err
}

// --- Context Resolution ---

func (c *LocalClient) ResolveContext(ctx context.Context, workingDir string) (*pb.ResolveContextResponse, error) {
	resp, err := c.rpc.ResolveContext(ctx, connect.NewRequest(&pb.ResolveContextRequest{
		WorkingDirectory: workingDir,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// --- Repo Management ---

func (c *LocalClient) RegisterRepo(ctx context.Context, req *pb.RegisterRepoRequest) (*pb.Repo, error) {
	resp, err := c.rpc.RegisterRepo(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Repo, nil
}

func (c *LocalClient) CloneAndRegisterRepo(ctx context.Context, req *pb.CloneAndRegisterRepoRequest) (*pb.Repo, error) {
	resp, err := c.rpc.CloneAndRegisterRepo(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Repo, nil
}

func (c *LocalClient) ListRepos(ctx context.Context) ([]*pb.Repo, error) {
	resp, err := c.rpc.ListRepos(ctx, connect.NewRequest(&pb.ListReposRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Repos, nil
}

func (c *LocalClient) RemoveRepo(ctx context.Context, id string) error {
	_, err := c.rpc.RemoveRepo(ctx, connect.NewRequest(&pb.RemoveRepoRequest{Id: id}))
	return err
}

func (c *LocalClient) ListRepoPRs(ctx context.Context, repoID string) ([]*pb.PRSummary, error) {
	resp, err := c.rpc.ListRepoPRs(ctx, connect.NewRequest(&pb.ListRepoPRsRequest{RepoId: repoID}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.PullRequests, nil
}

// --- Session Lifecycle ---

func (c *LocalClient) CreateSession(ctx context.Context, req *pb.CreateSessionRequest) (*pb.Session, error) {
	resp, err := c.rpc.CreateSession(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Session, nil
}

func (c *LocalClient) GetSession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := c.rpc.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Session, nil
}

func (c *LocalClient) ListSessions(ctx context.Context, req *pb.ListSessionsRequest) ([]*pb.Session, error) {
	resp, err := c.rpc.ListSessions(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Sessions, nil
}

func (c *LocalClient) AttachSession(ctx context.Context, id string) (AttachStream, error) {
	stream, err := c.rpc.AttachSession(ctx, connect.NewRequest(&pb.AttachSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return &localAttachStream{stream: stream}, nil
}

func (c *LocalClient) StopSession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := c.rpc.StopSession(ctx, connect.NewRequest(&pb.StopSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Session, nil
}

func (c *LocalClient) PauseSession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := c.rpc.PauseSession(ctx, connect.NewRequest(&pb.PauseSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Session, nil
}

func (c *LocalClient) ResumeSession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := c.rpc.ResumeSession(ctx, connect.NewRequest(&pb.ResumeSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Session, nil
}

func (c *LocalClient) RetrySession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := c.rpc.RetrySession(ctx, connect.NewRequest(&pb.RetrySessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Session, nil
}

func (c *LocalClient) CloseSession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := c.rpc.CloseSession(ctx, connect.NewRequest(&pb.CloseSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Session, nil
}

func (c *LocalClient) RemoveSession(ctx context.Context, id string) error {
	_, err := c.rpc.RemoveSession(ctx, connect.NewRequest(&pb.RemoveSessionRequest{Id: id}))
	return err
}

// --- Archive / Resurrect ---

func (c *LocalClient) ArchiveSession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := c.rpc.ArchiveSession(ctx, connect.NewRequest(&pb.ArchiveSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Session, nil
}

func (c *LocalClient) ResurrectSession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := c.rpc.ResurrectSession(ctx, connect.NewRequest(&pb.ResurrectSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Session, nil
}

func (c *LocalClient) EmptyTrash(ctx context.Context, req *pb.EmptyTrashRequest) (int32, error) {
	resp, err := c.rpc.EmptyTrash(ctx, connect.NewRequest(req))
	if err != nil {
		return 0, err
	}
	return resp.Msg.DeletedCount, nil
}

// localAttachStream wraps the DaemonService AttachSession stream.
type localAttachStream struct {
	stream *connect.ServerStreamForClient[pb.AttachSessionResponse]
}

func (s *localAttachStream) Receive() bool {
	return s.stream.Receive()
}

func (s *localAttachStream) Msg() *AttachEvent {
	msg := s.stream.Msg()
	ev := &AttachEvent{}
	switch e := msg.Event.(type) {
	case *pb.AttachSessionResponse_OutputLine:
		ev.OutputLine = e.OutputLine
	case *pb.AttachSessionResponse_StateChange:
		ev.StateChange = e.StateChange
	case *pb.AttachSessionResponse_SessionEnded:
		ev.SessionEnded = e.SessionEnded
	}
	return ev
}

func (s *localAttachStream) Err() error {
	return s.stream.Err()
}

func (s *localAttachStream) Close() error {
	return s.stream.Close()
}
