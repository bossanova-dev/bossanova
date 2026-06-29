package server

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/displaystatus"
	"github.com/recurser/bossalib/errortrack"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/safego"
	"github.com/recurser/bossalib/trackerprompt"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/cron"
	"github.com/recurser/bossd/internal/db"
	gitpkg "github.com/recurser/bossd/internal/git"
	"github.com/recurser/bossd/internal/mergepolicy"
	"github.com/recurser/bossd/internal/plugin"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/status"
	"github.com/recurser/bossd/internal/tmux"
	"github.com/rs/zerolog"
	"golang.org/x/sync/singleflight"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const maxSetupOutputLineBytes = 256 * 1024

// DefaultSocketPath returns the default Unix socket path for the daemon.
// On macOS: ~/Library/Application Support/bossanova/bossd.sock
// On Linux: ~/.config/bossanova/bossd.sock
func DefaultSocketPath() (string, error) {
	dir, err := config.DefaultAppDataDir()
	if err != nil {
		return "", fmt.Errorf("resolve app data dir: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create socket dir: %w", err)
	}
	return filepath.Join(dir, "bossd.sock"), nil
}

// DefaultSocketPathForSettings returns the daemon socket path for loaded
// settings. socket_path wins over app_data_dir. When both are unset, existing
// platform defaults are preserved.
func DefaultSocketPathForSettings(settings config.Settings) (string, error) {
	if p, ok, err := config.ConfiguredSocketPath(settings); err != nil {
		return "", err
	} else if ok {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return "", fmt.Errorf("create socket dir: %w", err)
		}
		return p, nil
	}
	return DefaultSocketPath()
}

// Server wraps the ConnectRPC DaemonService handler and a Unix socket listener.
type Server struct {
	repos              db.RepoStore
	sessions           db.SessionStore
	attempts           db.AttemptStore
	agentChats         db.AgentChatStore
	workflows          db.WorkflowStore
	taskMappings       db.TaskMappingStore
	cronJobs           db.CronJobStore
	checkSnapshots     db.CheckSnapshotStore
	cronScheduler      *cron.Scheduler
	chatStatus         *status.Tracker
	displayTracker     *status.DisplayTracker
	tmuxPoller         *status.TmuxStatusPoller
	lifecycle          *session.Lifecycle
	agent              agent.AgentRunner
	agentClients       map[string]agent.AgentRunnerClient
	worktrees          gitpkg.WorktreeManager
	provider           vcs.Provider
	prResolver         PRAssociationResolver
	pluginHost         *plugin.Host
	tmux               *tmux.Client
	chatWakeGroup      singleflight.Group // per-chat idempotency for WakeChat
	branchStartMu      sync.Mutex
	branchStartLocks   map[string]*branchStartLock
	wakeHook           wakeHook // test-only: zero in production; see wake_chat.go
	completionNotifier session.SessionCompletionNotifier
	authNotifier       AuthNotifier
	onSessionDeleted   func(context.Context, string)
	onSessionUpdated   func(context.Context, *pb.Session)
	logger             zerolog.Logger
	listener           net.Listener
	srv                *http.Server

	bossanovav1connect.UnimplementedDaemonServiceHandler
}

type branchStartLock struct {
	mu   sync.Mutex
	refs int
}

type listSessionsPRAssociationResult struct {
	updated int64
	err     error
}

var (
	providerSessionIDBackgroundDiscoveryTimeout      = time.Minute
	providerSessionIDBackgroundDiscoveryPollInterval = time.Second
	listSessionsPRAssociationTimeout                 = 10 * time.Second
)

// Config holds all dependencies for creating a new Server.
type Config struct {
	Repos              db.RepoStore
	Sessions           db.SessionStore
	Attempts           db.AttemptStore
	AgentChats         db.AgentChatStore
	Workflows          db.WorkflowStore
	TaskMappings       db.TaskMappingStore
	CronJobs           db.CronJobStore
	CheckSnapshots     db.CheckSnapshotStore
	CronScheduler      *cron.Scheduler
	ChatStatus         *status.Tracker
	DisplayTracker     *status.DisplayTracker
	TmuxPoller         *status.TmuxStatusPoller
	Lifecycle          *session.Lifecycle
	Agent              agent.AgentRunner
	AgentClients       map[string]agent.AgentRunnerClient
	Worktrees          gitpkg.WorktreeManager
	Provider           vcs.Provider
	PRResolver         PRAssociationResolver // optional, may be nil
	PluginHost         *plugin.Host
	Tmux               *tmux.Client
	CompletionNotifier session.SessionCompletionNotifier // optional, may be nil
	AuthNotifier       AuthNotifier                      // optional, nil in local-only mode
	// OnSessionDeleted, when non-nil, is invoked after a session row is
	// removed from the local DB. cmd/main.go wires this to publish a
	// SessionDelta_KIND_DELETED on the reverse stream so bosso's in-memory
	// Registry stops surfacing the deleted session in the web UI. Without
	// this callback bosso only learns about deletions on daemon reconnect
	// (when it replaces its state with a fresh DaemonSnapshot).
	OnSessionDeleted func(context.Context, string)
	// OnSessionUpdated, when non-nil, is invoked after local session
	// mutations that should replace bosso's in-memory session projection.
	// cmd/main.go wires this to publish SessionDelta_KIND_UPDATED on the
	// reverse stream; archive/resurrect depend on it because they hide/show
	// rows in the default web session list without deleting the DB row.
	OnSessionUpdated func(context.Context, *pb.Session)
	Logger           zerolog.Logger
}

// AuthNotifier is the narrow interface the NotifyAuthChange RPC
// delegates to. Under the legacy heartbeat Manager this was the full
// upstream.Manager; under the new StreamClient wiring it's satisfied by
// a small adapter in cmd/main.go that toggles the stream's
// reconnect-with-new-token path. Defined here rather than in the
// upstream package so the server doesn't need to import upstream just
// to depend on this one interface.
type AuthNotifier interface {
	NotifyLogin(ctx context.Context, repoIDs []string) error
	NotifyLogout()
}

// PRAssociationResolver refreshes local session-to-PR links.
type PRAssociationResolver interface {
	ReconcileSessions(context.Context, []*models.Session) (int64, error)
}

// New creates a new Server wired to the given stores and lifecycle orchestrator.
func New(cfg Config) *Server {
	return &Server{
		repos:          cfg.Repos,
		sessions:       cfg.Sessions,
		attempts:       cfg.Attempts,
		agentChats:     cfg.AgentChats,
		workflows:      cfg.Workflows,
		taskMappings:   cfg.TaskMappings,
		cronJobs:       cfg.CronJobs,
		checkSnapshots: cfg.CheckSnapshots,
		cronScheduler:  cfg.CronScheduler,
		chatStatus:     cfg.ChatStatus,
		displayTracker: cfg.DisplayTracker,
		tmuxPoller:     cfg.TmuxPoller,
		lifecycle:      cfg.Lifecycle,
		agent:          cfg.Agent,
		agentClients:   cfg.AgentClients,
		worktrees:      cfg.Worktrees,
		provider:       cfg.Provider,

		prResolver:         cfg.PRResolver,
		pluginHost:         cfg.PluginHost,
		tmux:               cfg.Tmux,
		branchStartLocks:   map[string]*branchStartLock{},
		completionNotifier: cfg.CompletionNotifier,
		authNotifier:       cfg.AuthNotifier,
		onSessionDeleted:   cfg.OnSessionDeleted,
		onSessionUpdated:   cfg.OnSessionUpdated,
		logger:             cfg.Logger,
	}
}

// Listen binds a Unix socket and initializes the underlying http.Server
// synchronously. After Listen returns the server is fully configured and
// Shutdown can be called safely; use Serve (typically in a goroutine) to
// accept requests.
//
// Splitting bind-and-configure from Serve eliminates a race where a caller
// firing Shutdown before the serving goroutine had written s.srv would
// observe a nil pointer (or worse, race the write under -race).
func (s *Server) Listen(socketPath string) error {
	// Defense in depth behind the singleton flock: if the socket is still being
	// served by a live daemon, refuse to start rather than os.Remove + rebind
	// (which would steal the socket from the running instance). The flock guard
	// in cmd/run should already prevent reaching here, but a socket left by a
	// process that crashed without releasing its lock would otherwise be stolen.
	if socketIsServing(socketPath) {
		return fmt.Errorf("socket %s is already served by a running daemon", socketPath)
	}

	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		return fmt.Errorf("create socket dir: %w", err)
	}

	// Remove only stale socket files from previous runs. Never unlink an
	// arbitrary configured path.
	if info, err := os.Lstat(socketPath); err == nil {
		if info.Mode().Type() != os.ModeSocket {
			return fmt.Errorf("socket path exists and is not a socket: %s", socketPath)
		}
		if err := os.Remove(socketPath); err != nil {
			return fmt.Errorf("remove stale socket: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat socket path: %w", err)
	}

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen unix %s: %w", socketPath, err)
	}
	s.listener = ln

	// Make socket accessible only to the current user.
	if err := os.Chmod(socketPath, 0o700); err != nil {
		_ = ln.Close()
		return fmt.Errorf("chmod socket: %w", err)
	}

	mux := http.NewServeMux()
	path, handler := bossanovav1connect.NewDaemonServiceHandler(s, connect.WithInterceptors(errortrack.Interceptor()))
	mux.Handle(path, withCreateSessionWriteDeadlineOverride(handler))

	s.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	return nil
}

func withCreateSessionWriteDeadlineOverride(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == bossanovav1connect.DaemonServiceCreateSessionProcedure {
			// CreateSession streams setup output and may legitimately run longer
			// than the daemon's non-streaming write timeout.
			_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
		}
		next.ServeHTTP(w, r)
	})
}

// Serve blocks serving requests on the listener created by Listen. It
// returns http.ErrServerClosed when Shutdown completes successfully.
func (s *Server) Serve() error {
	return s.srv.Serve(s.listener)
}

// ListenAndServe is a convenience for Listen followed by Serve. Callers
// that need to invoke Shutdown concurrently with the serving goroutine
// should prefer the two-phase API (Listen sync, Serve in a goroutine).
func (s *Server) ListenAndServe(socketPath string) error {
	if err := s.Listen(socketPath); err != nil {
		return err
	}
	return s.Serve()
}

// Shutdown gracefully stops the server. Safe to call before or after
// Serve, but the caller must ensure Listen has returned first.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.srv != nil {
		return s.srv.Shutdown(ctx)
	}
	return nil
}

// --- Repo Validation ---

