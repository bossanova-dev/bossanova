package tuitest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

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

// seedSettingsAcknowledged writes a minimal settings.json with
// ProvidersAcknowledged=true into the per-test HOME so the boss subprocess
// skips the first-run onboarding gate.
func seedSettingsAcknowledged(t *testing.T, home, worktreeBaseDir string) {
	t.Helper()
	if err := SeedSettingsAcknowledged(home, worktreeBaseDir); err != nil {
		t.Fatalf("seed acknowledged settings: %v", err)
	}
}

// seedFirstRunSettings writes a settings.json that points boss at the test
// daemon socket but leaves providers unacknowledged. This makes the first-run
// onboarding gate fire (boss skips the daemon-startup preflight only when
// BOSS_SOCKET is set, so onboarding harnesses leave it unset) while boss still
// resolves the mock daemon directly via socket_path — no socket proxy needed.
func seedFirstRunSettings(t *testing.T, home, socketPath string) {
	t.Helper()
	if err := SeedFirstRunSettings(home, socketPath); err != nil {
		t.Fatalf("seed first run settings: %v", err)
	}
}

// Harness manages a mock daemon + TUI driver for integration testing.
type Harness struct {
	Driver *tuidriver.Driver
	Daemon *MockDaemon
}

// Option configures the test harness.
type Option func(*harnessConfig)

// E2ECloudAccessState names the cloud subscription state that the e2e-tagged
// boss binary should return from its fake cloud-access client.
type E2ECloudAccessState string

const (
	E2ECloudAccessActive             E2ECloudAccessState = "active"
	E2ECloudAccessNeedsSubscription  E2ECloudAccessState = "needs_subscription"
	E2ECloudAccessPendingEntitlement E2ECloudAccessState = "pending_entitlement_refresh"
	E2ECloudAccessBillingUnavailable E2ECloudAccessState = "billing_unavailable"
)

