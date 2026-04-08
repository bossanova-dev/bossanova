package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"errors"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/claude"
	"github.com/recurser/bossd/internal/db"
	gitpkg "github.com/recurser/bossd/internal/git"
	"github.com/recurser/bossd/internal/plugin"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/status"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// DefaultSocketPath returns the default Unix socket path for the daemon.
// On macOS: ~/Library/Application Support/bossanova/bossd.sock
func DefaultSocketPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	dir := filepath.Join(home, "Library", "Application Support", "bossanova")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create socket dir: %w", err)
	}
	return filepath.Join(dir, "bossd.sock"), nil
}

// Server wraps the ConnectRPC DaemonService handler and a Unix socket listener.
type Server struct {
	repos              db.RepoStore
	sessions           db.SessionStore
	attempts           db.AttemptStore
	claudeChats        db.ClaudeChatStore
	workflows          db.WorkflowStore
	chatStatus         *status.Tracker
	prDisplay          *status.PRTracker
	lifecycle          *session.Lifecycle
	claude             claude.ClaudeRunner
	worktrees          gitpkg.WorktreeManager
	provider           vcs.Provider
	pluginHost         *plugin.Host
	completionNotifier session.SessionCompletionNotifier
	logger             zerolog.Logger
	listener           net.Listener
	srv                *http.Server

	bossanovav1connect.UnimplementedDaemonServiceHandler
}

// Config holds all dependencies for creating a new Server.
type Config struct {
	Repos              db.RepoStore
	Sessions           db.SessionStore
	Attempts           db.AttemptStore
	ClaudeChats        db.ClaudeChatStore
	Workflows          db.WorkflowStore
	ChatStatus         *status.Tracker
	PRDisplay          *status.PRTracker
	Lifecycle          *session.Lifecycle
	Claude             claude.ClaudeRunner
	Worktrees          gitpkg.WorktreeManager
	Provider           vcs.Provider
	PluginHost         *plugin.Host
	CompletionNotifier session.SessionCompletionNotifier // optional, may be nil
	Logger             zerolog.Logger
}

// New creates a new Server wired to the given stores and lifecycle orchestrator.
func New(cfg Config) *Server {
	return &Server{
		repos:              cfg.Repos,
		sessions:           cfg.Sessions,
		attempts:           cfg.Attempts,
		claudeChats:        cfg.ClaudeChats,
		workflows:          cfg.Workflows,
		chatStatus:         cfg.ChatStatus,
		prDisplay:          cfg.PRDisplay,
		lifecycle:          cfg.Lifecycle,
		claude:             cfg.Claude,
		worktrees:          cfg.Worktrees,
		provider:           cfg.Provider,
		pluginHost:         cfg.PluginHost,
		completionNotifier: cfg.CompletionNotifier,
		logger:             cfg.Logger,
	}
}

// ListenAndServe starts the server on a Unix socket at the given path.
// It removes any stale socket file before binding. The caller should
// call Shutdown to stop the server gracefully.
func (s *Server) ListenAndServe(socketPath string) error {
	// Remove stale socket file from previous run.
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale socket: %w", err)
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
	path, handler := bossanovav1connect.NewDaemonServiceHandler(s)
	mux.Handle(path, handler)

	s.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second, // streaming RPCs need longer write timeout
		IdleTimeout:       120 * time.Second,
	}
	return s.srv.Serve(ln)
}

// Shutdown gracefully stops the server.
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
	if msg.CanAutoAddressReviews != nil {
		params.CanAutoAddressReviews = msg.CanAutoAddressReviews
	}
	if msg.CanAutoResolveConflicts != nil {
		params.CanAutoResolveConflicts = msg.CanAutoResolveConflicts
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

	pbPRs := make([]*pb.PRSummary, len(prs))
	for i, pr := range prs {
		pbPRs[i] = &pb.PRSummary{
			Number:     int32(pr.Number),
			Title:      pr.Title,
			HeadBranch: pr.HeadBranch,
			State:      pb.PRState(pr.State),
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

	// Check LinearAPIKey is configured.
	if repo.LinearAPIKey == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("linear_api_key not configured for this repo"))
	}

	// Find Linear plugin among TaskSource plugins.
	if s.pluginHost == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("plugin host not available"))
	}
	sources := s.pluginHost.GetTaskSources()
	if len(sources) == 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("no task source plugins available"))
	}

	var source plugin.TaskSource
	for _, src := range sources {
		info, err := src.GetInfo(ctx)
		if err != nil {
			continue
		}
		if info.Name == "linear" {
			source = src
			break
		}
	}
	if source == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("linear plugin not found"))
	}

	// Call ListAvailableIssues.
	config := map[string]string{
		"linear_api_key": repo.LinearAPIKey,
	}
	issues, err := source.ListAvailableIssues(ctx, repo.OriginURL, config)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list tracker issues: %w", err))
	}

	return connect.NewResponse(&pb.ListTrackerIssuesResponse{Issues: issues}), nil
}

