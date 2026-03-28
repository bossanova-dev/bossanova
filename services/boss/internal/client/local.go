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

func (c *LocalClient) ValidateRepoPath(ctx context.Context, localPath string) (*pb.ValidateRepoPathResponse, error) {
	resp, err := c.rpc.ValidateRepoPath(ctx, connect.NewRequest(&pb.ValidateRepoPathRequest{
		LocalPath: localPath,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

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

func (c *LocalClient) UpdateRepo(ctx context.Context, req *pb.UpdateRepoRequest) (*pb.Repo, error) {
	resp, err := c.rpc.UpdateRepo(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Repo, nil
}

func (c *LocalClient) ListRepoPRs(ctx context.Context, repoID string) ([]*pb.PRSummary, error) {
	resp, err := c.rpc.ListRepoPRs(ctx, connect.NewRequest(&pb.ListRepoPRsRequest{RepoId: repoID}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.PullRequests, nil
}

// --- Session Lifecycle ---

func (c *LocalClient) CreateSession(ctx context.Context, req *pb.CreateSessionRequest) (CreateSessionStream, error) {
	stream, err := c.rpc.CreateSession(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return &localCreateSessionStream{stream: stream}, nil
}

// localCreateSessionStream wraps the DaemonService CreateSession stream.
type localCreateSessionStream struct {
	stream *connect.ServerStreamForClient[pb.CreateSessionResponse]
}

func (s *localCreateSessionStream) Receive() bool {
	return s.stream.Receive()
}

func (s *localCreateSessionStream) Msg() *pb.CreateSessionResponse {
	return s.stream.Msg()
}

func (s *localCreateSessionStream) Err() error {
	return s.stream.Err()
}

func (s *localCreateSessionStream) Close() error {
	return s.stream.Close()
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

func (c *LocalClient) UpdateSession(ctx context.Context, req *pb.UpdateSessionRequest) (*pb.Session, error) {
	resp, err := c.rpc.UpdateSession(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Session, nil
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

// --- Claude Chat Tracking ---

func (c *LocalClient) RecordChat(ctx context.Context, sessionID, claudeID, title string) (*pb.ClaudeChat, error) {
	resp, err := c.rpc.RecordChat(ctx, connect.NewRequest(&pb.RecordChatRequest{
		SessionId: sessionID,
		ClaudeId:  claudeID,
		Title:     title,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Chat, nil
}

func (c *LocalClient) ListChats(ctx context.Context, sessionID string) ([]*pb.ClaudeChat, error) {
	resp, err := c.rpc.ListChats(ctx, connect.NewRequest(&pb.ListChatsRequest{
		SessionId: sessionID,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Chats, nil
}

func (c *LocalClient) UpdateChatTitle(ctx context.Context, claudeID, title string) error {
	_, err := c.rpc.UpdateChatTitle(ctx, connect.NewRequest(&pb.UpdateChatTitleRequest{
		ClaudeId: claudeID,
		Title:    title,
	}))
	return err
}

func (c *LocalClient) DeleteChat(ctx context.Context, claudeID string) error {
	_, err := c.rpc.DeleteChat(ctx, connect.NewRequest(&pb.DeleteChatRequest{
		ClaudeId: claudeID,
	}))
	return err
}

// --- Chat Status ---

func (c *LocalClient) ReportChatStatus(ctx context.Context, statuses []*pb.ChatStatusReport) error {
	_, err := c.rpc.ReportChatStatus(ctx, connect.NewRequest(&pb.ReportChatStatusRequest{
		Reports: statuses,
	}))
	return err
}

func (c *LocalClient) GetChatStatuses(ctx context.Context, sessionID string) ([]*pb.ChatStatusEntry, error) {
	resp, err := c.rpc.GetChatStatuses(ctx, connect.NewRequest(&pb.GetChatStatusesRequest{
		SessionId: sessionID,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Statuses, nil
}

func (c *LocalClient) GetSessionStatuses(ctx context.Context, sessionIDs []string) ([]*pb.SessionStatusEntry, error) {
	resp, err := c.rpc.GetSessionStatuses(ctx, connect.NewRequest(&pb.GetSessionStatusesRequest{
		SessionIds: sessionIDs,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Statuses, nil
}

// --- Autopilot Workflows ---

func (c *LocalClient) StartAutopilot(ctx context.Context, req *pb.StartAutopilotRequest) (*pb.AutopilotWorkflow, error) {
	resp, err := c.rpc.StartAutopilot(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Workflow, nil
}

func (c *LocalClient) PauseAutopilot(ctx context.Context, workflowID string) (*pb.AutopilotWorkflow, error) {
	resp, err := c.rpc.PauseAutopilot(ctx, connect.NewRequest(&pb.PauseAutopilotRequest{WorkflowId: workflowID}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Workflow, nil
}

func (c *LocalClient) ResumeAutopilot(ctx context.Context, workflowID string) (*pb.AutopilotWorkflow, error) {
	resp, err := c.rpc.ResumeAutopilot(ctx, connect.NewRequest(&pb.ResumeAutopilotRequest{WorkflowId: workflowID}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Workflow, nil
}

func (c *LocalClient) CancelAutopilot(ctx context.Context, workflowID string) (*pb.AutopilotWorkflow, error) {
	resp, err := c.rpc.CancelAutopilot(ctx, connect.NewRequest(&pb.CancelAutopilotRequest{WorkflowId: workflowID}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Workflow, nil
}

func (c *LocalClient) GetAutopilotStatus(ctx context.Context, workflowID string) (*pb.AutopilotWorkflow, error) {
	resp, err := c.rpc.GetAutopilotStatus(ctx, connect.NewRequest(&pb.GetAutopilotStatusRequest{WorkflowId: workflowID}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Workflow, nil
}

func (c *LocalClient) ListAutopilotWorkflows(ctx context.Context, req *pb.ListAutopilotWorkflowsRequest) ([]*pb.AutopilotWorkflow, error) {
	resp, err := c.rpc.ListAutopilotWorkflows(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Workflows, nil
}

func (c *LocalClient) StreamAutopilotOutput(ctx context.Context, workflowID string) (AutopilotOutputStream, error) {
	stream, err := c.rpc.StreamAutopilotOutput(ctx, connect.NewRequest(&pb.StreamAutopilotOutputRequest{WorkflowId: workflowID}))
	if err != nil {
		return nil, err
	}
	return &localAutopilotStream{stream: stream}, nil
}

// localAutopilotStream wraps the DaemonService StreamAutopilotOutput stream.
type localAutopilotStream struct {
	stream *connect.ServerStreamForClient[pb.StreamAutopilotOutputResponse]
}

func (s *localAutopilotStream) Receive() bool {
	return s.stream.Receive()
}

func (s *localAutopilotStream) Msg() *pb.StreamAutopilotOutputResponse {
	return s.stream.Msg()
}

func (s *localAutopilotStream) Err() error {
	return s.stream.Err()
}

func (s *localAutopilotStream) Close() error {
	return s.stream.Close()
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