type harnessConfig struct {
	repos                         []*pb.Repo
	sessions                      []*pb.Session
	chats                         []*pb.ClaudeChat
	cronJobs                      []*pb.CronJob
	prs                           map[string][]*pb.PRSummary
	trackerIssues                 map[string][]*pb.TrackerIssue
	agents                        []*pb.AgentInfo
	args                          []string
	loggedInEmail                 string
	e2eLoginEmail                 string
	e2eCloudAccessSequence        []E2ECloudAccessState
	e2eCloudCheckoutURL           string
	e2eCloudCheckoutError         bool
	e2eCloudRefreshInterval       string
	e2eGitHubAppInstalledRepos    []string
	e2eGitHubAppInstallAfterPolls string
	terminalWidth                 int
	terminalHeight                int
	archiveDelay                  time.Duration
	archiveError                  string
	chatListDelay                 time.Duration
	chatListError                 string
	env                           []string
	skipSettingsAcknowledgedSeed  bool
	firstRunOnboarding            bool
	worktreeBaseDir               string
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

// WithWorktreeBaseDir seeds settings.json with a fixed worktree_base_dir so the
// rendered settings screen is deterministic. Without it the value defaults to
// `$HOME/.bossanova/worktrees`, which under a per-test temp HOME leaks a
// machine-specific, high-entropy path into captured screens.
func WithWorktreeBaseDir(dir string) Option {
	return func(c *harnessConfig) {
		c.worktreeBaseDir = dir
	}
}

// WithArchiveDelay slows ArchiveSession so tests can assert in-flight UI state.
func WithArchiveDelay(delay time.Duration) Option {
	return func(c *harnessConfig) {
		c.archiveDelay = delay
	}
}

// SetArchiveDelay slows ArchiveSession on a live daemon. The proof bridge uses
// it so the in-flight optimistic "archiving" row state is actually visible in
// recorded captures (BOS-251) — an instant mock archive settles before any
// observe() could ever catch it. Guarded by m.mu because the daemon is already
// serving: ArchiveSession reads archiveDelay on the RPC goroutine.
func (m *MockDaemon) SetArchiveDelay(delay time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.archiveDelay = delay
}

// WithArchiveError makes ArchiveSession fail with the given message.
func WithArchiveError(message string) Option {
	return func(c *harnessConfig) {
		c.archiveError = message
	}
}

// WithChatListDelay slows ListChats so tests can assert loading-state behavior.
func WithChatListDelay(delay time.Duration) Option {
	return func(c *harnessConfig) {
		c.chatListDelay = delay
	}
}

// WithChatListError makes ListChats fail with the given message.
func WithChatListError(message string) Option {
	return func(c *harnessConfig) {
		c.chatListError = message
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

// WithCronJobs seeds the mock daemon with cron jobs.
func WithCronJobs(jobs ...*pb.CronJob) Option {
	return func(c *harnessConfig) {
		c.cronJobs = append(c.cronJobs, jobs...)
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

// WithAgents overrides the agent runners returned by ListAgents. Tests
// that exercise multi-agent flows (settings render, agent picker) seed
// agents through this option.
func WithAgents(agents ...*pb.AgentInfo) Option {
	return func(c *harnessConfig) {
		c.agents = append(c.agents, agents...)
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

// WithE2ELogin makes the e2e-tagged boss subprocess complete the login flow
// as the given email through its fake auth device-code client.
func WithE2ELogin(email string) Option {
	return func(c *harnessConfig) {
		c.e2eLoginEmail = email
	}
}

// WithE2ECloudAccessSequence configures the fake cloud-access client to return
// the supplied subscription states in order.
func WithE2ECloudAccessSequence(states ...E2ECloudAccessState) Option {
	return func(c *harnessConfig) {
		c.e2eCloudAccessSequence = append([]E2ECloudAccessState(nil), states...)
	}
}

// WithE2ECloudCheckoutURL configures the fake cloud-access client checkout URL.
func WithE2ECloudCheckoutURL(rawURL string) Option {
	return func(c *harnessConfig) {
		c.e2eCloudCheckoutURL = rawURL
	}
}

// WithE2ECloudCheckoutError makes the fake cloud-access checkout request fail.
func WithE2ECloudCheckoutError() Option {
	return func(c *harnessConfig) {
		c.e2eCloudCheckoutError = true
	}
}

// WithE2ECloudRefreshInterval configures how often the fake cloud-access client
// should report entitlement refreshes to the e2e-tagged boss subprocess.
func WithE2ECloudRefreshInterval(interval string) Option {
	return func(c *harnessConfig) {
		c.e2eCloudRefreshInterval = interval
	}
}

// WithE2EGitHubAppInstalledRepos configures the e2e cloud client to report
// installed GitHub App repositories by owner/name.
func WithE2EGitHubAppInstalledRepos(repos ...string) Option {
	return func(c *harnessConfig) {
		c.e2eGitHubAppInstalledRepos = append(c.e2eGitHubAppInstalledRepos, repos...)
	}
}

// WithE2EGitHubAppInstallAfterPolls delays the fake GitHub App installed repo
// list until the Nth poll.
func WithE2EGitHubAppInstallAfterPolls(polls string) Option {
	return func(c *harnessConfig) {
		c.e2eGitHubAppInstallAfterPolls = polls
	}
}

// WithTerminalSize overrides the PTY dimensions for the TUI driver.
// Useful for tests that need more screen real estate (e.g. long forms).
func WithTerminalSize(width, height int) Option {
	return func(c *harnessConfig) {
		c.terminalWidth = width
		c.terminalHeight = height
	}
}

// WithEnv appends environment overrides for the boss subprocess.
func WithEnv(vars ...string) Option {
	return func(c *harnessConfig) {
		c.env = append(c.env, vars...)
	}
}

// WithoutSettingsAcknowledgedSeed skips the default ProvidersAcknowledged seed.
func WithoutSettingsAcknowledgedSeed() Option {
	return func(c *harnessConfig) {
		c.skipSettingsAcknowledgedSeed = true
	}
}

// WithFirstRunOnboarding launches boss into the first-run onboarding gate. It
// seeds a settings file that leaves providers unacknowledged but points
// socket_path at the mock daemon, and leaves BOSS_SOCKET unset so boss runs the
// local daemon-startup preflight (which hosts onboarding) while still dialing
// the mock daemon directly.
func WithFirstRunOnboarding() Option {
	return func(c *harnessConfig) {
		c.firstRunOnboarding = true
	}
}

// WithArgs sets the CLI arguments passed to the boss subprocess. Useful for
// tests that exercise subcommands like `boss new --agent <name>` directly,
// bypassing the home screen entry point.
func WithArgs(args ...string) Option {
	return func(c *harnessConfig) {
		c.args = append(c.args, args...)
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
	for _, j := range cfg.cronJobs {
		daemon.AddCronJob(j)
	}
	for repoID, prs := range cfg.prs {
		daemon.AddPRs(repoID, prs)
	}
	for repoID, issues := range cfg.trackerIssues {
		daemon.AddTrackerIssues(repoID, issues)
	}
	if len(cfg.agents) > 0 {
		daemon.SetAgents(cfg.agents)
	}
	daemon.archiveDelay = cfg.archiveDelay
	daemon.archiveError = cfg.archiveError
	daemon.chatListDelay = cfg.chatListDelay
	daemon.chatListError = cfg.chatListError

	// Each tuitest gets its own HOME so the boss subprocess's settings file
	// doesn't leak into the developer's real config (and vice versa). We
	// pre-seed ProvidersAcknowledged=true so the first-run onboarding gate
	// doesn't fire — tests that exercise onboarding can override by writing
	// their own settings file before launch (or omit the seed via a future
	// option if we add one).
	tempHome := t.TempDir()
	switch {
	case cfg.firstRunOnboarding:
		seedFirstRunSettings(t, tempHome, daemon.SocketPath())
	case !cfg.skipSettingsAcknowledgedSeed:
		seedSettingsAcknowledged(t, tempHome, cfg.worktreeBaseDir)
	}

	// Filter out env vars we override to avoid conflicts with the developer's environment.
	env := BaseHarnessEnv(os.Environ())
	env = append(env,
		"BOSS_SKIP_SKILLS=1",
		"BOSS_SKIP_PROVIDER_STARTUP_DAEMON_RESTART=1",
		"TERM=xterm-256color",
		"HOME="+tempHome,
	)
	if !cfg.firstRunOnboarding {
		// With BOSS_SOCKET set, boss skips the local daemon-startup preflight
		// (and therefore first-run onboarding) and dials this socket directly.
		// First-run onboarding harnesses leave it unset and resolve the mock
		// daemon via the seeded settings' socket_path instead.
		env = append(env, "BOSS_SOCKET="+daemon.SocketPath())
	}
	if runtime.GOOS != "darwin" {
		env = append(env, "XDG_CONFIG_HOME="+filepath.Join(tempHome, ".config"))
	}
	if cfg.loggedInEmail != "" {
		env = append(env, "BOSS_AUTH_E2E_EMAIL="+cfg.loggedInEmail)
	}
	if cfg.e2eLoginEmail != "" {
		env = append(env, "BOSS_AUTH_E2E_LOGIN_EMAIL="+cfg.e2eLoginEmail)
	}
	if len(cfg.e2eCloudAccessSequence) > 0 {
		states := make([]string, 0, len(cfg.e2eCloudAccessSequence))
		for _, state := range cfg.e2eCloudAccessSequence {
			states = append(states, string(state))
		}
		env = append(env, "BOSS_CLOUD_ACCESS_E2E_SEQUENCE="+strings.Join(states, ","))
	}
	if cfg.e2eCloudCheckoutURL != "" {
		env = append(env, "BOSS_CLOUD_ACCESS_E2E_CHECKOUT_URL="+cfg.e2eCloudCheckoutURL)
	}
	if cfg.e2eCloudCheckoutError {
		env = append(env, "BOSS_CLOUD_ACCESS_E2E_CHECKOUT_ERROR=1")
	}
	if cfg.e2eCloudRefreshInterval != "" {
		env = append(env, "BOSS_CLOUD_ACCESS_E2E_REFRESH_INTERVAL="+cfg.e2eCloudRefreshInterval)
	}
	if len(cfg.e2eGitHubAppInstalledRepos) > 0 {
		env = append(env, "BOSS_GITHUB_APP_E2E_INSTALLED_REPOS="+strings.Join(cfg.e2eGitHubAppInstalledRepos, ","))
	}
	if cfg.e2eGitHubAppInstallAfterPolls != "" {
		env = append(env, "BOSS_GITHUB_APP_E2E_INSTALL_AFTER_POLLS="+cfg.e2eGitHubAppInstallAfterPolls)
	}
	env = append(env, cfg.env...)

	width := 120
	if cfg.terminalWidth > 0 {
		width = cfg.terminalWidth
	}
	height := 30
	if cfg.terminalHeight > 0 {
		height = cfg.terminalHeight
	}
	driver, err := tuidriver.New(tuidriver.Options{
		Command: bossBinaryPath,
		Env:     env,
		Width:   width,
		Height:  height,
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