// --- Session Lifecycle ---

func (s *Server) CreateSession(ctx context.Context, req *connect.Request[pb.CreateSessionRequest], stream *connect.ServerStream[pb.CreateSessionResponse]) error {
	msg := req.Msg
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

	createParams := db.CreateSessionParams{
		RepoID:     msg.RepoId,
		Title:      msg.Title,
		Plan:       msg.Plan,
		BaseBranch: baseBranch,
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
			err := s.lifecycle.StartSession(ctx, sess.ID, headBranch, msg.ForceBranch, false, pw)
			done <- lifecycleResult{err: err}
		}()

		// Stream setup script output lines to the client.
		scanner := bufio.NewScanner(pr)
		for scanner.Scan() {
			if err := stream.Send(&pb.CreateSessionResponse{
				Event: &pb.CreateSessionResponse_SetupOutput{
					SetupOutput: &pb.SetupScriptOutput{
						Text: scanner.Text(),
					},
				},
			}); err != nil {
				// Client disconnected — close the pipe reader to unblock
				// the goroutine if it's blocked on pw.Write(), then wait
				// for it to finish so we don't leak.
				_ = pr.Close()
				result := <-done
				_ = result.err
				return err
			}
		}

		// Close the pipe reader to unblock the goroutine if it's still
		// writing (e.g. scanner hit MaxScanTokenSize and stopped reading).
		_ = pr.Close()

		// Pipe closed — lifecycle goroutine is done.
		result := <-done
		startErr = result.err
	}
	if err := startErr; err != nil {
		// Re-fetch session to get worktree/branch info set during lifecycle.
		if failedSess, getErr := s.sessions.Get(ctx, sess.ID); getErr == nil {
			// Clean up worktree and branch (local + remote).
			if failedSess.RepoID != "" && failedSess.BranchName != "" {
				if repo, repoErr := s.repos.Get(ctx, failedSess.RepoID); repoErr == nil {
					_ = s.worktrees.EmptyTrash(ctx, repo.LocalPath, []string{failedSess.BranchName})
				}
			}
		}
		// Delete the orphaned session record.
		_ = s.sessions.Delete(ctx, sess.ID)
		if errors.Is(err, gitpkg.ErrBranchExists) {
			return connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("branch already exists for this session title"))
		}
		s.logger.Error().Err(err).
			Str("session", sess.ID).
			Str("title", msg.Title).
			Msg("start session failed")
		return connect.NewError(connect.CodeInternal, fmt.Errorf("start session: %w", err))
	}

	// Re-fetch the session to get updated fields from lifecycle.
	sess, err = s.sessions.Get(ctx, sess.ID)
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("get session: %w", err))
	}

	return stream.Send(&pb.CreateSessionResponse{
		Event: &pb.CreateSessionResponse_SessionCreated{
			SessionCreated: &pb.SessionCreated{
				Session: sessionToProto(sess),
			},
		},
	})
}

func (s *Server) GetSession(ctx context.Context, req *connect.Request[pb.GetSessionRequest]) (*connect.Response[pb.GetSessionResponse], error) {
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	session, err := s.sessions.Get(ctx, req.Msg.Id)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session not found: %w", err))
	}

	p := sessionToProto(session)

	// Hydrate attention status from session state and repo flags.
	if repo, err := s.repos.Get(ctx, session.RepoID); err == nil {
		p.RepoDisplayName = repo.DisplayName
		p.AttentionStatus = attentionStatusToProto(vcs.ComputeAttentionStatus(session, repo))
	}

	// Hydrate PR display status from the in-memory tracker.
	if s.prDisplay != nil {
		if e := s.prDisplay.Get(session.ID); e != nil {
			p.PrDisplayStatus = pb.PRDisplayStatus(e.Status)
			p.PrDisplayHasFailures = e.HasFailures
			p.PrDisplayHasChangesRequested = e.HasChangesRequested
		}
	}

	return connect.NewResponse(&pb.GetSessionResponse{Session: p}), nil
}