func (s *Server) ValidateRepoPath(ctx context.Context, req *connect.Request[pb.ValidateRepoPathRequest]) (*connect.Response[pb.ValidateRepoPathResponse], error) {
	localPath := req.Msg.LocalPath
	if localPath == "" {
		return connect.NewResponse(&pb.ValidateRepoPathResponse{
			IsValid:      false,
			ErrorMessage: "path is required",
		}), nil
	}

	// Check path exists and is a directory.
	info, err := os.Stat(localPath)
	if err != nil {
		return connect.NewResponse(&pb.ValidateRepoPathResponse{
			IsValid:      false,
			ErrorMessage: fmt.Sprintf("path does not exist: %s", localPath),
		}), nil
	}
	if !info.IsDir() {
		return connect.NewResponse(&pb.ValidateRepoPathResponse{
			IsValid:      false,
			ErrorMessage: fmt.Sprintf("path is not a directory: %s", localPath),
		}), nil
	}

	// Check it's a git repo.
	if !s.worktrees.IsGitRepo(ctx, localPath) {
		return connect.NewResponse(&pb.ValidateRepoPathResponse{
			IsValid:      false,
			ErrorMessage: fmt.Sprintf("not a git repository: %s", localPath),
		}), nil
	}

	// Detect metadata.
	originURL, _ := s.worktrees.DetectOriginURL(ctx, localPath)
	defaultBranch, _ := s.worktrees.DetectDefaultBranch(ctx, localPath)

	return connect.NewResponse(&pb.ValidateRepoPathResponse{
		IsValid:       true,
		OriginUrl:     originURL,
		IsGithub:      vcs.IsGitHubURL(originURL),
		DefaultBranch: defaultBranch,
	}), nil
}

// --- Repo Management ---

func (s *Server) RegisterRepo(ctx context.Context, req *connect.Request[pb.RegisterRepoRequest]) (*connect.Response[pb.RegisterRepoResponse], error) {
	msg := req.Msg
	if msg.LocalPath == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("local_path is required"))
	}

	// Validate the path exists, is a directory, and is a git repo.
	info, err := os.Stat(msg.LocalPath)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("path does not exist: %s", msg.LocalPath))
	}
	if !info.IsDir() {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("path is not a directory: %s", msg.LocalPath))
	}
	if !s.worktrees.IsGitRepo(ctx, msg.LocalPath) {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("not a git repository: %s", msg.LocalPath))
	}

	var setupScript *string
	if msg.SetupScript != nil {
		setupScript = msg.SetupScript
	}

	// Auto-detect origin URL from git config.
	originURL, _ := s.worktrees.DetectOriginURL(ctx, msg.LocalPath)

	repo, err := s.repos.Create(ctx, db.CreateRepoParams{
		DisplayName:       msg.DisplayName,
		LocalPath:         msg.LocalPath,
		OriginURL:         originURL,
		DefaultBaseBranch: msg.DefaultBaseBranch,
		WorktreeBaseDir:   msg.WorktreeBaseDir,
		SetupScript:       setupScript,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create repo: %w", err))
	}

	return connect.NewResponse(&pb.RegisterRepoResponse{Repo: repoToProto(repo)}), nil
}

func (s *Server) CloneAndRegisterRepo(ctx context.Context, req *connect.Request[pb.CloneAndRegisterRepoRequest]) (*connect.Response[pb.CloneAndRegisterRepoResponse], error) {
	msg := req.Msg
	if msg.CloneUrl == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("clone_url is required"))
	}
	if msg.LocalPath == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("local_path is required"))
	}

	// Check if the target path already exists.
	if info, err := os.Stat(msg.LocalPath); err == nil {
		if !info.IsDir() {
			return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("local_path exists and is not a directory"))
		}
		// Directory exists — check if it's already a git repo with matching origin.
		existingOrigin, _ := s.worktrees.DetectOriginURL(ctx, msg.LocalPath)
		if existingOrigin == "" {
			return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("local_path already exists but is not a git repository"))
		}
		// Origin exists but doesn't match — error.
		if existingOrigin != msg.CloneUrl {
			return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("local_path is a git repo with different origin %q", existingOrigin))
		}
		// Matching origin — skip clone, just register.
	} else {
		// Path doesn't exist — clone.
		if err := s.worktrees.Clone(ctx, msg.CloneUrl, msg.LocalPath); err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("clone: %w", err))
		}
	}

	// Auto-detect origin URL from the cloned repo.
	originURL, _ := s.worktrees.DetectOriginURL(ctx, msg.LocalPath)

	var setupScript *string
	if msg.SetupScript != nil {
		setupScript = msg.SetupScript
	}

	repo, err := s.repos.Create(ctx, db.CreateRepoParams{
		DisplayName:       msg.DisplayName,
		LocalPath:         msg.LocalPath,
		OriginURL:         originURL,
		DefaultBaseBranch: msg.DefaultBaseBranch,
		WorktreeBaseDir:   msg.WorktreeBaseDir,
		SetupScript:       setupScript,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create repo: %w", err))
	}

	return connect.NewResponse(&pb.CloneAndRegisterRepoResponse{Repo: repoToProto(repo)}), nil
}

func (s *Server) ListRepos(ctx context.Context, req *connect.Request[pb.ListReposRequest]) (*connect.Response[pb.ListReposResponse], error) {
	repos, err := s.repos.List(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list repos: %w", err))
	}

	pbRepos := make([]*pb.Repo, len(repos))
	for i, r := range repos {
		pbRepos[i] = repoToProto(r)
	}

	return connect.NewResponse(&pb.ListReposResponse{Repos: pbRepos}), nil
}

func (s *Server) RemoveRepo(ctx context.Context, req *connect.Request[pb.RemoveRepoRequest]) (*connect.Response[pb.RemoveRepoResponse], error) {
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	if err := s.repos.Delete(ctx, req.Msg.Id); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("remove repo: %w", err))
	}

	return connect.NewResponse(&pb.RemoveRepoResponse{}), nil
}

func (s *Server) UpdateRepo(ctx context.Context, req *connect.Request[pb.UpdateRepoRequest]) (*connect.Response[pb.UpdateRepoResponse], error) {
	msg := req.Msg
	if msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	params := db.UpdateRepoParams{}
	if msg.DisplayName != nil {
		params.DisplayName = msg.DisplayName
	}
	if msg.CanAutoMerge != nil {
		params.CanAutoMerge = msg.CanAutoMerge
	}
	if msg.CanAutoMergeDependabot != nil {
		params.CanAutoMergeDependabot = msg.CanAutoMergeDependabot
	}
	if msg.CanAutoRepair != nil {
		params.CanAutoRepair = msg.CanAutoRepair
	}
	if msg.MergeStrategy != nil {
		ms := models.MergeStrategy(*msg.MergeStrategy)
		params.MergeStrategy = &ms
	}
	if msg.SetupScript != nil {
		if *msg.SetupScript == "" {
			// Empty string clears the setup command (set DB to NULL).
			params.SetupScript = new(*string)
		} else {
			params.SetupScript = &msg.SetupScript
		}
	}
	if msg.LinearApiKey != nil {
		params.LinearAPIKey = msg.LinearApiKey
	}
	if msg.SentryApiKey != nil {
		params.SentryAPIKey = msg.SentryApiKey
	}
	if msg.SentryOrg != nil {
		params.SentryOrg = msg.SentryOrg
	}
	repo, err := s.repos.Update(ctx, msg.Id, params)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("update repo: %w", err))
	}

	return connect.NewResponse(&pb.UpdateRepoResponse{Repo: repoToProto(repo)}), nil
}

func (s *Server) ListRepoPRs(ctx context.Context, req *connect.Request[pb.ListRepoPRsRequest]) (*connect.Response[pb.ListRepoPRsResponse], error) {
	if req.Msg.RepoId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("repo_id is required"))
	}

	// Verify the repo exists and get its origin URL.
	repo, err := s.repos.Get(ctx, req.Msg.RepoId)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("repo not found: %w", err))
	}

	if repo.OriginURL == "" {
		return connect.NewResponse(&pb.ListRepoPRsResponse{}), nil
	}

	prs, err := s.provider.ListOpenPRs(ctx, repo.OriginURL)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list PRs: %w", err))
	}

	keys, err := s.activeSessionKeysForRepo(ctx, req.Msg.RepoId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list active sessions: %w", err))
	}
	prs = keys.excludePRs(prs)

	pbPRs := make([]*pb.PRSummary, len(prs))
	for i, pr := range prs {
		pbPRs[i] = &pb.PRSummary{
			Number:     int32(pr.Number),
			Title:      pr.Title,
			HeadBranch: pr.HeadBranch,
			State:      pb.PRState(pr.State),
			Author:     pr.Author,
		}
	}

	return connect.NewResponse(&pb.ListRepoPRsResponse{PullRequests: pbPRs}), nil
}

func (s *Server) ListTrackerIssues(ctx context.Context, req *connect.Request[pb.ListTrackerIssuesRequest]) (*connect.Response[pb.ListTrackerIssuesResponse], error) {
	if req.Msg.RepoId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("repo_id is required"))
	}

	// Get repo from store.
	repo, err := s.repos.Get(ctx, req.Msg.RepoId)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("repo not found: %w", err))
	}

	// Resolve the task source. Defaults to "linear" so existing callers (and
	// older clients that don't set the field) keep working unchanged.
	want := req.Msg.GetSource()
	if want == "" {
		want = "linear"
	}

	// Validate credentials and build the plugin config map for the chosen
	// source. Each source addresses its API differently, so the required fields
	// differ; a missing field is reported by name.
	var config map[string]string
	switch want {
	case "linear":
		if repo.LinearAPIKey == "" {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("linear_api_key not configured for this repo"))
		}
		config = map[string]string{
			"linear_api_key": repo.LinearAPIKey,
		}
	case "sentry":
		var missing []string
		if repo.SentryAPIKey == "" {
			missing = append(missing, "sentry_api_key")
		}
		if repo.SentryOrg == "" {
			missing = append(missing, "sentry_org")
		}
		if len(missing) > 0 {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("sentry not configured for this repo: missing %s", strings.Join(missing, ", ")))
		}
		config = map[string]string{
			"sentry_api_key": repo.SentryAPIKey,
			"sentry_org":     repo.SentryOrg,
		}
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown tracker source %q", want))
	}

	// Find the matching task source plugin by name.
	if s.pluginHost == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("plugin host not available"))
	}
	sources := s.pluginHost.GetTaskSources()
	if len(sources) == 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("no task source plugins available"))
	}

	source := selectTaskSource(ctx, sources, want)
	if source == nil {
		// Distinguish "no plugin by this name is loaded" from a runtime failure:
		// the usual cause is a plugin binary that exists but isn't loaded (config
		// stripped the entry, or the daemon wasn't restarted after it was added).
		// List what IS loaded and point at the fix instead of a bare "not found".
		loaded := loadedTaskSourceNames(ctx, sources)
		available := "none"
		if len(loaded) > 0 {
			available = strings.Join(loaded, ", ")
		}
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("no task source plugin named %q is loaded (loaded task sources: %s); add it to settings.json and restart bossd", want, available))
	}

	// Call ListAvailableIssues. The query is optional; when set, the plugin pushes
	// it down to the tracker API as a filter so the search isn't limited to the
	// first page that the plugin happens to have cached locally.
	query := strings.TrimSpace(req.Msg.Query)
	issues, err := source.ListAvailableIssues(ctx, repo.OriginURL, query, config)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list tracker issues: %w", err))
	}

	keys, err := s.activeSessionKeysForRepo(ctx, req.Msg.RepoId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list active sessions: %w", err))
	}
	issues = keys.excludeIssues(issues)

	return connect.NewResponse(&pb.ListTrackerIssuesResponse{Issues: issues}), nil
}

