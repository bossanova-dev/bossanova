// Package socketbackend supplies a bossmcp.Backend backed by the local bossd
// daemon over its Unix socket. It speaks Connect RPC to bossanova.v1.DaemonService
// exactly like the boss CLI's local client, so the MCP tools see the same
// behaviour the CLI would.
package socketbackend

import (
	"context"
	"net"
	"net/http"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/bossmcp"
	"github.com/recurser/bossalib/config"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

// Backend implements bossmcp.Backend over a Unix-socket Connect client.
type Backend struct {
	rpc        bossanovav1connect.DaemonServiceClient
	socketPath string
}

// Verify Backend satisfies the bossmcp interface at compile time.
var _ bossmcp.Backend = (*Backend)(nil)

// New connects to the bossd daemon at socketPath. The base URL host is ignored
// because the Unix-socket dialer overrides it.
func New(socketPath string) (*Backend, error) {
	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
	}

	rpc := bossanovav1connect.NewDaemonServiceClient(httpClient, "http://localhost")
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
// SessionCreated event, returning the final Session.
func (b *Backend) CreateSession(ctx context.Context, req *pb.CreateSessionRequest) (*pb.Session, error) {
	stream, err := b.rpc.CreateSession(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.Close() }()

	var session *pb.Session
	for stream.Receive() {
		if created := stream.Msg().GetSessionCreated(); created != nil {
			session = created.GetSession()
		}
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	return session, nil
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