func (s *Server) ListSessions(ctx context.Context, req *connect.Request[pb.ListSessionsRequest]) (*connect.Response[pb.ListSessionsResponse], error) {
	msg := req.Msg
	repoID := msg.GetRepoId()

	var sessions []*models.Session
	var err error

	if msg.IncludeArchived {
		sessions, err = s.sessions.List(ctx, repoID)
	} else {
		sessions, err = s.sessions.ListActive(ctx, repoID)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list sessions: %w", err))
	}

	// Filter by states if specified.
	if len(msg.States) > 0 {
		stateSet := make(map[pb.SessionState]bool, len(msg.States))
		for _, st := range msg.States {
			stateSet[st] = true
		}
		filtered := make([]*models.Session, 0, len(sessions))
		for _, sess := range sessions {
			if stateSet[pb.SessionState(sess.State)] {
				filtered = append(filtered, sess)
			}
		}
		sessions = filtered
	}

	// Build repo lookup for denormalization and attention hydration.
	repoCache := make(map[string]*models.Repo)
	for _, sess := range sessions {
		if _, ok := repoCache[sess.RepoID]; !ok {
			if repo, err := s.repos.Get(ctx, sess.RepoID); err == nil {
				repoCache[sess.RepoID] = repo
			}
		}
	}

	pbSessions := make([]*pb.Session, len(sessions))
	for i, sess := range sessions {
		p := sessionToProto(sess)
		if repo, ok := repoCache[sess.RepoID]; ok {
			p.RepoDisplayName = repo.DisplayName
			p.AttentionStatus = attentionStatusToProto(vcs.ComputeAttentionStatus(sess, repo))
		}
		pbSessions[i] = p
	}

	// Hydrate PR display statuses from the in-memory tracker.
	sessionIDs := make([]string, len(sessions))
	for i, sess := range sessions {
		sessionIDs[i] = sess.ID
	}
	var entries map[string]*status.PRDisplayEntry
	if s.prDisplay != nil {
		entries = s.prDisplay.GetBatch(sessionIDs)
		for i, sess := range sessions {
			if e, ok := entries[sess.ID]; ok {
				pbSessions[i].PrDisplayStatus = pb.PRDisplayStatus(e.Status)
				pbSessions[i].PrDisplayHasFailures = e.HasFailures
				pbSessions[i].PrDisplayHasChangesRequested = e.HasChangesRequested
				pbSessions[i].IsRepairing = e.IsRepairing
			}
		}
	}

	// Hydrate active workflow display fields.
	if s.workflows != nil {
		activeWorkflows, err := s.workflows.ListActiveBySessionIDs(ctx, sessionIDs)
		if err == nil {
			// Build map: session ID → best (highest-priority) active workflow.
			best := make(map[string]*models.Workflow, len(activeWorkflows))
			for _, w := range activeWorkflows {
				if existing, ok := best[w.SessionID]; !ok || workflowPriority(w.Status) > workflowPriority(existing.Status) {
					best[w.SessionID] = w
				}
			}
			for i, sess := range sessions {
				if w, ok := best[sess.ID]; ok {
					// Don't show stale workflow status for sessions with merged/closed PRs.
					if prEntry, hasEntry := entries[sess.ID]; hasEntry &&
						(prEntry.Status == vcs.PRDisplayStatusMerged || prEntry.Status == vcs.PRDisplayStatusClosed) {
						continue
					}
					pbSessions[i].WorkflowDisplayStatus = workflowStatusToProto(w.Status)
					pbSessions[i].WorkflowDisplayLeg = int32(w.FlightLeg)
					pbSessions[i].WorkflowDisplayMaxLegs = int32(w.MaxLegs)
				}
			}
		}
	}

	return connect.NewResponse(&pb.ListSessionsResponse{Sessions: pbSessions}), nil
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
	if sess.ClaudeSessionID == nil || !s.claude.IsRunning(*sess.ClaudeSessionID) {
		return stream.Send(&pb.AttachSessionResponse{
			Event: &pb.AttachSessionResponse_SessionEnded{
				SessionEnded: &pb.SessionEnded{
					FinalState: pb.SessionState(sess.State),
				},
			},
		})
	}

	claudeSessionID := *sess.ClaudeSessionID

	// Send existing ring buffer contents as initial burst.
	history := s.claude.History(claudeSessionID)
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

	// Subscribe to new output lines.
	ch, err := s.claude.Subscribe(ctx, claudeSessionID)
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("subscribe: %w", err))
	}

	// Stream new lines until process exits or client disconnects.
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

	return connect.NewResponse(&pb.StopSessionResponse{Session: sessionToProto(sess)}), nil
}

