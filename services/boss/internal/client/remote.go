package client

import (
	"context"
	"fmt"
	"net/http"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

// RemoteClient communicates with the orchestrator service, proxying
// session operations to the appropriate daemon.
type RemoteClient struct {
	rpc bossanovav1connect.OrchestratorServiceClient
}

// Verify RemoteClient implements BossClient at compile time.
var _ BossClient = (*RemoteClient)(nil)

// NewRemote creates a RemoteClient connected to the orchestrator at the given URL.
// The token is sent as a Bearer token on every request.
func NewRemote(baseURL, token string) *RemoteClient {
	rpc := bossanovav1connect.NewOrchestratorServiceClient(
		http.DefaultClient,
		baseURL,
		connect.WithInterceptors(newAuthInterceptor(token)),
	)
	return &RemoteClient{rpc: rpc}
}

// authInterceptor injects a Bearer token into every outgoing request.
type authInterceptor struct {
	token string
}

func newAuthInterceptor(token string) *authInterceptor {
	return &authInterceptor{token: token}
}

func (a *authInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("Authorization", "Bearer "+a.token)
		return next(ctx, req)
	}
}

func (a *authInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set("Authorization", "Bearer "+a.token)
		return conn
	}
}

func (a *authInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next // server-side; no-op for client interceptor
}

// errLocalOnly is returned for operations that only work on a local daemon.
func errLocalOnly(op string) error {
	return connect.NewError(connect.CodeUnimplemented, fmt.Errorf("%s is only available on a local daemon", op))
}

// --- Ping ---

func (c *RemoteClient) Ping(ctx context.Context) error {
	_, err := c.rpc.ProxyListSessions(ctx, connect.NewRequest(&pb.ProxyListSessionsRequest{}))
	return err
}

// --- Context Resolution (local only) ---

func (c *RemoteClient) ResolveContext(_ context.Context, _ string) (*pb.ResolveContextResponse, error) {
	return nil, errLocalOnly("ResolveContext")
}

// --- Repo Management (local only) ---

func (c *RemoteClient) ValidateRepoPath(_ context.Context, _ string) (*pb.ValidateRepoPathResponse, error) {
	return nil, errLocalOnly("ValidateRepoPath")
}

func (c *RemoteClient) RegisterRepo(_ context.Context, _ *pb.RegisterRepoRequest) (*pb.Repo, error) {
	return nil, errLocalOnly("RegisterRepo")
}

func (c *RemoteClient) CloneAndRegisterRepo(_ context.Context, _ *pb.CloneAndRegisterRepoRequest) (*pb.Repo, error) {
	return nil, errLocalOnly("CloneAndRegisterRepo")
}

func (c *RemoteClient) ListRepos(_ context.Context) ([]*pb.Repo, error) {
	return nil, errLocalOnly("ListRepos")
}

func (c *RemoteClient) RemoveRepo(_ context.Context, _ string) error {
	return errLocalOnly("RemoveRepo")
}

func (c *RemoteClient) UpdateRepo(_ context.Context, _ *pb.UpdateRepoRequest) (*pb.Repo, error) {
	return nil, errLocalOnly("UpdateRepo")
}

func (c *RemoteClient) ListRepoPRs(_ context.Context, _ string) ([]*pb.PRSummary, error) {
	return nil, errLocalOnly("ListRepoPRs")
}

// --- Session Lifecycle ---

func (c *RemoteClient) CreateSession(_ context.Context, _ *pb.CreateSessionRequest) (*pb.Session, error) {
	return nil, errLocalOnly("CreateSession")
}

