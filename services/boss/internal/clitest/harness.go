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
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/tuitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// Harness runs boss CLI commands against a mock daemon and captures output.
type Harness struct {
	Daemon  *tuitest.MockDaemon
	binPath string
	env     []string
}

// Option configures a Harness.
type Option func(*harnessConfig)

type harnessConfig struct {
	repos    []*pb.Repo
	sessions []*pb.Session
	chats    []*pb.ClaudeChat
	extraEnv []string
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

	var env []string
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "BOSS_SOCKET=") ||
			strings.HasPrefix(e, "BOSS_SKIP_SKILLS=") ||
			strings.HasPrefix(e, "BOSS_AUTH_E2E_EMAIL=") ||
			strings.HasPrefix(e, "BOSS_DAEMON_SKIP_LAUNCHCTL=") {
			continue
		}
		env = append(env, e)
	}
	env = append(env,
		"BOSS_SOCKET="+daemon.SocketPath(),
		"BOSS_SKIP_SKILLS=1",
	)
	env = append(env, cfg.extraEnv...)

	return &Harness{
		Daemon:  daemon,
		binPath: binPath,
		env:     env,
	}
}

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