func (s *Server) PauseSession(ctx context.Context, req *connect.Request[pb.PauseSessionRequest]) (*connect.Response[pb.PauseSessionResponse], error) {
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	// Pause disables automation. State machine integration in Leg 6.
	automationEnabled := false
	if _, err := s.sessions.Update(ctx, req.Msg.Id, db.UpdateSessionParams{
		AutomationEnabled: &automationEnabled,
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("pause session: %w", err))
	}

	session, err := s.sessions.Get(ctx, req.Msg.Id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get session: %w", err))
	}

	return connect.NewResponse(&pb.PauseSessionResponse{Session: sessionToProto(session)}), nil
}

func (s *Server) ResumeSession(ctx context.Context, req *connect.Request[pb.ResumeSessionRequest]) (*connect.Response[pb.ResumeSessionResponse], error) {
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	// Resume re-enables automation. State machine integration in Leg 6.
	automationEnabled := true
	if _, err := s.sessions.Update(ctx, req.Msg.Id, db.UpdateSessionParams{
		AutomationEnabled: &automationEnabled,
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("resume session: %w", err))
	}

	session, err := s.sessions.Get(ctx, req.Msg.Id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get session: %w", err))
	}

	return connect.NewResponse(&pb.ResumeSessionResponse{Session: sessionToProto(session)}), nil
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

	return connect.NewResponse(&pb.RetrySessionResponse{Session: sessionToProto(session)}), nil
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

	return connect.NewResponse(&pb.CloseSessionResponse{Session: sessionToProto(session)}), nil
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

	p := sessionToProto(sess)
	if repo, err := s.repos.Get(ctx, sess.RepoID); err == nil {
		p.RepoDisplayName = repo.DisplayName
	}

	return connect.NewResponse(&pb.UpdateSessionResponse{Session: p}), nil
}

// --- Archive / Resurrect ---

func (s *Server) ArchiveSession(ctx context.Context, req *connect.Request[pb.ArchiveSessionRequest]) (*connect.Response[pb.ArchiveSessionResponse], error) {
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	if err := s.lifecycle.ArchiveSession(ctx, req.Msg.Id); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("archive session: %w", err))
	}

	sess, err := s.sessions.Get(ctx, req.Msg.Id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get session: %w", err))
	}

	return connect.NewResponse(&pb.ArchiveSessionResponse{Session: sessionToProto(sess)}), nil
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

	return connect.NewResponse(&pb.ResurrectSessionResponse{Session: sessionToProto(sess)}), nil
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
	if msg.ClaudeId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("claude_id is required"))
	}

	chat, err := s.claudeChats.Create(ctx, db.CreateClaudeChatParams{
		SessionID: msg.SessionId,
		ClaudeID:  msg.ClaudeId,
		Title:     msg.Title,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("record chat: %w", err))
	}

	return connect.NewResponse(&pb.RecordChatResponse{Chat: claudeChatToProto(chat)}), nil
}

func (s *Server) ListChats(ctx context.Context, req *connect.Request[pb.ListChatsRequest]) (*connect.Response[pb.ListChatsResponse], error) {
	if req.Msg.SessionId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("session_id is required"))
	}

	chats, err := s.claudeChats.ListBySession(ctx, req.Msg.SessionId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list chats: %w", err))
	}

	pbChats := make([]*pb.ClaudeChat, len(chats))
	for i, c := range chats {
		pbChats[i] = claudeChatToProto(c)
	}

	return connect.NewResponse(&pb.ListChatsResponse{Chats: pbChats}), nil
}