// selectTaskSource returns the first task source whose GetInfo name matches
// want, or nil if none does. This is how ListTrackerIssues routes a request to
// the Linear or Sentry plugin: the request's source name is matched against
// each loaded plugin's self-reported name. A plugin whose GetInfo errors is
// skipped rather than aborting the search.
func selectTaskSource(ctx context.Context, sources []plugin.TaskSource, want string) plugin.TaskSource {
	for _, src := range sources {
		info, err := src.GetInfo(ctx)
		if err != nil {
			continue
		}
		if info.Name == want {
			return src
		}
	}
	return nil
}

// loadedTaskSourceNames returns the GetInfo names of every task source that
// responds, sorted. It backs the actionable error ListTrackerIssues returns
// when a requested source isn't loaded. Sources whose GetInfo errors are
// skipped, matching selectTaskSource's behavior.
func loadedTaskSourceNames(ctx context.Context, sources []plugin.TaskSource) []string {
	names := make([]string, 0, len(sources))
	for _, src := range sources {
		info, err := src.GetInfo(ctx)
		if err != nil {
			continue
		}
		names = append(names, info.Name)
	}
	sort.Strings(names)
	return names
}

// ListAgents returns the names + GetInfo metadata of every AgentRunner
// plugin currently wired into the daemon. The boss TUI uses this to
// drive the agent picker in `boss session new` and to surface plugin
// versions in /diagnose. A plugin whose GetInfo errors is included with
// only Name populated rather than dropped silently — operators see the
// failure and can still pick the plugin.
//
// Sourced from s.agentClients (the same registry the orchestrator and
// status poller use) rather than the plugin host, so a future test can
// inject fakes without spinning up a real go-plugin subprocess. Returns
// agents in lexicographic order so callers don't see jitter run-to-run.
func (s *Server) ListAgents(ctx context.Context, _ *connect.Request[pb.ListAgentsRequest]) (*connect.Response[pb.ListAgentsResponse], error) {
	out := &pb.ListAgentsResponse{}
	if len(s.agentClients) == 0 {
		return connect.NewResponse(out), nil
	}
	names := make([]string, 0, len(s.agentClients))
	for name := range s.agentClients {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		client := s.agentClients[name]
		info, err := client.GetInfo(ctx)
		if err != nil || info == nil {
			s.logger.Warn().Err(err).Str("agent", name).Msg("ListAgents: GetInfo failed; including bare name only")
			out.Agents = append(out.Agents, &pb.AgentInfo{Name: name})
			continue
		}
		out.Agents = append(out.Agents, &pb.AgentInfo{
			Name:         info.GetName(),
			Version:      info.GetVersion(),
			UserSettings: info.GetUserSettings(),
		})
	}
	return connect.NewResponse(out), nil
}

// ListPlugins returns the runtime status of every plugin the daemon
// attempted to load this run. Loaded plugins are reported alongside
// disabled and failed entries so an operator with a typo'd path or a
// stale binary on disk can debug it without grepping the daemon log.
func (s *Server) ListPlugins(_ context.Context, _ *connect.Request[pb.ListPluginsRequest]) (*connect.Response[pb.ListPluginsResponse], error) {
	out := &pb.ListPluginsResponse{}
	if s.pluginHost == nil {
		return connect.NewResponse(out), nil
	}
	for _, p := range s.pluginHost.AllPlugins() {
		out.Plugins = append(out.Plugins, &pb.InstalledPlugin{
			Name:         p.Name,
			Path:         p.Path,
			Enabled:      p.Enabled,
			Status:       installedPluginStatus(p),
			Capabilities: append([]string(nil), p.Capabilities...),
			Error:        p.Error,
		})
	}
	return connect.NewResponse(out), nil
}

// installedPluginStatus collapses (Enabled, Loaded, Error) into the
// proto enum surfaced to the CLI.
func installedPluginStatus(p plugin.PluginStatus) pb.InstalledPlugin_Status {
	switch {
	case p.Loaded:
		return pb.InstalledPlugin_STATUS_LOADED
	case !p.Enabled:
		return pb.InstalledPlugin_STATUS_DISABLED
	default:
		return pb.InstalledPlugin_STATUS_LOAD_FAILED
	}
}

// --- Session Lifecycle ---

func (s *Server) lockBranchStart(repoID, branch string) func() {
	if repoID == "" || branch == "" {
		return func() {}
	}
	key := repoID + "\x00" + branch

	s.branchStartMu.Lock()
	if s.branchStartLocks == nil {
		s.branchStartLocks = map[string]*branchStartLock{}
	}
	lock := s.branchStartLocks[key]
	if lock == nil {
		lock = &branchStartLock{}
		s.branchStartLocks[key] = lock
	}
	lock.refs++
	s.branchStartMu.Unlock()

	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()

		s.branchStartMu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(s.branchStartLocks, key)
		}
		s.branchStartMu.Unlock()
	}
}

func (s *Server) sendExistingSessionForBranch(ctx context.Context, repoID, branch string, emit createSessionSender) (bool, error) {
	if branch == "" {
		return false, nil
	}
	active, err := s.sessions.ListActive(ctx, repoID)
	if err != nil {
		return false, connect.NewError(connect.CodeInternal, fmt.Errorf("list active sessions: %w", err))
	}
	for _, sess := range active {
		if sess.BranchName != branch {
			continue
		}
		if sess.WorktreePath == "" {
			return true, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("session already starting for branch %q", branch))
		}
		if _, err := os.Stat(sess.WorktreePath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return true, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("session already exists for branch %q but worktree directory is missing", branch))
			}
			return true, connect.NewError(connect.CodeInternal, fmt.Errorf("stat existing session worktree: %w", err))
		}
		return true, emit.Send(&pb.CreateSessionResponse{
			Event: &pb.CreateSessionResponse_SessionCreated{
				SessionCreated: &pb.SessionCreated{
					Session: SessionToProto(sess),
				},
			},
		})
	}
	return false, nil
}

func createSessionConnectError(err error) error {
	if session.IsDuplicateActiveSessionError(err) {
		return connect.NewError(connect.CodeAlreadyExists, err)
	}
	return connect.NewError(connect.CodeInternal, err)
}

func isDependabotBranch(branch string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(branch)), "dependabot/")
}

func (s *Server) cleanupFailedCreateSession(ctx context.Context, sessionID string) {
	if failedSess, getErr := s.sessions.Get(ctx, sessionID); getErr == nil {
		if failedSess.RepoID != "" && failedSess.BranchName != "" {
			if repo, repoErr := s.repos.Get(ctx, failedSess.RepoID); repoErr == nil {
				// Only delete the branch (remote + local) when a worktree was
				// actually created. This preserves the property that a
				// worktree-creation failure (WorktreePath == "") never deletes
				// the branch — critical for dependabot PR head branches, which
				// EmptyTrash would otherwise `git push origin --delete`.
				if failedSess.WorktreePath != "" {
					_ = s.worktrees.EmptyTrash(ctx, repo.LocalPath, []string{failedSess.BranchName})
				}
				// Always remove any leftover worktree directory — including when
				// creation failed before WorktreePath was recorded — so a stale
				// directory can't wedge the branch on the next attempt (the
				// dependabot repair loop). Does NOT delete the branch.
				s.worktrees.PurgeWorktree(ctx, repo.LocalPath, repo.DisplayName, repo.WorktreeBaseDir, failedSess.BranchName)
			}
		}
	}
	_ = s.sessions.Delete(ctx, sessionID)
	if s.onSessionDeleted != nil {
		s.onSessionDeleted(ctx, sessionID)
	}
}

// CreateSession is the thin ConnectRPC handler: it delegates to the
// streaming core StreamCreateSession, wiring the connect server stream's
// Send as the emit sink. All validation/behaviour lives in the core so the
// reverse-stream path (bosso → daemon CreateSessionCommand) can reuse it
// without a non-mockable *connect.ServerStream.
func (s *Server) CreateSession(ctx context.Context, req *connect.Request[pb.CreateSessionRequest], stream *connect.ServerStream[pb.CreateSessionResponse]) error {
	return s.StreamCreateSession(ctx, req.Msg, stream.Send)
}

// StreamCreateSession holds the full CreateSession body. Each result frame is
// delivered through emit (the connect handler passes stream.Send; the reverse-
// stream adapter passes a channel-pushing closure). Behaviour and validation
// are identical to the original handler — this is a pure extract-method.
func (s *Server) StreamCreateSession(ctx context.Context, msg *pb.CreateSessionRequest, emit func(*pb.CreateSessionResponse) error) error {
	if msg.RepoId == "" {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("repo_id is required"))
	}
	if msg.Title == "" {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("title is required"))
	}

	// Verify repo exists.
	repo, err := s.repos.Get(ctx, msg.RepoId)
	if err != nil {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("repo not found: %w", err))
	}

	baseBranch := msg.BaseBranch
	if baseBranch == "" {
		baseBranch = repo.DefaultBaseBranch
	}

	var prNumber *int
	var prURL *string
	var headBranch string
	if msg.PrNumber != nil {
		n := int(*msg.PrNumber)
		prNumber = &n

		// Fetch PR metadata to get head branch and construct PR URL.
		if repo.OriginURL != "" {
			prStatus, prErr := s.provider.GetPRStatus(ctx, repo.OriginURL, n)
			if prErr == nil {
				headBranch = prStatus.HeadBranch
				baseBranch = prStatus.BaseBranch
			}
			// Construct PR URL from origin.
			u := constructPRURL(repo.OriginURL, n)
			if u != "" {
				prURL = &u
			}
		}
	} else if msg.BranchName != nil && *msg.BranchName != "" {
		// Use the branch name from the request (e.g., from Linear's suggested branch name).
		headBranch = *msg.BranchName
	}
	if !msg.QuickChat && strings.TrimSpace(baseBranch) == "" {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("repo %s has no default base branch; update repo settings before creating a session", repo.ID))
	}

	unlockStart := session.LockStartPath()
	startLocked := true
	defer func() {
		if startLocked {
			unlockStart()
		}
	}()

	unlockBranch := s.lockBranchStart(msg.RepoId, headBranch)
	branchLocked := true
	defer func() {
		if branchLocked {
			unlockBranch()
		}
	}()

	if handled, err := s.sendExistingSessionForBranch(ctx, msg.RepoId, headBranch, emitSender{emit}); handled || err != nil {
		return err
	}

	if prNumber != nil && isDependabotBranch(headBranch) {
		if err := session.EnsureNoActivePRSession(ctx, s.sessions, msg.RepoId, prNumber); err != nil {
			return createSessionConnectError(err)
		}
	}

	// When the client supplies a selected tracker issue but no explicit plan
	// (web new-session Linear/Sentry flow), format the plan server-side from the
	// shared formatter — single source of truth with the TUI (trackerprompt).
	plan := msg.GetPlan()
	if plan == "" && msg.GetTrackerIssue() != nil {
		plan = trackerprompt.Format(msg.GetTrackerIssue(), msg.GetTrackerSource())
	}

	// Resolve the agent name. The proto field is a oneof (*string), so an
	// unset request resolves via the shared rule (single-loaded runner, then
	// Settings.DefaultAgent) — see resolveAgentName.
	agentName := s.resolveAgentName(msg.GetAgentName())

	createParams := db.CreateSessionParams{
		RepoID:     msg.RepoId,
		Title:      msg.Title,
		Plan:       plan,
		BranchName: headBranch,
		BaseBranch: baseBranch,
		AgentName:  agentName,
		PRNumber:   prNumber,
		TrackerID:  msg.TrackerId,
		TrackerURL: msg.TrackerUrl,
	}
	if prURL != nil {
		createParams.PRURL = prURL
	}

	sess, err := s.sessions.Create(ctx, createParams)
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("create session: %w", err))
	}
	unlockBranch()
	branchLocked = false

	// Start the session lifecycle: create worktree, start Claude, fire state machine.
	// Quick chat sessions skip worktree/branch/PR creation entirely.
	var startErr error
	if msg.QuickChat {
		startErr = s.lifecycle.StartQuickChatSession(ctx, sess.ID)
	} else {
		// Create a pipe to stream setup script output to the client.
		pr, pw := io.Pipe()
		defer pr.Close() //nolint:errcheck // best-effort cleanup

		type lifecycleResult struct {
			err error
		}
		done := make(chan lifecycleResult, 1)

		go func() {
			defer pw.Close() //nolint:errcheck // best-effort cleanup
			err := s.lifecycle.StartSession(ctx, sess.ID, session.StartSessionOpts{
				ExistingBranch: headBranch,
				ForceBranch:    msg.ForceBranch,
				SetupOutput:    pw,
			})
			done <- lifecycleResult{err: err}
		}()

		if err := streamSetupOutput(pr, emitSender{emit}); err != nil {
			// Client disconnected or the setup-output pipe failed — close the
			// pipe reader to unblock the goroutine if it's blocked on pw.Write(),
			// then wait for it to finish before removing the persisted session.
			_ = pr.Close()
			<-done
			s.cleanupFailedCreateSession(context.WithoutCancel(ctx), sess.ID)
			return err
		}

		// Close the pipe reader to unblock the goroutine if it's still writing.
		_ = pr.Close()

		// Pipe closed — lifecycle goroutine is done.
		result := <-done
		startErr = result.err
	}
	if err := startErr; err != nil {
		s.cleanupFailedCreateSession(context.WithoutCancel(ctx), sess.ID)
		if errors.Is(err, gitpkg.ErrBranchExists) {
			return connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("branch already exists for this session title"))
		}
		s.logger.Error().Err(err).
			Str("session", sess.ID).
			Str("title", msg.Title).
			Msg("start session failed")
		return createSessionConnectError(fmt.Errorf("start session: %w", err))
	}
	unlockStart()
	startLocked = false

	// Re-fetch the session to get updated fields from lifecycle.
	sess, err = s.sessions.Get(ctx, sess.ID)
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("get session: %w", err))
	}

	return emit(&pb.CreateSessionResponse{
		Event: &pb.CreateSessionResponse_SessionCreated{
			SessionCreated: &pb.SessionCreated{
				Session: SessionToProto(sess),
			},
		},
	})
}