func (c *RemoteClient) GetSession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := c.rpc.ProxyGetSession(ctx, connect.NewRequest(&pb.ProxyGetSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Session, nil
}

func (c *RemoteClient) ListSessions(ctx context.Context, req *pb.ListSessionsRequest) ([]*pb.Session, error) {
	proxyReq := &pb.ProxyListSessionsRequest{
		IncludeArchived: req.IncludeArchived,
		States:          req.States,
	}
	if req.RepoId != nil {
		proxyReq.RepoId = req.RepoId
	}
	resp, err := c.rpc.ProxyListSessions(ctx, connect.NewRequest(proxyReq))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Sessions, nil
}

func (c *RemoteClient) AttachSession(ctx context.Context, id string) (AttachStream, error) {
	stream, err := c.rpc.ProxyAttachSession(ctx, connect.NewRequest(&pb.ProxyAttachSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return &remoteAttachStream{stream: stream}, nil
}

func (c *RemoteClient) StopSession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := c.rpc.ProxyStopSession(ctx, connect.NewRequest(&pb.ProxyStopSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Session, nil
}

func (c *RemoteClient) PauseSession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := c.rpc.ProxyPauseSession(ctx, connect.NewRequest(&pb.ProxyPauseSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Session, nil
}

func (c *RemoteClient) ResumeSession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := c.rpc.ProxyResumeSession(ctx, connect.NewRequest(&pb.ProxyResumeSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Session, nil
}

func (c *RemoteClient) RetrySession(_ context.Context, _ string) (*pb.Session, error) {
	return nil, errLocalOnly("RetrySession")
}

func (c *RemoteClient) CloseSession(_ context.Context, _ string) (*pb.Session, error) {
	return nil, errLocalOnly("CloseSession")
}

func (c *RemoteClient) RemoveSession(_ context.Context, _ string) error {
	return errLocalOnly("RemoveSession")
}

// --- Archive / Resurrect (local only) ---

func (c *RemoteClient) ArchiveSession(_ context.Context, _ string) (*pb.Session, error) {
	return nil, errLocalOnly("ArchiveSession")
}

func (c *RemoteClient) ResurrectSession(_ context.Context, _ string) (*pb.Session, error) {
	return nil, errLocalOnly("ResurrectSession")
}

func (c *RemoteClient) EmptyTrash(_ context.Context, _ *pb.EmptyTrashRequest) (int32, error) {
	return 0, errLocalOnly("EmptyTrash")
}

// --- Claude Chat Tracking (local only) ---

func (c *RemoteClient) RecordChat(_ context.Context, _, _, _ string) (*pb.ClaudeChat, error) {
	return nil, errLocalOnly("RecordChat")
}

func (c *RemoteClient) ListChats(_ context.Context, _ string) ([]*pb.ClaudeChat, error) {
	return nil, errLocalOnly("ListChats")
}

func (c *RemoteClient) UpdateChatTitle(_ context.Context, _, _ string) error {
	return errLocalOnly("UpdateChatTitle")
}

func (c *RemoteClient) DeleteChat(_ context.Context, _ string) error {
	return errLocalOnly("DeleteChat")
}

// --- Chat Status (local only) ---

func (c *RemoteClient) ReportChatStatus(_ context.Context, _ []*pb.ChatStatusReport) error {
	return errLocalOnly("ReportChatStatus")
}

func (c *RemoteClient) GetChatStatuses(_ context.Context, _ string) ([]*pb.ChatStatusEntry, error) {
	return nil, errLocalOnly("GetChatStatuses")
}

func (c *RemoteClient) GetSessionStatuses(_ context.Context, _ []string) ([]*pb.SessionStatusEntry, error) {
	return nil, errLocalOnly("GetSessionStatuses")
}

// remoteAttachStream wraps the OrchestratorService ProxyAttachSession stream.
type remoteAttachStream struct {
	stream *connect.ServerStreamForClient[pb.ProxyAttachSessionResponse]
}

func (s *remoteAttachStream) Receive() bool {
	return s.stream.Receive()
}

func (s *remoteAttachStream) Msg() *AttachEvent {
	msg := s.stream.Msg()
	ev := &AttachEvent{}
	switch e := msg.Event.(type) {
	case *pb.ProxyAttachSessionResponse_OutputLine:
		ev.OutputLine = e.OutputLine
	case *pb.ProxyAttachSessionResponse_StateChange:
		ev.StateChange = e.StateChange
	case *pb.ProxyAttachSessionResponse_SessionEnded:
		ev.SessionEnded = e.SessionEnded
	}
	return ev
}

func (s *remoteAttachStream) Err() error {
	return s.stream.Err()
}

func (s *remoteAttachStream) Close() error {
	return s.stream.Close()
}