func (s *Server) UpdateChatTitle(ctx context.Context, req *connect.Request[pb.UpdateChatTitleRequest]) (*connect.Response[pb.UpdateChatTitleResponse], error) {
	msg := req.Msg
	if msg.ClaudeId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("claude_id is required"))
	}
	if msg.Title == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("title is required"))
	}

	if err := s.claudeChats.UpdateTitleByClaudeID(ctx, msg.ClaudeId, msg.Title); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("update chat title: %w", err))
	}

	return connect.NewResponse(&pb.UpdateChatTitleResponse{}), nil
}

func (s *Server) DeleteChat(ctx context.Context, req *connect.Request[pb.DeleteChatRequest]) (*connect.Response[pb.DeleteChatResponse], error) {
	if req.Msg.ClaudeId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("claude_id is required"))
	}

	if err := s.claudeChats.DeleteByClaudeID(ctx, req.Msg.ClaudeId); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("delete chat: %w", err))
	}

	// Also clean up any cached status for this chat.
	if s.chatStatus != nil {
		s.chatStatus.Remove(req.Msg.ClaudeId)
	}

	return connect.NewResponse(&pb.DeleteChatResponse{}), nil
}

// --- Chat Status ---

func (s *Server) ReportChatStatus(_ context.Context, req *connect.Request[pb.ReportChatStatusRequest]) (*connect.Response[pb.ReportChatStatusResponse], error) {
	if s.chatStatus == nil {
		return connect.NewResponse(&pb.ReportChatStatusResponse{}), nil
	}
	for _, r := range req.Msg.Reports {
		if r.ClaudeId == "" {
			continue
		}
		var lastOutputAt time.Time
		if r.LastOutputAt != nil {
			lastOutputAt = r.LastOutputAt.AsTime()
		}
		s.chatStatus.Update(r.ClaudeId, r.Status, lastOutputAt)
	}
	return connect.NewResponse(&pb.ReportChatStatusResponse{}), nil
}

func (s *Server) GetChatStatuses(ctx context.Context, req *connect.Request[pb.GetChatStatusesRequest]) (*connect.Response[pb.GetChatStatusesResponse], error) {
	if req.Msg.SessionId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("session_id is required"))
	}

	// Look up chats for this session.
	chats, err := s.claudeChats.ListBySession(ctx, req.Msg.SessionId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list chats: %w", err))
	}

	if s.chatStatus == nil || len(chats) == 0 {
		return connect.NewResponse(&pb.GetChatStatusesResponse{}), nil
	}

	claudeIDs := make([]string, len(chats))
	for i, c := range chats {
		claudeIDs[i] = c.ClaudeID
	}

	entries := s.chatStatus.GetBatch(claudeIDs)
	statuses := make([]*pb.ChatStatusEntry, 0, len(entries))
	for id, e := range entries {
		entry := &pb.ChatStatusEntry{
			ClaudeId: id,
			Status:   e.Status,
		}
		if !e.LastOutputAt.IsZero() {
			entry.LastOutputAt = timestamppb.New(e.LastOutputAt)
		}
		statuses = append(statuses, entry)
	}

	return connect.NewResponse(&pb.GetChatStatusesResponse{Statuses: statuses}), nil
}