type createSessionSender interface {
	Send(*pb.CreateSessionResponse) error
}

// emitSender adapts an emit func(*pb.CreateSessionResponse) error to the
// createSessionSender interface so the extracted streaming core can pass its
// sink to helpers (sendExistingSessionForBranch, streamSetupOutput) that were
// written against the Send-method shape.
type emitSender struct {
	emit func(*pb.CreateSessionResponse) error
}

func (e emitSender) Send(resp *pb.CreateSessionResponse) error { return e.emit(resp) }

func streamSetupOutput(r io.Reader, stream createSessionSender) error {
	reader := bufio.NewReaderSize(r, maxSetupOutputLineBytes+1)
	for {
		line, err := reader.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			return connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("setup output line exceeds %d bytes", maxSetupOutputLineBytes))
		}
		if len(line) > 0 {
			line = bytes.TrimSuffix(line, []byte("\n"))
			line = bytes.TrimSuffix(line, []byte("\r"))
			if len(line) > maxSetupOutputLineBytes {
				return connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("setup output line exceeds %d bytes", maxSetupOutputLineBytes))
			}
			if sendErr := stream.Send(&pb.CreateSessionResponse{
				Event: &pb.CreateSessionResponse_SetupOutput{
					SetupOutput: &pb.SetupScriptOutput{
						Text: string(line),
					},
				},
			}); sendErr != nil {
				return sendErr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return connect.NewError(connect.CodeInternal, fmt.Errorf("read setup output: %w", err))
		}
	}
}

func (s *Server) GetSession(ctx context.Context, req *connect.Request[pb.GetSessionRequest]) (*connect.Response[pb.GetSessionResponse], error) {
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	session, err := s.sessions.Get(ctx, req.Msg.Id)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session not found: %w", err))
	}

	p := SessionToProto(session)

	// Hydrate attention status from session state and repo flags.
	if repo, err := s.repos.Get(ctx, session.RepoID); err == nil {
		p.RepoDisplayName = repo.DisplayName
		p.RepoOriginUrl = CanonicalRepoOriginURL(repo.OriginURL)
		p.AttentionStatus = attentionStatusToProto(vcs.ComputeAttentionStatus(session, repo))
	}

	// Hydrate PR display status from the in-memory tracker.
	if s.displayTracker != nil {
		if e := s.displayTracker.Get(session.ID); e != nil {
			p.DisplayStatus = pb.DisplayStatus(e.Status)
			p.DisplayHasFailures = e.HasFailures
			p.DisplayHasChangesRequested = e.HasChangesRequested
			p.DisplayIsRepairing = e.IsRepairing
			p.DisplaySettingUp = e.SettingUp
		}
	}

	return connect.NewResponse(&pb.GetSessionResponse{Session: p}), nil
}

func (s *Server) ListSessions(ctx context.Context, req *connect.Request[pb.ListSessionsRequest]) (*connect.Response[pb.ListSessionsResponse], error) {
	msg := req.Msg
	repoID := msg.GetRepoId()

	rows, err := s.loadSessionsForList(ctx, repoID, msg.IncludeArchived, msg.States)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list sessions: %w", err))
	}

	sessionsNeedingPR := sessionRowsNeedingPRAssociation(rows)
	if s.prResolver != nil && len(sessionsNeedingPR) > 0 {
		updated, reconcileErr := s.reconcileListSessionPRAssociations(ctx, sessionsNeedingPR)
		if reconcileErr != nil {
			s.logger.Warn().Err(reconcileErr).Msg("ListSessions: reconcile PR associations")
		}
		if updated > 0 {
			rows, err = s.loadSessionsForList(ctx, repoID, msg.IncludeArchived, msg.States)
			if err != nil {
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list sessions: %w", err))
			}
		}
	}

	// Build repo lookup for denormalization and attention hydration.
	repoCache := make(map[string]*models.Repo)
	for _, row := range rows {
		if row == nil || row.Session == nil {
			continue
		}
		sess := row.Session
		if _, ok := repoCache[sess.RepoID]; !ok {
			if repo, err := s.repos.Get(ctx, sess.RepoID); err == nil {
				repoCache[sess.RepoID] = repo
			}
		}
	}

	pbSessions := make([]*pb.Session, 0, len(rows))
	for _, row := range rows {
		if row == nil || row.Session == nil {
			continue
		}
		sess := row.Session
		p := SessionToProto(sess)
		if repo, ok := repoCache[sess.RepoID]; ok {
			p.RepoDisplayName = repo.DisplayName
			p.RepoOriginUrl = CanonicalRepoOriginURL(repo.OriginURL)
			p.AttentionStatus = attentionStatusToProto(vcs.ComputeAttentionStatus(sess, repo))
		} else {
			p.RepoDisplayName = row.RepoDisplayName
			p.RepoOriginUrl = CanonicalRepoOriginURL(row.RepoOriginURL)
		}
		pbSessions = append(pbSessions, p)
	}

	// Hydrate PR display statuses from the in-memory tracker.
	//
	// The composite display fields (DisplayLabel/Intent/Spinner) are persisted on
	// the session row, but the per-axis fields below (DisplayStatus,
	// DisplayHasFailures, DisplayHasChangesRequested, DisplayIsRepairing, and the
	// WorkflowDisplay* block hydrated next) are NOT in models.Session — only the
	// in-memory tracker knows them. Remaining typed-enum consumers: the TUI
	// (home.go/chatpicker.go/theme.go via GetDisplayStatus) and the repair plugin
	// (server.go via GetDisplayStatus + GetDisplayHasFailures).
	sessionIDs := make([]string, len(pbSessions))
	for i, sess := range pbSessions {
		sessionIDs[i] = sess.Id
	}
	var entries map[string]*status.DisplayEntry
	if s.displayTracker != nil {
		entries = s.displayTracker.GetBatch(sessionIDs)
		for i, sess := range pbSessions {
			if e, ok := entries[sess.Id]; ok {
				pbSessions[i].DisplayStatus = pb.DisplayStatus(e.Status)
				pbSessions[i].DisplayHasFailures = e.HasFailures
				pbSessions[i].DisplayHasChangesRequested = e.HasChangesRequested
				pbSessions[i].DisplayIsRepairing = e.IsRepairing
				pbSessions[i].DisplaySettingUp = e.SettingUp
			}
		}
	}
	for _, p := range pbSessions {
		suppressStaleConflictAttention(p)
	}

	// Workflow display status used to be hydrated from the workflows table,
	// which was populated by the autopilot plugin. With autopilot gone the
	// table has no writers, so the derivation has been removed. Phase 4 of
	// the autopilot removal will reintroduce a repair-driven derivation.

	chatsBySession, chatsLoaded := s.chatsBySessionForStatuses(ctx, sessionIDs)
	for _, p := range pbSessions {
		_, hasDisplayEntry := entries[p.Id]
		chatStatus, ok := s.chatStatusFromSessionChats(ctx, chatsBySession[p.Id], chatsLoaded)
		if !ok || (!hasDisplayEntry && chatStatus == pb.ChatStatus_CHAT_STATUS_STOPPED) {
			continue
		}
		if shouldKeepCheckingCompositeOverReview(p, chatStatus) {
			continue
		}
		out := displaystatus.Compute(displaystatus.Input{
			Session:    p,
			ChatStatus: chatStatus,
		})
		p.DisplayLabel = out.Label
		p.DisplayIntent = out.Intent
		p.DisplaySpinner = out.Spinner
	}

	return connect.NewResponse(&pb.ListSessionsResponse{Sessions: pbSessions}), nil
}

func (s *Server) reconcileListSessionPRAssociations(ctx context.Context, sessions []*models.Session) (int64, error) {
	reconcileCtx, cancel := context.WithTimeout(ctx, listSessionsPRAssociationTimeout)
	defer cancel()

	// The result channel is buffered so the goroutine can always send and exit
	// even if we have already returned on the timeout below. safego.Go gives the
	// goroutine panic recovery so a misbehaving provider can't crash the daemon
	// on the ListSessions hot path.
	resultCh := make(chan listSessionsPRAssociationResult, 1)
	safego.Go(s.logger, func() {
		updated, err := s.prResolver.ReconcileSessions(reconcileCtx, sessions)
		resultCh <- listSessionsPRAssociationResult{updated: updated, err: err}
	})

	select {
	case result := <-resultCh:
		return result.updated, result.err
	case <-reconcileCtx.Done():
		return 0, reconcileCtx.Err()
	}
}

