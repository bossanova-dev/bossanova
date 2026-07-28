// Package client provides interfaces and implementations for communicating
// with the bossanova daemon, both locally (Unix socket) and remotely (orchestrator).
package client

import (
	"context"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// AttachStream abstracts a server-streaming attach response, allowing both
// local (DaemonService) and remote (OrchestratorService) implementations.
type AttachStream interface {
	// Receive advances the stream. Returns false when done or on error.
	Receive() bool
	// Msg returns the most recent message from the stream.
	Msg() *AttachEvent
	// Err returns the stream error, if any.
	Err() error
	// Close closes the stream.
	Close() error
}

// AttachEvent is a unified attach event for both local and remote streams.
type AttachEvent struct {
	OutputLine   *pb.OutputLine
	StateChange  *pb.StateChange
	SessionEnded *pb.SessionEnded
}

// BossClient defines the interface for all daemon operations.
// Both LocalClient (Unix socket) and RemoteClient (orchestrator proxy) implement this.
type BossClient interface {
	// Ping verifies the daemon is reachable.
	Ping(ctx context.Context) error

	// Context resolution
	ResolveContext(ctx context.Context, workingDir string) (*pb.ResolveContextResponse, error)

	// Repo management
	ValidateRepoPath(ctx context.Context, localPath string) (*pb.ValidateRepoPathResponse, error)
	RegisterRepo(ctx context.Context, req *pb.RegisterRepoRequest) (*pb.Repo, error)
	CloneAndRegisterRepo(ctx context.Context, req *pb.CloneAndRegisterRepoRequest) (*pb.Repo, error)
	ListRepos(ctx context.Context) ([]*pb.Repo, error)
	RemoveRepo(ctx context.Context, id string) error
	UpdateRepo(ctx context.Context, req *pb.UpdateRepoRequest) (*pb.Repo, error)
	ListRepoPRs(ctx context.Context, repoID string) ([]*pb.PRSummary, error)
	ListTrackerIssues(ctx context.Context, repoID, query, source string) ([]*pb.TrackerIssue, error)

	// Session lifecycle
	CreateSession(ctx context.Context, req *pb.CreateSessionRequest) (CreateSessionStream, error)
	// GetSession reads one session. opts.IncludeLocalHTTPEndpoints gates
	// machine-local endpoint hydration (LocalClient only); the zero value is a
	// plain read. See SessionReadOptions.
	GetSession(ctx context.Context, id string, opts SessionReadOptions) (*pb.Session, error)
	// ListSessions reads sessions. opts.IncludeLocalHTTPEndpoints gates
	// machine-local endpoint hydration (LocalClient only); the zero value is a
	// plain read. See SessionReadOptions.
	ListSessions(ctx context.Context, req *pb.ListSessionsRequest, opts SessionReadOptions) ([]*pb.Session, error)
	AttachSession(ctx context.Context, id string) (AttachStream, error)
	StopSession(ctx context.Context, id string) (*pb.Session, error)
	PauseSession(ctx context.Context, id string) (*pb.Session, error)
	ResumeSession(ctx context.Context, id string) (*pb.Session, error)
	RetrySession(ctx context.Context, id string) (*pb.Session, error)
	CloseSession(ctx context.Context, id string) (*pb.Session, error)
	MergeSession(ctx context.Context, id string) (*pb.Session, error)
	RemoveSession(ctx context.Context, id string) error
	UpdateSession(ctx context.Context, req *pb.UpdateSessionRequest) (*pb.Session, error)
	LinkSessionPR(ctx context.Context, id, pr string) (*pb.Session, error)
	// SwitchSessionAccount stops the session's live chat, rebinds it to the
	// chosen rotation account, and brings the chat back up under it. Local-daemon
	// only, like the account registry RPCs — the orchestrator does not host tmux
	// panes to swap.
	SwitchSessionAccount(ctx context.Context, req *pb.SwitchSessionAccountRequest) (*pb.SwitchSessionAccountResponse, error)

	// Archive / Resurrect
	ArchiveSession(ctx context.Context, id string) (*pb.Session, error)
	ResurrectSession(ctx context.Context, id string) (*pb.Session, error)
	EmptyTrash(ctx context.Context, req *pb.EmptyTrashRequest) (int32, error)

	// Claude chat tracking
	RecordChat(ctx context.Context, sessionID, agentSessionID, title, agentName string, resume bool) (*pb.ClaudeChat, error)
	ListChats(ctx context.Context, sessionID string) ([]*pb.ClaudeChat, error)
	UpdateChatTitle(ctx context.Context, agentSessionID, title string) error
	DeleteChat(ctx context.Context, agentSessionID string) error
	// WakeChat asks the daemon to bring a stopped chat back online. sessionID
	// is required for the remote-orchestrator authz check; LocalClient ignores
	// it. The returned outcome lets the UI distinguish ALREADY_LIVE / RESUMED /
	// FRESH_FALLBACK so it can render the right confirmation.
	WakeChat(ctx context.Context, sessionID, agentSessionID string, forceFresh bool) (*pb.WakeChatResponse, error)

	// DescribeChatLaunch returns the exact command bossd launches a chat's agent
	// with (already login-shell wrapped), its worktree, and the daemon host, so
	// the attach UI can show a reproduction command when a pane dies on startup.
	// Local-only: the remote orchestrator does not host tmux panes.
	DescribeChatLaunch(ctx context.Context, agentSessionID string) (*pb.DescribeChatLaunchResponse, error)

	// Chat transcript and messaging
	GetChatTranscript(ctx context.Context, req *pb.GetChatTranscriptRequest) (*pb.GetChatTranscriptResponse, error)
	SendChatMessage(ctx context.Context, req *pb.SendChatMessageRequest) (*pb.SendChatMessageResponse, error)

	// Chat status (cross-client heartbeat sharing)
	ReportChatStatus(ctx context.Context, statuses []*pb.ChatStatusReport) error
	GetChatStatuses(ctx context.Context, sessionID string) ([]*pb.ChatStatusEntry, error)
	GetSessionStatuses(ctx context.Context, sessionIDs []string) ([]*pb.SessionStatusEntry, error)

	// Auth change notification
	NotifyAuthChange(ctx context.Context, action string) error

	// Cron jobs
	CreateCronJob(ctx context.Context, req *pb.CreateCronJobRequest) (*pb.CronJob, error)
	GetCronJob(ctx context.Context, id string) (*pb.CronJob, error)
	ListCronJobs(ctx context.Context, repoID string) ([]*pb.CronJob, error)
	UpdateCronJob(ctx context.Context, req *pb.UpdateCronJobRequest) (*pb.CronJob, error)
	DeleteCronJob(ctx context.Context, id string) error
	RunCronJobNow(ctx context.Context, id string) (*pb.RunCronJobNowResponse, error)

	// GitHub callbacks (durable one-shot PR-event registrations). Routed by
	// target chat id: local goes straight to the daemon; remote proxies through
	// the orchestrator to the owning bossd (reusing the caller's own auth).
	CreateGithubCallback(ctx context.Context, req *pb.CreateGithubCallbackRequest) (*pb.GithubCallback, error)
	// ListGithubCallbacks returns callbacks matching the optional filters in req.
	// Remote: an unset target_chat_id fans out across the caller's Ready daemons.
	ListGithubCallbacks(ctx context.Context, req *pb.ListGithubCallbacksRequest) ([]*pb.GithubCallback, error)
	// DeleteGithubCallback removes a callback by id. Idempotent. targetChatID is
	// the remote routing key (owning daemon); LocalClient ignores it.
	DeleteGithubCallback(ctx context.Context, targetChatID, id string) error

	// Notes (BOS-553): repo-scoped free-text notes with optional session/chat
	// provenance and tags. CreateNote and ListNotes carry repo_id inside their
	// request message, so they take no extra parameter. Get/Update/Delete take
	// repoID as a separate argument that is the remote routing key only —
	// LocalClient ignores it (the local daemon resolves by id alone), exactly
	// as DeleteGithubCallback does for target-chat routing.
	CreateNote(ctx context.Context, req *pb.CreateNoteRequest) (*pb.Note, error)
	GetNote(ctx context.Context, repoID, id string) (*pb.Note, error)
	// ListNotes returns notes matching the optional filters in req.
	ListNotes(ctx context.Context, req *pb.ListNotesRequest) ([]*pb.Note, error)
	UpdateNote(ctx context.Context, repoID string, req *pb.UpdateNoteRequest) (*pb.Note, error)
	DeleteNote(ctx context.Context, repoID, id string) error

	// Broadcasts (BOS-551): one message fanned out to the audience a selector
	// resolves to, plus standing subscriptions that fire one when a session
	// settles. Local-daemon only today — the orchestrator proto carries no
	// Proxy*Broadcast RPCs, so RemoteClient refuses rather than pretending.
	// The message body travels inbound on the request and is NEVER echoed back
	// on any response: Broadcast.message is cleared on every read surface and
	// BroadcastSubscription has no body field at all.
	SendBroadcast(ctx context.Context, req *pb.SendBroadcastRequest) (*pb.SendBroadcastResponse, error)
	// ListBroadcasts returns broadcasts matching the optional filters in req.
	ListBroadcasts(ctx context.Context, req *pb.ListBroadcastsRequest) ([]*pb.Broadcast, error)
	// DeleteBroadcast removes a broadcast by id. Idempotent.
	DeleteBroadcast(ctx context.Context, id string) error
	// CreateBroadcastSubscription registers a standing rule on a session's outcome.
	CreateBroadcastSubscription(ctx context.Context, req *pb.CreateBroadcastSubscriptionRequest) (*pb.BroadcastSubscription, error)
	// ListBroadcastSubscriptions returns subscriptions matching the optional filters in req.
	ListBroadcastSubscriptions(ctx context.Context, req *pb.ListBroadcastSubscriptionsRequest) ([]*pb.BroadcastSubscription, error)
	// DeleteBroadcastSubscription removes a subscription by id. Idempotent.
	DeleteBroadcastSubscription(ctx context.Context, id string) error

	// Accounts (agent credential registry). Local-daemon only, like cron jobs.
	ListAccounts(ctx context.Context, provider string, refresh bool) ([]*pb.Account, error)
	AddAccount(ctx context.Context, req *pb.AddAccountRequest) (*pb.Account, error)
	UpdateAccount(ctx context.Context, req *pb.UpdateAccountRequest) (*pb.Account, error)
	RemoveAccount(ctx context.Context, id string) error
	TestAccount(ctx context.Context, id string) (*pb.TestAccountResponse, error)

	// Repair diagnostics — surfaced via `boss repair doctor`.
	RepairDoctor(ctx context.Context) (*pb.RepairDoctorResponse, error)

	// StartRepairWorkflow (re-)arms the auto-repair workflow — surfaced via
	// `boss repair start`.
	StartRepairWorkflow(ctx context.Context) (*pb.StartRepairWorkflowResponse, error)

	// ListCheckSnapshots — surfaced via `boss session checks <id>`.
	ListCheckSnapshots(ctx context.Context, sessionID string, limit int32) (*pb.ListCheckSnapshotsResponse, error)

	// ListAgents returns the agent runner plugins currently loaded by the
	// daemon. Used by the TUI to drive provider-aware UI (onboarding, the
	// per-session agent picker, settings).
	ListAgents(ctx context.Context) ([]AgentInfo, error)

	// ListPlugins returns every plugin the daemon attempted to load this
	// run, including disabled and failed entries. Surfaced via
	// `boss plugin list`.
	ListPlugins(ctx context.Context) ([]*pb.InstalledPlugin, error)
}

// CreateSessionStream abstracts a server-streaming create session response.
type CreateSessionStream interface {
	// Receive advances the stream. Returns false when done or on error.
	Receive() bool
	// Msg returns the most recent message from the stream.
	Msg() *pb.CreateSessionResponse
	// Err returns the stream error, if any.
	Err() error
	// Close closes the stream.
	Close() error
}
