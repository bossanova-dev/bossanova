// Package serve runs repository serve commands and publishes their reported links.
package serve

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/adapter/setupscript"
	"github.com/recurser/bossd/internal/adapter/tmux"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/status"
)

var (
	ErrSessionArchived = errors.New("session is archived or closed")
	ErrNoWorktree      = errors.New("session has no worktree")
	ErrNoServeCommand  = errors.New("repo has no serve command")
	ErrInvalidSpec     = setupscript.ErrInvalidSpec
)

type sessionReader interface {
	Get(context.Context, string) (*models.Session, error)
	List(context.Context, string) ([]*models.Session, error)
}
type repoReader interface {
	Get(context.Context, string) (*models.Repo, error)
}
type tmuxHost interface {
	NewSession(context.Context, tmux.NewSessionOpts) error
	HasSessionStatus(context.Context, string) (bool, error)
	KillSession(context.Context, string) error
	KillPaneProcessGroup(context.Context, string) error
	ListSessions(context.Context) ([]tmux.LiveSession, error)
	ShowEnv(context.Context, string, string) (string, tmux.ShowEnvStatus)
}
type linkPublisher interface {
	SetServiceLinks(string, []status.ServiceLink)
}

type serveStatePublisher interface {
	SetServeState(sessionID string, running bool, serveErr string, links []status.ServiceLink)
}

// Config supplies daemon-owned storage, identity and the configured login shell.
type Config struct{ DataDir, DaemonID, LoginShell string }

// State is a snapshot of a session's serve command.
type State struct {
	IsRunning bool
	Links     []status.ServiceLink
	LastError string
	StartedAt time.Time
	TmuxName  string
}
type tracked struct {
	State
	mtime    time.Time
	size     int64
	observed bool
}

// Runner serializes starts, stops and polling so an exited command never
// publishes links after an explicit stop.
type Runner struct {
	mu        sync.Mutex
	sessions  sessionReader
	repos     repoReader
	tmux      tmuxHost
	publisher linkPublisher
	cfg       Config
	states    map[string]*tracked
	liveTmux  sync.Map
}

// New constructs a runner. Run owns adoption and the polling lifecycle.
func New(sessions sessionReader, repos repoReader, host tmuxHost, publisher linkPublisher, cfg Config) *Runner {
	return &Runner{sessions: sessions, repos: repos, tmux: host, publisher: publisher, cfg: cfg, states: map[string]*tracked{}}
}

// State returns an owned snapshot, safe for concurrent callers.
func (r *Runner) State(id string) State { r.mu.Lock(); defer r.mu.Unlock(); return r.snapshot(id) }
func (r *Runner) snapshot(id string) State {
	if s := r.states[id]; s != nil {
		out := s.State
		out.Links = slices.Clone(out.Links)
		return out
	}
	return State{}
}

func (r *Runner) publishState(id string, s State) {
	if sp, ok := r.publisher.(serveStatePublisher); ok {
		sp.SetServeState(id, s.IsRunning, s.LastError, s.Links)
		return
	}
	r.publisher.SetServiceLinks(id, s.Links)
}