func (s *Server) GetSessionStatuses(ctx context.Context, req *connect.Request[pb.GetSessionStatusesRequest]) (*connect.Response[pb.GetSessionStatusesResponse], error) {
	if s.chatStatus == nil {
		return connect.NewResponse(&pb.GetSessionStatusesResponse{}), nil
	}

	statuses := make([]*pb.SessionStatusEntry, 0, len(req.Msg.SessionIds))

	for _, sessionID := range req.Msg.SessionIds {
		chats, err := s.claudeChats.ListBySession(ctx, sessionID)
		if err != nil {
			s.logger.Warn().Err(err).Str("session_id", sessionID).Msg("list chats for session status")
			continue
		}
		if len(chats) == 0 {
			statuses = append(statuses, &pb.SessionStatusEntry{
				SessionId: sessionID,
				Status:    pb.ChatStatus_CHAT_STATUS_STOPPED,
			})
			continue
		}

		claudeIDs := make([]string, len(chats))
		for i, c := range chats {
			claudeIDs[i] = c.ClaudeID
		}
		entries := s.chatStatus.GetBatch(claudeIDs)

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
		statuses = append(statuses, &pb.SessionStatusEntry{
			SessionId: sessionID,
			Status:    best,
		})
	}

	return connect.NewResponse(&pb.GetSessionStatusesResponse{Statuses: statuses}), nil
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
				resp.Session = sessionToProto(sess)
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

// --- Autopilot ---

func (s *Server) StartAutopilot(ctx context.Context, req *connect.Request[pb.StartAutopilotRequest]) (*connect.Response[pb.StartAutopilotResponse], error) {
	msg := req.Msg
	if msg.PlanPath == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("plan_path is required"))
	}

	// Resolve repo and session context from working directory.
	repoID, sessionID, err := s.resolveAutopilotContext(ctx, msg.WorkingDirectory)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	// Resolve worktree root (needed for both leg counting and config).
	var rootDir string
	if sessionID != "" {
		if sess, err := s.sessions.Get(ctx, sessionID); err == nil && sess.WorktreePath != "" {
			rootDir = sess.WorktreePath
		}
	}
	if rootDir == "" {
		if repo, err := s.repos.Get(ctx, repoID); err == nil {
			rootDir = repo.LocalPath
		}
	}

	// Auto-detect max legs from plan file if not explicitly set.
	if msg.MaxLegs == 0 && rootDir != "" {
		planAbs := filepath.Join(rootDir, msg.PlanPath)
		if count := countPlanFlightLegs(planAbs); count > 0 {
			msg.MaxLegs = count
			s.logger.Debug().
				Str("plan", planAbs).
				Int32("count", count).
				Msg("auto-detected flight leg count from plan file")
		}
	}
	// Fallback: try from the CLI's working directory when rootDir didn't
	// resolve to the correct location (e.g. worktree path mismatch).
	if msg.MaxLegs == 0 && msg.WorkingDirectory != "" {
		planAbs := filepath.Join(msg.WorkingDirectory, msg.PlanPath)
		if count := countPlanFlightLegs(planAbs); count > 0 {
			msg.MaxLegs = count
			s.logger.Debug().
				Str("plan", planAbs).
				Int32("count", count).
				Msg("auto-detected flight leg count from plan file (working directory)")
		}
	}

	// Build config JSON with work_dir for the plugin.
	configJSON := fmt.Sprintf(`{"work_dir":%q}`, rootDir)

	// Find the workflow service plugin.
	wfService := s.getWorkflowService()
	if wfService == nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("autopilot plugin not available"))
	}

	// Delegate to the plugin.
	resp, err := wfService.StartWorkflow(ctx, &pb.StartWorkflowRequest{
		PlanPath:    msg.PlanPath,
		SessionId:   sessionID,
		RepoId:      repoID,
		MaxLegs:     msg.MaxLegs,
		ConfirmLand: msg.ConfirmLand,
		ConfigJson:  configJSON,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("start workflow: %w", err))
	}

	// Read the created workflow from the store.
	w, err := s.workflows.Get(ctx, resp.GetWorkflowId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get workflow: %w", err))
	}

	return connect.NewResponse(&pb.StartAutopilotResponse{
		Workflow: autopilotWorkflowToProto(w),
	}), nil
}

func (s *Server) PauseAutopilot(ctx context.Context, req *connect.Request[pb.PauseAutopilotRequest]) (*connect.Response[pb.PauseAutopilotResponse], error) {
	if req.Msg.WorkflowId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("workflow_id is required"))
	}

	wfService := s.getWorkflowService()
	if wfService == nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("autopilot plugin not available"))
	}

	if _, err := wfService.PauseWorkflow(ctx, req.Msg.WorkflowId); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("pause workflow: %w", err))
	}

	w, err := s.workflows.Get(ctx, req.Msg.WorkflowId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get workflow: %w", err))
	}

	return connect.NewResponse(&pb.PauseAutopilotResponse{
		Workflow: autopilotWorkflowToProto(w),
	}), nil
}

func (s *Server) ResumeAutopilot(ctx context.Context, req *connect.Request[pb.ResumeAutopilotRequest]) (*connect.Response[pb.ResumeAutopilotResponse], error) {
	if req.Msg.WorkflowId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("workflow_id is required"))
	}

	wfService := s.getWorkflowService()
	if wfService == nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("autopilot plugin not available"))
	}

	if _, err := wfService.ResumeWorkflow(ctx, req.Msg.WorkflowId); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("resume workflow: %w", err))
	}

	w, err := s.workflows.Get(ctx, req.Msg.WorkflowId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get workflow: %w", err))
	}

	return connect.NewResponse(&pb.ResumeAutopilotResponse{
		Workflow: autopilotWorkflowToProto(w),
	}), nil
}

