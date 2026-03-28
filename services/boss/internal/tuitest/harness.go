package tuitest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/tuidriver"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// bossBinaryPath and bossBinaryErr are set once by BuildBoss via TestMain.
var (
	bossBinaryPath string
	bossBinaryErr  error
)

// BuildBoss compiles the boss binary to a temporary directory.
// Call this from TestMain before m.Run(). The returned cleanup function
// removes the temporary binary.
func BuildBoss() (cleanup func()) {
	dir, err := os.MkdirTemp("", "boss-tuitest-*")
	if err != nil {
		bossBinaryErr = fmt.Errorf("mkdirtemp: %w", err)
		return func() {}
	}

	bossBinaryPath = filepath.Join(dir, "boss")
	cmd := exec.Command("go", "build", "-o", bossBinaryPath, "./cmd")
	cmd.Dir = serviceDir()
	out, err := cmd.CombinedOutput()
	if err != nil {
		bossBinaryErr = fmt.Errorf("build boss: %w\n%s", err, out)
	}

	return func() { _ = os.RemoveAll(dir) }
}

// serviceDir returns the path to the services/boss directory.
func serviceDir() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "..")
}

// Harness manages a mock daemon + TUI driver for integration testing.
type Harness struct {
	Driver *tuidriver.Driver
	Daemon *MockDaemon
	t      *testing.T
}

// Option configures the test harness.
type Option func(*harnessConfig)

type harnessConfig struct {
	repos    []*pb.Repo
	sessions []*pb.Session
	args     []string
}

// WithRepos seeds the mock daemon with repos.
func WithRepos(repos ...*pb.Repo) Option {
	return func(c *harnessConfig) {
		c.repos = append(c.repos, repos...)
	}
}

// WithSessions seeds the mock daemon with sessions.
func WithSessions(sessions ...*pb.Session) Option {
	return func(c *harnessConfig) {
		c.sessions = append(c.sessions, sessions...)
	}
}

// New creates a test harness with a mock daemon and TUI driver.
// It requires BuildBoss to have been called from TestMain.
func New(t *testing.T, opts ...Option) *Harness {
	t.Helper()

	if bossBinaryErr != nil {
		t.Fatalf("boss binary not available: %v", bossBinaryErr)
	}
	if bossBinaryPath == "" {
		t.Fatal("BuildBoss was not called from TestMain")
	}

	daemon := NewMockDaemon(t)

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

	// Filter out env vars we override to avoid conflicts with the developer's environment.
	var env []string
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "BOSS_SOCKET=") || strings.HasPrefix(e, "BOSS_SKIP_SKILLS=") {
			continue
		}
		env = append(env, e)
	}
	env = append(env,
		"BOSS_SOCKET="+daemon.SocketPath(),
		"BOSS_SKIP_SKILLS=1",
		"TERM=xterm-256color",
	)

	driver, err := tuidriver.New(tuidriver.Options{
		Command: bossBinaryPath,
		Env:     env,
		Width:   120,
		Height:  30,
		Args:    cfg.args,
	})
	if err != nil {
		t.Fatalf("create TUI driver: %v", err)
	}
	t.Cleanup(func() { _ = driver.Close() })

	return &Harness{
		Driver: driver,
		Daemon: daemon,
		t:      t,
	}
}
