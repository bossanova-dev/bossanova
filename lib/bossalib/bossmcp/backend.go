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
//   - CreateSession is reduced to a bounded call returning a
//     *CreateSessionResult: the adapter drains the daemon's setup stream and
//     returns the final session together with AttachedExisting, captured from
//     the terminal SessionCreated frame (true when the daemon attached to an
//     active session that already owned the target branch/PR/tracker rather than
//     creating a new one — in which case the supplied prompt was NOT run).
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

	// Sessions (CreateSession drains the setup stream and returns the final
	// Session plus whether the daemon attached to an existing session)
	CreateSession(ctx context.Context, req *pb.CreateSessionRequest) (*CreateSessionResult, error)
	GetSession(ctx context.Context, id string) (*pb.Session, error)
	ListSessions(ctx context.Context, req *pb.ListSessionsRequest) ([]*pb.Session, error)
	StopSession(ctx context.Context, id string) (*pb.Session, error)
	PauseSession(ctx context.Context, id string) (*pb.Session, error)
	ResumeSession(ctx context.Context, id string) (*pb.Session, error)
	RetrySession(ctx context.Context, id string) (*pb.Session, error)
	CloseSession(ctx context.Context, id string) (*pb.Session, error)
	// MergeSession returns the merged session plus the daemon's detail note —
	// most importantly a merge-strategy substitution (a rebase that was refused
	// and squashed instead). The shape mirrors client.BossClient.MergeSession
	// deliberately: neither surface should silently narrow the other.
	MergeSession(ctx context.Context, id string) (*pb.Session, string, error)
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
	GetChatTranscript(ctx context.Context, req *pb.GetChatTranscriptRequest) (*pb.GetChatTranscriptResponse, error)
	SendChatMessage(ctx context.Context, req *pb.SendChatMessageRequest) (*pb.SendChatMessageResponse, error)

	// Cron
	CreateCronJob(ctx context.Context, req *pb.CreateCronJobRequest) (*pb.CronJob, error)
	ListCronJobs(ctx context.Context) ([]*pb.CronJob, error)
	GetCronJob(ctx context.Context, id string) (*pb.CronJob, error)
	UpdateCronJob(ctx context.Context, req *pb.UpdateCronJobRequest) (*pb.CronJob, error)
	DeleteCronJob(ctx context.Context, id string) error
	RunCronJobNow(ctx context.Context, id string) (*pb.RunCronJobNowResponse, error)

	// GitHub callbacks (deferred one-shot delivery when a PR reaches a trigger
	// state). DeleteGithubCallback takes the owning target chat id alongside the
	// callback id so the hosted gateway can route the delete to the daemon that
	// owns the callback; the local socket adapter ignores it (its own daemon owns
	// every callback in its registry, so the id alone resolves it).
	CreateGithubCallback(ctx context.Context, req *pb.CreateGithubCallbackRequest) (*pb.GithubCallback, error)
	ListGithubCallbacks(ctx context.Context, req *pb.ListGithubCallbacksRequest) ([]*pb.GithubCallback, error)
	DeleteGithubCallback(ctx context.Context, targetChatID, id string) error

	// Notes (durable free-text a run records against a repository so a later
	// sweep can harvest what was learned). A note is repo-scoped; its session
	// and chat are PROVENANCE only, so deleting or archiving a session never
	// removes its notes.
	//
	// CreateNoteRequest and ListNotesRequest already carry repo_id, so those two
	// pass their request through unchanged. GetNote, UpdateNote and DeleteNote
	// address the note by id alone on the daemon, but take repoID alongside it
	// so the hosted gateway can route the call to the daemon that owns the note;
	// the local socket adapter ignores it (its own daemon owns every note in its
	// store, so the id alone resolves it) — exactly as DeleteGithubCallback
	// takes targetChatID.
	//
	// A note's body is NOT a secret. Unlike GithubCallback.message and
	// Broadcast.message — which are scrubbed at the tool layer — the body is the
	// payload the caller asked for and is returned UNREDACTED on every read
	// surface. There is no redactNote, and its absence is deliberate: do not
	// "restore" one.
	CreateNote(ctx context.Context, req *pb.CreateNoteRequest) (*pb.Note, error)
	GetNote(ctx context.Context, repoID, id string) (*pb.Note, error)
	ListNotes(ctx context.Context, req *pb.ListNotesRequest) ([]*pb.Note, error)
	UpdateNote(ctx context.Context, repoID string, req *pb.UpdateNoteRequest) (*pb.Note, error)
	DeleteNote(ctx context.Context, repoID, id string) error

	// Broadcasts (one message addressed at the set of chats a selector resolves
	// to, plus the standing subscriptions that fire one when a session settles).
	// SendBroadcast and CreateBroadcastSubscription carry a SECRET message body
	// inbound; nothing here ever returns it — pb.Broadcast.message is scrubbed at
	// the tool layer (redactBroadcast) and pb.BroadcastSubscription deliberately
	// has no body field at all. Both deletes are idempotent: an absent id
	// succeeds.
	SendBroadcast(ctx context.Context, req *pb.SendBroadcastRequest) (*pb.SendBroadcastResponse, error)
	ListBroadcasts(ctx context.Context, req *pb.ListBroadcastsRequest) ([]*pb.Broadcast, error)
	DeleteBroadcast(ctx context.Context, id string) error
	CreateBroadcastSubscription(ctx context.Context, req *pb.CreateBroadcastSubscriptionRequest) (*pb.BroadcastSubscription, error)
	ListBroadcastSubscriptions(ctx context.Context, req *pb.ListBroadcastSubscriptionsRequest) ([]*pb.BroadcastSubscription, error)
	DeleteBroadcastSubscription(ctx context.Context, id string) error

	// Accounts (agent credential registry). AddAccount consumes an inbound
	// credential blob straight into the keyring; no method ever returns it.
	ListAccounts(ctx context.Context, provider string, refresh bool) ([]*pb.Account, error)
	AddAccount(ctx context.Context, req *pb.AddAccountRequest) (*pb.Account, error)
	RefreshAccount(ctx context.Context, req *pb.RefreshAccountRequest) (*pb.RefreshAccountResponse, error)
	UpdateAccount(ctx context.Context, req *pb.UpdateAccountRequest) (*pb.Account, error)
	RemoveAccount(ctx context.Context, id string) error
	TestAccount(ctx context.Context, id string) (*pb.TestAccountResponse, error)
	SwitchSessionAccount(ctx context.Context, req *pb.SwitchSessionAccountRequest) (*pb.SwitchSessionAccountResponse, error)

	// Diagnostics
	ListCheckSnapshots(ctx context.Context, sessionID string, limit int32) (*pb.ListCheckSnapshotsResponse, error)
	RepairDoctor(ctx context.Context) (*pb.RepairDoctorResponse, error)
	StartRepairWorkflow(ctx context.Context) (*pb.StartRepairWorkflowResponse, error)
	ListAgents(ctx context.Context) ([]*pb.AgentInfo, error)
	ListPlugins(ctx context.Context) ([]*pb.InstalledPlugin, error)

	// Global settings (the TUI-editable subset of settings.json). GetSettings
	// returns the current values; UpdateSettings applies a partial update and
	// returns the post-write values.
	GetSettings(ctx context.Context) (*pb.GlobalSettings, error)
	UpdateSettings(ctx context.Context, req *pb.UpdateSettingsRequest) (*pb.GlobalSettings, error)
}

// CreateSessionResult is the bounded outcome of Backend.CreateSession. Session
// is the terminal session drained from the setup stream. AttachedExisting is
// captured from the final SessionCreated frame: it is true when the daemon
// attached to an active session that already owned the target branch/PR/tracker
// instead of creating a new one — in that case the caller's prompt was NOT run.
type CreateSessionResult struct {
	Session          *pb.Session
	AttachedExisting bool
}

// Options tunes tool registration per deployment.
type Options struct {
	// ReadOnly omits every non-read-only tool from registration.
	ReadOnly bool

	// Only, when non-nil, restricts registration to the named tools (by their
	// MCP tool name, e.g. "list_sessions"). A nil map registers every tool;
	// an empty map registers none. Hosted deployments that can back only a
	// subset of the Backend (e.g. the orchestrator gateway, which proxies a
	// subset of operations) pass the names they support so tools/list
	// advertises exactly the tools that work. Only composes with ReadOnly:
	// both filters must admit a tool for it to register.
	Only map[string]bool
}
