package tuitest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/realstack"
	"github.com/recurser/boss/internal/tuidriver"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// bossBinaryPath and bossBinaryErr are set once by BuildBoss via TestMain.
// bossBinaryErr records only a failed build.
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
	// #nosec G204 -- test-only go build -tags e2e -o <tmp> ./cmd/boss; only tmp out-path varies
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	cmd := exec.Command("go", "build", "-tags", "e2e", "-o", bossBinaryPath, "./cmd/boss")
	cmd.Dir = serviceDir()
	out, err := cmd.CombinedOutput()
	if err != nil {
		bossBinaryErr = fmt.Errorf("build boss: %w\n%s", err, out)
		return func() { _ = os.RemoveAll(dir) }
	}

	// The first exec of a freshly linked binary is slow on macOS (0.6s idle and
	// up to 3.6s under load, against 0.02s after). Paying it here keeps it out
	// of the first test's wait for boss's first screen. It only warms the
	// binary, so a failed run is logged and the tests still run.
	if err := warmBoss(bossBinaryPath, dir, bossWarmUpTimeout); err != nil {
		fmt.Fprintf(os.Stderr, "tuitest: warm-up run of boss failed, continuing: %v\n", err)
	}

	return func() { _ = os.RemoveAll(dir) }
}

// bossWarmUpTimeout bounds the warm-up run, so a boss that hangs on --help
// cannot stall the package before its first test.
const bossWarmUpTimeout = 30 * time.Second

// warmBoss runs binary --help once with home as HOME.
func warmBoss(binary, home string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// #nosec G204 -- test-only run of the boss binary built above, in a throwaway HOME
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	warm := exec.CommandContext(ctx, binary, "--help")
	warm.Env = append(BaseHarnessEnv(os.Environ()), "HOME="+home, "BOSS_SKIP_SKILLS=1")
	warm.WaitDelay = time.Second
	if out, err := warm.CombinedOutput(); err != nil {
		return fmt.Errorf("%s --help: %w\n%s", binary, err, out)
	}
	return nil
}

// serviceDir returns the path to the services/boss directory.
func serviceDir() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "..")
}

// Harness manages a TUI driver and its real isolated daemon.
type Harness struct {
	Driver *tuidriver.Driver
	Stack  *realstack.Stack
	// RepoIDs and SessionIDs map fixture keys to store-generated identities.
	RepoIDs    map[string]string
	SessionIDs map[string]string
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
	chats                         []*pb.AgentChat
	cronJobs                      []*pb.CronJob
	githubCallbacks               []*pb.GitHubCallback
	notes                         []*pb.Note
	checkSnapshots                map[string][]*pb.CheckSnapshot
	prs                           map[string][]*pb.PRSummary
	trackerIssues                 map[string][]*pb.TrackerIssue
	args                          []string
	loggedInEmail                 string
	e2eLoginEmail                 string
	e2eLogoutError                bool
	e2eCloudAccessSequence        []E2ECloudAccessState
	e2eCloudCheckoutURL           string
	e2eCloudCheckoutError         bool
	e2eCloudRefreshInterval       string
	e2eGitHubAppInstalledRepos    []string
	e2eGitHubAppInstallAfterPolls string
	terminalWidth                 int
	terminalHeight                int
	env                           []string
	firstRunOnboarding            bool
	worktreeBaseDir               string
}

// WithRepos seeds the real daemon with repos.
func WithRepos(repos ...*pb.Repo) Option {
	return func(c *harnessConfig) {
		c.repos = append(c.repos, repos...)
	}
}

// WithSessions seeds the real daemon with sessions.
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

// WithChats seeds the real daemon with claude chats.
func WithChats(chats ...*pb.AgentChat) Option {
	return func(c *harnessConfig) {
		c.chats = append(c.chats, chats...)
	}
}

// WithPRs seeds the real daemon with pull request summaries for a repo.
func WithPRs(repoID string, prs ...*pb.PRSummary) Option {
	return func(c *harnessConfig) {
		if c.prs == nil {
			c.prs = make(map[string][]*pb.PRSummary)
		}
		c.prs[repoID] = append(c.prs[repoID], prs...)
	}
}

