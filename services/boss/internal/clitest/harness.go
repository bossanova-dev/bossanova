// Package clitest provides end-to-end test infrastructure for the boss CLI's
// non-interactive commands (ls, show, archive, trash, repo, settings, daemon,
// ...). It reuses the TUI test infrastructure: the mock daemon implementation
// and the single compiled boss binary built by tuitest.BuildBoss.
//
// Each Harness instance spins up its own mock daemon on a Unix socket and
// runs the real compiled boss binary as a subprocess with BOSS_SOCKET pointed
// at that socket — no TUI, just stdout/stderr/exit-code capture.
package clitest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/recurser/boss/internal/tuitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// Harness runs boss CLI commands against a mock daemon and captures output.
type Harness struct {
	Daemon       *tuitest.MockDaemon
	binPath      string
	env          []string
	settingsPath string
}

const harnessWaitDelay = 5 * time.Second

// Option configures a Harness.
type Option func(*harnessConfig)

type harnessConfig struct {
	repos           []*pb.Repo
	sessions        []*pb.Session
	chats           []*pb.ClaudeChat
	cronJobs        []*pb.CronJob
	githubCallbacks []*pb.GithubCallback
	accounts        []*pb.Account
	notes           []*pb.Note
	checkSnapshots  map[string][]*pb.CheckSnapshot
	resolvedRepo    *pb.Repo
	resolvedSession *pb.Session
	extraEnv        []string
}

// WithRepos seeds the mock daemon with repos.
func WithRepos(repos ...*pb.Repo) Option {
	return func(c *harnessConfig) { c.repos = append(c.repos, repos...) }
}

// WithSessions seeds the mock daemon with sessions.
func WithSessions(sessions ...*pb.Session) Option {
	return func(c *harnessConfig) { c.sessions = append(c.sessions, sessions...) }
}

// WithChats seeds the mock daemon with claude chats.
func WithChats(chats ...*pb.ClaudeChat) Option {
	return func(c *harnessConfig) { c.chats = append(c.chats, chats...) }
}

// WithCronJobs seeds the mock daemon with cron jobs.
func WithCronJobs(jobs ...*pb.CronJob) Option {
	return func(c *harnessConfig) { c.cronJobs = append(c.cronJobs, jobs...) }
}

// WithGithubCallbacks seeds the mock daemon with GitHub callbacks.
func WithGithubCallbacks(cbs ...*pb.GithubCallback) Option {
	return func(c *harnessConfig) { c.githubCallbacks = append(c.githubCallbacks, cbs...) }
}

// WithAccounts seeds the mock daemon with accounts.
func WithAccounts(accounts ...*pb.Account) Option {
	return func(c *harnessConfig) { c.accounts = append(c.accounts, accounts...) }
}

// WithNotes seeds the mock daemon with notes.
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

// WithResolvedContext makes the mock daemon report the given repo and session
// as the working directory's context, so commands that fall back to
// ResolveContext (e.g. `boss notes add` with no --repo and no BOSS_REPO_ID) can
// be driven without arranging the subprocess's cwd. Either argument may be nil;
// passing two nils is the same as not using the option at all, because an
// unseeded daemon already reports no context.
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

// New creates a test harness with a mock daemon. It requires BuildBoss to
// have been called from TestMain so the compiled boss binary is reachable.
func New(t *testing.T, opts ...Option) *Harness {
	t.Helper()

	binPath, err := tuitest.BossBinaryPath()
	if err != nil {
		t.Fatalf("boss binary not available: %v", err)
	}

	daemon := tuitest.NewMockDaemon(t)

	var cfg harnessConfig
	for _, o := range opts {
		o(&cfg)
	}
	for _, r := range cfg.repos {
		daemon.AddRepo(r)
	}
	for _, s := range cfg.sessions {
		daemon.AddSession(s)
	}
	for _, c := range cfg.chats {
		daemon.AddChat(c)
	}
	for _, j := range cfg.cronJobs {
		daemon.AddCronJob(j)
	}
	for _, cb := range cfg.githubCallbacks {
		daemon.AddGithubCallback(cb)
	}
	for _, a := range cfg.accounts {
		daemon.SeedAccount(a, []byte("seed-credential"))
	}
	for _, n := range cfg.notes {
		daemon.AddNote(n)
	}
	for sessionID, snaps := range cfg.checkSnapshots {
		daemon.AddCheckSnapshots(sessionID, snaps...)
	}
	if cfg.resolvedRepo != nil || cfg.resolvedSession != nil {
		daemon.SetResolvedContext(cfg.resolvedRepo, cfg.resolvedSession)
	}

	// Default every harness to an isolated, per-test HOME and settings file so a
	// CLI command that writes settings.json can never touch the developer's real
	// file. Tests that need specific values (e.g. settings_profile_test, the
	// daemon-install tests' HOME) append their own via WithEnv, which wins
	// because os/exec dedups cmd.Env last-wins and extraEnv is appended last.
	tmp := t.TempDir()
	homeDir := filepath.Join(tmp, "home")
	if err := os.MkdirAll(homeDir, 0o700); err != nil {
		t.Fatalf("create temp HOME: %v", err)
	}
	settingsPath := filepath.Join(tmp, "settings.json")

	var env []string
	for _, e := range os.Environ() {
		// Strip vars the harness owns or that could leak the developer's real
		// config location into the subprocess: HOME/XDG_CONFIG_HOME resolve the
		// default settings path, and an inherited BOSS_SETTINGS_PATH would point
		// at the real file.
		if strings.HasPrefix(e, "BOSS_SOCKET=") ||
			strings.HasPrefix(e, "BOSS_SKIP_SKILLS=") ||
			strings.HasPrefix(e, "BOSS_AUTH_E2E_EMAIL=") ||
			strings.HasPrefix(e, "BOSS_DAEMON_SKIP_LAUNCHCTL=") ||
			strings.HasPrefix(e, "BOSS_SETTINGS_PATH=") ||
			strings.HasPrefix(e, "BOSS_REFUSE_DEFAULT_SETTINGS=") ||
			// Strip the ambient session/chat/repo context vars so tests default
			// deterministically (e.g. `callback add` with no --chat must fail when
			// BOSS_AGENT_SESSION_ID is unset). Tests that need them set their own
			// values via WithEnv, which is appended last and wins.
			strings.HasPrefix(e, "BOSS_AGENT_SESSION_ID=") ||
			strings.HasPrefix(e, "BOSS_SESSION_ID=") ||
			strings.HasPrefix(e, "BOSS_REPO_ID=") ||
			strings.HasPrefix(e, "HOME=") ||
			strings.HasPrefix(e, "XDG_CONFIG_HOME=") {
			continue
		}
		env = append(env, e)
	}
	env = append(env,
		"BOSS_SOCKET="+daemon.SocketPath(),
		"BOSS_SKIP_SKILLS=1",
		"HOME="+homeDir,
		"XDG_CONFIG_HOME="+filepath.Join(homeDir, ".config"),
		"BOSS_SETTINGS_PATH="+settingsPath,
		// Refuse the real default path in the subprocess too (testing.Testing()
		// is false there), so a test that overrides BOSS_SETTINGS_PATH with a
		// bad value still can't fall back to clobbering the real file.
		"BOSS_REFUSE_DEFAULT_SETTINGS=1",
	)
	env = append(env, cfg.extraEnv...)

	return &Harness{
		Daemon:       daemon,
		binPath:      binPath,
		env:          env,
		settingsPath: settingsPath,
	}
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
	cmd := exec.CommandContext(ctx, h.binPath, args...)
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
	cmd := exec.CommandContext(ctx, h.binPath, args...)
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
