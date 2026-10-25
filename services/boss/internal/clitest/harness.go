// Package clitest provides end-to-end test infrastructure for the boss CLI's
// non-interactive commands. Each harness boots an isolated real bossd with the
// stub runner, then captures the compiled boss subprocess output.
package clitest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/recurser/boss/internal/realstack"
	"github.com/recurser/boss/internal/tuitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// Harness runs boss CLI commands against a real daemon and captures output.
type Harness struct {
	Stack                                                                      *realstack.Stack
	RepoIDs, SessionIDs, ChatIDs, CronJobIDs, AccountIDs, NoteIDs, CallbackIDs map[string]string
	dir                                                                        string
	binPath                                                                    string
	env                                                                        []string
	settingsPath                                                               string
}

const harnessWaitDelay = 5 * time.Second

// Option configures a Harness.
type Option func(*harnessConfig)

type harnessConfig struct {
	repos           []*pb.Repo
	sessions        []*pb.Session
	chats           []*pb.AgentChat
	cronJobs        []*pb.CronJob
	githubCallbacks []*pb.GitHubCallback
	accounts        []*pb.AgentAccount
	notes           []*pb.Note
	checkSnapshots  map[string][]*pb.CheckSnapshot
	resolvedRepo    *pb.Repo
	resolvedSession *pb.Session
	extraEnv        []string
}

// WithRepos seeds the real daemon with repos.
func WithRepos(repos ...*pb.Repo) Option {
	return func(c *harnessConfig) { c.repos = append(c.repos, repos...) }
}

// WithSessions seeds the real daemon with sessions.
func WithSessions(sessions ...*pb.Session) Option {
	return func(c *harnessConfig) { c.sessions = append(c.sessions, sessions...) }
}

// WithChats seeds the real daemon with claude chats.
func WithChats(chats ...*pb.AgentChat) Option {
	return func(c *harnessConfig) { c.chats = append(c.chats, chats...) }
}

// WithCronJobs seeds the real daemon with cron jobs.
func WithCronJobs(jobs ...*pb.CronJob) Option {
	return func(c *harnessConfig) { c.cronJobs = append(c.cronJobs, jobs...) }
}

// WithGitHubCallbacks seeds the real daemon with GitHub callbacks.
func WithGitHubCallbacks(cbs ...*pb.GitHubCallback) Option {
	return func(c *harnessConfig) { c.githubCallbacks = append(c.githubCallbacks, cbs...) }
}

// WithAgentAccounts seeds the real daemon with accounts.
func WithAgentAccounts(accounts ...*pb.AgentAccount) Option {
	return func(c *harnessConfig) { c.accounts = append(c.accounts, accounts...) }
}

// WithNotes seeds the real daemon with notes.
func WithNotes(notes ...*pb.Note) Option {
	return func(c *harnessConfig) { c.notes = append(c.notes, notes...) }
}

// WithCheckSnapshots seeds a session's CI check snapshots. Pass them newest
// first — the order the daemon returns them and the order `--limit`
// truncates from.
func WithCheckSnapshots(sessionID string, snaps ...*pb.CheckSnapshot) Option {
	return func(c *harnessConfig) {
		if c.checkSnapshots == nil {
			c.checkSnapshots = make(map[string][]*pb.CheckSnapshot)
		}
		c.checkSnapshots[sessionID] = append(c.checkSnapshots[sessionID], snaps...)
	}
}

// WithResolvedContext runs boss inside the seeded repository or session worktree,
// so the real daemon resolves its working directory through ResolveContext.
func WithResolvedContext(repo *pb.Repo, session *pb.Session) Option {
	return func(c *harnessConfig) {
		c.resolvedRepo, c.resolvedSession = repo, session
	}
}

// WithEnv adds extra env vars to every subprocess invocation (e.g. HOME=/tmp/xxx
// for daemon-install tests that need to redirect the plist path).
func WithEnv(entries ...string) Option {
	return func(c *harnessConfig) { c.extraEnv = append(c.extraEnv, entries...) }
}

// New creates a test harness with a real daemon. It requires BuildBoss to
// have been called from TestMain so the compiled boss binary is reachable.
func New(t *testing.T, opts ...Option) *Harness {
	t.Helper()

	binPath, err := tuitest.BossBinaryPath()
	if err != nil {
		t.Fatalf("boss binary not available: %v", err)
	}

	var cfg harnessConfig
	for _, option := range opts {
		option(&cfg)
	}
	cfg = completeFixtures(cfg)
	stack := realstack.Start(t, realstack.Options{InteractiveStub: true})
	seedOptions := make([]tuitest.Option, 0, 2+len(cfg.checkSnapshots))
	seedOptions = append(seedOptions, tuitest.WithRepos(cfg.repos...), tuitest.WithSessions(cfg.sessions...))
	for id, snapshots := range cfg.checkSnapshots {
		seedOptions = append(seedOptions, tuitest.WithCheckSnapshots(id, snapshots...))
	}
	repoIDs, sessionIDs := tuitest.SeedStack(t, stack, seedOptions...)
	h := &Harness{Stack: stack, binPath: binPath, RepoIDs: repoIDs, SessionIDs: sessionIDs,
		ChatIDs: map[string]string{}, CronJobIDs: map[string]string{}, AccountIDs: map[string]string{}, NoteIDs: map[string]string{}, CallbackIDs: map[string]string{},
		settingsPath: filepath.Join(stack.Home(), "settings.json"), dir: stack.Home()}
	h.seed(t, cfg)
	env := append(stack.Env(), "BOSS_SOCKET="+stack.SocketPath(), "BOSS_SKIP_SKILLS=1")
	for _, entry := range cfg.extraEnv {
		key, value, found := strings.Cut(entry, "=")
		if found {
			entry = key + "=" + h.ID(value)
		}
		env = append(env, entry)
	}
	if cfg.resolvedSession != nil {
		for _, session := range h.Sessions(t) {
			if session.Id == h.ID(cfg.resolvedSession.Id) {
				h.dir = session.WorktreePath
			}
		}
	} else if cfg.resolvedRepo != nil {
		for _, repo := range h.Repos(t) {
			if repo.Id == h.ID(cfg.resolvedRepo.Id) {
				h.dir = repo.LocalPath
			}
		}
	}
	env = append(env, "PWD="+h.dir)
	h.env = env
	return h
}

