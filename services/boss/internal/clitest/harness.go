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

// Option configures a Harness.
type Option func(*harnessConfig)

type harnessConfig struct {
	repos           []*pb.Repo
	sessions        []*pb.Session
	chats           []*pb.ClaudeChat
	cronJobs        []*pb.CronJob
	githubCallbacks []*pb.GithubCallback
	accounts        []*pb.Account
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
