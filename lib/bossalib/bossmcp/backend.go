package bossmcp

import (
	"context"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// Backend is the narrow operation surface the MCP tools call. Both the local
// socket client and the hosted orchestrator adapter satisfy it structurally.
//
// Every method's signature mirrors the corresponding operation on the boss
// daemon client (services/boss/internal/client.BossClient) so the socket
// adapter is a thin pass-through, EXCEPT:
//
//   - CreateSession is reduced to a bounded call returning the terminal
//     *pb.Session: the adapter drains the daemon's setup stream and returns the
//     final session.
//   - ListAgents returns []*pb.AgentInfo (the shared proto type) rather than the
//     boss client's package-local AgentInfo, since this package may only import
//     the shared proto types; the adapter converts.
//
// Interactive AttachSession is intentionally omitted in v1.
type Backend interface {
	Ping(ctx context.Context) error
	ResolveContext(ctx context.Context, workingDir string) (*pb.ResolveContextResponse, error)

	// Repos
	ValidateRepoPath(ctx context.Context, localPath string) (*pb.ValidateRepoPathResponse, error)
	RegisterRepo(ctx context.Context, req *pb.RegisterRepoRequest) (*pb.Repo, error)
	CloneAndRegisterRepo(ctx context.Context, req *pb.CloneAndRegisterRepoRequest) (*pb.Repo, error)
	ListRepos(ctx context.Context) ([]*pb.Repo, error)
	RemoveRepo(ctx context.Context, id string) error
	UpdateRepo(ctx context.Context, req *pb.UpdateRepoRequest) (*pb.Repo, error)
	ListRepoPRs(ctx context.Context, repoID string) ([]*pb.PRSummary, error)
	ListTrackerIssues(ctx context.Context, repoID, query, source string) ([]*pb.TrackerIssue, error)

	// Sessions (CreateSession drains the setup stream and returns the final Session)
	CreateSession(ctx context.Context, req *pb.CreateSessionRequest) (*pb.Session, error)
	GetSession(ctx context.Context, id string) (*pb.Session, error)
	ListSessions(ctx context.Context, req *pb.ListSessionsRequest) ([]*pb.Session, error)
	StopSession(ctx context.Context, id string) (*pb.Session, error)
	PauseSession(ctx context.Context, id string) (*pb.Session, error)
	ResumeSession(ctx context.Context, id string) (*pb.Session, error)
	RetrySession(ctx context.Context, id string) (*pb.Session, error)
	CloseSession(ctx context.Context, id string) (*pb.Session, error)
	MergeSession(ctx context.Context, id string) (*pb.Session, error)
	RemoveSession(ctx context.Context, id string) error
	UpdateSession(ctx context.Context, req *pb.UpdateSessionRequest) (*pb.Session, error)
	LinkSessionPR(ctx context.Context, id, pr string) (*pb.Session, error)
	ArchiveSession(ctx context.Context, id string) (*pb.Session, error)
	ResurrectSession(ctx context.Context, id string) (*pb.Session, error)
	EmptyTrash(ctx context.Context, req *pb.EmptyTrashRequest) (int32, error)

	// Chats (agent chats; positional RecordChat / scalar-return mutators mirror
	// the boss client surface exactly).
	RecordChat(ctx context.Context, sessionID, agentSessionID, title, agentName string, resume bool) (*pb.ClaudeChat, error)
	ListChats(ctx context.Context, sessionID string) ([]*pb.ClaudeChat, error)
	UpdateChatTitle(ctx context.Context, agentSessionID, title string) error
	DeleteChat(ctx context.Context, agentSessionID string) error
	WakeChat(ctx context.Context, sessionID, agentSessionID string, forceFresh bool) (*pb.WakeChatResponse, error)
	ReportChatStatus(ctx context.Context, statuses []*pb.ChatStatusReport) error
	GetChatStatuses(ctx context.Context, sessionID string) ([]*pb.ChatStatusEntry, error)
	GetSessionStatuses(ctx context.Context, sessionIDs []string) ([]*pb.SessionStatusEntry, error)

	// Cron
	CreateCronJob(ctx context.Context, req *pb.CreateCronJobRequest) (*pb.CronJob, error)
	ListCronJobs(ctx context.Context) ([]*pb.CronJob, error)
	GetCronJob(ctx context.Context, id string) (*pb.CronJob, error)
	UpdateCronJob(ctx context.Context, req *pb.UpdateCronJobRequest) (*pb.CronJob, error)
	DeleteCronJob(ctx context.Context, id string) error
	RunCronJobNow(ctx context.Context, id string) (*pb.RunCronJobNowResponse, error)

	// Diagnostics
	ListCheckSnapshots(ctx context.Context, sessionID string, limit int32) (*pb.ListCheckSnapshotsResponse, error)
	RepairDoctor(ctx context.Context) (*pb.RepairDoctorResponse, error)
	ListAgents(ctx context.Context) ([]*pb.AgentInfo, error)
	ListPlugins(ctx context.Context) ([]*pb.InstalledPlugin, error)
}

// Options tunes tool registration per deployment.
type Options struct {
	// ReadOnly omits every non-read-only tool from registration.
	ReadOnly bool
}
