package serve

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/adapter/tmux"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/status"
)

type serveSessionFixture struct{ sessions []*models.Session }

func (f *serveSessionFixture) Get(_ context.Context, id string) (*models.Session, error) {
	for _, s := range f.sessions {
		if s.ID == id {
			return s, nil
		}
	}
	return nil, domain.ErrNotFound
}
func (f *serveSessionFixture) List(context.Context, string) ([]*models.Session, error) {
	return f.sessions, nil
}

type serveRepoFixture struct{ repo *models.Repo }

func (f serveRepoFixture) Get(context.Context, string) (*models.Repo, error) { return f.repo, nil }

type serveTmuxFixture struct {
	live       map[string]bool
	opts       []tmux.NewSessionOpts
	env        map[string]map[string]string
	killed     []string
	killGroups []string
	probeErr   error
}

func (f *serveTmuxFixture) NewSession(_ context.Context, o tmux.NewSessionOpts) error {
	f.opts = append(f.opts, o)
	f.live[o.Name] = true
	f.env[o.Name] = o.Env
	return nil
}
func (f *serveTmuxFixture) HasSessionStatus(_ context.Context, n string) (bool, error) {
	return f.live[n], f.probeErr
}
func (f *serveTmuxFixture) KillSession(_ context.Context, n string) error {
	delete(f.live, n)
	f.killed = append(f.killed, n)
	return nil
}
func (f *serveTmuxFixture) KillPaneProcessGroup(_ context.Context, n string) error {
	f.killGroups = append(f.killGroups, n)
	return nil
}
func (f *serveTmuxFixture) ListSessions(context.Context) ([]tmux.LiveSession, error) {
	var out []tmux.LiveSession
	for n := range f.live {
		out = append(out, tmux.LiveSession{Name: n, Created: time.Now()})
	}
	return out, nil
}
func (f *serveTmuxFixture) ShowEnv(_ context.Context, n, k string) (string, tmux.ShowEnvStatus) {
	v, ok := f.env[n][k]
	if !ok {
		return "", tmux.ShowEnvUnset
	}
	return v, tmux.ShowEnvSet
}

type serveLinkRecorder struct {
	links map[string][]status.ServiceLink
	calls int
}

