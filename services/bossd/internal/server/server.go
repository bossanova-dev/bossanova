package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
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
	repos    db.RepoStore
	sessions db.SessionStore
	attempts db.AttemptStore
	listener net.Listener
	srv      *http.Server

	bossanovav1connect.UnimplementedDaemonServiceHandler
}

// New creates a new Server wired to the given stores.
func New(repos db.RepoStore, sessions db.SessionStore, attempts db.AttemptStore) *Server {
	return &Server{
		repos:    repos,
		sessions: sessions,
		attempts: attempts,
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

	s.srv = &http.Server{Handler: mux}
	return s.srv.Serve(ln)
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.srv != nil {
		return s.srv.Shutdown(ctx)
	}
	return nil
}

// --- Repo Management ---

func (s *Server) RegisterRepo(ctx context.Context, req *connect.Request[pb.RegisterRepoRequest]) (*connect.Response[pb.RegisterRepoResponse], error) {
	msg := req.Msg
	if msg.LocalPath == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("local_path is required"))
	}

	var setupScript *string
	if msg.SetupScript != nil {
		setupScript = msg.SetupScript
	}

	repo, err := s.repos.Create(ctx, db.CreateRepoParams{
		DisplayName:       msg.DisplayName,
		LocalPath:         msg.LocalPath,
		OriginURL:         "", // Detected from git repo in Leg 6
		DefaultBaseBranch: msg.DefaultBaseBranch,
		WorktreeBaseDir:   msg.WorktreeBaseDir,
		SetupScript:       setupScript,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create repo: %w", err))
	}

	return connect.NewResponse(&pb.RegisterRepoResponse{Repo: repoToProto(repo)}), nil
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

func (s *Server) ListRepoPRs(ctx context.Context, req *connect.Request[pb.ListRepoPRsRequest]) (*connect.Response[pb.ListRepoPRsResponse], error) {
	if req.Msg.RepoId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("repo_id is required"))
	}

	// Verify the repo exists.
	if _, err := s.repos.Get(ctx, req.Msg.RepoId); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("repo not found: %w", err))
	}

	// Stub: real implementation requires VCS provider (Leg 7).
	return connect.NewResponse(&pb.ListRepoPRsResponse{}), nil
}

// --- Session Lifecycle ---

func (s *Server) CreateSession(ctx context.Context, req *connect.Request[pb.CreateSessionRequest]) (*connect.Response[pb.CreateSessionResponse], error) {
	msg := req.Msg
	if msg.RepoId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("repo_id is required"))
	}
	if msg.Title == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("title is required"))
	}

	// Verify repo exists.
	repo, err := s.repos.Get(ctx, msg.RepoId)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("repo not found: %w", err))
	}

	baseBranch := msg.BaseBranch
	if baseBranch == "" {
		baseBranch = repo.DefaultBaseBranch
	}

	session, err := s.sessions.Create(ctx, db.CreateSessionParams{
		RepoID:       msg.RepoId,
		Title:        msg.Title,
		Plan:         msg.Plan,
		WorktreePath: "", // Set by worktree manager in Leg 6
		BranchName:   "", // Set by worktree manager in Leg 6
		BaseBranch:   baseBranch,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create session: %w", err))
	}

	return connect.NewResponse(&pb.CreateSessionResponse{Session: sessionToProto(session)}), nil
}

func (s *Server) GetSession(ctx context.Context, req *connect.Request[pb.GetSessionRequest]) (*connect.Response[pb.GetSessionResponse], error) {
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	session, err := s.sessions.Get(ctx, req.Msg.Id)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session not found: %w", err))
	}

	return connect.NewResponse(&pb.GetSessionResponse{Session: sessionToProto(session)}), nil
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

	pbSessions := make([]*pb.Session, len(sessions))
	for i, sess := range sessions {
		pbSessions[i] = sessionToProto(sess)
	}

	return connect.NewResponse(&pb.ListSessionsResponse{Sessions: pbSessions}), nil
}