// loadSessionsForList loads the session rows for a ListSessions request and
// applies the optional state filter. It is called twice: once up front, and
// again after PR-association reconciliation when associations changed. Using
// the *WithRepo store methods means the rows carry denormalized repo fields,
// which back the display-name/origin fallback used when the repo cache misses.
func (s *Server) loadSessionsForList(ctx context.Context, repoID string, includeArchived bool, states []pb.SessionState) ([]*db.SessionWithRepo, error) {
	var rows []*db.SessionWithRepo
	var err error
	if includeArchived {
		rows, err = s.sessions.ListWithRepo(ctx, repoID)
	} else {
		rows, err = s.sessions.ListActiveWithRepo(ctx, repoID)
	}
	if err != nil {
		return nil, err
	}
	if len(states) > 0 {
		rows = filterSessionRowsByStates(rows, states)
	}
	return rows, nil
}

func filterSessionRowsByStates(rows []*db.SessionWithRepo, states []pb.SessionState) []*db.SessionWithRepo {
	stateSet := make(map[pb.SessionState]bool, len(states))
	for _, st := range states {
		stateSet[st] = true
	}

	filtered := make([]*db.SessionWithRepo, 0, len(rows))
	for _, row := range rows {
		if row == nil || row.Session == nil {
			continue
		}
		if stateSet[pb.SessionState(row.State)] {
			filtered = append(filtered, row)
		}
	}
	return filtered
}

func sessionRowsNeedingPRAssociation(rows []*db.SessionWithRepo) []*models.Session {
	sessions := make([]*models.Session, 0, len(rows))
	for _, row := range rows {
		if row == nil || row.Session == nil {
			continue
		}
		sess := row.Session
		if sess.ArchivedAt == nil && sess.PRNumber == nil && sess.BranchName != "" {
			sessions = append(sessions, sess)
		}
	}
	return sessions
}

func shouldKeepCheckingCompositeOverReview(sess *pb.Session, chatStatus pb.ChatStatus) bool {
	if sess == nil {
		return false
	}
	if chatStatus == pb.ChatStatus_CHAT_STATUS_QUESTION || chatStatus == pb.ChatStatus_CHAT_STATUS_WORKING {
		return false
	}
	// Keep the visible list/status-view label consistent when a review-required
	// PR axis arrives while the persisted composite is still actively checking.
	return sess.GetDisplayStatus() == pb.DisplayStatus_DISPLAY_STATUS_REVIEW &&
		!sess.GetDisplayIsRepairing() &&
		sess.GetDisplayLabel() == "checking" &&
		sess.GetDisplaySpinner()
}

func (s *Server) AttachSession(ctx context.Context, req *connect.Request[pb.AttachSessionRequest], stream *connect.ServerStream[pb.AttachSessionResponse]) error {
	if req.Msg.Id == "" {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	// Verify the session exists.
	sess, err := s.sessions.Get(ctx, req.Msg.Id)
	if err != nil {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("session not found: %w", err))
	}

	// Send initial state to let the client know the session is valid.
	if err := stream.Send(&pb.AttachSessionResponse{
		Event: &pb.AttachSessionResponse_StateChange{
			StateChange: &pb.StateChange{
				PreviousState: pb.SessionState(sess.State),
				NewState:      pb.SessionState(sess.State),
			},
		},
	}); err != nil {
		return err
	}

	// If no Claude process is running, send ended and return.
	if sess.AgentSessionID == nil || !s.agent.IsRunning(*sess.AgentSessionID) {
		return stream.Send(&pb.AttachSessionResponse{
			Event: &pb.AttachSessionResponse_SessionEnded{
				SessionEnded: &pb.SessionEnded{
					FinalState: pb.SessionState(sess.State),
				},
			},
		})
	}

	claudeSessionID := *sess.AgentSessionID

	// Send existing ring buffer contents as initial burst.
	history := s.agent.History(claudeSessionID)
	for _, line := range history {
		if err := stream.Send(&pb.AttachSessionResponse{
			Event: &pb.AttachSessionResponse_OutputLine{
				OutputLine: &pb.OutputLine{
					Text:      line.Text,
					Timestamp: timestamppb.New(line.Timestamp),
				},
			},
		}); err != nil {
			return err
		}
	}

	// Subscribe to new output lines. Use a subscription-scoped context so the
	// subscriber is always deregistered from the broadcaster when this handler
	// returns — including on stream.Send errors (client disconnect), panics, or
	// normal process exit. Without this, a cleanup goroutine inside the
	// broadcaster relies on the handler's ctx being cancelled by the transport
	// layer, which is a leak risk under unusual client-disconnect semantics.
	subCtx, cancelSub := context.WithCancel(ctx)
	defer cancelSub()

	ch, err := s.agent.Subscribe(subCtx, claudeSessionID)
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("subscribe: %w", err))
	}

	// Stream new lines until process exits or client disconnects. On Send
	// error, return immediately; the deferred cancelSub triggers subscriber
	// removal so the broadcaster stops referencing this channel.
	for line := range ch {
		if err := stream.Send(&pb.AttachSessionResponse{
			Event: &pb.AttachSessionResponse_OutputLine{
				OutputLine: &pb.OutputLine{
					Text:      line.Text,
					Timestamp: timestamppb.New(line.Timestamp),
				},
			},
		}); err != nil {
			return err
		}
	}

	// Process exited — send session ended.
	// Re-fetch session to get latest state.
	sess, err = s.sessions.Get(ctx, req.Msg.Id)
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("get session: %w", err))
	}

	return stream.Send(&pb.AttachSessionResponse{
		Event: &pb.AttachSessionResponse_SessionEnded{
			SessionEnded: &pb.SessionEnded{
				FinalState: pb.SessionState(sess.State),
			},
		},
	})
}

func (s *Server) StopSession(ctx context.Context, req *connect.Request[pb.StopSessionRequest]) (*connect.Response[pb.StopSessionResponse], error) {
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	if err := s.lifecycle.StopSession(ctx, req.Msg.Id); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("stop session: %w", err))
	}

	// Notify the task orchestrator so it can unblock the repo's task queue.
	if s.completionNotifier != nil {
		s.completionNotifier.HandleSessionCompleted(ctx, req.Msg.Id, models.TaskMappingStatusFailed)
	}

	sess, err := s.sessions.Get(ctx, req.Msg.Id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get session: %w", err))
	}

	return connect.NewResponse(&pb.StopSessionResponse{Session: SessionToProto(sess)}), nil
}

// setSessionAutomation validates the id, toggles the session's automation flag,
// and returns the refreshed session proto. The action label is woven into the
// update error so callers surface a context-specific message (e.g. "pause
// session: ...", "resume session: ..."). State machine integration in Leg 6.
func (s *Server) setSessionAutomation(ctx context.Context, id string, enabled bool, action string) (*pb.Session, error) {
	if id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	if _, err := s.sessions.Update(ctx, id, db.UpdateSessionParams{
		AutomationEnabled: &enabled,
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("%s session: %w", action, err))
	}

	session, err := s.sessions.Get(ctx, id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get session: %w", err))
	}

	return SessionToProto(session), nil
}

func (s *Server) PauseSession(ctx context.Context, req *connect.Request[pb.PauseSessionRequest]) (*connect.Response[pb.PauseSessionResponse], error) {
	session, err := s.setSessionAutomation(ctx, req.Msg.Id, false, "pause")
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&pb.PauseSessionResponse{Session: session}), nil
}

func (s *Server) ResumeSession(ctx context.Context, req *connect.Request[pb.ResumeSessionRequest]) (*connect.Response[pb.ResumeSessionResponse], error) {
	session, err := s.setSessionAutomation(ctx, req.Msg.Id, true, "resume")
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&pb.ResumeSessionResponse{Session: session}), nil
}

func (s *Server) RetrySession(ctx context.Context, req *connect.Request[pb.RetrySessionRequest]) (*connect.Response[pb.RetrySessionResponse], error) {
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	// Retry clears blocked reason and re-enables automation. Full state machine in Leg 6.
	var nilStr *string
	blockedReason := &nilStr // double pointer: set to NULL
	automationEnabled := true
	if _, err := s.sessions.Update(ctx, req.Msg.Id, db.UpdateSessionParams{
		BlockedReason:     blockedReason,
		AutomationEnabled: &automationEnabled,
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("retry session: %w", err))
	}

	session, err := s.sessions.Get(ctx, req.Msg.Id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get session: %w", err))
	}

	return connect.NewResponse(&pb.RetrySessionResponse{Session: SessionToProto(session)}), nil
}

func (s *Server) CloseSession(ctx context.Context, req *connect.Request[pb.CloseSessionRequest]) (*connect.Response[pb.CloseSessionResponse], error) {
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	closedState := int(machine.Closed)
	if _, err := s.sessions.Update(ctx, req.Msg.Id, db.UpdateSessionParams{
		State: &closedState,
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("close session: %w", err))
	}

	// Notify the task orchestrator so it can unblock the repo's task queue.
	if s.completionNotifier != nil {
		s.completionNotifier.HandleSessionCompleted(ctx, req.Msg.Id, models.TaskMappingStatusFailed)
	}

	session, err := s.sessions.Get(ctx, req.Msg.Id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get session: %w", err))
	}

	return connect.NewResponse(&pb.CloseSessionResponse{Session: SessionToProto(session)}), nil
}

func (s *Server) MergeSession(ctx context.Context, req *connect.Request[pb.MergeSessionRequest]) (*connect.Response[pb.MergeSessionResponse], error) {
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	sess, err := s.sessions.Get(ctx, req.Msg.Id)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session not found: %w", err))
	}

	// Guard against merging while CI is still red or reviews are outstanding.
	// The webhook-driven tracker is authoritative; fall back to allowing the
	// merge if the tracker has no entry (gh will itself reject an unmergeable PR).
	if sess.PRNumber != nil && s.displayTracker != nil {
		if e := s.displayTracker.Get(sess.ID); e != nil && e.Status != vcs.DisplayStatusPassing {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("PR is not passing"))
		}
	}

	repo, err := s.repos.Get(ctx, sess.RepoID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get repo: %w", err))
	}

	// Sync the branch the PR actually targets, which may differ from the
	// repo's default branch (e.g. a PR into `develop`).
	baseBranch := sess.BaseBranch
	if baseBranch == "" {
		baseBranch = repo.DefaultBaseBranch
	}

	if sess.PRNumber == nil {
		// Local-only merge path: no PR on a remote, just a local session
		// branch that needs to land on the repo's base. The session's
		// branch name is authoritative.
		if sess.BranchName == "" {
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("session has no PR and no branch name to merge"))
		}
		if err := s.worktrees.MergeLocalBranch(ctx, repo.LocalPath, baseBranch, sess.BranchName, string(repo.MergeStrategy)); err != nil {
			if errors.Is(err, gitpkg.ErrBaseBranchNotReady) || errors.Is(err, gitpkg.ErrMergeConflict) {
				return nil, connect.NewError(connect.CodeFailedPrecondition, err)
			}
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("local merge: %w", err))
		}
	} else {
		// Verify the main repo can be safely fast-forwarded after the merge.
		// Doing this BEFORE `gh pr merge` means we never end up with a
		// successful server-side merge paired with a local repo the user would
		// need to untangle by hand. The error message is safe to surface.
		if err := s.worktrees.EnsureBaseBranchReadyForSync(ctx, repo.LocalPath, baseBranch); err != nil {
			if errors.Is(err, gitpkg.ErrBaseBranchNotReady) {
				return nil, connect.NewError(connect.CodeFailedPrecondition, err)
			}
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("pre-merge sync check: %w", err))
		}

		strategy, err := mergepolicy.ResolveStrategy(ctx, s.provider, repo.OriginURL, string(repo.MergeStrategy))
		if err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}

		if err := s.provider.MergePR(ctx, repo.OriginURL, *sess.PRNumber, strategy); err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("merge PR: %w", err))
		}

		// Verify the merge actually landed on origin/<base>. Catches
		// post-merge history rewrites, branch-protection races, and any
		// case where gh reported "merged" but the commit isn't where we
		// expect — the class of bug that previously went undetected for
		// days (madverts-core PR #2222).
		if err := mergepolicy.VerifyOnBase(ctx, s.provider, s.worktrees,
			repo.LocalPath, repo.OriginURL, baseBranch, *sess.PRNumber,
		); err != nil {
			return nil, connect.NewError(connect.CodeInternal,
				fmt.Errorf("merge verification failed: %w", err))
		}

		// Pull the merged commit into the user's local main repo so subsequent
		// worktrees, branches, and manual operations on <base> start from the
		// post-merge tip. Sync failures are logged — the server-side merge
		// is already verified to be on origin/<base>.
		if err := s.worktrees.SyncBaseBranch(ctx, repo.LocalPath, baseBranch); err != nil {
			s.logger.Warn().Err(err).
				Str("repo", repo.LocalPath).
				Str("base", baseBranch).
				Msg("post-merge sync of local base branch failed; run `git fetch` + fast-forward manually")
		}
	}

	// Re-fetch so callers see any server-side side effects; the state
	// transition to Merged itself arrives asynchronously via the PR-merged
	// webhook handled by session.Dispatcher.
	sess, err = s.sessions.Get(ctx, req.Msg.Id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get session: %w", err))
	}

	return connect.NewResponse(&pb.MergeSessionResponse{Session: SessionToProto(sess)}), nil
}