// Start starts the configured command once, or returns the live command's state.
func (r *Runner) Start(ctx context.Context, id string) (State, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sess, err := r.sessions.Get(ctx, id)
	if err != nil {
		return State{}, err
	}
	if sess == nil {
		return State{}, domain.ErrNotFound
	}
	if sess.ArchivedAt != nil || sess.State == machine.Closed {
		return State{}, ErrSessionArchived
	}
	if sess.WorktreePath == "" {
		return State{}, ErrNoWorktree
	}
	name := tmux.ServeSessionName(sess.RepoID, id)
	alive, err := r.tmux.HasSessionStatus(ctx, name)
	if err != nil {
		return State{}, err
	}
	if alive {
		if r.states[id] == nil {
			r.states[id] = &tracked{State: State{IsRunning: true, TmuxName: name}}
			r.liveTmux.Store(id, name)
		}
		snap := r.snapshot(id)
		r.publishState(id, snap)
		return snap, nil
	}
	repo, err := r.repos.Get(ctx, sess.RepoID)
	if err != nil {
		return State{}, err
	}
	if repo == nil || repo.ServeScript == nil || *repo.ServeScript == "" {
		return State{}, ErrNoServeCommand
	}
	spec, err := setupscript.Parse(*repo.ServeScript)
	if err != nil {
		return State{}, err
	}
	argv, err := spec.BuildArgv(setupscript.ExecuteOpts{WorktreePath: sess.WorktreePath, RepoPath: repo.LocalPath, LoginShell: r.cfg.LoginShell}, "serve.sh")
	if err != nil {
		return State{}, err
	}
	path, err := r.prepareFile(id)
	if err != nil {
		return State{}, err
	}
	env := map[string]string{"BOSS_SERVICES_FILE": path, "BOSS_SESSION_ID": id, "REPO_DIR": repo.LocalPath, "WORKTREE_DIR": sess.WorktreePath}
	if err = r.tmux.NewSession(ctx, tmux.NewSessionOpts{Name: name, WorkDir: sess.WorktreePath, Command: argv, Env: env, ExitOnCompletion: true}); err != nil {
		return State{}, err
	}
	r.states[id] = &tracked{State: State{IsRunning: true, TmuxName: name, StartedAt: time.Now()}}
	r.liveTmux.Store(id, name)
	snap := r.snapshot(id)
	r.publishState(id, snap)
	return snap, nil
}
func (r *Runner) filePath(id string) string {
	return filepath.Join(r.cfg.DataDir, "serve", id, "services.json")
}
func (r *Runner) prepareFile(id string) (string, error) {
	path := r.filePath(id)
	if err := os.RemoveAll(filepath.Dir(path)); err != nil {
		return "", fmt.Errorf("empty services directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create services directory: %w", err)
	}
	return path, nil
}

// Stop kills the serve process group and pane, then clears published links.
func (r *Runner) Stop(ctx context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.states[id]
	if s != nil && !s.IsRunning {
		s.LastError = ""
		return r.clear(id, s)
	}
	if s == nil {
		sess, err := r.sessions.Get(ctx, id)
		if err != nil {
			return err
		}
		if sess == nil {
			return domain.ErrNotFound
		}
		// Start creates this directory before launching tmux, so it also
		// identifies surviving panes before restart adoption has completed.
		if _, err := os.Stat(filepath.Dir(r.filePath(id))); os.IsNotExist(err) {
			return nil
		} else if err != nil {
			return err
		}
		s = &tracked{State: State{IsRunning: true, TmuxName: tmux.ServeSessionName(sess.RepoID, id)}}
		r.states[id] = s
	}
	if err := r.kill(ctx, s.TmuxName); err != nil {
		s.LastError = err.Error()
		return err
	}
	s.LastError = ""
	return r.clear(id, s)
}
func (r *Runner) kill(ctx context.Context, name string) error {
	alive, err := r.tmux.HasSessionStatus(ctx, name)
	if err != nil {
		return err
	}
	if !alive {
		return nil
	}
	if err = r.tmux.KillPaneProcessGroup(ctx, name); err != nil {
		return err
	}
	return r.tmux.KillSession(ctx, name)
}
func (r *Runner) clear(id string, s *tracked) error {
	s.IsRunning = false
	r.liveTmux.Delete(id)
	s.Links = nil
	s.observed = false
	if err := os.RemoveAll(filepath.Dir(r.filePath(id))); err != nil {
		s.LastError = err.Error()
		r.publishState(id, s.State)
		return err
	}
	r.publishState(id, s.State)
	return nil
}

// LiveTmuxName returns the cached live serve pane for local port discovery.
func (r *Runner) LiveTmuxName(id string) string {
	if name, ok := r.liveTmux.Load(id); ok {
		value, _ := name.(string)
		return value
	}
	return ""
}

// Get validates the session before returning its current owned snapshot.
func (r *Runner) Get(ctx context.Context, id string) (State, error) {
	session, err := r.sessions.Get(ctx, id)
	if err != nil {
		return State{}, err
	}
	if session == nil {
		return State{}, domain.ErrNotFound
	}
	return r.State(id), nil
}