func (s *Server) AttachSession(ctx context.Context, req *connect.Request[pb.AttachSessionRequest], stream *connect.ServerStream[pb.AttachSessionResponse]) error {
	if req.Msg.Id == "" {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	// Verify the session exists.
	session, err := s.sessions.Get(ctx, req.Msg.Id)
	if err != nil {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("session not found: %w", err))
	}

	// Send initial state to let the client know the session is valid.
	if err := stream.Send(&pb.AttachSessionResponse{
		Event: &pb.AttachSessionResponse_StateChange{
			StateChange: &pb.StateChange{
				PreviousState: pb.SessionState(session.State),
				NewState:      pb.SessionState(session.State),
			},
		},
	}); err != nil {
		return err
	}

	// Stub: real streaming (tail log, watch state changes) in Leg 6.
	// For now, stream closes immediately after the initial state message.
	return nil
}

func (s *Server) StopSession(ctx context.Context, req *connect.Request[pb.StopSessionRequest]) (*connect.Response[pb.StopSessionResponse], error) {
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	// State machine transition will be handled by session lifecycle (Leg 6).
	// For now, update state to Closed via store.
	closedState := int(machine.Closed)
	if _, err := s.sessions.Update(ctx, req.Msg.Id, db.UpdateSessionParams{
		State: &closedState,
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("stop session: %w", err))
	}

	session, err := s.sessions.Get(ctx, req.Msg.Id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get session: %w", err))
	}

	return connect.NewResponse(&pb.StopSessionResponse{Session: sessionToProto(session)}), nil
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

	if err := s.sessions.Delete(ctx, req.Msg.Id); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("remove session: %w", err))
	}

	return connect.NewResponse(&pb.RemoveSessionResponse{}), nil
}

// --- Archive / Resurrect ---

func (s *Server) ArchiveSession(ctx context.Context, req *connect.Request[pb.ArchiveSessionRequest]) (*connect.Response[pb.ArchiveSessionResponse], error) {
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	if err := s.sessions.Archive(ctx, req.Msg.Id); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("archive session: %w", err))
	}

	session, err := s.sessions.Get(ctx, req.Msg.Id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get session: %w", err))
	}

	return connect.NewResponse(&pb.ArchiveSessionResponse{Session: sessionToProto(session)}), nil
}

func (s *Server) ResurrectSession(ctx context.Context, req *connect.Request[pb.ResurrectSessionRequest]) (*connect.Response[pb.ResurrectSessionResponse], error) {
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	if err := s.sessions.Resurrect(ctx, req.Msg.Id); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("resurrect session: %w", err))
	}

	session, err := s.sessions.Get(ctx, req.Msg.Id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get session: %w", err))
	}

	return connect.NewResponse(&pb.ResurrectSessionResponse{Session: sessionToProto(session)}), nil
}

func (s *Server) EmptyTrash(ctx context.Context, req *connect.Request[pb.EmptyTrashRequest]) (*connect.Response[pb.EmptyTrashResponse], error) {
	// Get all archived sessions, optionally filtering by age.
	// For now, delete all archived sessions. olderThan filtering requires
	// a store query enhancement (deferred to Leg 6 worktree integration).
	repoID := "" // all repos
	archived, err := s.sessions.ListArchived(ctx, repoID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list archived: %w", err))
	}

	olderThan := protoToTimestamp(req.Msg.OlderThan)
	deleted := int32(0)
	for _, sess := range archived {
		if olderThan != nil && sess.ArchivedAt != nil && sess.ArchivedAt.After(*olderThan) {
			continue
		}
		if err := s.sessions.Delete(ctx, sess.ID); err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("delete archived session %s: %w", sess.ID, err))
		}
		deleted++
	}

	return connect.NewResponse(&pb.EmptyTrashResponse{DeletedCount: deleted}), nil
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