func (s *Server) RemoveSession(ctx context.Context, req *connect.Request[pb.RemoveSessionRequest]) (*connect.Response[pb.RemoveSessionResponse], error) {
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	// Get session first to find branch name and repo for git cleanup.
	sess, err := s.sessions.Get(ctx, req.Msg.Id)
	if err != nil {
		// If not found, nothing to delete.
		return connect.NewResponse(&pb.RemoveSessionResponse{}), nil
	}

	// Notify the task orchestrator BEFORE deleting, because sessions.Delete
	// nullifies task_mappings.session_id, making GetBySessionID unable to
	// find the mapping afterward.
	if s.completionNotifier != nil {
		s.completionNotifier.HandleSessionCompleted(ctx, req.Msg.Id, models.TaskMappingStatusFailed)
	}

	// Best-effort git cleanup: delete branch + prune worktree.
	if sess.RepoID != "" && sess.BranchName != "" {
		repo, err := s.repos.Get(ctx, sess.RepoID)
		if err == nil {
			_ = s.worktrees.EmptyTrash(ctx, repo.LocalPath, []string{sess.BranchName})
		}
	}

	// Delete from DB.
	if err := s.sessions.Delete(ctx, req.Msg.Id); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("remove session: %w", err))
	}
	if s.onSessionDeleted != nil {
		s.onSessionDeleted(ctx, req.Msg.Id)
	}

	return connect.NewResponse(&pb.RemoveSessionResponse{}), nil
}

func (s *Server) UpdateSession(ctx context.Context, req *connect.Request[pb.UpdateSessionRequest]) (*connect.Response[pb.UpdateSessionResponse], error) {
	msg := req.Msg
	if msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	params := db.UpdateSessionParams{}
	if msg.Title != nil {
		title := strings.TrimSpace(*msg.Title)
		if title == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("title cannot be empty"))
		}
		params.Title = &title
	}

	sess, err := s.sessions.Update(ctx, msg.Id, params)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("update session: %w", err))
	}

	// Best-effort: update the PR title if this session has a PR.
	if msg.Title != nil && sess.PRNumber != nil && *sess.PRNumber > 0 {
		repo, repoErr := s.repos.Get(ctx, sess.RepoID)
		if repoErr == nil && repo.OriginURL != "" {
			if prErr := s.provider.UpdatePRTitle(ctx, repo.OriginURL, *sess.PRNumber, *params.Title); prErr != nil {
				s.logger.Warn().Err(prErr).
					Str("session", sess.ID).
					Int("pr", *sess.PRNumber).
					Msg("failed to update PR title (best-effort)")
			}
		}
	}

	p := SessionToProto(sess)
	if repo, err := s.repos.Get(ctx, sess.RepoID); err == nil {
		p.RepoDisplayName = repo.DisplayName
		p.RepoOriginUrl = CanonicalRepoOriginURL(repo.OriginURL)
	}

	// Propagate the update (e.g. a rename) to the cloud/web via the upstream
	// stream, matching LinkSessionPR/Archive/Resurrect. Without this the local
	// DB and TUI reflect the new title but bosso never sees it until the next
	// full daemon snapshot.
	if s.onSessionUpdated != nil {
		s.onSessionUpdated(ctx, p)
	}

	return connect.NewResponse(&pb.UpdateSessionResponse{Session: p}), nil
}

func (s *Server) LinkSessionPR(ctx context.Context, req *connect.Request[pb.LinkSessionPRRequest]) (*connect.Response[pb.LinkSessionPRResponse], error) {
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}
	if strings.TrimSpace(req.Msg.Pr) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("pr is required"))
	}

	sess, err := s.lifecycle.LinkPR(ctx, req.Msg.Id, req.Msg.Pr)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("link PR: %w", err))
	}

	p := s.sessionProtoWithRepo(ctx, sess)
	if s.onSessionUpdated != nil {
		s.onSessionUpdated(ctx, p)
	}

	return connect.NewResponse(&pb.LinkSessionPRResponse{Session: p}), nil
}

// --- Archive / Resurrect ---

// ArchiveSessionAndNotify performs the full session archive (stop processes,
// kill tmux, remove worktree, set archived_at) and emits the stream update so
// the TUI/bosso read-model reflect the session immediately. It is idempotent:
// a genuinely missing session emits KIND_DELETED, while an already-archived
// session emits an update (keeping it archived/in trash). Both return nil.
// Shared by the ArchiveSession RPC and the dependabot auto-archive path (BOS-101).
func (s *Server) ArchiveSessionAndNotify(ctx context.Context, id string) error {
	if err := s.lifecycle.ArchiveSession(ctx, id); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("archive session: %w", err)
	}
	// A sql.ErrNoRows from ArchiveSession is ambiguous: the session row may be
	// gone, or it may exist but already be archived (the archived_at IS NULL
	// update matched no rows). The Get below is the source of truth — only a
	// genuinely missing row emits the delete delta. An already-archived session
	// must stay archived/in trash, not be dropped from the read model (BOS-101).
	sess, err := s.sessions.Get(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if s.onSessionDeleted != nil {
				s.onSessionDeleted(ctx, id)
			}
			return nil
		}
		return fmt.Errorf("get session: %w", err)
	}
	p := s.sessionProtoWithRepo(ctx, sess)
	if s.onSessionUpdated != nil {
		s.onSessionUpdated(ctx, p)
	}
	return nil
}

func (s *Server) ArchiveSession(ctx context.Context, req *connect.Request[pb.ArchiveSessionRequest]) (*connect.Response[pb.ArchiveSessionResponse], error) {
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}
	if err := s.ArchiveSessionAndNotify(ctx, req.Msg.Id); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	sess, err := s.sessions.Get(ctx, req.Msg.Id)
	if err != nil {
		return connect.NewResponse(&pb.ArchiveSessionResponse{Session: &pb.Session{Id: req.Msg.Id}}), nil
	}
	return connect.NewResponse(&pb.ArchiveSessionResponse{Session: s.sessionProtoWithRepo(ctx, sess)}), nil
}

func (s *Server) ResurrectSession(ctx context.Context, req *connect.Request[pb.ResurrectSessionRequest]) (*connect.Response[pb.ResurrectSessionResponse], error) {
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	if err := s.lifecycle.ResurrectSession(ctx, req.Msg.Id); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("resurrect session: %w", err))
	}

	sess, err := s.sessions.Get(ctx, req.Msg.Id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get session: %w", err))
	}

	p := s.sessionProtoWithRepo(ctx, sess)
	if s.onSessionUpdated != nil {
		s.onSessionUpdated(ctx, p)
	}

	return connect.NewResponse(&pb.ResurrectSessionResponse{Session: p}), nil
}

func (s *Server) sessionProtoWithRepo(ctx context.Context, sess *models.Session) *pb.Session {
	p := SessionToProto(sess)
	if sess.RepoID != "" {
		if repo, err := s.repos.Get(ctx, sess.RepoID); err == nil && repo != nil {
			p.RepoDisplayName = repo.DisplayName
			p.RepoOriginUrl = CanonicalRepoOriginURL(repo.OriginURL)
		}
	}
	return p
}

func (s *Server) EmptyTrash(ctx context.Context, req *connect.Request[pb.EmptyTrashRequest]) (*connect.Response[pb.EmptyTrashResponse], error) {
	repoID := "" // all repos
	archived, err := s.sessions.ListArchived(ctx, repoID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list archived: %w", err))
	}

	olderThan := protoToTimestamp(req.Msg.OlderThan)

	// Collect branches to delete, grouped by repo, and delete DB records.
	repoBranches := make(map[string][]string) // repoID -> branch names
	deleted := int32(0)
	for _, sess := range archived {
		if olderThan != nil && sess.ArchivedAt != nil && sess.ArchivedAt.After(*olderThan) {
			continue
		}
		if sess.RepoID != "" && sess.BranchName != "" {
			repoBranches[sess.RepoID] = append(repoBranches[sess.RepoID], sess.BranchName)
		}
		if err := s.sessions.Delete(ctx, sess.ID); err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("delete archived session %s: %w", sess.ID, err))
		}
		if s.onSessionDeleted != nil {
			s.onSessionDeleted(ctx, sess.ID)
		}
		deleted++
	}

	// Best-effort git cleanup: delete branches and prune worktrees per repo.
	for repoID, branches := range repoBranches {
		repo, err := s.repos.Get(ctx, repoID)
		if err != nil {
			continue
		}
		_ = s.worktrees.EmptyTrash(ctx, repo.LocalPath, branches)
	}

	return connect.NewResponse(&pb.EmptyTrashResponse{DeletedCount: deleted}), nil
}

// --- Claude Chat Tracking ---

