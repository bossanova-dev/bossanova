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
	"github.com/recurser/bossalib/vcs"
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

// errCloudOnly is the mirror of errLocalOnly for operations a local daemon can
// never serve because the data lives in bosso: organizations and the
// repo-to-organization mapping. It is deliberately not errLocalOnly — that
// helper's message says the operation "is only available on a local daemon",
// which is exactly backwards when a LocalClient is the one refusing, and would
// tell a signed-out user to do the thing they are already doing.
func errCloudOnly(op string) error {
	return connect.NewError(connect.CodeUnimplemented, fmt.Errorf("%s is only available when signed in to Bossanova Cloud", op))
}

// errBroadcastLocalOnly is returned for the broadcast RPCs, which have no
// orchestrator proxy today. It is deliberately distinct from errLocalOnly:
// broadcasts are not permanently local — cross-daemon delivery is a separate,
// planned child — so the message says "not yet routed through the
// orchestrator" rather than implying the operation will never work remotely.
func errBroadcastLocalOnly(op string) error {
	return connect.NewError(connect.CodeUnimplemented, fmt.Errorf(
		"%s is only available against a local daemon: broadcasts are not yet routed through the orchestrator, so run this without --remote (or with BOSS_SOCKET pointed at a local bossd)", op))
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
		DaemonId:                        daemonID,
		RepoId:                          req.GetId(),
		DisplayName:                     req.DisplayName,
		SetupScript:                     req.SetupScript,
		CanAutoMerge:                    req.CanAutoMerge,
		CanAutoMergeDependabot:          req.CanAutoMergeDependabot,
		CanAutoRepair:                   req.CanAutoRepair,
		ShouldArchiveSessionsAfterMerge: req.ShouldArchiveSessionsAfterMerge,
		SentryOrg:                       req.SentryOrg,
		ExpectedUpdatedAt:               req.GetExpectedUpdatedAt(),
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
		Id:                              s.GetId(),
		DisplayName:                     s.GetDisplayName(),
		MergeStrategy:                   remoteMergeStrategyEnumToString(s.GetMergeStrategy()),
		CanAutoMerge:                    s.GetCanAutoMerge(),
		CanAutoMergeDependabot:          s.GetCanAutoMergeDependabot(),
		CanAutoRepair:                   s.GetCanAutoRepair(),
		ShouldArchiveSessionsAfterMerge: s.GetShouldArchiveSessionsAfterMerge(),
		SentryOrg:                       s.GetSentryOrg(),
		UpdatedAt:                       s.GetUpdatedAt(),
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

// GetSession ignores opts entirely: machine-local endpoints are never surfaced
// through the orchestrator, so RemoteClient never sets the opt-in or namespace
// fields on the proxy request. The ProxyGetSession surface has no such fields.
func (c *RemoteClient) GetSession(ctx context.Context, id string, _ SessionReadOptions) (*pb.Session, error) {
	resp, err := c.rpc.ProxyGetSession(ctx, connect.NewRequest(&pb.ProxyGetSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Session, nil
}

// ListSessions ignores opts entirely (see GetSession): the orchestrator never
// carries machine-local endpoints.
func (c *RemoteClient) ListSessions(ctx context.Context, req *pb.ListSessionsRequest, opts SessionReadOptions) ([]*pb.Session, error) {
	sessions, _, err := c.ListSessionsWithReadFailures(ctx, req, opts)
	return sessions, err
}

// ListSessionsWithReadFailures reads across every organization the caller
// belongs to. That union is the default of ProxyListSessions; a failed
// organization is reported, never promoted to an error, so sessions that were
// read still reach the screen.
func (c *RemoteClient) ListSessionsWithReadFailures(ctx context.Context, req *pb.ListSessionsRequest, _ SessionReadOptions) ([]*pb.Session, []*pb.OrganizationSessionReadFailure, error) {
	proxyReq := &pb.ProxyListSessionsRequest{
		IncludeArchived: req.IncludeArchived,
		States:          req.States,
	}
	if req.RepoId != nil {
		proxyReq.RepoId = req.RepoId
	}
	resp, err := c.rpc.ProxyListSessions(ctx, connect.NewRequest(proxyReq))
	if err != nil {
		return nil, nil, err
	}
	return resp.Msg.GetSessions(), resp.Msg.GetFailedOrganizations(), nil
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

// MergeSession proxies the merge through the orchestrator, which routes it to
// the owning daemon. The detail return is always "": ProxyMergeSessionResponse
// carries only the session, so a merge-strategy substitution note cannot cross
// the remote boundary. Adding the field would be an observable API change
// requiring a date-based apiversion bump plus a down-convert transform.
func (c *RemoteClient) MergeSession(ctx context.Context, id string) (*pb.Session, string, error) {
	resp, err := c.rpc.ProxyMergeSession(ctx, connect.NewRequest(&pb.ProxyMergeSessionRequest{Id: id}))
	if err != nil {
		return nil, "", err
	}
	return resp.Msg.GetSession(), "", nil
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

func (c *RemoteClient) RefreshSessionPR(context.Context, *pb.RefreshSessionPRRequest) (*pb.Session, error) {
	return nil, errors.New("refresh session PR is not supported via the hosted orchestrator")
}

// --- Archive / Resurrect (local only) ---

func (c *RemoteClient) ArchiveSession(_ context.Context, _ string) (*pb.Session, error) {
	return nil, errLocalOnly("ArchiveSession")
}

// ResurrectSession adapts the orchestrator's UNARY ProxyResurrectSession into
// the streaming shape the Client interface now uses (BOS-984).
//
// The proxy RPC is deliberately left unary: its request/response shapes are on
// bosso's versioned OrchestratorService surface, and changing them would owe an
// apiversion bump plus a down-convert transform. The daemon-local streaming
// conversion is what fixes the timeout; this leg simply reports the one result
// it gets as a single terminal frame, with no progress to relay.
func (c *RemoteClient) ResurrectSession(ctx context.Context, id string) (ResurrectSessionStream, error) {
	resp, err := c.rpc.ProxyResurrectSession(ctx, connect.NewRequest(&pb.ProxyResurrectSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return &singleFrameResurrectStream{
		frame: &pb.ResurrectSessionResponse{
			Event: &pb.ResurrectSessionResponse_SessionResurrected{
				SessionResurrected: &pb.SessionResurrected{Session: resp.Msg.GetSession()},
			},
		},
	}, nil
}

// singleFrameResurrectStream replays exactly one already-received frame. It
// exists so a unary upstream can satisfy client.ResurrectSessionStream without
// the callers needing to know which client they hold.
type singleFrameResurrectStream struct {
	frame *pb.ResurrectSessionResponse
	sent  bool
}

func (s *singleFrameResurrectStream) Receive() bool {
	if s.sent {
		return false
	}
	s.sent = true
	return true
}

func (s *singleFrameResurrectStream) Msg() *pb.ResurrectSessionResponse { return s.frame }

func (s *singleFrameResurrectStream) Err() error { return nil }

func (s *singleFrameResurrectStream) Close() error { return nil }

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

// DescribeChatMCP is local-only by construction: answering it spawns a probe
// process in the chat's worktree on the daemon host, which a remote
// orchestrator has neither of.
func (c *RemoteClient) DescribeChatMCP(_ context.Context, _ string) (*pb.DescribeChatMCPResponse, error) {
	return nil, errLocalOnly("DescribeChatMCP")
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
	// This is one of TWO hand-rolled converters that rebuild the response field by
	// field instead of passing it through — the sibling is (*Backend).SendChatMessage
	// in services/mcp-gateway/internal/proxybackend/proxybackend.go — so both are
	// places where a new daemon-side signal can be dropped silently, and a field
	// added here has to be added there too (they have already drifted once).
	// delivery_state is an additive proxy response field. Older bosso builds omit
	// it, so notice_text remains the mixed-version fallback that lets cloud
	// callers distinguish unconfirmed/queued sends from a bare delivered=false.
	return &pb.SendChatMessageResponse{
		TmuxSessionName: resp.Msg.GetTmuxSessionName(),
		Delivered:       resp.Msg.GetDelivered(),
		DeliveryState:   resp.Msg.GetDeliveryState(),
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

// GetChatStatuses proxies a single session's per-chat status read through the
// orchestrator, which routes it to that session's owning daemon by session_id.
// Deliberately not served by GetSessionStatuses: that aggregates a session's
// chats into one entry, so it cannot say whether one specific chat has settled.
func (c *RemoteClient) GetChatStatuses(ctx context.Context, sessionID string) ([]*pb.ChatStatusEntry, error) {
	resp, err := c.rpc.ProxyGetChatStatuses(ctx, connect.NewRequest(&pb.ProxyGetChatStatusesRequest{SessionId: sessionID}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetStatuses(), nil
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

// NotifyAuthChange is a no-op in remote mode: there is no local daemon holding
// credentials to reload, so there is no verdict to report either. A nil response
// is the honest answer and renders as silence, never as a false "OK".
func (c *RemoteClient) NotifyAuthChange(_ context.Context, _ string) (*pb.NotifyAuthChangeResponse, error) {
	return nil, nil
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

// --- Organizations and repo-organization mapping (cloud only) ---
//
// These live on RemoteClient (and, as a defined refusal, on LocalClient) but
// deliberately not on the BossClient interface: the mapping is bosso-owned and
// only ever reached through the authenticated cloud client, exactly like the
// cloud-billing and GitHub App methods above. Keeping them off the interface
// keeps every BossClient fake in the tree compiling unchanged.

// ListOrganizations returns the organizations the authenticated caller belongs
// to. The server scopes the list to the caller, so this is already "exactly the
// caller's organizations" with no client-side filtering.
func (c *RemoteClient) ListOrganizations(ctx context.Context) ([]*pb.Organization, error) {
	resp, err := c.rpc.ListOrganizations(ctx, connect.NewRequest(&pb.ListOrganizationsRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetOrganizations(), nil
}

// GetRepoOrganization returns the organization mapping for a repo origin, or a
// nil mapping when the origin is unmapped. An unmapped origin is a miss, not an
// error: the server answers an unset mapping and so does this.
func (c *RemoteClient) GetRepoOrganization(ctx context.Context, repoOriginURL string) (*pb.RepoOrganizationMapping, error) {
	resp, err := c.rpc.GetRepoOrganization(ctx, connect.NewRequest(&pb.GetRepoOrganizationRequest{
		RepoOriginUrl: canonicalRepoOriginURL(repoOriginURL),
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetMapping(), nil
}

// SetRepoOrganization maps a repo origin to an organization. The server fails
// closed when the caller is not an active member of organizationID, so a
// non-member attempt surfaces here as a PermissionDenied error rather than a
// silent write.
func (c *RemoteClient) SetRepoOrganization(ctx context.Context, repoOriginURL, organizationID string) (*pb.RepoOrganizationMapping, error) {
	resp, err := c.rpc.SetRepoOrganization(ctx, connect.NewRequest(&pb.SetRepoOrganizationRequest{
		RepoOriginUrl:  canonicalRepoOriginURL(repoOriginURL),
		OrganizationId: organizationID,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetMapping(), nil
}

// ClearRepoOrganization releases organizationID's claim on a repo origin,
// returning the repo to the unmapped ("Personal") state. Deleting a row that is
// already gone is success, not NotFound.
func (c *RemoteClient) ClearRepoOrganization(ctx context.Context, repoOriginURL, organizationID string) error {
	_, err := c.rpc.ClearRepoOrganization(ctx, connect.NewRequest(&pb.ClearRepoOrganizationRequest{
		RepoOriginUrl:  canonicalRepoOriginURL(repoOriginURL),
		OrganizationId: organizationID,
	}))
	return err
}

// canonicalRepoOriginURLPasses bounds the canonicalization loop below, matching
// the bound bosso's validateRepoOriginURL uses. The loop exists because
// vcs.NormalizeRepoURL is not idempotent on every input: ".../alpha.git/"
// settles on ".../alpha.git", which normalizes again to ".../alpha", because
// the parser strips ".git" before the trailing slash. One pass is therefore
// "closer to canonical", not canonical.
const canonicalRepoOriginURLPasses = 8

// canonicalRepoOriginURL puts a repo origin into the same canonical
// https://<host>/<owner>/<repo> spelling bosso stores mappings under, so the
// TUI keys a mapping the same way the server does rather than sending whatever
// raw origin (ssh shorthand, trailing ".git") the repo happens to carry.
//
// It iterates to the normalization fixed point rather than normalizing once,
// because the fixed point is what validateRepoOriginURL stores and compares
// against; stopping one pass short would send a spelling the server accepts but
// rewrites, which is the same key by luck rather than by construction.
//
// An origin that cannot be parsed, or that does not settle within the bound, is
// passed through untouched so the server's own InvalidArgument is what the user
// sees, rather than this turning it into an empty-origin error of our invention.
func canonicalRepoOriginURL(originURL string) string {
	canonical := vcs.NormalizeRepoURL(strings.TrimSpace(originURL))
	if canonical == "" {
		return originURL
	}
	// Same control flow as validateRepoOriginURL, so "the same bound" is the
	// same number of passes and not just the same constant: the limit is tested
	// after next is computed, which allows one more pass than a loop condition
	// would. Only the outcome differs — an origin that does not settle is
	// passed through for the server to refuse rather than refused here.
	for pass := 0; ; pass++ {
		next := vcs.NormalizeRepoURL(canonical)
		if next == canonical {
			return canonical
		}
		if next == "" || pass >= canonicalRepoOriginURLPasses {
			return originURL
		}
		canonical = next
	}
}

// --- Cron Jobs (local only) ---

// CreateCronJob proxies a cron-job create through the orchestrator. daemon_id is
// left empty so bosso resolves the caller's sole Ready daemon (FailedPrecondition
// when the caller has more than one — never a silent guess).
func (c *RemoteClient) CreateCronJob(ctx context.Context, req *pb.CreateCronJobRequest) (*pb.CronJob, error) {
	resp, err := c.rpc.ProxyCreateCronJob(ctx, connect.NewRequest(&pb.ProxyCreateCronJobRequest{
		RepoId:                req.GetRepoId(),
		Name:                  req.GetName(),
		Prompt:                req.GetPrompt(),
		Schedule:              req.GetSchedule(),
		Timezone:              req.GetTimezone(),
		IsEnabled:             req.GetIsEnabled(),
		AgentName:             req.GetAgentName(),
		Model:                 req.GetModel(),
		GateCommand:           req.GetGateCommand(),
		ShouldRunSetupCommand: req.ShouldRunSetupCommand,
		IsZeroOutput:          req.IsZeroOutput,
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
		Id:                    req.GetId(),
		Name:                  req.Name,
		Prompt:                req.Prompt,
		Schedule:              req.Schedule,
		Timezone:              req.Timezone,
		IsEnabled:             req.IsEnabled,
		AgentName:             req.AgentName,
		Model:                 req.Model,
		GateCommand:           req.GateCommand,
		ShouldRunSetupCommand: req.ShouldRunSetupCommand,
		IsZeroOutput:          req.IsZeroOutput,
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

// --- GitHub Callbacks ---

// CreateGithubCallback proxies a callback registration through the orchestrator,
// which routes to the owning bossd by target_chat_id (FindDaemonForChat) and
// reuses the caller's own auth — no service credential. The message body is
// carried verbatim and never logged on either hop.
func (c *RemoteClient) CreateGithubCallback(ctx context.Context, req *pb.CreateGithubCallbackRequest) (*pb.GithubCallback, error) {
	resp, err := c.rpc.ProxyCreateGithubCallback(ctx, connect.NewRequest(&pb.ProxyCreateGithubCallbackRequest{
		GroupId:                 req.GroupId,
		TargetChatId:            req.GetTargetChatId(),
		RepoOwner:               req.GetRepoOwner(),
		RepoName:                req.GetRepoName(),
		PrNumber:                req.GetPrNumber(),
		Trigger:                 req.GetTrigger(),
		Message:                 req.GetMessage(),
		ExpiresAt:               req.GetExpiresAt(),
		ShouldRequireTransition: req.ShouldRequireTransition,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetGithubCallback(), nil
}

// ListGithubCallbacks proxies the callback list through the orchestrator. When
// req carries a target_chat_id it routes to that chat's owning daemon; otherwise
// bosso fans out across the caller's Ready daemons and concatenates the results.
// The optional filter pointers pass straight through so an unset field is not
// constrained.
func (c *RemoteClient) ListGithubCallbacks(ctx context.Context, req *pb.ListGithubCallbacksRequest) ([]*pb.GithubCallback, error) {
	resp, err := c.rpc.ProxyListGithubCallbacks(ctx, connect.NewRequest(&pb.ProxyListGithubCallbacksRequest{
		TargetChatId: req.TargetChatId,
		RepoOwner:    req.RepoOwner,
		RepoName:     req.RepoName,
		PrNumber:     req.PrNumber,
		Trigger:      req.Trigger,
		State:        req.State,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetGithubCallbacks(), nil
}

// DeleteGithubCallback proxies a callback delete through the orchestrator,
// routing to the owning daemon by target_chat_id.
func (c *RemoteClient) DeleteGithubCallback(ctx context.Context, targetChatID, id string) (*pb.DeleteGithubCallbackResponse, error) {
	resp, err := c.rpc.ProxyDeleteGithubCallback(ctx, connect.NewRequest(&pb.ProxyDeleteGithubCallbackRequest{
		TargetChatId:       targetChatID,
		Id:                 id,
		ExpectTargetChatId: stringPtrIfNotEmpty(targetChatID),
	}))
	if err != nil {
		return nil, err
	}
	return &pb.DeleteGithubCallbackResponse{Outcome: resp.Msg.GetOutcome()}, nil
}

// --- Notes ---

// CreateNote proxies a note creation through the orchestrator, routed by
// repo_id to the owning daemon.
func (c *RemoteClient) CreateNote(ctx context.Context, req *pb.CreateNoteRequest) (*pb.Note, error) {
	resp, err := c.rpc.ProxyCreateNote(ctx, connect.NewRequest(&pb.ProxyCreateNoteRequest{
		RepoId:         req.GetRepoId(),
		SessionId:      req.SessionId,
		ChatId:         req.ChatId,
		Body:           req.GetBody(),
		Tags:           req.Tags,
		IdempotencyKey: req.IdempotencyKey,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetNote(), nil
}

// GetNote proxies a note read through the orchestrator, routing to the
// owning daemon by repoID.
func (c *RemoteClient) GetNote(ctx context.Context, repoID, id string) (*pb.Note, error) {
	resp, err := c.rpc.ProxyGetNote(ctx, connect.NewRequest(&pb.ProxyGetNoteRequest{
		RepoId: repoID,
		Id:     id,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetNote(), nil
}

// ListNotes proxies the note list through the orchestrator. When req carries
// a repo_id it routes to that repo's owning daemon; otherwise bosso fans out
// across the caller's Ready daemons and concatenates the results. The
// optional filter pointers pass straight through so an unset field is not
// constrained.
func (c *RemoteClient) ListNotes(ctx context.Context, req *pb.ListNotesRequest) ([]*pb.Note, error) {
	resp, err := c.rpc.ProxyListNotes(ctx, connect.NewRequest(&pb.ProxyListNotesRequest{
		RepoId:    req.RepoId,
		SessionId: req.SessionId,
		ChatId:    req.ChatId,
		Tags:      req.Tags,
		Search:    req.Search,
		Limit:     req.GetLimit(),
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetNotes(), nil
}

// UpdateNote proxies a note mutation through the orchestrator, routing to the
// owning daemon by repoID. req.Tags is *pb.NoteTagSet and is passed through
// unmodified — nil means leave the tag set alone, a non-nil pointer (even one
// wrapping an empty list) replaces it wholesale; normalizing nil to an empty
// set here would silently turn "leave alone" into "clear all tags".
func (c *RemoteClient) UpdateNote(ctx context.Context, repoID string, req *pb.UpdateNoteRequest) (*pb.Note, error) {
	resp, err := c.rpc.ProxyUpdateNote(ctx, connect.NewRequest(&pb.ProxyUpdateNoteRequest{
		RepoId: repoID,
		Id:     req.GetId(),
		Body:   req.Body,
		Tags:   req.Tags,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetNote(), nil
}

// DeleteNote proxies a note delete through the orchestrator, routing to the
// owning daemon by repoID.
func (c *RemoteClient) DeleteNote(ctx context.Context, repoID, id string) error {
	_, err := c.rpc.ProxyDeleteNote(ctx, connect.NewRequest(&pb.ProxyDeleteNoteRequest{
		RepoId: repoID,
		Id:     id,
	}))
	return err
}

// --- Broadcasts (local only) ---

func (c *RemoteClient) SendBroadcast(_ context.Context, _ *pb.SendBroadcastRequest) (*pb.SendBroadcastResponse, error) {
	return nil, errBroadcastLocalOnly("SendBroadcast")
}

func (c *RemoteClient) ListBroadcasts(_ context.Context, _ *pb.ListBroadcastsRequest) ([]*pb.Broadcast, error) {
	return nil, errBroadcastLocalOnly("ListBroadcasts")
}

func (c *RemoteClient) DeleteBroadcast(_ context.Context, _ string) error {
	return errBroadcastLocalOnly("DeleteBroadcast")
}

func (c *RemoteClient) CreateBroadcastSubscription(_ context.Context, _ *pb.CreateBroadcastSubscriptionRequest) (*pb.BroadcastSubscription, error) {
	return nil, errBroadcastLocalOnly("CreateBroadcastSubscription")
}

func (c *RemoteClient) ListBroadcastSubscriptions(_ context.Context, _ *pb.ListBroadcastSubscriptionsRequest) ([]*pb.BroadcastSubscription, error) {
	return nil, errBroadcastLocalOnly("ListBroadcastSubscriptions")
}

func (c *RemoteClient) DeleteBroadcastSubscription(_ context.Context, _ string) error {
	return errBroadcastLocalOnly("DeleteBroadcastSubscription")
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

// GetAuthState is local-only by construction. The question it answers is "can
// THIS daemon reach the orchestrator", and a remote client's answer is already
// implied by the fact that it is talking to the orchestrator at all. There is
// no orchestrator proxy for it, and inventing one would report some other
// daemon's auth state under the local daemon's name.
func (c *RemoteClient) GetAuthState(_ context.Context) (*pb.GetAuthStateResponse, error) {
	return nil, errLocalOnly("daemon auth state")
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

func (c *RemoteClient) GetRunCost(_ context.Context, _ *pb.GetRunCostRequest) (*pb.GetRunCostResponse, error) {
	return nil, errLocalOnly("run cost telemetry")
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