func (s *Server) CancelAutopilot(ctx context.Context, req *connect.Request[pb.CancelAutopilotRequest]) (*connect.Response[pb.CancelAutopilotResponse], error) {
	if req.Msg.WorkflowId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("workflow_id is required"))
	}

	wfService := s.getWorkflowService()
	if wfService == nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("autopilot plugin not available"))
	}

	if _, err := wfService.CancelWorkflow(ctx, req.Msg.WorkflowId); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("cancel workflow: %w", err))
	}

	w, err := s.workflows.Get(ctx, req.Msg.WorkflowId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get workflow: %w", err))
	}

	return connect.NewResponse(&pb.CancelAutopilotResponse{
		Workflow: autopilotWorkflowToProto(w),
	}), nil
}

func (s *Server) GetAutopilotStatus(ctx context.Context, req *connect.Request[pb.GetAutopilotStatusRequest]) (*connect.Response[pb.GetAutopilotStatusResponse], error) {
	if req.Msg.WorkflowId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("workflow_id is required"))
	}

	w, err := s.workflows.Get(ctx, req.Msg.WorkflowId)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("workflow not found: %w", err))
	}

	return connect.NewResponse(&pb.GetAutopilotStatusResponse{
		Workflow: autopilotWorkflowToProto(w),
	}), nil
}

func (s *Server) ListAutopilotWorkflows(ctx context.Context, req *connect.Request[pb.ListAutopilotWorkflowsRequest]) (*connect.Response[pb.ListAutopilotWorkflowsResponse], error) {
	var workflows []*models.Workflow
	var err error

	if req.Msg.IncludeAll {
		workflows, err = s.workflows.List(ctx)
	} else {
		// Show only active workflows (running + paused) by default.
		running, runErr := s.workflows.ListByStatus(ctx, string(models.WorkflowStatusRunning))
		if runErr != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list running workflows: %w", runErr))
		}
		paused, pauseErr := s.workflows.ListByStatus(ctx, string(models.WorkflowStatusPaused))
		if pauseErr != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list paused workflows: %w", pauseErr))
		}
		pending, pendErr := s.workflows.ListByStatus(ctx, string(models.WorkflowStatusPending))
		if pendErr != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list pending workflows: %w", pendErr))
		}
		workflows = append(workflows, running...)
		workflows = append(workflows, paused...)
		workflows = append(workflows, pending...)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list workflows: %w", err))
	}

	pbWorkflows := make([]*pb.AutopilotWorkflow, len(workflows))
	for i, w := range workflows {
		pbWorkflows[i] = autopilotWorkflowToProto(w)
	}

	return connect.NewResponse(&pb.ListAutopilotWorkflowsResponse{
		Workflows: pbWorkflows,
	}), nil
}

