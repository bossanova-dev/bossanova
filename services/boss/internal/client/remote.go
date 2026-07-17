package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

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

// RemoveRepo resolves which of the caller's live daemons serves repoID (via the
// origin-deduped aggregate) and forwards the destructive remove through
// ProxyRemoveRepo. The resolution is scoped to the caller's own repos, so a
// foreign repo id is indistinguishable from absent.
func (c *RemoteClient) RemoveRepo(ctx context.Context, id string) error {
	daemonID, err := c.resolveDaemonForRepo(ctx, id)
	if err != nil {
		return err
	}
	_, err = c.rpc.ProxyRemoveRepo(ctx, connect.NewRequest(&pb.ProxyRemoveRepoRequest{
		DaemonId: daemonID,
		RepoId:   id,
	}))
	return err
}

// resolveDaemonForRepo finds which of the caller's live daemons serves repoID
// using the same origin-deduped aggregate the gateway backend uses.
func (c *RemoteClient) resolveDaemonForRepo(ctx context.Context, repoID string) (string, error) {
	if repoID == "" {
		return "", connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("repo_id is required"))
	}
	resp, err := c.rpc.ProxyListReposAggregated(ctx, connect.NewRequest(&pb.ProxyListReposAggregatedRequest{}))
	if err != nil {
		return "", err
	}
	for _, agg := range resp.Msg.GetRepos() {
		for _, ref := range agg.GetDaemons() {
			if ref.GetRepoId() == repoID {
				return ref.GetDaemonId(), nil
			}
		}
	}
	return "", connect.NewError(connect.CodeNotFound, fmt.Errorf("no live daemon serves repo %q", repoID))
}

