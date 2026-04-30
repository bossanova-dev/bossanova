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

// BossBinaryPath returns the path to the compiled boss binary, or an error
// if BuildBoss has not been called or the build failed. Exported so other
// test packages (e.g. clitest) can reuse the single built binary.
func BossBinaryPath() (string, error) {
	if bossBinaryErr != nil {
		return "", bossBinaryErr
	}
	if bossBinaryPath == "" {
		return "", fmt.Errorf("BuildBoss was not called from TestMain")
	}
	return bossBinaryPath, nil
}

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
	// Build with the e2e tag so test-only overrides compile in (auth token
	// store override, etc). The production build omits the tag and therefore
	// never reaches these hooks.
	cmd := exec.Command("go", "build", "-tags", "e2e", "-o", bossBinaryPath, "./cmd")
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
}

// Option configures the test harness.
type Option func(*harnessConfig)

type harnessConfig struct {
	repos         []*pb.Repo
	sessions      []*pb.Session
	chats         []*pb.ClaudeChat
	prs           map[string][]*pb.PRSummary
	trackerIssues map[string][]*pb.TrackerIssue
	args          []string
	loggedInEmail string
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

// WithChats seeds the mock daemon with claude chats.
func WithChats(chats ...*pb.ClaudeChat) Option {
	return func(c *harnessConfig) {
		c.chats = append(c.chats, chats...)
	}
}

// WithPRs seeds the mock daemon with pull request summaries for a repo.
func WithPRs(repoID string, prs ...*pb.PRSummary) Option {
	return func(c *harnessConfig) {
		if c.prs == nil {
			c.prs = make(map[string][]*pb.PRSummary)
		}
		c.prs[repoID] = append(c.prs[repoID], prs...)
	}
}

// WithTrackerIssues seeds the mock daemon with Linear/tracker issues for a repo.
func WithTrackerIssues(repoID string, issues ...*pb.TrackerIssue) Option {
	return func(c *harnessConfig) {
		if c.trackerIssues == nil {
			c.trackerIssues = make(map[string][]*pb.TrackerIssue)
		}
		c.trackerIssues[repoID] = append(c.trackerIssues[repoID], issues...)
	}
}

// WithLoggedInUser makes the boss subprocess behave as if the given email is
// already authenticated. Wired via the BOSS_AUTH_E2E_EMAIL env var, which the
// e2e-tagged token-store override in services/boss/cmd/authstore_e2e.go reads
// to return an in-memory store with tokens for that email.
func WithLoggedInUser(email string) Option {
	return func(c *harnessConfig) {
		c.loggedInEmail = email
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
	for _, c := range cfg.chats {
		daemon.AddChat(c)
	}
	for repoID, prs := range cfg.prs {
		daemon.AddPRs(repoID, prs)
	}
	for repoID, issues := range cfg.trackerIssues {
		daemon.AddTrackerIssues(repoID, issues)
	}

	// Filter out env vars we override to avoid conflicts with the developer's environment.
	var env []string
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "BOSS_SOCKET=") ||
			strings.HasPrefix(e, "BOSS_SKIP_SKILLS=") ||
			strings.HasPrefix(e, "BOSS_AUTH_E2E_EMAIL=") {
			continue
		}
		env = append(env, e)
	}
	env = append(env,
		"BOSS_SOCKET="+daemon.SocketPath(),
		"BOSS_SKIP_SKILLS=1",
		"TERM=xterm-256color",
	)
	if cfg.loggedInEmail != "" {
		env = append(env, "BOSS_AUTH_E2E_EMAIL="+cfg.loggedInEmail)
	}

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
	}
}