func (s *Server) RecordChat(ctx context.Context, req *connect.Request[pb.RecordChatRequest]) (*connect.Response[pb.RecordChatResponse], error) {
	msg := req.Msg
	if msg.SessionId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("session_id is required"))
	}
	if msg.AgentSessionId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("agent_session_id is required"))
	}

	// Idempotent: reuse the existing row if one already exists for this
	// agent_session_id (resume case), otherwise insert a new one.
	chat, err := s.agentChats.GetByAgentSessionID(ctx, msg.AgentSessionId)
	if err == nil && chat != nil && chat.SessionID != msg.SessionId {
		// The chat exists but under a different session than the caller
		// authorized. Reject rather than resume it into the wrong session —
		// the bosso proxy routes by the session's owning daemon, so a
		// mismatch here means the requested session_id never owned this chat.
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("chat does not belong to session %q", msg.SessionId))
	}
	if err != nil {
		// Prefer an explicit override on the request, falling back to the
		// parent session's agent_name. Without the fallback, the SQLite
		// store would default to "claude" regardless of which agent
		// actually owns the session. Empty override + missing session →
		// empty AgentName, which the store handles via its existing
		// ""→"claude" default.
		agentName := msg.GetAgentName()
		if agentName == "" {
			if sess, sErr := s.sessions.Get(ctx, msg.SessionId); sErr == nil {
				agentName = sess.AgentName
			}
		}
		chat, err = s.agentChats.Create(ctx, db.CreateAgentChatParams{
			SessionID:      msg.SessionId,
			AgentSessionID: msg.AgentSessionId,
			AgentName:      agentName,
			Title:          msg.Title,
		})
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("record chat: %w", err))
		}
	}

	// Ensure a tmux session is alive to host the chat. The tmux server is
	// independent of bossd, so this lets the chat survive bossd restarts —
	// reattaching just reconnects to the existing tmux session.
	if err := s.ensureChatTmuxSession(ctx, chat, msg.GetResume()); err != nil {
		s.logger.Warn().Err(err).
			Str("agentSessionID", chat.AgentSessionID).
			Msg("failed to ensure tmux session for chat")
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("ensure chat tmux session: %w", err))
	} else {
		// Refetch so the response carries the persisted tmux_session_name.
		if refreshed, refreshErr := s.agentChats.GetByAgentSessionID(ctx, chat.AgentSessionID); refreshErr == nil {
			chat = refreshed
		}
	}

	return connect.NewResponse(&pb.RecordChatResponse{Chat: agentChatToProto(chat)}), nil
}

// ensureChatTmuxSession guarantees that a `tmux` session named per
// tmux.ChatSessionName exists and is hosting the chat's agent (claude,
// codex, …) for the given chat. Idempotent: a no-op if the session is
// already alive. Persists the resolved name onto the chat row so kill/list
// paths can find it later.
func (s *Server) ensureChatTmuxSession(ctx context.Context, chat *models.AgentChat, resume bool) error {
	if s.tmux == nil || !s.tmux.Available(ctx) {
		return nil
	}
	sess, err := s.sessions.Get(ctx, chat.SessionID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}
	tmuxName := tmux.ChatSessionName(sess.RepoID, chat.AgentSessionID)

	deps := spawnDeps{
		Tmux:        liveTmuxSpawner{c: s.tmux},
		Transcripts: liveTranscriptOracle{clients: s.agentClients},
		Argv:        liveArgvBuilder{clients: s.agentClients},
	}
	if s.agentClients != nil {
		deps.Resolver = liveInteractiveSessionResolver{clients: s.agentClients}
	}
	if resume {
		if _, reason, backfillErr := s.backfillCodexProviderSessionID(ctx, chat, sess.WorktreePath, deps.Resolver); backfillErr != nil {
			return backfillErr
		} else if reason != "" {
			s.logger.Warn().
				Str("agent_session_id", chat.AgentSessionID).
				Str("agent", chat.AgentName).
				Str("reason", reason).
				Msg("legacy provider session id discovery ambiguous before attach")
		}
	}
	mcpConfigPath := session.SessionMcpConfigPath(sess, chat.AgentSessionID, chat.AgentName)
	result, err := spawnChatTmux(ctx, deps, spawnInput{
		Chat:               chat,
		WorktreePath:       sess.WorktreePath,
		TmuxName:           tmuxName,
		ForceFresh:         !resume,
		AppendSystemPrompt: session.AppendSystemPromptFor(sess, chat.AgentSessionID, chat.AgentName, mcpConfigPath),
		SessionEnv:         session.ManagedSessionEnv(sess, chat.AgentSessionID, chat.AgentName),
		Model:              sess.Model,
		McpConfigPath:      mcpConfigPath,
	})
	if err != nil {
		return err
	}

	if result.Outcome == OutcomeFreshFallback && !result.LaunchedAt.IsZero() && result.ProviderSessionID == "" {
		ev := s.logger.Warn().
			Str("agent_session_id", chat.AgentSessionID).
			Str("agent", chat.AgentName)
		if result.DiscoveryAmbiguous {
			ev = ev.Bool("ambiguous", true)
		}
		if result.DiscoveryReason != "" {
			ev = ev.Str("reason", result.DiscoveryReason)
		}
		ev.Msg("interactive provider session id not discovered after launch")
		if deps.Resolver != nil && !result.DiscoveryAmbiguous {
			s.discoverProviderSessionIDInBackground(chat, sess.WorktreePath, result.LaunchedAt, deps.Resolver)
		}
	}
	if result.ProviderSessionID != "" && (chat.ProviderSessionID == nil || *chat.ProviderSessionID != result.ProviderSessionID) {
		providerID := result.ProviderSessionID
		if err := s.agentChats.UpdateProviderSessionID(ctx, chat.AgentSessionID, &providerID); err != nil {
			return fmt.Errorf("persist provider session id: %w", err)
		}
		chat.ProviderSessionID = &providerID
	}

	if chat.TmuxSessionName == nil || *chat.TmuxSessionName != tmuxName {
		if err := s.agentChats.UpdateTmuxSessionName(ctx, chat.AgentSessionID, &tmuxName); err != nil {
			return fmt.Errorf("persist tmux session name: %w", err)
		}
	}
	return nil
}

func (s *Server) backfillCodexProviderSessionID(ctx context.Context, chat *models.AgentChat, worktreePath string, resolver interactiveSessionResolver) (bool, string, error) {
	if chat == nil || chat.AgentName != "codex" || chat.ProviderSessionID != nil || chat.CreatedAt.IsZero() || resolver == nil {
		return false, "", nil
	}
	legacyLaunchedAfter := chat.CreatedAt.Add(-5 * time.Minute)
	resolution, err := resolver.ResolveInteractiveSessionID(ctx, chat.AgentName, worktreePath, chat.AgentSessionID, legacyLaunchedAfter, chat.CreatedAt, true)
	if err != nil {
		return false, "", err
	}
	if resolution.SessionID == "" {
		if resolution.Ambiguous {
			return false, resolution.Reason, nil
		}
		return false, "", nil
	}
	providerID := resolution.SessionID
	if err := s.agentChats.UpdateProviderSessionID(ctx, chat.AgentSessionID, &providerID); err != nil {
		return false, "", fmt.Errorf("persist provider session id: %w", err)
	}
	chat.ProviderSessionID = &providerID
	return true, "", nil
}

func (s *Server) discoverProviderSessionIDInBackground(chat *models.AgentChat, worktreePath string, launchedAt time.Time, resolver interactiveSessionResolver) {
	if chat == nil || resolver == nil || launchedAt.IsZero() {
		return
	}
	agentSessionID := chat.AgentSessionID
	agentName := chat.AgentName
	if agentName == "" {
		agentName = defaultLegacyAgent
	}
	timeout := providerSessionIDBackgroundDiscoveryTimeout
	interval := providerSessionIDBackgroundDiscoveryPollInterval
	if timeout <= 0 || interval <= 0 {
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			resolution, err := resolver.ResolveInteractiveSessionID(ctx, agentName, worktreePath, agentSessionID, launchedAt, time.Time{}, false)
			if err != nil {
				if ctx.Err() == nil {
					s.logger.Warn().Err(err).
						Str("agent_session_id", agentSessionID).
						Str("agent", agentName).
						Msg("background provider session id discovery failed")
				}
				return
			}
			if resolution.SessionID != "" {
				providerID := resolution.SessionID
				if err := s.agentChats.UpdateProviderSessionID(ctx, agentSessionID, &providerID); err != nil {
					s.logger.Warn().Err(err).
						Str("agent_session_id", agentSessionID).
						Str("provider_session_id", providerID).
						Msg("failed to persist background provider session id")
				}
				return
			}
			if resolution.Ambiguous {
				s.logger.Warn().
					Str("agent_session_id", agentSessionID).
					Str("agent", agentName).
					Str("reason", resolution.Reason).
					Msg("background provider session id discovery ambiguous")
				return
			}

			select {
			case <-ctx.Done():
				s.logger.Warn().
					Str("agent_session_id", agentSessionID).
					Str("agent", agentName).
					Msg("background provider session id discovery timed out")
				return
			case <-ticker.C:
			}
		}
	}()
}

func (s *Server) ListChats(ctx context.Context, req *connect.Request[pb.ListChatsRequest]) (*connect.Response[pb.ListChatsResponse], error) {
	if req.Msg.SessionId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("session_id is required"))
	}

	chats, err := s.agentChats.ListBySession(ctx, req.Msg.SessionId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list chats: %w", err))
	}

	pbChats := make([]*pb.ClaudeChat, len(chats))
	for i, c := range chats {
		pbChats[i] = agentChatToProto(c)
	}

	return connect.NewResponse(&pb.ListChatsResponse{Chats: pbChats}), nil
}

func (s *Server) UpdateChatTitle(ctx context.Context, req *connect.Request[pb.UpdateChatTitleRequest]) (*connect.Response[pb.UpdateChatTitleResponse], error) {
	msg := req.Msg
	if msg.AgentSessionId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("agent_session_id is required"))
	}
	if msg.Title == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("title is required"))
	}

	if err := s.agentChats.UpdateTitleByAgentSessionID(ctx, msg.AgentSessionId, msg.Title); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("update chat title: %w", err))
	}

	return connect.NewResponse(&pb.UpdateChatTitleResponse{}), nil
}

func (s *Server) killChatTmuxSession(ctx context.Context, chat *models.AgentChat) {
	if chat == nil || chat.TmuxSessionName == nil || *chat.TmuxSessionName == "" || s.tmux == nil {
		return
	}

	tmuxName := *chat.TmuxSessionName
	if err := s.tmux.KillSession(ctx, tmuxName); err != nil {
		s.logger.Warn().Err(err).
			Str("session", chat.SessionID).
			Str("agentSessionID", chat.AgentSessionID).
			Str("tmuxSession", tmuxName).
			Msg("failed to kill chat tmux session during delete")
	}
}

func isAgentChatNotFound(err error) bool {
	return errors.Is(err, db.ErrAgentChatNotFound)
}

func (s *Server) DeleteChat(ctx context.Context, req *connect.Request[pb.DeleteChatRequest]) (*connect.Response[pb.DeleteChatResponse], error) {
	if req.Msg.AgentSessionId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("agent_session_id is required"))
	}

	chat, err := s.agentChats.GetByAgentSessionID(ctx, req.Msg.AgentSessionId)
	if err != nil && !isAgentChatNotFound(err) {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get chat: %w", err))
	}
	// Enforce session scoping when the caller provided a session_id: the chat
	// must belong to that session. This makes bosso's session-level authz
	// (it routes by the session's owning daemon) actually binding rather than
	// advisory — without it the daemon would delete any chat with this
	// agent_session_id regardless of which session was authorized. A nil chat
	// is left to the idempotent no-op delete below.
	if req.Msg.SessionId != "" && chat != nil && chat.SessionID != req.Msg.SessionId {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("chat does not belong to session %q", req.Msg.SessionId))
	}
	s.killChatTmuxSession(ctx, chat)

	if err := s.agentChats.DeleteByAgentSessionID(ctx, req.Msg.AgentSessionId); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("delete chat: %w", err))
	}

	// Also clean up any cached status for this chat.
	if s.chatStatus != nil {
		s.chatStatus.Remove(req.Msg.AgentSessionId)
	}

	return connect.NewResponse(&pb.DeleteChatResponse{}), nil
}