// SettingsPath returns the per-harness settings.json path injected into the
// subprocess via BOSS_SETTINGS_PATH. Tests read settings back from here.
func (h *Harness) SettingsPath() string { return h.settingsPath }

// Result is the outcome of running a boss CLI command.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Success reports whether the command exited with code 0.
func (r Result) Success() bool { return r.ExitCode == 0 }

// Run executes the compiled boss binary with the given args and returns its
// captured stdout/stderr and exit code. It kills the subprocess after 30s
// to avoid hanging the test if a command unexpectedly blocks on input.
func (h *Harness) Run(args ...string) Result {
	return h.run(nil, args)
}

// RunWithStdin is like Run but pipes the given string to the subprocess's stdin.
// Useful for commands that prompt for confirmation (e.g. `boss trash delete`).
func (h *Harness) RunWithStdin(stdin string, args ...string) Result {
	return h.run(strings.NewReader(stdin), args)
}

// RunningResult is a boss subprocess started by Start. It lets tests inspect
// stdout/stderr before a deliberately delayed command has exited.
type RunningResult struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
	stdout *lockedBuffer
	stderr *lockedBuffer
}

// Start launches the compiled boss binary and returns immediately. Call Wait to
// reap it. The subprocess is still protected by the same 30s timeout as Run.
func (h *Harness) Start(args ...string) *RunningResult {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	// #nosec G204 -- test-only harness runs the compiled boss binary with test-controlled args
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	cmd := exec.CommandContext(ctx, h.binPath, h.args(args)...)
	cmd.Dir = h.dir
	cmd.Env = h.env
	stdout := &lockedBuffer{}
	stderr := &lockedBuffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = harnessWaitDelay
	if err := cmd.Start(); err != nil {
		cancel()
		return &RunningResult{
			cmd:    nil,
			cancel: func() {},
			stdout: stdout,
			stderr: &lockedBuffer{initial: "start subprocess: " + err.Error()},
		}
	}
	return &RunningResult{cmd: cmd, cancel: cancel, stdout: stdout, stderr: stderr}
}

// Stdout returns the stdout captured so far.
func (r *RunningResult) Stdout() string { return r.stdout.String() }

// Stderr returns the stderr captured so far.
func (r *RunningResult) Stderr() string { return r.stderr.String() }

// Wait waits for the subprocess and returns its final captured output.
func (r *RunningResult) Wait() Result {
	defer r.cancel()
	if r.cmd == nil {
		return Result{Stdout: r.Stdout(), Stderr: r.Stderr(), ExitCode: -1}
	}
	err := r.cmd.Wait()
	exitCode := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exitCode = ee.ExitCode()
		} else {
			exitCode = -1
		}
	}
	return Result{Stdout: r.Stdout(), Stderr: r.Stderr(), ExitCode: exitCode}
}

type lockedBuffer struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	initial string
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.initial + b.buf.String()
}

func (h *Harness) run(stdin io.Reader, args []string) Result {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// #nosec G204 -- test-only harness runs the compiled boss binary with test-controlled args
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	cmd := exec.CommandContext(ctx, h.binPath, h.args(args)...)
	cmd.Dir = h.dir
	cmd.Env = h.env
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = harnessWaitDelay

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exitCode = ee.ExitCode()
		} else {
			exitCode = -1
		}
	}
	return Result{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
	}
}

func completeFixtures(cfg harnessConfig) harnessConfig {
	if cfg.resolvedRepo != nil {
		found := false
		for _, repo := range cfg.repos {
			found = found || repo.Id == cfg.resolvedRepo.Id
		}
		if !found {
			cfg.repos = append(cfg.repos, cfg.resolvedRepo)
		}
	}
	if cfg.resolvedSession != nil {
		found := false
		for _, session := range cfg.sessions {
			found = found || session.Id == cfg.resolvedSession.Id
		}
		if !found {
			cfg.sessions = append(cfg.sessions, cfg.resolvedSession)
		}
	}
	// Notes and scheduled jobs may be supplied without a repository list. Their
	// owning repository must still be real, rather than an orphan mock row.
	referencedRepos := make([]string, 0, len(cfg.notes)+len(cfg.cronJobs))
	for _, note := range cfg.notes {
		referencedRepos = append(referencedRepos, note.RepoId)
	}
	for _, job := range cfg.cronJobs {
		referencedRepos = append(referencedRepos, job.RepoId)
	}
	for _, id := range referencedRepos {
		found := false
		for _, repo := range cfg.repos {
			found = found || repo.Id == id
		}
		if !found {
			cfg.repos = append(cfg.repos, &pb.Repo{Id: id, DisplayName: id})
		}
	}
	return cfg
}