func (s *Server) StreamAutopilotOutput(ctx context.Context, req *connect.Request[pb.StreamAutopilotOutputRequest], stream *connect.ServerStream[pb.StreamAutopilotOutputResponse]) error {
	if req.Msg.WorkflowId == "" {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("workflow_id is required"))
	}

	// Get the workflow to find its session.
	w, err := s.workflows.Get(ctx, req.Msg.WorkflowId)
	if err != nil {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("workflow not found: %w", err))
	}

	// Send initial status.
	if err := stream.Send(&pb.StreamAutopilotOutputResponse{
		Event: &pb.StreamAutopilotOutputResponse_StatusUpdate{
			StatusUpdate: autopilotWorkflowToProto(w),
		},
	}); err != nil {
		return err
	}

	// Find the Claude session for this workflow's boss session.
	sess, err := s.sessions.Get(ctx, w.SessionID)
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("get session: %w", err))
	}

	if sess.ClaudeSessionID == nil || !s.claude.IsRunning(*sess.ClaudeSessionID) {
		// No active Claude process — send final status and return.
		w, _ = s.workflows.Get(ctx, req.Msg.WorkflowId)
		return stream.Send(&pb.StreamAutopilotOutputResponse{
			Event: &pb.StreamAutopilotOutputResponse_StatusUpdate{
				StatusUpdate: autopilotWorkflowToProto(w),
			},
		})
	}

	claudeSessionID := *sess.ClaudeSessionID

	// Send existing ring buffer contents as initial burst.
	history := s.claude.History(claudeSessionID)
	for _, line := range history {
		if err := stream.Send(&pb.StreamAutopilotOutputResponse{
			Event: &pb.StreamAutopilotOutputResponse_OutputLine{
				OutputLine: &pb.OutputLine{
					Text:      line.Text,
					Timestamp: timestamppb.New(line.Timestamp),
				},
			},
		}); err != nil {
			return err
		}
	}

	// Subscribe to new output lines.
	ch, err := s.claude.Subscribe(ctx, claudeSessionID)
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("subscribe: %w", err))
	}

	// Stream new lines until process exits or client disconnects.
	for line := range ch {
		if err := stream.Send(&pb.StreamAutopilotOutputResponse{
			Event: &pb.StreamAutopilotOutputResponse_OutputLine{
				OutputLine: &pb.OutputLine{
					Text:      line.Text,
					Timestamp: timestamppb.New(line.Timestamp),
				},
			},
		}); err != nil {
			return err
		}
	}

	// Process exited — send final workflow status.
	w, _ = s.workflows.Get(ctx, req.Msg.WorkflowId)
	return stream.Send(&pb.StreamAutopilotOutputResponse{
		Event: &pb.StreamAutopilotOutputResponse_StatusUpdate{
			StatusUpdate: autopilotWorkflowToProto(w),
		},
	})
}

// resolveAutopilotContext resolves a working directory into a repo ID and session ID.
func (s *Server) resolveAutopilotContext(ctx context.Context, workingDir string) (repoID, sessionID string, err error) {
	if workingDir == "" {
		return "", "", fmt.Errorf("working_directory is required")
	}

	absWD, err := filepath.Abs(workingDir)
	if err != nil {
		return "", "", fmt.Errorf("resolve path: %w", err)
	}

	// Check repos and sessions for a match.
	repos, err := s.repos.List(ctx)
	if err != nil {
		return "", "", fmt.Errorf("list repos: %w", err)
	}

	for _, repo := range repos {
		sessions, err := s.sessions.List(ctx, repo.ID)
		if err != nil {
			continue
		}
		for _, sess := range sessions {
			if sess.WorktreePath != "" && isSubdirOf(absWD, sess.WorktreePath) {
				return repo.ID, sess.ID, nil
			}
		}
		if isSubdirOf(absWD, repo.LocalPath) {
			return repo.ID, "", nil
		}
	}

	return "", "", fmt.Errorf("working directory not inside any registered repo: %s", workingDir)
}

// getWorkflowService returns the first available workflow service plugin.
func (s *Server) getWorkflowService() plugin.WorkflowService {
	if s.pluginHost == nil {
		return nil
	}
	services := s.pluginHost.GetWorkflowServices()
	if len(services) == 0 {
		return nil
	}
	return services[0]
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

// countPlanFlightLegs counts "## Flight Leg" headings and [HANDOFF] markers
// in a plan file. Returns -1 if the file can't be read, 1 if the file is
// readable but contains no markers (single-leg plan), or N for N markers.
// [HANDOFF] markers are heading lines (starting with #) that contain
// "[handoff]" (case-insensitive). The result is max(flightLegs, handoffs).
// legHeadingRe matches markdown headings where the text starts with
// "Leg N" or "Flight Leg N". Sub-headings that merely reference a leg
// number (e.g. "### Post-Flight Checks for Flight Leg 1") are excluded.
//
//	## Flight Leg 1: Setup
//	### Leg 2: Build
//	#### Leg  3 — Polish
var legHeadingRe = regexp.MustCompile(`(?i)^#{1,6}\s+(?:flight\s+)?leg\s+\d+`)

func countPlanFlightLegs(planPath string) int32 {
	f, err := os.Open(planPath)
	if err != nil {
		return -1
	}
	defer func() { _ = f.Close() }()

	var flightLegCount, handoffCount int32
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		lower := strings.ToLower(line)
		if legHeadingRe.MatchString(line) {
			flightLegCount++
		}
		if strings.HasPrefix(line, "#") && strings.Contains(lower, "[handoff]") {
			handoffCount++
		}
	}

	count := flightLegCount
	if handoffCount > count {
		count = handoffCount
	}
	if count == 0 {
		return 1
	}
	return count
}