// --- Chat Status ---

func (s *Server) ReportChatStatus(ctx context.Context, req *connect.Request[pb.ReportChatStatusRequest]) (*connect.Response[pb.ReportChatStatusResponse], error) {
	if s.chatStatus == nil {
		return connect.NewResponse(&pb.ReportChatStatusResponse{}), nil
	}
	for _, r := range req.Msg.Reports {
		if r.AgentSessionId == "" {
			continue
		}
		// Tmux-tracked chats are owned by the daemon's TmuxStatusPoller,
		// which captures the live pane every 3 s. The boss CLI heartbeat
		// reports the same chat from its own PTY ring buffer (or worse, a
		// stale `tmux attach` child whose Process.done has closed and
		// reports STOPPED forever) — accepting both writers makes the
		// tracker oscillate and the session label flash between
		// "? question" and "draft". Drop client reports for such chats
		// and let the poller be the single source of truth.
		if s.agentChats != nil {
			if chat, err := s.agentChats.GetByAgentSessionID(ctx, r.AgentSessionId); err == nil &&
				chat != nil && chat.TmuxSessionName != nil && *chat.TmuxSessionName != "" {
				continue
			}
		}
		var lastOutputAt time.Time
		if r.LastOutputAt != nil {
			lastOutputAt = r.LastOutputAt.AsTime()
		}
		s.chatStatus.Update(r.AgentSessionId, r.Status, lastOutputAt)
	}
	return connect.NewResponse(&pb.ReportChatStatusResponse{}), nil
}

func (s *Server) GetChatStatuses(ctx context.Context, req *connect.Request[pb.GetChatStatusesRequest]) (*connect.Response[pb.GetChatStatusesResponse], error) {
	if req.Msg.SessionId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("session_id is required"))
	}

	// Look up chats for this session.
	chats, err := s.agentChats.ListBySession(ctx, req.Msg.SessionId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list chats: %w", err))
	}

	if s.chatStatus == nil || len(chats) == 0 {
		return connect.NewResponse(&pb.GetChatStatusesResponse{}), nil
	}

	agentSessionIDs := make([]string, len(chats))
	for i, c := range chats {
		agentSessionIDs[i] = c.AgentSessionID
	}

	entries := s.chatStatus.GetBatch(agentSessionIDs)

	// Build a map of which chats have live tmux sessions (per-chat liveness).
	tmuxAlive := make(map[string]bool, len(chats))
	for _, c := range chats {
		if c.TmuxSessionName != nil && *c.TmuxSessionName != "" &&
			s.lifecycle.IsTmuxSessionAlive(ctx, *c.TmuxSessionName) {
			tmuxAlive[c.AgentSessionID] = true
		}
	}

	statuses := make([]*pb.ChatStatusEntry, 0, len(entries))
	for id, e := range entries {
		entry := &pb.ChatStatusEntry{
			AgentSessionId: id,
			Status:         e.Status,
		}
		if !e.LastOutputAt.IsZero() {
			entry.LastOutputAt = timestamppb.New(e.LastOutputAt)
		}
		// When heartbeats report stopped, fall back to per-chat tmux liveness.
		// Default to IDLE (not WORKING) — the poller will upgrade to WORKING
		// within 3s if Claude is actually producing output.
		if entry.Status == pb.ChatStatus_CHAT_STATUS_STOPPED && tmuxAlive[id] {
			entry.Status = pb.ChatStatus_CHAT_STATUS_IDLE
		}
		statuses = append(statuses, entry)
	}

	// If any tmux-active chat had no heartbeat entry at all, add a synthetic one.
	for _, c := range chats {
		if !tmuxAlive[c.AgentSessionID] {
			continue
		}
		found := false
		for _, st := range statuses {
			if st.AgentSessionId == c.AgentSessionID {
				found = true
				break
			}
		}
		if !found {
			statuses = append(statuses, &pb.ChatStatusEntry{
				AgentSessionId: c.AgentSessionID,
				Status:         pb.ChatStatus_CHAT_STATUS_IDLE,
			})
		}
	}

	return connect.NewResponse(&pb.GetChatStatusesResponse{Statuses: statuses}), nil
}

func (s *Server) GetSessionStatuses(ctx context.Context, req *connect.Request[pb.GetSessionStatusesRequest]) (*connect.Response[pb.GetSessionStatusesResponse], error) {
	if s.chatStatus == nil {
		return connect.NewResponse(&pb.GetSessionStatusesResponse{}), nil
	}

	statuses := make([]*pb.SessionStatusEntry, 0, len(req.Msg.SessionIds))

	for _, sessionID := range req.Msg.SessionIds {
		best, ok := s.chatStatusForSession(ctx, sessionID)
		if !ok {
			continue
		}

		statuses = append(statuses, &pb.SessionStatusEntry{
			SessionId: sessionID,
			Status:    best,
		})
	}

	return connect.NewResponse(&pb.GetSessionStatusesResponse{Statuses: statuses}), nil
}

func (s *Server) chatsBySessionForStatuses(ctx context.Context, sessionIDs []string) (map[string][]*models.AgentChat, bool) {
	if s.chatStatus == nil || s.agentChats == nil {
		return nil, true
	}
	chatsBySession, err := s.agentChats.ListBySessions(ctx, sessionIDs)
	if err != nil {
		s.logger.Warn().Err(err).Msg("list chats for session statuses")
		return nil, false
	}
	return chatsBySession, true
}

func (s *Server) chatStatusForSession(ctx context.Context, sessionID string) (pb.ChatStatus, bool) {
	if s.chatStatus == nil || s.agentChats == nil {
		return pb.ChatStatus_CHAT_STATUS_STOPPED, true
	}
	chats, err := s.agentChats.ListBySession(ctx, sessionID)
	if err != nil {
		s.logger.Warn().Err(err).Str("session_id", sessionID).Msg("list chats for session status")
		return pb.ChatStatus_CHAT_STATUS_STOPPED, false
	}
	return s.chatStatusFromSessionChats(ctx, chats, true)
}

func (s *Server) chatStatusFromSessionChats(ctx context.Context, chats []*models.AgentChat, loaded bool) (pb.ChatStatus, bool) {
	if s.chatStatus == nil || s.agentChats == nil {
		return pb.ChatStatus_CHAT_STATUS_STOPPED, true
	}
	if !loaded {
		return pb.ChatStatus_CHAT_STATUS_STOPPED, false
	}
	if len(chats) == 0 {
		return pb.ChatStatus_CHAT_STATUS_STOPPED, true
	}

	agentSessionIDs := make([]string, len(chats))
	for i, c := range chats {
		agentSessionIDs[i] = c.AgentSessionID
	}
	entries := s.chatStatus.GetBatch(agentSessionIDs)

	// Compute best status: question > working > idle > stopped.
	best := pb.ChatStatus_CHAT_STATUS_STOPPED
	for _, e := range entries {
		if e.Status == pb.ChatStatus_CHAT_STATUS_QUESTION {
			best = pb.ChatStatus_CHAT_STATUS_QUESTION
			break
		}
		if e.Status == pb.ChatStatus_CHAT_STATUS_WORKING && best != pb.ChatStatus_CHAT_STATUS_QUESTION {
			best = pb.ChatStatus_CHAT_STATUS_WORKING
		}
		if e.Status == pb.ChatStatus_CHAT_STATUS_IDLE && best == pb.ChatStatus_CHAT_STATUS_STOPPED {
			best = pb.ChatStatus_CHAT_STATUS_IDLE
		}
	}
	// When heartbeats report stopped, fall back to per-chat tmux liveness.
	// Default to IDLE (not WORKING) — the poller will upgrade to WORKING
	// within 3s if Claude is actually producing output.
	if best == pb.ChatStatus_CHAT_STATUS_STOPPED && s.lifecycle != nil {
		for _, c := range chats {
			if c.TmuxSessionName != nil && *c.TmuxSessionName != "" &&
				s.lifecycle.IsTmuxSessionAlive(ctx, *c.TmuxSessionName) {
				best = pb.ChatStatus_CHAT_STATUS_IDLE
				break
			}
		}
	}
	return best, true
}

// --- Context Resolution ---

func (s *Server) ResolveContext(ctx context.Context, req *connect.Request[pb.ResolveContextRequest]) (*connect.Response[pb.ResolveContextResponse], error) {
	wd := req.Msg.WorkingDirectory
	if wd == "" {
		var err error
		wd, err = os.Getwd()
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get working directory: %w", err))
		}
	}

	// Resolve to absolute path.
	absWD, err := filepath.Abs(wd)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("resolve absolute path: %w", err))
	}

	resp := &pb.ResolveContextResponse{}

	// Check if inside a session worktree first (more specific match).
	repos, err := s.repos.List(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list repos: %w", err))
	}

	for _, repo := range repos {
		sessions, err := s.sessions.List(ctx, repo.ID)
		if err != nil {
			continue
		}
		for _, sess := range sessions {
			if sess.WorktreePath != "" && isSubdirOf(absWD, sess.WorktreePath) {
				resp.Repo = repoToProto(repo)
				resp.Session = SessionToProto(sess)
				return connect.NewResponse(resp), nil
			}
		}

		// Check if inside the repo root.
		if isSubdirOf(absWD, repo.LocalPath) {
			resp.Repo = repoToProto(repo)
			return connect.NewResponse(resp), nil
		}
	}

	// Not inside any registered repo or worktree.
	return connect.NewResponse(resp), nil
}

// --- Auth Change Notification ---

func (s *Server) NotifyAuthChange(ctx context.Context, req *connect.Request[pb.NotifyAuthChangeRequest]) (*connect.Response[pb.NotifyAuthChangeResponse], error) {
	action := req.Msg.Action
	if action != "login" && action != "logout" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("action must be \"login\" or \"logout\""))
	}

	if s.authNotifier == nil {
		// No orchestrator configured — nothing to do.
		return connect.NewResponse(&pb.NotifyAuthChangeResponse{}), nil
	}

	switch action {
	case "login":
		repos, err := s.repos.List(ctx)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list repos: %w", err))
		}
		repoIDs := make([]string, len(repos))
		for i, r := range repos {
			repoIDs[i] = r.ID
		}
		if err := s.authNotifier.NotifyLogin(ctx, repoIDs); err != nil {
			s.logger.Warn().Err(err).Msg("upstream connect after login failed")
			// Non-fatal: daemon still works in local mode.
		}
	case "logout":
		s.authNotifier.NotifyLogout()
	}

	return connect.NewResponse(&pb.NotifyAuthChangeResponse{}), nil
}

// isSubdirOf checks if child is the same as or a subdirectory of parent.
func isSubdirOf(child, parent string) bool {
	// Clean both paths for consistent comparison.
	child = filepath.Clean(child)
	parent = filepath.Clean(parent)

	if child == parent {
		return true
	}

	// Ensure parent ends with separator for prefix matching.
	parentPrefix := parent + string(filepath.Separator)
	return len(child) > len(parentPrefix) && child[:len(parentPrefix)] == parentPrefix
}
