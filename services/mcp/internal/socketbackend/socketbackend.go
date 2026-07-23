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
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/bossmcp"
	"github.com/recurser/bossalib/config"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/socketauth"
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
}

// Verify Backend satisfies the bossmcp interface at compile time.
var _ bossmcp.Backend = (*Backend)(nil)

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
	return &Backend{rpc: rpc, socketPath: socketPath}, nil
}

// DefaultSocketPath resolves the configured daemon socket path from settings,
// falling back to the default app-data location when none is configured.
func DefaultSocketPath() (string, error) {
	settings, err := loadSettings()
	if err != nil {
		return "", err
	}
	if p, ok, err := config.ConfiguredSocketPath(settings); err != nil {
		return "", err
	} else if ok {
		return p, nil
	}

	dir, err := config.DefaultAppDataDir()
	if err != nil {
		return "", err
	}
	return dir + "/bossd.sock", nil
}

func loadSettings() (config.Settings, error) {
	p, err := config.Path()
	if err != nil {
		return config.DefaultSettings(), err
	}
	return config.LoadFrom(p)
}

// SocketPath returns the path this backend dials.
func (b *Backend) SocketPath() string { return b.socketPath }

func (b *Backend) Ping(ctx context.Context) error {
	_, err := b.rpc.ListRepos(ctx, connect.NewRequest(&pb.ListReposRequest{}))
	return err
}