func (f *serveLinkRecorder) SetServiceLinks(id string, l []status.ServiceLink) {
	f.links[id] = l
	f.calls++
}
func runnerFixture(t *testing.T) (*Runner, *serveTmuxFixture, *serveLinkRecorder, *serveSessionFixture, *models.Repo) {
	t.Helper()
	script := `{"type":"command","argv":["pnpm","dev"]}`
	repo := &models.Repo{ID: "aaaaaaaa11111111", LocalPath: t.TempDir(), ServeScript: &script}
	sessions := &serveSessionFixture{sessions: []*models.Session{{ID: "bbbbbbbb11111111", RepoID: repo.ID, WorktreePath: t.TempDir()}}}
	mux := &serveTmuxFixture{live: map[string]bool{}, env: map[string]map[string]string{}}
	links := &serveLinkRecorder{links: map[string][]status.ServiceLink{}}
	r := New(sessions, serveRepoFixture{repo}, mux, links, Config{DataDir: t.TempDir(), DaemonID: "daemon"})
	return r, mux, links, sessions, repo
}
func TestRunnerStartPublishAndExit_Sessions_AC250701(t *testing.T) {
	r, mux, pub, sessions, repo := runnerFixture(t)
	id := sessions.sessions[0].ID
	ctx := context.Background()
	state, err := r.Start(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !state.IsRunning || len(mux.opts) != 1 {
		t.Fatalf("state=%+v opts=%v", state, mux.opts)
	}
	opts := mux.opts[0]
	if opts.WorkDir != sessions.sessions[0].WorktreePath || opts.Command[0] != "pnpm" || opts.RemainOnExit || !opts.ExitOnCompletion || opts.Env["BOSS_SESSION_ID"] != id || opts.Env["REPO_DIR"] != repo.LocalPath || opts.Env["WORKTREE_DIR"] != opts.WorkDir {
		t.Fatalf("opts=%+v", opts)
	}
	if !filepath.IsAbs(opts.Env["BOSS_SERVICES_FILE"]) {
		t.Fatal("services file is not absolute")
	}
	if _, err = r.Start(ctx, id); err != nil || len(mux.opts) != 1 {
		t.Fatalf("second start: %v", err)
	}
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(opts.Env["BOSS_SERVICES_FILE"], []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"api":"https://one.example"}`)
	r.poll(ctx)
	if len(pub.links[id]) != 1 || pub.links[id][0].URL != "https://one.example" {
		t.Fatalf("links=%v", pub.links)
	}
	calls := pub.calls
	r.poll(ctx)
	if pub.calls != calls {
		t.Fatal("unchanged links republished")
	}
	write(`{"api":"https://second.example"}`)
	r.poll(ctx)
	if pub.links[id][0].URL != "https://second.example" {
		t.Fatalf("links=%v", pub.links)
	}
	mux.probeErr = errors.New("tmux unavailable")
	r.poll(ctx)
	if !r.State(id).IsRunning {
		t.Fatal("probe failure treated as exit")
	}
	mux.probeErr = nil
	delete(mux.live, state.TmuxName)
	r.poll(ctx)
	if r.State(id).IsRunning || len(pub.links[id]) != 0 || r.LiveTmuxName(id) != "" {
		t.Fatal("exit retained links")
	}
	if _, err := os.Stat(opts.Env["BOSS_SERVICES_FILE"]); !os.IsNotExist(err) {
		t.Fatalf("services file remains: %v", err)
	}
}
func TestRunnerStopAndTypedErrors(t *testing.T) {
	r, mux, pub, sessions, repo := runnerFixture(t)
	id := sessions.sessions[0].ID
	ctx := context.Background()
	state, err := r.Start(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Stop(ctx, id); err != nil {
		t.Fatal(err)
	}
	if mux.live[state.TmuxName] || len(mux.killGroups) != 1 || len(pub.links[id]) != 0 || r.LiveTmuxName(id) != "" {
		t.Fatal("stop did not clear pane and links")
	}
	now := time.Now()
	sessions.sessions[0].ArchivedAt = &now
	if _, err = r.Start(ctx, id); !errors.Is(err, ErrSessionArchived) {
		t.Fatalf("archived: %v", err)
	}
	sessions.sessions[0].ArchivedAt = nil
	repo.ServeScript = nil
	if _, err = r.Start(ctx, id); !errors.Is(err, ErrNoServeCommand) {
		t.Fatalf("missing command: %v", err)
	}
	if _, err = r.Start(ctx, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing session: %v", err)
	}
}
func TestRunnerRestartAdoption_Sessions_AC250701(t *testing.T) {
	r, mux, pub, sessions, _ := runnerFixture(t)
	s := sessions.sessions[0]
	active := tmux.ServeSessionName(s.RepoID, s.ID)
	now := time.Now()
	archived := &models.Session{ID: "cccccccc11111111", RepoID: s.RepoID, WorktreePath: t.TempDir(), ArchivedAt: &now}
	sessions.sessions = append(sessions.sessions, archived)
	dead := tmux.ServeSessionName(s.RepoID, archived.ID)
	unknown := "boss-aaaaaaaa-dddddddd-serve"
	foreign := "boss-aaaaaaaa-eeeeeeee-serve"
	for n, id := range map[string]string{active: s.ID, dead: archived.ID, unknown: "unknown", foreign: "foreign"} {
		mux.live[n] = true
		mux.env[n] = map[string]string{tmux.DaemonIDEnvKey: "daemon", "BOSS_SESSION_ID": id}
	}
	mux.env[foreign][tmux.DaemonIDEnvKey] = "other"
	path, err := r.prepareFile(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte(`{"api":"https://adopted.example"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.adopt(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(pub.links[s.ID]) != 1 || pub.links[s.ID][0].URL != "https://adopted.example" {
		t.Fatalf("adopted links=%v", pub.links)
	}
	if !r.State(s.ID).IsRunning || r.LiveTmuxName(s.ID) != active || mux.live[dead] || mux.live[unknown] || !mux.live[foreign] {
		t.Fatalf("adoption state=%+v panes=%v", r.State(s.ID), mux.live)
	}
}

func TestRunnerStopNeverServedWithoutTmux(t *testing.T) {
	r, mux, _, sessions, _ := runnerFixture(t)
	mux.probeErr = errors.New("tmux executable not found")
	if err := r.Stop(context.Background(), sessions.sessions[0].ID); err != nil {
		t.Fatalf("stop session without services: %v", err)
	}
}

func TestRunnerStoppedServeDoesNotNeedTmux(t *testing.T) {
	r, mux, _, sessions, _ := runnerFixture(t)
	id := sessions.sessions[0].ID
	if _, err := r.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := r.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	mux.probeErr = errors.New("tmux executable not found")
	if err := r.Stop(t.Context(), id); err != nil {
		t.Fatalf("repeat stop of exited services: %v", err)
	}
	r.states = map[string]*tracked{}
	if err := r.Stop(t.Context(), id); err != nil {
		t.Fatalf("stop after daemon restart: %v", err)
	}
}

func TestRunnerUnadoptedStopFailureRemainsRetryable(t *testing.T) {
	r, mux, _, sessions, _ := runnerFixture(t)
	id := sessions.sessions[0].ID
	if _, err := r.prepareFile(id); err != nil {
		t.Fatal(err)
	}
	name := tmux.ServeSessionName(sessions.sessions[0].RepoID, id)
	mux.live[name] = true
	mux.probeErr = errors.New("tmux liveness unavailable")
	for range 2 {
		if err := r.Stop(t.Context(), id); !errors.Is(err, mux.probeErr) {
			t.Fatalf("unconfirmed stop error=%v", err)
		}
	}
	mux.probeErr = nil
	if err := r.Stop(t.Context(), id); err != nil || mux.live[name] {
		t.Fatalf("retry error=%v alive=%v", err, mux.live[name])
	}
}

func TestRunnerStopBeforeAdoption(t *testing.T) {
	r, mux, _, sessions, _ := runnerFixture(t)
	id := sessions.sessions[0].ID
	if _, err := r.prepareFile(id); err != nil {
		t.Fatal(err)
	}
	name := tmux.ServeSessionName(sessions.sessions[0].RepoID, id)
	mux.live[name] = true
	if err := r.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if mux.live[name] || len(mux.killGroups) != 1 {
		t.Fatal("unadopted serve pane survived teardown")
	}
}