// UpdateRepo resolves the daemon serving the repo (scoped to the caller's own
// daemons via ProxyListReposAggregated) and forwards the update through the
// orchestrator's ProxyUpdateRepo RPC. It returns the security-masked settings
// projected back to a *pb.Repo (secret/physical fields stay empty), mirroring the
// mcp-gateway proxybackend.
func (c *RemoteClient) UpdateRepo(ctx context.Context, req *pb.UpdateRepoRequest) (*pb.Repo, error) {
	daemonID, err := c.resolveRemoteDaemonForRepo(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	proxyReq := &pb.ProxyUpdateRepoRequest{
		DaemonId:                  daemonID,
		RepoId:                    req.GetId(),
		DisplayName:               req.DisplayName,
		SetupScript:               req.SetupScript,
		CanAutoMerge:              req.CanAutoMerge,
		CanAutoMergeDependabot:    req.CanAutoMergeDependabot,
		CanAutoRepair:             req.CanAutoRepair,
		ArchiveSessionsAfterMerge: req.ArchiveSessionsAfterMerge,
		SentryOrg:                 req.SentryOrg,
		ExpectedUpdatedAt:         req.GetExpectedUpdatedAt(),
	}
	if req.MergeStrategy != nil {
		ms := remoteMergeStrategyStringToEnum(req.GetMergeStrategy())
		proxyReq.MergeStrategy = &ms
	}
	proxyReq.LinearKey = remoteChooseSecretUpdate(req.GetLinearKey(), req.LinearApiKey)
	proxyReq.SentryKey = remoteChooseSecretUpdate(req.GetSentryKey(), req.SentryApiKey)
	resp, err := c.rpc.ProxyUpdateRepo(ctx, connect.NewRequest(proxyReq))
	if err != nil {
		return nil, err
	}
	return remoteSettingsToRepo(resp.Msg.GetSettings()), nil
}

// resolveRemoteDaemonForRepo finds which of the caller's Ready daemons serves
// repoID, using the same aggregate ListRepos exposes. A repo the caller does not
// own is absent from the aggregate, so it never resolves.
func (c *RemoteClient) resolveRemoteDaemonForRepo(ctx context.Context, repoID string) (string, error) {
	if repoID == "" {
		return "", errors.New("repo id is required")
	}
	resp, err := c.rpc.ProxyListReposAggregated(ctx, connect.NewRequest(&pb.ProxyListReposAggregatedRequest{}))
	if err != nil {
		return "", err
	}
	for _, agg := range resp.Msg.GetRepos() {
		for _, ref := range agg.GetDaemons() {
			if ref.GetRepoId() == repoID {
				return ref.GetDaemonId(), nil
			}
		}
	}
	return "", fmt.Errorf("no live daemon serves repo %q", repoID)
}

func remoteMergeStrategyStringToEnum(s string) pb.MergeStrategy {
	switch strings.ToLower(s) {
	case "rebase":
		return pb.MergeStrategy_MERGE_STRATEGY_REBASE
	case "squash":
		return pb.MergeStrategy_MERGE_STRATEGY_SQUASH
	case "merge":
		return pb.MergeStrategy_MERGE_STRATEGY_MERGE
	default:
		return pb.MergeStrategy_MERGE_STRATEGY_UNSPECIFIED
	}
}

func remoteMergeStrategyEnumToString(ms pb.MergeStrategy) string {
	switch ms {
	case pb.MergeStrategy_MERGE_STRATEGY_REBASE:
		return "rebase"
	case pb.MergeStrategy_MERGE_STRATEGY_SQUASH:
		return "squash"
	case pb.MergeStrategy_MERGE_STRATEGY_MERGE:
		return "merge"
	default:
		return ""
	}
}

func remoteChooseSecretUpdate(explicit *pb.SecretUpdate, legacy *string) *pb.SecretUpdate {
	if explicit != nil && explicit.GetAction() != pb.SecretAction_SECRET_ACTION_UNSPECIFIED {
		return explicit
	}
	if legacy != nil {
		return &pb.SecretUpdate{Action: pb.SecretAction_SECRET_ACTION_SET, Value: legacy}
	}
	return nil
}

func remoteSettingsToRepo(s *pb.RepoSettings) *pb.Repo {
	if s == nil {
		return nil
	}
	repo := &pb.Repo{
		Id:                        s.GetId(),
		DisplayName:               s.GetDisplayName(),
		MergeStrategy:             remoteMergeStrategyEnumToString(s.GetMergeStrategy()),
		CanAutoMerge:              s.GetCanAutoMerge(),
		CanAutoMergeDependabot:    s.GetCanAutoMergeDependabot(),
		CanAutoRepair:             s.GetCanAutoRepair(),
		ArchiveSessionsAfterMerge: s.GetArchiveSessionsAfterMerge(),
		SentryOrg:                 s.GetSentryOrg(),
		UpdatedAt:                 s.GetUpdatedAt(),
	}
	if s.SetupScript != nil {
		repo.SetupScript = s.SetupScript
	}
	return repo
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

func (c *RemoteClient) RetrySession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := c.rpc.ProxyRetrySession(ctx, connect.NewRequest(&pb.ProxyRetrySessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSession(), nil
}

func (c *RemoteClient) CloseSession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := c.rpc.ProxyCloseSession(ctx, connect.NewRequest(&pb.ProxyCloseSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Session, nil
}

func (c *RemoteClient) MergeSession(_ context.Context, _ string) (*pb.Session, error) {
	return nil, errLocalOnly("MergeSession")
}

func (c *RemoteClient) RemoveSession(ctx context.Context, id string) error {
	_, err := c.rpc.ProxyRemoveSession(ctx, connect.NewRequest(&pb.ProxyRemoveSessionRequest{Id: id}))
	return err
}

func (c *RemoteClient) UpdateSession(ctx context.Context, req *pb.UpdateSessionRequest) (*pb.Session, error) {
	resp, err := c.rpc.ProxyUpdateSession(ctx, connect.NewRequest(&pb.ProxyUpdateSessionRequest{
		Id:         req.GetId(),
		Title:      req.Title,
		TrackerUrl: req.TrackerUrl,
		TrackerId:  req.TrackerId,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSession(), nil
}

func (c *RemoteClient) LinkSessionPR(ctx context.Context, id, pr string) (*pb.Session, error) {
	resp, err := c.rpc.ProxyLinkSessionPR(ctx, connect.NewRequest(&pb.ProxyLinkSessionPRRequest{Id: id, Pr: pr}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSession(), nil
}

// --- Archive / Resurrect (local only) ---

func (c *RemoteClient) ArchiveSession(_ context.Context, _ string) (*pb.Session, error) {
	return nil, errLocalOnly("ArchiveSession")
}

func (c *RemoteClient) ResurrectSession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := c.rpc.ProxyResurrectSession(ctx, connect.NewRequest(&pb.ProxyResurrectSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Session, nil
}

// EmptyTrash proxies the daemon-wide trash purge through the orchestrator, which
// aggregates across the caller's Ready daemons and sums the deleted counts.
func (c *RemoteClient) EmptyTrash(ctx context.Context, req *pb.EmptyTrashRequest) (int32, error) {
	resp, err := c.rpc.ProxyEmptyTrash(ctx, connect.NewRequest(&pb.ProxyEmptyTrashRequest{
		OlderThan: req.GetOlderThan(),
	}))
	if err != nil {
		return 0, err
	}
	return resp.Msg.GetDeletedCount(), nil
}

// --- Claude Chat Tracking (local only) ---

func (c *RemoteClient) RecordChat(_ context.Context, _, _, _, _ string, _ bool) (*pb.ClaudeChat, error) {
	return nil, errLocalOnly("RecordChat")
}

// ListChats proxies a session's chat list through the orchestrator, which routes
// by session_id to the owning daemon and enforces the session-level authz check.
func (c *RemoteClient) ListChats(ctx context.Context, sessionID string) ([]*pb.ClaudeChat, error) {
	resp, err := c.rpc.ProxyListChats(ctx, connect.NewRequest(&pb.ProxyListChatsRequest{SessionId: sessionID}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetChats(), nil
}

func (c *RemoteClient) DescribeChatLaunch(_ context.Context, _ string) (*pb.DescribeChatLaunchResponse, error) {
	return nil, errLocalOnly("DescribeChatLaunch")
}

func (c *RemoteClient) UpdateChatTitle(ctx context.Context, agentSessionID, title string) error {
	_, err := c.rpc.ProxyUpdateChatTitle(ctx, connect.NewRequest(&pb.ProxyUpdateChatTitleRequest{
		AgentSessionId: agentSessionID,
		Title:          title,
	}))
	return err
}

// DeleteChat proxies a chat delete through the orchestrator, which resolves the
// owning daemon by agent_session_id (scoped to the caller's daemons) since the
// delete_chat tool carries no session_id.
func (c *RemoteClient) DeleteChat(ctx context.Context, agentSessionID string) error {
	_, err := c.rpc.ProxyDeleteChat(ctx, connect.NewRequest(&pb.ProxyDeleteChatRequest{
		AgentSessionId: agentSessionID,
	}))
	return err
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
		Reason:             resp.Msg.GetReason(),
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
		// Thread the mechanical-outcome notice (e.g. an intercepted
		// "/boss switch") from the proxy response back to the local caller.
		NoticeText: resp.Msg.GetNoticeText(),
	}, nil
}

// --- Chat Status ---

func (c *RemoteClient) ReportChatStatus(ctx context.Context, reports []*pb.ChatStatusReport) error {
	_, err := c.rpc.ProxyReportChatStatus(ctx, connect.NewRequest(&pb.ProxyReportChatStatusRequest{Reports: reports}))
	return err
}

func (c *RemoteClient) GetChatStatuses(_ context.Context, _ string) ([]*pb.ChatStatusEntry, error) {
	return nil, errLocalOnly("GetChatStatuses")
}

// GetSessionStatuses proxies a multi-session status read through the orchestrator,
// which fans each session_id out to its owning daemon (scoped to the caller's
// daemons) and unions the results, skipping unknown ids.
func (c *RemoteClient) GetSessionStatuses(ctx context.Context, sessionIDs []string) ([]*pb.SessionStatusEntry, error) {
	resp, err := c.rpc.ProxyGetSessionStatuses(ctx, connect.NewRequest(&pb.ProxyGetSessionStatusesRequest{SessionIds: sessionIDs}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetStatuses(), nil
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

// CreateCronJob proxies a cron-job create through the orchestrator. daemon_id is
// left empty so bosso resolves the caller's sole Ready daemon (FailedPrecondition
// when the caller has more than one — never a silent guess).
func (c *RemoteClient) CreateCronJob(ctx context.Context, req *pb.CreateCronJobRequest) (*pb.CronJob, error) {
	resp, err := c.rpc.ProxyCreateCronJob(ctx, connect.NewRequest(&pb.ProxyCreateCronJobRequest{
		RepoId:          req.GetRepoId(),
		Name:            req.GetName(),
		Prompt:          req.GetPrompt(),
		Schedule:        req.GetSchedule(),
		Timezone:        req.GetTimezone(),
		Enabled:         req.GetEnabled(),
		AgentName:       req.GetAgentName(),
		Model:           req.GetModel(),
		GateCommand:     req.GetGateCommand(),
		RunSetupCommand: req.RunSetupCommand,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetJob(), nil
}

// GetCronJob proxies a by-id cron-job read through the orchestrator, which
// returns the first daemon whose registry holds the id (NotFound if none).
func (c *RemoteClient) GetCronJob(ctx context.Context, id string) (*pb.CronJob, error) {
	resp, err := c.rpc.ProxyGetCronJob(ctx, connect.NewRequest(&pb.ProxyGetCronJobRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetCronJob(), nil
}

// ListCronJobs proxies the aggregate cron-job list through the orchestrator,
// which fans out across the caller's Ready daemons. When repoID is non-empty the
// result is filtered client-side to that repo, matching the local daemon's
// repo-scoped listing.
func (c *RemoteClient) ListCronJobs(ctx context.Context, repoID string) ([]*pb.CronJob, error) {
	resp, err := c.rpc.ProxyListCronJobs(ctx, connect.NewRequest(&pb.ProxyListCronJobsRequest{}))
	if err != nil {
		return nil, err
	}
	jobs := make([]*pb.CronJob, 0, len(resp.Msg.GetJobs()))
	for _, jwd := range resp.Msg.GetJobs() {
		job := jwd.GetJob()
		if repoID != "" && job.GetRepoId() != repoID {
			continue
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

// UpdateCronJob proxies a cron-job update through the orchestrator, which
// resolves the owning daemon by id (daemon_id left empty). Optional field
// pointers pass straight through so an unset field leaves the value untouched.
func (c *RemoteClient) UpdateCronJob(ctx context.Context, req *pb.UpdateCronJobRequest) (*pb.CronJob, error) {
	resp, err := c.rpc.ProxyUpdateCronJob(ctx, connect.NewRequest(&pb.ProxyUpdateCronJobRequest{
		Id:              req.GetId(),
		Name:            req.Name,
		Prompt:          req.Prompt,
		Schedule:        req.Schedule,
		Timezone:        req.Timezone,
		Enabled:         req.Enabled,
		AgentName:       req.AgentName,
		Model:           req.Model,
		GateCommand:     req.GateCommand,
		RunSetupCommand: req.RunSetupCommand,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetJob(), nil
}

// DeleteCronJob proxies a cron-job delete through the orchestrator, resolving the
// owning daemon by id (daemon_id left empty).
func (c *RemoteClient) DeleteCronJob(ctx context.Context, id string) error {
	_, err := c.rpc.ProxyDeleteCronJob(ctx, connect.NewRequest(&pb.ProxyDeleteCronJobRequest{Id: id}))
	return err
}

// RunCronJobNow proxies a manual cron-job fire through the orchestrator,
// resolving the owning daemon by id (daemon_id left empty).
func (c *RemoteClient) RunCronJobNow(ctx context.Context, id string) (*pb.RunCronJobNowResponse, error) {
	resp, err := c.rpc.ProxyRunCronJobNow(ctx, connect.NewRequest(&pb.ProxyRunCronJobNowRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return &pb.RunCronJobNowResponse{
		Session:       resp.Msg.GetSession(),
		SkippedReason: resp.Msg.GetSkippedReason(),
	}, nil
}

// --- Accounts (local only) ---

func (c *RemoteClient) ListAccounts(_ context.Context, _ string, _ bool) ([]*pb.Account, error) {
	return nil, errLocalOnly("ListAccounts")
}

func (c *RemoteClient) AddAccount(_ context.Context, _ *pb.AddAccountRequest) (*pb.Account, error) {
	return nil, errLocalOnly("AddAccount")
}

func (c *RemoteClient) RefreshAccount(_ context.Context, _ *pb.RefreshAccountRequest) (*pb.RefreshAccountResponse, error) {
	return nil, errLocalOnly("RefreshAccount")
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

// RepairDoctor proxies the repair diagnostics through the orchestrator, which
// merges checks and recent logs across the caller's Ready daemons.
func (c *RemoteClient) RepairDoctor(ctx context.Context) (*pb.RepairDoctorResponse, error) {
	resp, err := c.rpc.ProxyRepairDoctor(ctx, connect.NewRequest(&pb.ProxyRepairDoctorRequest{}))
	if err != nil {
		return nil, err
	}
	return &pb.RepairDoctorResponse{
		Checks:     resp.Msg.GetChecks(),
		RecentLogs: resp.Msg.GetRecentLogs(),
	}, nil
}

func (c *RemoteClient) StartRepairWorkflow(_ context.Context) (*pb.StartRepairWorkflowResponse, error) {
	return nil, errLocalOnly("StartRepairWorkflow")
}

// ListCheckSnapshots proxies a session's CI check-snapshot history through the
// orchestrator, which routes by session_id to the owning daemon.
func (c *RemoteClient) ListCheckSnapshots(ctx context.Context, sessionID string, limit int32) (*pb.ListCheckSnapshotsResponse, error) {
	resp, err := c.rpc.ProxyListCheckSnapshots(ctx, connect.NewRequest(&pb.ProxyListCheckSnapshotsRequest{
		SessionId: sessionID,
		Limit:     limit,
	}))
	if err != nil {
		return nil, err
	}
	return &pb.ListCheckSnapshotsResponse{Snapshots: resp.Msg.GetSnapshots()}, nil
}

// ListAgents proxies the aggregate agent-runner list through the orchestrator,
// which concatenates each Ready daemon's loaded plugin set, converting each proto
// AgentInfo into the package-local type views consume.
func (c *RemoteClient) ListAgents(ctx context.Context) ([]AgentInfo, error) {
	resp, err := c.rpc.ProxyListAgentsAggregated(ctx, connect.NewRequest(&pb.ProxyListAgentsAggregatedRequest{}))
	if err != nil {
		return nil, err
	}
	agents := make([]AgentInfo, 0, len(resp.Msg.GetAgents()))
	for _, a := range resp.Msg.GetAgents() {
		agents = append(agents, agentInfoFromProto(a))
	}
	return agents, nil
}

// ListPlugins proxies the aggregate installed-plugin list through the
// orchestrator, which concatenates each Ready daemon's plugin set.
func (c *RemoteClient) ListPlugins(ctx context.Context) ([]*pb.InstalledPlugin, error) {
	resp, err := c.rpc.ProxyListPlugins(ctx, connect.NewRequest(&pb.ProxyListPluginsRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetPlugins(), nil
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