func (b *Backend) ResolveContext(ctx context.Context, workingDir string) (*pb.ResolveContextResponse, error) {
	resp, err := b.rpc.ResolveContext(ctx, connect.NewRequest(&pb.ResolveContextRequest{WorkingDirectory: workingDir}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// --- Repos ---

func (b *Backend) ValidateRepoPath(ctx context.Context, localPath string) (*pb.ValidateRepoPathResponse, error) {
	resp, err := b.rpc.ValidateRepoPath(ctx, connect.NewRequest(&pb.ValidateRepoPathRequest{LocalPath: localPath}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (b *Backend) RegisterRepo(ctx context.Context, req *pb.RegisterRepoRequest) (*pb.Repo, error) {
	resp, err := b.rpc.RegisterRepo(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetRepo(), nil
}

func (b *Backend) CloneAndRegisterRepo(ctx context.Context, req *pb.CloneAndRegisterRepoRequest) (*pb.Repo, error) {
	resp, err := b.rpc.CloneAndRegisterRepo(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetRepo(), nil
}

func (b *Backend) ListRepos(ctx context.Context) ([]*pb.Repo, error) {
	resp, err := b.rpc.ListRepos(ctx, connect.NewRequest(&pb.ListReposRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetRepos(), nil
}

func (b *Backend) RemoveRepo(ctx context.Context, id string) error {
	_, err := b.rpc.RemoveRepo(ctx, connect.NewRequest(&pb.RemoveRepoRequest{Id: id}))
	return err
}

func (b *Backend) UpdateRepo(ctx context.Context, req *pb.UpdateRepoRequest) (*pb.Repo, error) {
	resp, err := b.rpc.UpdateRepo(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetRepo(), nil
}

func (b *Backend) ListRepoPRs(ctx context.Context, repoID string) ([]*pb.PRSummary, error) {
	resp, err := b.rpc.ListRepoPRs(ctx, connect.NewRequest(&pb.ListRepoPRsRequest{RepoId: repoID}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetPullRequests(), nil
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

// --- Sessions ---

// CreateSession opens the daemon's setup stream and drains it to the terminal
// SessionCreated event, returning the final Session together with whether the
// daemon attached to an existing session (captured from the same frame).
func (b *Backend) CreateSession(ctx context.Context, req *pb.CreateSessionRequest) (*bossmcp.CreateSessionResult, error) {
	stream, err := b.rpc.CreateSession(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.Close() }()

	var result bossmcp.CreateSessionResult
	for stream.Receive() {
		if created := stream.Msg().GetSessionCreated(); created != nil {
			result.Session = created.GetSession()
			result.AttachedExisting = created.GetAttachedExisting()
		}
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	return &result, nil
}

func (b *Backend) GetSession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := b.rpc.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSession(), nil
}

func (b *Backend) ListSessions(ctx context.Context, req *pb.ListSessionsRequest) ([]*pb.Session, error) {
	resp, err := b.rpc.ListSessions(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSessions(), nil
}

func (b *Backend) StopSession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := b.rpc.StopSession(ctx, connect.NewRequest(&pb.StopSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSession(), nil
}

func (b *Backend) PauseSession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := b.rpc.PauseSession(ctx, connect.NewRequest(&pb.PauseSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSession(), nil
}

func (b *Backend) ResumeSession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := b.rpc.ResumeSession(ctx, connect.NewRequest(&pb.ResumeSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSession(), nil
}

func (b *Backend) RetrySession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := b.rpc.RetrySession(ctx, connect.NewRequest(&pb.RetrySessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSession(), nil
}

func (b *Backend) CloseSession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := b.rpc.CloseSession(ctx, connect.NewRequest(&pb.CloseSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSession(), nil
}

func (b *Backend) MergeSession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := b.rpc.MergeSession(ctx, connect.NewRequest(&pb.MergeSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSession(), nil
}

func (b *Backend) RemoveSession(ctx context.Context, id string) error {
	_, err := b.rpc.RemoveSession(ctx, connect.NewRequest(&pb.RemoveSessionRequest{Id: id}))
	return err
}

func (b *Backend) UpdateSession(ctx context.Context, req *pb.UpdateSessionRequest) (*pb.Session, error) {
	resp, err := b.rpc.UpdateSession(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSession(), nil
}

func (b *Backend) LinkSessionPR(ctx context.Context, id, pr string) (*pb.Session, error) {
	resp, err := b.rpc.LinkSessionPR(ctx, connect.NewRequest(&pb.LinkSessionPRRequest{Id: id, Pr: pr}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSession(), nil
}

func (b *Backend) ArchiveSession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := b.rpc.ArchiveSession(ctx, connect.NewRequest(&pb.ArchiveSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSession(), nil
}

func (b *Backend) ResurrectSession(ctx context.Context, id string) (*pb.Session, error) {
	resp, err := b.rpc.ResurrectSession(ctx, connect.NewRequest(&pb.ResurrectSessionRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSession(), nil
}

func (b *Backend) EmptyTrash(ctx context.Context, req *pb.EmptyTrashRequest) (int32, error) {
	resp, err := b.rpc.EmptyTrash(ctx, connect.NewRequest(req))
	if err != nil {
		return 0, err
	}
	return resp.Msg.GetDeletedCount(), nil
}

// --- Chats ---

func (b *Backend) RecordChat(ctx context.Context, sessionID, agentSessionID, title, agentName string, resume bool) (*pb.ClaudeChat, error) {
	req := &pb.RecordChatRequest{
		SessionId:      sessionID,
		AgentSessionId: agentSessionID,
		Title:          title,
		Resume:         resume,
	}
	if agentName != "" {
		req.AgentName = &agentName
	}
	resp, err := b.rpc.RecordChat(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetChat(), nil
}

func (b *Backend) ListChats(ctx context.Context, sessionID string) ([]*pb.ClaudeChat, error) {
	resp, err := b.rpc.ListChats(ctx, connect.NewRequest(&pb.ListChatsRequest{SessionId: sessionID}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetChats(), nil
}

func (b *Backend) UpdateChatTitle(ctx context.Context, agentSessionID, title string) error {
	_, err := b.rpc.UpdateChatTitle(ctx, connect.NewRequest(&pb.UpdateChatTitleRequest{
		AgentSessionId: agentSessionID,
		Title:          title,
	}))
	return err
}

func (b *Backend) DeleteChat(ctx context.Context, agentSessionID string) error {
	_, err := b.rpc.DeleteChat(ctx, connect.NewRequest(&pb.DeleteChatRequest{AgentSessionId: agentSessionID}))
	return err
}

func (b *Backend) WakeChat(ctx context.Context, _, agentSessionID string, forceFresh bool) (*pb.WakeChatResponse, error) {
	resp, err := b.rpc.WakeChat(ctx, connect.NewRequest(&pb.WakeChatRequest{
		AgentSessionId: agentSessionID,
		ForceFresh:     forceFresh,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (b *Backend) ReportChatStatus(ctx context.Context, statuses []*pb.ChatStatusReport) error {
	_, err := b.rpc.ReportChatStatus(ctx, connect.NewRequest(&pb.ReportChatStatusRequest{Reports: statuses}))
	return err
}

func (b *Backend) GetChatStatuses(ctx context.Context, sessionID string) ([]*pb.ChatStatusEntry, error) {
	resp, err := b.rpc.GetChatStatuses(ctx, connect.NewRequest(&pb.GetChatStatusesRequest{SessionId: sessionID}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetStatuses(), nil
}

func (b *Backend) GetSessionStatuses(ctx context.Context, sessionIDs []string) ([]*pb.SessionStatusEntry, error) {
	resp, err := b.rpc.GetSessionStatuses(ctx, connect.NewRequest(&pb.GetSessionStatusesRequest{SessionIds: sessionIDs}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetStatuses(), nil
}

func (b *Backend) GetChatTranscript(ctx context.Context, req *pb.GetChatTranscriptRequest) (*pb.GetChatTranscriptResponse, error) {
	resp, err := b.rpc.GetChatTranscript(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (b *Backend) SendChatMessage(ctx context.Context, req *pb.SendChatMessageRequest) (*pb.SendChatMessageResponse, error) {
	resp, err := b.rpc.SendChatMessage(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// --- Cron ---

func (b *Backend) CreateCronJob(ctx context.Context, req *pb.CreateCronJobRequest) (*pb.CronJob, error) {
	resp, err := b.rpc.CreateCronJob(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetCronJob(), nil
}

func (b *Backend) ListCronJobs(ctx context.Context) ([]*pb.CronJob, error) {
	resp, err := b.rpc.ListCronJobs(ctx, connect.NewRequest(&pb.ListCronJobsRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetCronJobs(), nil
}

// GetCronJob is not on the boss client interface, but the DaemonService RPC
// exists, so the adapter calls it directly.
func (b *Backend) GetCronJob(ctx context.Context, id string) (*pb.CronJob, error) {
	resp, err := b.rpc.GetCronJob(ctx, connect.NewRequest(&pb.GetCronJobRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetCronJob(), nil
}

func (b *Backend) UpdateCronJob(ctx context.Context, req *pb.UpdateCronJobRequest) (*pb.CronJob, error) {
	resp, err := b.rpc.UpdateCronJob(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetCronJob(), nil
}

func (b *Backend) DeleteCronJob(ctx context.Context, id string) error {
	_, err := b.rpc.DeleteCronJob(ctx, connect.NewRequest(&pb.DeleteCronJobRequest{Id: id}))
	return err
}

func (b *Backend) RunCronJobNow(ctx context.Context, id string) (*pb.RunCronJobNowResponse, error) {
	resp, err := b.rpc.RunCronJobNow(ctx, connect.NewRequest(&pb.RunCronJobNowRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// --- GitHub callbacks ---

func (b *Backend) CreateGithubCallback(ctx context.Context, req *pb.CreateGithubCallbackRequest) (*pb.GithubCallback, error) {
	resp, err := b.rpc.CreateGithubCallback(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetGithubCallback(), nil
}

func (b *Backend) ListGithubCallbacks(ctx context.Context, req *pb.ListGithubCallbacksRequest) ([]*pb.GithubCallback, error) {
	resp, err := b.rpc.ListGithubCallbacks(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetGithubCallbacks(), nil
}

// DeleteGithubCallback ignores targetChatID: this adapter's own daemon owns
// every callback in its registry, so the id alone resolves it. The parameter
// exists for the hosted gateway, which uses it to route the delete.
func (b *Backend) DeleteGithubCallback(ctx context.Context, _ string, id string) error {
	_, err := b.rpc.DeleteGithubCallback(ctx, connect.NewRequest(&pb.DeleteGithubCallbackRequest{Id: id}))
	return err
}

// --- Accounts ---

func (b *Backend) ListAccounts(ctx context.Context, provider string, refresh bool) ([]*pb.Account, error) {
	req := &pb.ListAccountsRequest{}
	if provider != "" {
		req.Provider = &provider
	}
	if refresh {
		req.Refresh = &refresh
	}
	resp, err := b.rpc.ListAccounts(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetAccounts(), nil
}

func (b *Backend) AddAccount(ctx context.Context, req *pb.AddAccountRequest) (*pb.Account, error) {
	resp, err := b.rpc.AddAccount(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetAccount(), nil
}

func (b *Backend) UpdateAccount(ctx context.Context, req *pb.UpdateAccountRequest) (*pb.Account, error) {
	resp, err := b.rpc.UpdateAccount(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetAccount(), nil
}

func (b *Backend) RefreshAccount(ctx context.Context, req *pb.RefreshAccountRequest) (*pb.RefreshAccountResponse, error) {
	resp, err := b.rpc.RefreshAccount(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (b *Backend) RemoveAccount(ctx context.Context, id string) error {
	_, err := b.rpc.RemoveAccount(ctx, connect.NewRequest(&pb.RemoveAccountRequest{Id: id}))
	return err
}

func (b *Backend) TestAccount(ctx context.Context, id string) (*pb.TestAccountResponse, error) {
	resp, err := b.rpc.TestAccount(ctx, connect.NewRequest(&pb.TestAccountRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (b *Backend) SwitchSessionAccount(ctx context.Context, req *pb.SwitchSessionAccountRequest) (*pb.SwitchSessionAccountResponse, error) {
	resp, err := b.rpc.SwitchSessionAccount(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// --- Diagnostics ---

func (b *Backend) ListCheckSnapshots(ctx context.Context, sessionID string, limit int32) (*pb.ListCheckSnapshotsResponse, error) {
	resp, err := b.rpc.ListCheckSnapshots(ctx, connect.NewRequest(&pb.ListCheckSnapshotsRequest{
		SessionId: sessionID,
		Limit:     limit,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (b *Backend) RepairDoctor(ctx context.Context) (*pb.RepairDoctorResponse, error) {
	resp, err := b.rpc.RepairDoctor(ctx, connect.NewRequest(&pb.RepairDoctorRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (b *Backend) StartRepairWorkflow(ctx context.Context) (*pb.StartRepairWorkflowResponse, error) {
	resp, err := b.rpc.StartRepairWorkflow(ctx, connect.NewRequest(&pb.StartRepairWorkflowRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (b *Backend) ListAgents(ctx context.Context) ([]*pb.AgentInfo, error) {
	resp, err := b.rpc.ListAgents(ctx, connect.NewRequest(&pb.ListAgentsRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetAgents(), nil
}

func (b *Backend) ListPlugins(ctx context.Context) ([]*pb.InstalledPlugin, error) {
	resp, err := b.rpc.ListPlugins(ctx, connect.NewRequest(&pb.ListPluginsRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetPlugins(), nil
}

// --- Global Settings ---

func (b *Backend) GetSettings(ctx context.Context) (*pb.GlobalSettings, error) {
	resp, err := b.rpc.GetSettings(ctx, connect.NewRequest(&pb.GetSettingsRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSettings(), nil
}

func (b *Backend) UpdateSettings(ctx context.Context, req *pb.UpdateSettingsRequest) (*pb.GlobalSettings, error) {
	resp, err := b.rpc.UpdateSettings(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSettings(), nil
}
