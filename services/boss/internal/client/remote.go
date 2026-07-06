package client

import (
	"context"
	"fmt"
	"net/http"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/apiversion"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"google.golang.org/protobuf/proto"
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
		connect.WithInterceptors(
			newAuthInterceptor(token),
			// Stamp the API version this client was built against so the
			// server can keep us on compatible behavior after it advances.
			apiversion.ClientInterceptor(apiversion.DefaultRegistry().Current()),
		),
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

func (c *RemoteClient) ListTrackerIssues(_ context.Context, _, _, _ string) ([]*pb.TrackerIssue, error) {
	return nil, errLocalOnly("ListTrackerIssues")
}

// --- Session Lifecycle ---

func (c *RemoteClient) CreateSession(_ context.Context, _ *pb.CreateSessionRequest) (CreateSessionStream, error) {
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

func (c *RemoteClient) MergeSession(_ context.Context, _ string) (*pb.Session, error) {
	return nil, errLocalOnly("MergeSession")
}

func (c *RemoteClient) RemoveSession(_ context.Context, _ string) error {
	return errLocalOnly("RemoveSession")
}

func (c *RemoteClient) UpdateSession(_ context.Context, _ *pb.UpdateSessionRequest) (*pb.Session, error) {
	return nil, errLocalOnly("UpdateSession")
}

func (c *RemoteClient) LinkSessionPR(_ context.Context, _, _ string) (*pb.Session, error) {
	return nil, errLocalOnly("LinkSessionPR")
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

func (c *RemoteClient) RecordChat(_ context.Context, _, _, _, _ string, _ bool) (*pb.ClaudeChat, error) {
	return nil, errLocalOnly("RecordChat")
}

func (c *RemoteClient) ListChats(_ context.Context, _ string) ([]*pb.ClaudeChat, error) {
	return nil, errLocalOnly("ListChats")
}

func (c *RemoteClient) DescribeChatLaunch(_ context.Context, _ string) (*pb.DescribeChatLaunchResponse, error) {
	return nil, errLocalOnly("DescribeChatLaunch")
}

func (c *RemoteClient) UpdateChatTitle(_ context.Context, _, _ string) error {
	return errLocalOnly("UpdateChatTitle")
}

func (c *RemoteClient) DeleteChat(_ context.Context, _ string) error {
	return errLocalOnly("DeleteChat")
}

// WakeChat proxies a wake request through the orchestrator. The orchestrator
// uses sessionID for the authz check (the user must own the session that owns
// the chat) and then forwards agentSessionID + forceFresh to the daemon.
//
// The orchestrator response uses WakeChatResult_Outcome (defined in stream.proto)
// while the daemon RPC uses WakeChatResponse_Outcome (daemon.proto). The two
// enums share the same numeric values today, but we translate explicitly so a
// future divergence on either side is caught at the boundary instead of
// silently corrupting the UI.
func (c *RemoteClient) WakeChat(ctx context.Context, sessionID, agentSessionID string, forceFresh bool) (*pb.WakeChatResponse, error) {
	resp, err := c.rpc.ProxyWakeChat(ctx, connect.NewRequest(&pb.ProxyWakeChatRequest{
		SessionId:      sessionID,
		AgentSessionId: agentSessionID,
		ForceFresh:     forceFresh,
	}))
	if err != nil {
		return nil, err
	}
	return &pb.WakeChatResponse{
		Outcome:         translateStreamOutcome(resp.Msg.Outcome),
		TmuxSessionName: resp.Msg.TmuxSessionName,
		Reason:          resp.Msg.Reason,
	}, nil
}

func translateStreamOutcome(o pb.WakeChatResult_Outcome) pb.WakeChatResponse_Outcome {
	switch o {
	case pb.WakeChatResult_OUTCOME_ALREADY_LIVE:
		return pb.WakeChatResponse_OUTCOME_ALREADY_LIVE
	case pb.WakeChatResult_OUTCOME_RESUMED:
		return pb.WakeChatResponse_OUTCOME_RESUMED
	case pb.WakeChatResult_OUTCOME_FRESH_FALLBACK:
		return pb.WakeChatResponse_OUTCOME_FRESH_FALLBACK
	default:
		return pb.WakeChatResponse_OUTCOME_UNSPECIFIED
	}
}

// --- Chat Transcript and Messaging (proxied through the orchestrator) ---

// GetChatTranscript proxies a transcript read through the orchestrator, which
// routes by (session_id, agent_session_id) to the owning daemon and enforces the
// session-level authz check.
func (c *RemoteClient) GetChatTranscript(ctx context.Context, req *pb.GetChatTranscriptRequest) (*pb.GetChatTranscriptResponse, error) {
	resp, err := c.rpc.ProxyGetChatTranscript(ctx, connect.NewRequest(&pb.ProxyGetChatTranscriptRequest{
		SessionId:      req.GetSessionId(),
		AgentSessionId: req.GetAgentSessionId(),
		MaxMessages:    req.GetMaxMessages(),
	}))
	if err != nil {
		return nil, err
	}
	return &pb.GetChatTranscriptResponse{
		Messages:           resp.Msg.GetMessages(),
		FinalAssistantText: resp.Msg.GetFinalAssistantText(),
		Exists:             resp.Msg.GetExists(),
	}, nil
}

// SendChatMessage proxies a send through the orchestrator, which resolves the
// owning daemon by agent_session_id (the send RPC carries no session_id).
func (c *RemoteClient) SendChatMessage(ctx context.Context, req *pb.SendChatMessageRequest) (*pb.SendChatMessageResponse, error) {
	resp, err := c.rpc.ProxySendChatMessage(ctx, connect.NewRequest(&pb.ProxySendChatMessageRequest{
		AgentSessionId: req.GetAgentSessionId(),
		Message:        req.GetMessage(),
		WakeIfAsleep:   req.GetWakeIfAsleep(),
		// Carry the BOS-242 submit intent through the proxy; always set it
		// (present) so an explicit --submit=false (prefill) survives rather than
		// being defaulted to submit=true server-side.
		Submit: proto.Bool(req.GetSubmit()),
	}))
	if err != nil {
		return nil, err
	}
	return &pb.SendChatMessageResponse{
		TmuxSessionName: resp.Msg.GetTmuxSessionName(),
		Delivered:       resp.Msg.GetDelivered(),
	}, nil
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

// --- Auth Change Notification (local only) ---

func (c *RemoteClient) NotifyAuthChange(_ context.Context, _ string) error {
	return nil // no-op in remote mode
}

// --- Cloud Billing ---

func (c *RemoteClient) GetCloudAccessStatus(ctx context.Context) (*pb.CloudAccessStatus, error) {
	resp, err := c.rpc.GetCloudAccessStatus(ctx, connect.NewRequest(&pb.GetCloudAccessStatusRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetStatus(), nil
}

func (c *RemoteClient) CreateCheckoutSession(ctx context.Context, returnURL, cancelURL string) (string, error) {
	resp, err := c.rpc.CreateCheckoutSession(ctx, connect.NewRequest(&pb.CreateCheckoutSessionRequest{
		ReturnUrl: returnURL,
		CancelUrl: cancelURL,
	}))
	if err != nil {
		return "", err
	}
	return resp.Msg.GetUrl(), nil
}

func (c *RemoteClient) CreateBillingPortalSession(ctx context.Context, returnURL string) (string, error) {
	resp, err := c.rpc.CreateBillingPortalSession(ctx, connect.NewRequest(&pb.CreateBillingPortalSessionRequest{
		ReturnUrl: returnURL,
	}))
	if err != nil {
		return "", err
	}
	return resp.Msg.GetUrl(), nil
}

func (c *RemoteClient) RefreshCloudEntitlements(ctx context.Context) (*pb.CloudAccessStatus, error) {
	resp, err := c.rpc.RefreshCloudEntitlements(ctx, connect.NewRequest(&pb.RefreshCloudEntitlementsRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetStatus(), nil
}

// --- GitHub App Setup ---

func (c *RemoteClient) GetGitHubAppInstallURL(ctx context.Context, returnURL string) (string, error) {
	resp, err := c.rpc.GetGitHubAppInstallURL(ctx, connect.NewRequest(&pb.GetGitHubAppInstallURLRequest{
		ReturnUrl: returnURL,
	}))
	if err != nil {
		return "", err
	}
	return resp.Msg.GetInstallUrl(), nil
}

func (c *RemoteClient) ListGitHubAppRepos(ctx context.Context) ([]*pb.GitHubAppRepoStatus, error) {
	resp, err := c.rpc.ListGitHubAppRepos(ctx, connect.NewRequest(&pb.ListGitHubAppReposRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetRepos(), nil
}

// --- Cron Jobs (local only) ---

func (c *RemoteClient) CreateCronJob(_ context.Context, _ *pb.CreateCronJobRequest) (*pb.CronJob, error) {
	return nil, errLocalOnly("CreateCronJob")
}

func (c *RemoteClient) GetCronJob(_ context.Context, _ string) (*pb.CronJob, error) {
	return nil, errLocalOnly("GetCronJob")
}

func (c *RemoteClient) ListCronJobs(_ context.Context, _ string) ([]*pb.CronJob, error) {
	return nil, errLocalOnly("ListCronJobs")
}

func (c *RemoteClient) UpdateCronJob(_ context.Context, _ *pb.UpdateCronJobRequest) (*pb.CronJob, error) {
	return nil, errLocalOnly("UpdateCronJob")
}

func (c *RemoteClient) DeleteCronJob(_ context.Context, _ string) error {
	return errLocalOnly("DeleteCronJob")
}

func (c *RemoteClient) RunCronJobNow(_ context.Context, _ string) (*pb.RunCronJobNowResponse, error) {
	return nil, errLocalOnly("RunCronJobNow")
}

// --- Accounts (local only) ---

func (c *RemoteClient) ListAccounts(_ context.Context, _ string) ([]*pb.Account, error) {
	return nil, errLocalOnly("ListAccounts")
}

func (c *RemoteClient) AddAccount(_ context.Context, _ *pb.AddAccountRequest) (*pb.Account, error) {
	return nil, errLocalOnly("AddAccount")
}

func (c *RemoteClient) UpdateAccount(_ context.Context, _ *pb.UpdateAccountRequest) (*pb.Account, error) {
	return nil, errLocalOnly("UpdateAccount")
}

func (c *RemoteClient) RemoveAccount(_ context.Context, _ string) error {
	return errLocalOnly("RemoveAccount")
}

func (c *RemoteClient) TestAccount(_ context.Context, _ string) (*pb.TestAccountResponse, error) {
	return nil, errLocalOnly("TestAccount")
}

// SwitchSessionAccount proxies the switch through the orchestrator, which routes
// by session_id to the owning daemon (mirroring StopSession/WakeChat). Account
// registry management stays local-only, but the switch acts on a session the
// remote client can already stop/resume, so it must reach the same daemon rather
// than return the local-only error. Empty agent_session_id ⇒ the session's
// primary live chat (the proxy request field carries no optional wrapper, and the
// daemon treats empty and unset identically).
func (c *RemoteClient) SwitchSessionAccount(ctx context.Context, req *pb.SwitchSessionAccountRequest) (*pb.SwitchSessionAccountResponse, error) {
	resp, err := c.rpc.ProxySwitchSessionAccount(ctx, connect.NewRequest(&pb.ProxySwitchSessionAccountRequest{
		SessionId:      req.GetSessionId(),
		AgentSessionId: req.GetAgentSessionId(),
		AccountId:      req.GetAccountId(),
		Force:          req.GetForce(),
	}))
	if err != nil {
		return nil, err
	}
	return &pb.SwitchSessionAccountResponse{
		Resumed:     resp.Msg.GetResumed(),
		TargetLabel: resp.Msg.GetTargetLabel(),
		NoticeText:  resp.Msg.GetNoticeText(),
	}, nil
}

func (c *RemoteClient) RepairDoctor(_ context.Context) (*pb.RepairDoctorResponse, error) {
	return nil, errLocalOnly("RepairDoctor")
}

func (c *RemoteClient) ListCheckSnapshots(_ context.Context, _ string, _ int32) (*pb.ListCheckSnapshotsResponse, error) {
	return nil, errLocalOnly("ListCheckSnapshots")
}

func (c *RemoteClient) ListAgents(_ context.Context) ([]AgentInfo, error) {
	// Agent listing is a local-daemon concept; the orchestrator doesn't expose
	// it (each daemon has its own loaded plugin set). Return an empty list so
	// callers can render "no agents loaded" instead of erroring out the way
	// other local-only RPCs do.
	return nil, nil
}

func (c *RemoteClient) ListPlugins(_ context.Context) ([]*pb.InstalledPlugin, error) {
	// Plugin status, like agent listing, is per-daemon and intentionally
	// not proxied through the orchestrator.
	return nil, errLocalOnly("ListPlugins")
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