// WithCronJobs seeds the real daemon with cron jobs.
func WithCronJobs(jobs ...*pb.CronJob) Option {
	return func(c *harnessConfig) {
		c.cronJobs = append(c.cronJobs, jobs...)
	}
}

// WithGitHubCallbacks seeds the real daemon with GitHub callbacks.
func WithGitHubCallbacks(cbs ...*pb.GitHubCallback) Option {
	return func(c *harnessConfig) {
		c.githubCallbacks = append(c.githubCallbacks, cbs...)
	}
}

// WithNotes seeds the real daemon with notes before the TUI starts, so Home's
// first read sees them.
func WithNotes(notes ...*pb.Note) Option {
	return func(c *harnessConfig) {
		c.notes = append(c.notes, notes...)
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

// WithE2ELogoutError makes the e2e token store reject logout without removing
// its seeded credentials, so TUI tests can exercise failed keychain deletion.
func WithE2ELogoutError() Option {
	return func(c *harnessConfig) {
		c.e2eLogoutError = true
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

// WithFirstRunOnboarding launches boss into the first-run onboarding gate. It
// seeds a settings file that leaves providers unacknowledged but points
// socket_path at the isolated daemon, and leaves BOSS_SOCKET unset so boss runs the
// local daemon-startup preflight (which hosts onboarding) while still dialing
// the isolated daemon directly.
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

func launch(t *testing.T, cfg harnessConfig, stack *realstack.Stack) *Harness {
	t.Helper()
	socket := stack.SocketPath()
	tempHome := stack.Home()
	env := append(stack.Env(),
		"BOSS_SKIP_SKILLS=1",
		"BOSS_SKIP_PROVIDER_STARTUP_DAEMON_RESTART=1",
		"TERM=xterm-256color",
	)
	if !cfg.firstRunOnboarding {
		env = append(env, "BOSS_SOCKET="+socket)
	}
	if runtime.GOOS != "darwin" {
		env = append(env, "XDG_CONFIG_HOME="+filepath.Join(tempHome, ".config"))
	}

	if cfg.loggedInEmail != "" {
		env = append(env, "BOSS_AUTH_E2E_EMAIL="+cfg.loggedInEmail)
	}
	if cfg.e2eLoginEmail != "" || len(cfg.e2eCloudAccessSequence) > 0 {
		env = append(env, "BOSS_CLOUD_URL=https://cloud.example.test")
	}
	if cfg.e2eLoginEmail != "" {
		env = append(env, "BOSS_AUTH_E2E_LOGIN_EMAIL="+cfg.e2eLoginEmail)
	}
	if cfg.e2eLogoutError {
		env = append(env, "BOSS_AUTH_E2E_LOGOUT_ERROR=1")
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
	rawOutput := captureCast(t, width, height)
	driver, err := tuidriver.New(tuidriver.Options{
		Command:   bossBinaryPath,
		Env:       env,
		Width:     width,
		Height:    height,
		Args:      cfg.args,
		RawOutput: rawOutput,
	})
	if err != nil {
		t.Fatalf("create TUI driver: %v", err)
	}
	t.Cleanup(func() { _ = driver.Close() })

	return &Harness{
		Driver: driver,
		Stack:  stack,
	}
}

// WithTrackerIssues seeds a real Linear task-source plugin through a local GraphQL server.
func WithTrackerIssues(repoID string, issues ...*pb.TrackerIssue) Option {
	return func(c *harnessConfig) {
		if c.trackerIssues == nil {
			c.trackerIssues = make(map[string][]*pb.TrackerIssue)
		}
		c.trackerIssues[repoID] = append(c.trackerIssues[repoID], issues...)
	}
}

// WithCheckSnapshots seeds persisted CI observations for a fixture session.
func WithCheckSnapshots(sessionID string, snapshots ...*pb.CheckSnapshot) Option {
	return func(c *harnessConfig) {
		if c.checkSnapshots == nil {
			c.checkSnapshots = map[string][]*pb.CheckSnapshot{}
		}
		c.checkSnapshots[sessionID] = snapshots
	}
}
