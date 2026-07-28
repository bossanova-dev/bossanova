package client

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/config"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/socketauth"
	"google.golang.org/protobuf/proto"
)

// DefaultSocketPath returns the default Unix socket path for the daemon.
// BOSS_SOCKET remains an explicit process-level override for tests and
// emergency debugging. Normal profile selection should use BOSS_SETTINGS_PATH
// plus socket_path/app_data_dir in settings.json.
func DefaultSocketPath() (string, error) {
	if p := os.Getenv("BOSS_SOCKET"); p != "" {
		return p, nil
	}

	settings, err := loadSettingsForSocketPath()
	if err != nil {
		return "", fmt.Errorf("load settings: %w", err)
	}
	if p, ok, err := config.ConfiguredSocketPath(settings); err != nil {
		return "", err
	} else if ok {
		return p, nil
	}

	dir, err := config.DefaultAppDataDir()
	if err != nil {
		return "", fmt.Errorf("resolve app data dir: %w", err)
	}
	return filepath.Join(dir, "bossd.sock"), nil
}

func loadSettingsForSocketPath() (config.Settings, error) {
	p, err := config.Path()
	if err != nil {
		return config.DefaultSettings(), err
	}
	return config.LoadFrom(p)
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

	// errMapInterceptor is outermost so it maps a CodeUnauthenticated from any
	// unary RPC (whichever one the TUI/CLI issues first) into a clear message.
	// The socket auth token (co-located with the socket) is attached inside it;
	// if the token file is missing or malformed we still build the client — the
	// RPC then fails with CodeUnauthenticated, which errMapInterceptor turns into
	// an actionable message.
	interceptors := []connect.Interceptor{errMapInterceptor{}}
	if token, err := socketauth.ReadToken(socketPath); err == nil {
		interceptors = append(interceptors, socketauth.NewClientInterceptor(token))
	}

	// The base URL host is ignored; the Unix socket dialer overrides it.
	rpc := bossanovav1connect.NewDaemonServiceClient(
		httpClient,
		"http://localhost",
		connect.WithInterceptors(interceptors...),
	)

	return &LocalClient{
		rpc:        rpc,
		socketPath: socketPath,
	}
}

// mapClientErr rewrites a daemon CodeUnauthenticated response — which means the
// socket auth token was missing or stale on this client — into an actionable
// message, instead of surfacing a raw "unauthenticated" to the TUI/CLI.
func mapClientErr(err error) error {
	if err != nil && connect.CodeOf(err) == connect.CodeUnauthenticated {
		return fmt.Errorf("daemon rejected this client (socket auth token missing or stale); restart the daemon and ensure boss is up to date: %w", err)
	}
	return err
}

// errMapInterceptor applies mapClientErr to every unary RPC's returned error, so
// whichever RPC the TUI/CLI issues first (ListSessions/ListRepos, not the rarely
// called Ping) surfaces a clear "restart the daemon / update boss" message on a
// missing or stale socket auth token rather than a raw CodeUnauthenticated. The
// streaming RPCs (CreateSession/AttachSession) map the same error in their stream
// wrappers' Err(), since a streaming client interceptor cannot see the open error.
type errMapInterceptor struct{}

func (errMapInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		resp, err := next(ctx, req)
		return resp, mapClientErr(err)
	}
}

func (errMapInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (errMapInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// Ping verifies the daemon is reachable. The unary error mapping (errMapInterceptor)
// turns a missing/stale-token CodeUnauthenticated into a clear error here too.
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

func (c *LocalClient) ListTrackerIssues(ctx context.Context, repoID, query, source string) ([]*pb.TrackerIssue, error) {
	req := &pb.ListTrackerIssuesRequest{RepoId: repoID, Query: query}
	if source != "" {
		req.Source = &source
	}
	resp, err := c.rpc.ListTrackerIssues(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Issues, nil
}

// --- Session Lifecycle ---

func (c *LocalClient) CreateSession(ctx context.Context, req *pb.CreateSessionRequest) (CreateSessionStream, error) {
	stream, err := c.rpc.CreateSession(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, mapClientErr(err)
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
	return mapClientErr(s.stream.Err())
}

func (s *localCreateSessionStream) Close() error {
	return s.stream.Close()
}

func (c *LocalClient) GetSession(ctx context.Context, id string, opts SessionReadOptions) (*pb.Session, error) {
	req := &pb.GetSessionRequest{Id: id}
	if opts.IncludeLocalHTTPEndpoints {
		req.IncludeLocalHttpEndpoints = true
		// On Linux this is the /proc/self/ns/net identity the daemon must match;
		// off Linux it is "" and the daemon treats the Unix-socket request as
		// local. An unreadable identity stays "" so the daemon fails closed.
		req.ClientNetworkNamespace = networkNamespaceIdentity()
	}
	resp, err := c.rpc.GetSession(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Session, nil
}

func (c *LocalClient) ListSessions(ctx context.Context, req *pb.ListSessionsRequest, opts SessionReadOptions) ([]*pb.Session, error) {
	if opts.IncludeLocalHTTPEndpoints {
		// The endpoint opt-in is per-call, so clone rather than mutate the
		// caller-owned request: a request object reused for a later default read
		// (zero-value SessionReadOptions) must never inherit the opt-in flag or the
		// stamped network-namespace identity.
		req = proto.CloneOf(req)
		req.IncludeLocalHttpEndpoints = true
		req.ClientNetworkNamespace = networkNamespaceIdentity()
	}
	resp, err := c.rpc.ListSessions(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Sessions, nil
}

func (c *LocalClient) AttachSession(ctx context.Context, id string) (AttachStream, error) {
	stream, err := c.rpc.AttachSession(ctx, connect.NewRequest(&pb.AttachSessionRequest{Id: id}))
	if err != nil {
		return nil, mapClientErr(err)
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

func (c *LocalClient) MergeSession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := c.rpc.MergeSession(ctx, connect.NewRequest(&pb.MergeSessionRequest{Id: id}))
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

func (c *LocalClient) LinkSessionPR(ctx context.Context, id, pr string) (*pb.Session, error) {
	resp, err := c.rpc.LinkSessionPR(ctx, connect.NewRequest(&pb.LinkSessionPRRequest{Id: id, Pr: pr}))
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

func (c *LocalClient) RecordChat(ctx context.Context, sessionID, agentSessionID, title, agentName string, resume bool) (*pb.ClaudeChat, error) {
	req := &pb.RecordChatRequest{
		SessionId:      sessionID,
		AgentSessionId: agentSessionID,
		Title:          title,
		Resume:         resume,
	}
	if agentName != "" {
		req.AgentName = &agentName
	}
	resp, err := c.rpc.RecordChat(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Chat, nil
}

func (c *LocalClient) DescribeChatLaunch(ctx context.Context, agentSessionID string) (*pb.DescribeChatLaunchResponse, error) {
	resp, err := c.rpc.DescribeChatLaunch(ctx, connect.NewRequest(&pb.DescribeChatLaunchRequest{
		AgentSessionId: agentSessionID,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
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

func (c *LocalClient) UpdateChatTitle(ctx context.Context, agentSessionID, title string) error {
	_, err := c.rpc.UpdateChatTitle(ctx, connect.NewRequest(&pb.UpdateChatTitleRequest{
		AgentSessionId: agentSessionID,
		Title:          title,
	}))
	return err
}

func (c *LocalClient) DeleteChat(ctx context.Context, agentSessionID string) error {
	_, err := c.rpc.DeleteChat(ctx, connect.NewRequest(&pb.DeleteChatRequest{
		AgentSessionId: agentSessionID,
	}))
	return err
}

// WakeChat asks the daemon to bring a stopped chat back online. The sessionID
// argument is part of the BossClient signature (used by RemoteClient for the
// orchestrator authz check) but ignored here since the local daemon scopes
// directly off agent_session_id.
func (c *LocalClient) WakeChat(ctx context.Context, _, agentSessionID string, forceFresh bool) (*pb.WakeChatResponse, error) {
	resp, err := c.rpc.WakeChat(ctx, connect.NewRequest(&pb.WakeChatRequest{
		AgentSessionId: agentSessionID,
		ForceFresh:     forceFresh,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// --- Chat Transcript and Messaging ---

func (c *LocalClient) GetChatTranscript(ctx context.Context, req *pb.GetChatTranscriptRequest) (*pb.GetChatTranscriptResponse, error) {
	resp, err := c.rpc.GetChatTranscript(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (c *LocalClient) SendChatMessage(ctx context.Context, req *pb.SendChatMessageRequest) (*pb.SendChatMessageResponse, error) {
	resp, err := c.rpc.SendChatMessage(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
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

// --- Auth Change Notification ---

func (c *LocalClient) NotifyAuthChange(ctx context.Context, action string) error {
	_, err := c.rpc.NotifyAuthChange(ctx, connect.NewRequest(&pb.NotifyAuthChangeRequest{
		Action: action,
	}))
	return err
}

// --- Cloud Billing (remote only) ---

func (c *LocalClient) GetCloudAccessStatus(_ context.Context) (*pb.CloudAccessStatus, error) {
	return nil, errLocalOnly("cloud billing")
}

func (c *LocalClient) CreateCheckoutSession(_ context.Context, _, _ string) (string, error) {
	return "", errLocalOnly("cloud billing")
}

func (c *LocalClient) CreateBillingPortalSession(_ context.Context, _ string) (string, error) {
	return "", errLocalOnly("cloud billing")
}

func (c *LocalClient) RefreshCloudEntitlements(_ context.Context) (*pb.CloudAccessStatus, error) {
	return nil, errLocalOnly("cloud billing")
}

// --- Cron Jobs ---

func (c *LocalClient) CreateCronJob(ctx context.Context, req *pb.CreateCronJobRequest) (*pb.CronJob, error) {
	resp, err := c.rpc.CreateCronJob(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.CronJob, nil
}

func (c *LocalClient) GetCronJob(ctx context.Context, id string) (*pb.CronJob, error) {
	resp, err := c.rpc.GetCronJob(ctx, connect.NewRequest(&pb.GetCronJobRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.CronJob, nil
}

func (c *LocalClient) ListCronJobs(ctx context.Context, repoID string) ([]*pb.CronJob, error) {
	req := &pb.ListCronJobsRequest{}
	if repoID != "" {
		req.RepoId = &repoID
	}
	resp, err := c.rpc.ListCronJobs(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.CronJobs, nil
}

func (c *LocalClient) UpdateCronJob(ctx context.Context, req *pb.UpdateCronJobRequest) (*pb.CronJob, error) {
	resp, err := c.rpc.UpdateCronJob(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.CronJob, nil
}

func (c *LocalClient) DeleteCronJob(ctx context.Context, id string) error {
	_, err := c.rpc.DeleteCronJob(ctx, connect.NewRequest(&pb.DeleteCronJobRequest{Id: id}))
	return err
}

func (c *LocalClient) RunCronJobNow(ctx context.Context, id string) (*pb.RunCronJobNowResponse, error) {
	resp, err := c.rpc.RunCronJobNow(ctx, connect.NewRequest(&pb.RunCronJobNowRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// --- GitHub Callbacks ---

func (c *LocalClient) CreateGithubCallback(ctx context.Context, req *pb.CreateGithubCallbackRequest) (*pb.GithubCallback, error) {
	resp, err := c.rpc.CreateGithubCallback(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetGithubCallback(), nil
}

func (c *LocalClient) ListGithubCallbacks(ctx context.Context, req *pb.ListGithubCallbacksRequest) ([]*pb.GithubCallback, error) {
	resp, err := c.rpc.ListGithubCallbacks(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetGithubCallbacks(), nil
}

// DeleteGithubCallback ignores targetChatID: the local daemon owns every
// callback in its own registry, so the id alone resolves it.
func (c *LocalClient) DeleteGithubCallback(ctx context.Context, _ string, id string) error {
	_, err := c.rpc.DeleteGithubCallback(ctx, connect.NewRequest(&pb.DeleteGithubCallbackRequest{Id: id}))
	return err
}

// --- Notes ---

func (c *LocalClient) CreateNote(ctx context.Context, req *pb.CreateNoteRequest) (*pb.Note, error) {
	resp, err := c.rpc.CreateNote(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetNote(), nil
}

// GetNote ignores repoID: the local daemon owns every note in its own
// registry, so the id alone resolves it.
func (c *LocalClient) GetNote(ctx context.Context, _ string, id string) (*pb.Note, error) {
	resp, err := c.rpc.GetNote(ctx, connect.NewRequest(&pb.GetNoteRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetNote(), nil
}

func (c *LocalClient) ListNotes(ctx context.Context, req *pb.ListNotesRequest) ([]*pb.Note, error) {
	resp, err := c.rpc.ListNotes(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetNotes(), nil
}

// UpdateNote ignores repoID: see GetNote. req.Tags is *pb.NoteTagSet and is
// passed through unmodified — nil means leave the tag set alone, a non-nil
// pointer (even one wrapping an empty list) replaces it wholesale.
func (c *LocalClient) UpdateNote(ctx context.Context, _ string, req *pb.UpdateNoteRequest) (*pb.Note, error) {
	resp, err := c.rpc.UpdateNote(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetNote(), nil
}

// DeleteNote ignores repoID: see GetNote.
func (c *LocalClient) DeleteNote(ctx context.Context, _ string, id string) error {
	_, err := c.rpc.DeleteNote(ctx, connect.NewRequest(&pb.DeleteNoteRequest{Id: id}))
	return err
}

// --- Broadcasts ---

func (c *LocalClient) SendBroadcast(ctx context.Context, req *pb.SendBroadcastRequest) (*pb.SendBroadcastResponse, error) {
	resp, err := c.rpc.SendBroadcast(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (c *LocalClient) ListBroadcasts(ctx context.Context, req *pb.ListBroadcastsRequest) ([]*pb.Broadcast, error) {
	resp, err := c.rpc.ListBroadcasts(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetBroadcasts(), nil
}

func (c *LocalClient) DeleteBroadcast(ctx context.Context, id string) error {
	_, err := c.rpc.DeleteBroadcast(ctx, connect.NewRequest(&pb.DeleteBroadcastRequest{Id: id}))
	return err
}

func (c *LocalClient) CreateBroadcastSubscription(ctx context.Context, req *pb.CreateBroadcastSubscriptionRequest) (*pb.BroadcastSubscription, error) {
	resp, err := c.rpc.CreateBroadcastSubscription(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSubscription(), nil
}

func (c *LocalClient) ListBroadcastSubscriptions(ctx context.Context, req *pb.ListBroadcastSubscriptionsRequest) ([]*pb.BroadcastSubscription, error) {
	resp, err := c.rpc.ListBroadcastSubscriptions(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSubscriptions(), nil
}

func (c *LocalClient) DeleteBroadcastSubscription(ctx context.Context, id string) error {
	_, err := c.rpc.DeleteBroadcastSubscription(ctx, connect.NewRequest(&pb.DeleteBroadcastSubscriptionRequest{Id: id}))
	return err
}

// --- Accounts ---

func (c *LocalClient) ListAccounts(ctx context.Context, provider string, refresh bool) ([]*pb.Account, error) {
	req := &pb.ListAccountsRequest{}
	if provider != "" {
		req.Provider = &provider
	}
	if refresh {
		req.Refresh = &refresh
	}
	resp, err := c.rpc.ListAccounts(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Accounts, nil
}

func (c *LocalClient) AddAccount(ctx context.Context, req *pb.AddAccountRequest) (*pb.Account, error) {
	resp, err := c.rpc.AddAccount(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Account, nil
}

func (c *LocalClient) RefreshAccount(ctx context.Context, req *pb.RefreshAccountRequest) (*pb.RefreshAccountResponse, error) {
	resp, err := c.rpc.RefreshAccount(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (c *LocalClient) UpdateAccount(ctx context.Context, req *pb.UpdateAccountRequest) (*pb.Account, error) {
	resp, err := c.rpc.UpdateAccount(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Account, nil
}

func (c *LocalClient) RemoveAccount(ctx context.Context, id string) error {
	_, err := c.rpc.RemoveAccount(ctx, connect.NewRequest(&pb.RemoveAccountRequest{Id: id}))
	return err
}

func (c *LocalClient) TestAccount(ctx context.Context, id string) (*pb.TestAccountResponse, error) {
	resp, err := c.rpc.TestAccount(ctx, connect.NewRequest(&pb.TestAccountRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (c *LocalClient) SwitchSessionAccount(ctx context.Context, req *pb.SwitchSessionAccountRequest) (*pb.SwitchSessionAccountResponse, error) {
	resp, err := c.rpc.SwitchSessionAccount(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (c *LocalClient) RepairDoctor(ctx context.Context) (*pb.RepairDoctorResponse, error) {
	resp, err := c.rpc.RepairDoctor(ctx, connect.NewRequest(&pb.RepairDoctorRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (c *LocalClient) StartRepairWorkflow(ctx context.Context) (*pb.StartRepairWorkflowResponse, error) {
	resp, err := c.rpc.StartRepairWorkflow(ctx, connect.NewRequest(&pb.StartRepairWorkflowRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (c *LocalClient) ListCheckSnapshots(ctx context.Context, sessionID string, limit int32) (*pb.ListCheckSnapshotsResponse, error) {
	resp, err := c.rpc.ListCheckSnapshots(ctx, connect.NewRequest(&pb.ListCheckSnapshotsRequest{
		SessionId: sessionID,
		Limit:     limit,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (c *LocalClient) ListAgents(ctx context.Context) ([]AgentInfo, error) {
	resp, err := c.rpc.ListAgents(ctx, connect.NewRequest(&pb.ListAgentsRequest{}))
	if err != nil {
		return nil, err
	}
	out := make([]AgentInfo, 0, len(resp.Msg.GetAgents()))
	for _, a := range resp.Msg.GetAgents() {
		out = append(out, agentInfoFromProto(a))
	}
	return out, nil
}

func (c *LocalClient) ListPlugins(ctx context.Context) ([]*pb.InstalledPlugin, error) {
	resp, err := c.rpc.ListPlugins(ctx, connect.NewRequest(&pb.ListPluginsRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetPlugins(), nil
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
	return mapClientErr(s.stream.Err())
}

func (s *localAttachStream) Close() error {
	return s.stream.Close()
}
