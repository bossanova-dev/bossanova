package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"
	"golang.org/x/mod/semver"
	"golang.org/x/term"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/boss/internal/accountflow"
	"github.com/recurser/boss/internal/client"
	"github.com/recurser/boss/internal/daemon"
	"github.com/recurser/boss/internal/preflight"
	"github.com/recurser/boss/internal/upgrade"
	"github.com/recurser/boss/internal/views"
	"github.com/recurser/bossalib/buildinfo"
	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/daemonstate"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// newClient picks the transport: the cloud orchestrator (--remote), a bossd on
// another machine over an ssh tunnel (--host), or the local daemon. Only the
// local branch may start a daemon.
func newClient(cmd *cobra.Command) (client.BossClient, error) {
	if remote := remoteURL(cmd); remote != "" {
		return newRemoteClient(cmd, remote)
	}
	if host := hostDestination(cmd); host != "" {
		return newHostClient(cmd, host)
	}
	socketPath, err := client.DefaultSocketPath()
	if err != nil {
		return nil, fmt.Errorf("socket path: %w", err)
	}

	// Skip auto-start when socket is explicitly provided (test mode). Routed
	// through the package var so a test can assert the --host branch never
	// reaches it.
	if os.Getenv("BOSS_SOCKET") == "" {
		if err := daemonEnsureRunning(socketPath); err != nil {
			return nil, fmt.Errorf("daemon failed: %w\nRun 'boss daemon install' to set up automatic startup, or start bossd manually", err)
		}
	}

	return client.NewLocal(socketPath), nil
}

// guardInteractiveStdin fails fast when an interactive registration flow would
// block on a terminal that is not attached. --token-stdin (claude) is the one
// non-interactive escape hatch and skips the guard. Returning cleanly here —
// before any client dial or subprocess — is what a headless agent or cron hits.
func guardInteractiveStdin(tokenStdin bool) error {
	if tokenStdin {
		return nil
	}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		return nil
	}
	return errors.New("boss account add is interactive; run it in a terminal (or pass --token-stdin for claude)")
}

// requireLocalRegistration refuses interactive account registration against a
// remote (--remote) daemon: the setup-token walkthrough and codex device flow
// mint credentials by running a LOCAL subprocess, so the flow is only coherent
// against the local daemon. It runs before any client dial or subprocess.
// The same reasoning applies verbatim to --host: the subprocess would run here,
// so the credential it mints would never be the remote machine's.
func requireLocalRegistration(cmd *cobra.Command) error {
	if remoteURL(cmd) != "" {
		return errors.New("boss account add registers credentials on the local daemon and cannot target a remote (--remote) daemon")
	}
	if hostDestination(cmd) != "" {
		return errors.New("boss account add registers credentials on the local daemon and cannot target a remote (--host) daemon; run it in a shell on that host instead")
	}
	return nil
}

// requireLocalDaemonTarget refuses the `boss daemon` subtree against --host.
// Those subcommands manage *this* machine's bossd — its LaunchAgent or systemd
// unit, its process, its socket, its token — by running local subprocesses, so
// under --host they would silently act on the local daemon while the user was
// plainly asking about the remote one. `daemon stop` and `daemon uninstall`
// make that mistake expensive, and `daemon restart` is exactly what a lost
// connection used to suggest. It runs in the root PersistentPreRunE, before any
// dial.
func requireLocalDaemonTarget(cmd *cobra.Command) error {
	if cmd == nil {
		return nil
	}
	path := cmd.CommandPath()
	if path != "boss daemon" && !strings.HasPrefix(path, "boss daemon ") {
		return nil
	}
	host := hostDestination(cmd)
	if host == "" {
		return nil
	}
	return fmt.Errorf("boss daemon commands manage the local bossd and cannot target a remote (--host %s) daemon; run `ssh %s boss daemon ...` on that host instead", host, host)
}

// runAccountAddClaude drives the interactive `boss account add claude`
// registration: the claude setup-token walkthrough, or a pasted/piped token
// with --token-stdin. The TTY guard runs before any RPC or subprocess.
func runAccountAddClaude(cmd *cobra.Command) error {
	if err := requireLocalRegistration(cmd); err != nil {
		return err
	}
	tokenStdin, _ := cmd.Flags().GetBool("token-stdin")
	if err := guardInteractiveStdin(tokenStdin); err != nil {
		return err
	}
	c, err := newClient(cmd)
	if err != nil {
		return err
	}
	label, _ := cmd.Flags().GetString("label")
	timeout, _ := cmd.Flags().GetDuration("timeout")
	priority, _ := cmd.Flags().GetInt32("priority")
	return accountflow.RunClaudeAdd(cmd.Context(), accountflow.ClaudeOptions{
		Exec:      accountflow.NewOSExec(),
		Prompter:  accountflow.NewIOPrompter(cmd.InOrStdin(), cmd.OutOrStdout()),
		Client:    c,
		Timeout:   timeout,
		PasteMode: tokenStdin,
		Label:     label,
		Priority:  priority,
	})
}

// runAccountAddCodex drives the interactive `boss account add codex` device
// flow. Codex has no token-stdin path (it needs an interactive browser
// round-trip), so --token-stdin is rejected up front.
func runAccountAddCodex(cmd *cobra.Command) error {
	if err := requireLocalRegistration(cmd); err != nil {
		return err
	}
	if tokenStdin, _ := cmd.Flags().GetBool("token-stdin"); tokenStdin {
		return errors.New("--token-stdin is not supported for codex; codex registration uses an interactive device flow")
	}
	if err := guardInteractiveStdin(false); err != nil {
		return err
	}
	c, err := newClient(cmd)
	if err != nil {
		return err
	}
	label, _ := cmd.Flags().GetString("label")
	timeout, _ := cmd.Flags().GetDuration("timeout")
	priority, _ := cmd.Flags().GetInt32("priority")
	return accountflow.RunCodexAdd(cmd.Context(), accountflow.CodexOptions{
		Exec:     accountflow.NewOSExec(),
		Prompter: accountflow.NewIOPrompter(cmd.InOrStdin(), cmd.OutOrStdout()),
		Client:   c,
		Timeout:  timeout,
		Label:    label,
		Priority: priority,
	})
}

// newRemoteClient creates a RemoteClient with a JWT from the keychain.
func newRemoteClient(cmd *cobra.Command, baseURL string) (client.BossClient, error) {
	mgr, err := newAuthManager(cmd)
	if err != nil {
		return nil, fmt.Errorf("auth: %w (run 'boss login' first)", err)
	}
	ctx := context.Background()
	token, err := mgr.AccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("access token: %w (run 'boss login' first)", err)
	}
	c := client.NewRemote(baseURL, token)
	if err := requireActiveCloudAccess(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

// launchTUI runs preflight checks, dials the daemon, builds the App, and
// starts the Bubble Tea program. Failures from the preflight or the daemon
// connection are shown as a blocking TUI screen rather than a stderr exit
// so the user sees the message in the same surface they launched.
//
// For local daemon failures the screen polls for the socket coming back
// and resumes startup automatically once it does — restarting bossd in
// another terminal is enough to recover without re-running boss.
//
// configure runs after the App is constructed and before tea.Run, giving
// callers a chance to override the initial view or seed view-specific
// state (e.g. the session id for an attach).
func launchTUI(cmd *cobra.Command, configure func(*views.App)) error {
	return launchTUIWithOptions(cmd, launchTUIOptions{configure: configure})
}

type launchTUIOptions struct {
	configure       func(*views.App)
	attachSessionID string
}

func launchTUIWithOptions(cmd *cobra.Command, opts launchTUIOptions) error {
	// Self-heal a terminal that a previous boss run left stranded in
	// mouse-reporting or focus-reporting mode. An abnormal exit (SSH drop =>
	// SIGHUP) skips every teardown defer, so neither the modes a proxied
	// tmux/Claude pane enabled nor boss's own ?1004 are ever reset, and the
	// user's shell echoes pointer/focus reports as garbage text. This launch
	// runs over a fresh connection to the same terminal, so writing the reset
	// here clears it; boss's own renderer re-asserts what it needs on its first
	// frame. Gated on stdout being a terminal so piped output is never
	// corrupted. See BOS-650.
	writeStdoutReset()
	// Arm the SIGHUP rescue before the *first* Bubble Tea program runs, not just
	// before the main one. Preflight and the daemon wait are full tea programs
	// that enable the same input-reporting modes, and views.RunDaemonWait is an
	// explicitly indefinite unattended wait — precisely the walked-away-and-the-
	// SSH-dropped case this ticket is about. The tmux options are not known yet,
	// so the cleanup reads them from tmuxRestore, which is filled in below; the
	// handler itself stays armed from here to teardown. See BOS-650.
	var tmuxRestore atomic.Pointer[func()]
	defer installHangupRescueFn(hangupCleanup(&tmuxRestore))()
	if needsLocalTmux(cmd) {
		if issue := preflight.CheckTmux(); issue != nil {
			return views.RunPreflight(*issue)
		}
	}
	if issue := preflight.CheckTerminal(); issue != nil {
		return views.RunPreflight(*issue)
	}
	if needsLocalDaemonStartup(cmd) {
		if issue := preflight.CheckShellTools(); issue != nil {
			return views.RunPreflight(*issue)
		}
		if err := runLocalProviderStartupBeforeClient(); err != nil {
			if views.IsPreflightCancelled(err) {
				return nil
			}
			return err
		}
	}
	// e2e-only: stage the --host tunnel-dropped screen before the client dials,
	// so a proof scenario can capture the reconnecting state on a harness with no
	// network. Compiles to an unconditional nil in production builds.
	if err := runE2EHostReconnectSeed(); err != nil {
		if views.IsPreflightCancelled(err) {
			return nil
		}
		return err
	}
	// e2e-only: put the TUI into a --host remote context without a tunnel, so a
	// proof scenario can capture the views that degrade under --host. Compiles to
	// an empty function in production builds.
	applyE2EHostAttachSeed()
	c, err := newClient(cmd)
	if err != nil {
		c, err = handleClientStartupError(cmd, err, func() (client.BossClient, error) {
			return waitForDaemon(cmd, err)
		})
		if err != nil {
			if views.IsPreflightCancelled(err) {
				return nil
			}
			return err
		}
	}
	authMgr := newOptionalAuthManager(cmd)
	settings := launchSettings(time.Now())
	if err := runAgentPreflights(context.Background(), cmd, c, settings, opts.attachSessionID); err != nil {
		if views.IsPreflightCancelled(err) {
			return nil
		}
		return err
	}
	app := views.NewApp(c, authMgr)
	app.WithSettings(settings)
	if resolveE2ELoginEmail() != "" || resolveE2ECloudAccessClient() != nil {
		views.DisableExternalBrowserForE2E()
	}
	if interval := e2eCloudRefreshInterval(); interval > 0 {
		views.SetSubscriptionPollIntervalOverride(interval)
		views.SetGitHubAppInstallPollIntervalOverride(interval)
	}
	app.WithTelemetry(commandTelemetryClient(cmd))
	if authMgr != nil {
		if cloud := cloudURL(cmd); cloud != "" {
			app.WithCloudAccessClient(newAuthCloudAccessClient(authMgr, cloud))
		}
		app.WithCheckoutURLs(cloudReturnURL(), cloudCancelURL())
		app.WithSubscriptionURL(cloudSubscribeURL())
	}
	if opts.configure != nil {
		opts.configure(&app)
	}
	// Under tmux, enable the session options boss needs to deliver desktop
	// notifications (allow-passthrough) and receive focus events (focus-events),
	// restoring them on exit. No-op outside tmux or when notifications are off.
	restoreTmux := views.EnableTmuxNotificationOptions(config.NotificationsEnabled(settings))
	defer restoreTmux()
	// A SIGHUP (dropped SSH connection, closed tab, `kill -HUP`) terminates Go
	// processes outright, so neither the defer above nor the terminal-mode reset
	// the PTY proxy runs on a clean return ever happens. Hand the tmux half to
	// the rescue armed above. (It does NOT restore raw mode — the PTY proxy's
	// saved termios is not reachable from here; see the plan's Risks section.)
	// Re-running restoreTmux is harmless: it unsets a captured option list, so
	// the window between the deferred restoreTmux above and the deferred stop
	// cannot do damage.
	tmuxRestore.Store(&restoreTmux)
	p := tea.NewProgram(app)
	_, err = p.Run()
	return err
}

type agentPreflightClient interface {
	ResolveContext(ctx context.Context, workingDir string) (*pb.ResolveContextResponse, error)
	GetSession(ctx context.Context, id string, opts client.SessionReadOptions) (*pb.Session, error)
	ListAgents(ctx context.Context) ([]client.AgentInfo, error)
}

var checkAgentResolvableForPreflight = preflight.CheckAgentResolvable

var cliBackedAgentProviders = map[string]bool{
	"claude": true,
	"codex":  true,
}

func runAgentPreflights(ctx context.Context, cmd *cobra.Command, c agentPreflightClient, settings config.Settings, sessionID string) error {
	// The probe runs `command -v <agent>` in a login shell *here*, against a
	// worktree path the daemon reported. Neither is local when the daemon is
	// not: --remote's agents run in the cloud, and --host's run on the other
	// end of the tunnel, where the CLI is installed and the worktree exists.
	// Probing the local machine for them would block the TUI over a binary it
	// was never going to launch — the same reasoning that already skips
	// daemon.EnsureRunning and preflight.CheckShellTools for both flags.
	if remoteURL(cmd) != "" || hostDestination(cmd) != "" {
		return nil
	}
	loadedAgents, err := c.ListAgents(ctx)
	if err != nil {
		return err
	}
	worktree, agentName, err := agentPreflightTarget(ctx, c, sessionID)
	if err != nil {
		return err
	}
	for _, agent := range enabledAgentProviders(settings, loadedAgents, agentName) {
		if issue := checkAgentResolvableForPreflight(settings.LoginShell, agent, worktree); issue != nil {
			return views.RunPreflight(*issue)
		}
	}
	return nil
}

func needsLocalDaemonStartup(cmd *cobra.Command) bool {
	return remoteURL(cmd) == "" && hostDestination(cmd) == "" && os.Getenv("BOSS_SOCKET") == ""
}

// needsLocalTmux reports whether this invocation will run a tmux client on THIS
// machine, and so whether the missing-tmux install screen is a real blocker.
//
// Under --host it is not: the tmux server lives on the remote host, and an
// attach reaches it as `ssh -t <destination> tmux ...` (views.buildAttachCommand),
// so the client machine needs ssh and nothing else. Gating the check is what
// makes that setup usable — an unconditional CheckTmux returns the install
// screen before the TUI ever starts, which no amount of correct remote setup can
// get past.
//
// Deliberately narrower than needsLocalDaemonStartup, which this must not be
// folded into despite the similar shape:
//
//   - --remote (cloud) still attaches through the LOCAL tmux — remoteHostDestination()
//     is empty there, so buildAttachCommand returns the local form — and still needs it.
//   - BOSS_SOCKET names a local daemon (agent worktrees set it), which is a
//     local tmux server like any other.
//
// The rest of boss's local tmux use — new-window in newtab.go, the notification
// options in tmuxopts.go — is already gated on $TMUX being set, which cannot be
// true without tmux, so this is the only gate that has to know about --host.
func needsLocalTmux(cmd *cobra.Command) bool {
	return hostDestination(cmd) == ""
}

func agentPreflightTarget(ctx context.Context, c agentPreflightClient, sessionID string) (string, string, error) {
	if sessionID != "" {
		sess, err := c.GetSession(ctx, sessionID, client.SessionReadOptions{})
		if err != nil {
			return "", "", err
		}
		if worktree := sess.GetWorktreePath(); worktree != "" {
			return worktree, sess.GetAgentName(), nil
		}
		return "", "", fmt.Errorf("session %s has no worktree path", sessionID)
	}

	wd, err := os.Getwd()
	if err != nil {
		return "", "", err
	}
	resolved, err := c.ResolveContext(ctx, wd)
	if err != nil || resolved == nil || resolved.GetSession() == nil {
		return wd, "", nil
	}
	sess := resolved.GetSession()
	if worktree := sess.GetWorktreePath(); worktree != "" {
		return worktree, sess.GetAgentName(), nil
	}
	return wd, sess.GetAgentName(), nil
}

func enabledAgentProviders(settings config.Settings, loadedAgents []client.AgentInfo, agentName string) []string {
	agentProviders := make(map[string]bool, len(loadedAgents))
	for _, agent := range loadedAgents {
		if agent.Name != "" {
			agentProviders[agent.Name] = true
		}
	}

	agents := make([]string, 0, 2)
	seen := map[string]bool{}
	for _, plugin := range settings.Plugins {
		if agentName != "" && plugin.Name != agentName {
			continue
		}
		if !plugin.Enabled || !agentProviders[plugin.Name] || !cliBackedAgentProviders[plugin.Name] || seen[plugin.Name] {
			continue
		}
		agents = append(agents, plugin.Name)
		seen[plugin.Name] = true
	}
	return agents
}

func handleClientStartupError(cmd *cobra.Command, err error, wait func() (client.BossClient, error)) (client.BossClient, error) {
	if isCloudGateError(err) {
		return nil, err
	}
	if remoteURL(cmd) != "" {
		return nil, err
	}
	return wait()
}

func remoteURL(cmd *cobra.Command) string {
	if cmd == nil || cmd.Root() == nil {
		return ""
	}
	remote, _ := cmd.Root().Flags().GetString("remote")
	return remote
}

// hostDestination returns the --host ssh destination, unparsed. An absent flag
// (bare cobra.Command in a test) reads as empty, like remoteURL.
func hostDestination(cmd *cobra.Command) string {
	if cmd == nil || cmd.Root() == nil {
		return ""
	}
	host, _ := cmd.Root().Flags().GetString("host")
	return host
}

// hostSocketOverride returns --host-socket, the escape hatch for remotes where
// `boss` is not on the non-interactive ssh PATH.
func hostSocketOverride(cmd *cobra.Command) string {
	if cmd == nil || cmd.Root() == nil {
		return ""
	}
	socket, _ := cmd.Root().Flags().GetString("host-socket")
	return socket
}

// waitForDaemon decides what to do when the initial newClient call fails.
// For a local socket it shows the auto-recovering daemon-wait screen and
// dials again once the socket is back. For remote/--remote or socket-path
// failures (which don't auto-recover) it falls back to the static preflight
// screen and propagates the original error.
//
// A --host failure gets its own auto-recovering screen: the supervised ssh
// tunnel redials on its own, so killing it mid-session must not kill boss.
func waitForDaemon(cmd *cobra.Command, origErr error) (client.BossClient, error) {
	issue := *preflight.DaemonIssue(origErr)
	if host := hostDestination(cmd); host != "" {
		return waitForHostDaemon(host, origErr)
	}
	remote, _ := cmd.Root().Flags().GetString("remote")
	if remote != "" {
		return nil, views.RunPreflight(issue)
	}
	socketPath, pathErr := client.DefaultSocketPath()
	if pathErr != nil {
		return nil, views.RunPreflight(issue)
	}
	check := func() bool { return daemon.IsSocketReachable(socketPath) }
	if err := views.RunDaemonWait(issue, check); err != nil {
		return nil, err
	}
	return client.NewLocal(socketPath), nil
}

func runTUI(cmd *cobra.Command) error {
	return launchTUI(cmd, nil)
}

func runLocalProviderStartupBeforeClient() error {
	result, err := runProviderStartupIfNeeded()
	if (result.LoginShellChanged || result.SettingsChanged) && os.Getenv("BOSS_SKIP_PROVIDER_STARTUP_DAEMON_RESTART") == "" {
		if restartErr := restartDaemonAfterLoginShellCapture(); restartErr != nil {
			return restartErr
		}
	}
	return err
}

var restartDaemonAfterLoginShellCapture = func() error {
	socketPath, err := defaultSocketPath()
	if err != nil {
		return fmt.Errorf("restart daemon after login shell capture: socket path: %w", err)
	}
	if !daemonSocketReachable(socketPath) {
		return nil
	}
	if err := restartReachableDaemonForSettingsReload(socketPath); err != nil {
		return fmt.Errorf("restart daemon after login shell capture: %w", err)
	}
	return nil
}

func restartReachableDaemonForSettingsReload(socketPath string) error {
	return restartReachableDaemonForSettingsReloadWith(
		socketPath,
		daemon.GetStatus,
		daemon.Stop,
		daemon.EnsureRunning,
		terminateBossdProcesses,
		waitForCurrentProfileBossdExit,
		waitForSocketGone,
	)
}

func restartReachableDaemonForSettingsReloadWith(
	socketPath string,
	getStatus func() (*daemon.Status, error),
	stop func() error,
	ensureRunning func(string) error,
	terminateStandalone func() (int, error),
	waitStandaloneExit func() bool,
	waitSocketGone func(string) bool,
) error {
	st, err := getStatus()
	if err != nil {
		return fmt.Errorf("daemon restart: %w", err)
	}
	if !st.Installed || !st.Running {
		n, err := terminateStandalone()
		if err != nil {
			return fmt.Errorf("restart standalone bossd failed: %w", err)
		}
		if n > 0 && socketPath != "" && !waitSocketGone(socketPath) {
			return fmt.Errorf("timed out waiting for standalone bossd to stop after %s", daemon.LifecycleShutdownTimeout)
		}
		if n > 0 && waitStandaloneExit != nil && !waitStandaloneExit() {
			return fmt.Errorf("timed out waiting for standalone bossd to exit after %s", daemon.LifecycleShutdownTimeout)
		}
		if err := ensureRunning(socketPath); err != nil {
			if !st.Installed {
				return fmt.Errorf("restart standalone bossd failed: %w", err)
			}
			return fmt.Errorf("restart daemon failed: %w", err)
		}
		return nil
	}
	if err := stop(); err != nil {
		return fmt.Errorf("stop daemon failed: %w", err)
	}
	if socketPath != "" && !waitSocketGone(socketPath) {
		return fmt.Errorf("timed out waiting for daemon socket to stop after %s", daemon.LifecycleShutdownTimeout)
	}
	if err := ensureRunning(socketPath); err != nil {
		return fmt.Errorf("restart daemon failed: %w", err)
	}
	return nil
}

var upgradeCurrentVersion = func() string {
	return buildinfo.Version
}

var checkUpgrade = func(ctx context.Context, current string) (upgrade.CheckResult, error) {
	return upgrade.Checker{Token: upgrade.ResolveGitHubToken(ctx)}.Check(ctx, current)
}

// upgradeActionCachePath resolves the shared upgrade-check cache path for the
// `boss upgrade` action so pressing [u] reuses a fresh (<24h) banner check
// instead of spending another (often anonymous, rate-limited) API request.
// Indirected so tests can point it at a temp file or disable it with "".
var upgradeActionCachePath = upgrade.DefaultCachePath

// freshUpgradeResultFromCache returns a CheckResult reconstructed from a fresh
// (<CacheTTL) cache entry matching current, letting `boss upgrade` skip the
// network call the TUI banner already made. Snooze state is intentionally
// ignored: the user explicitly asked to upgrade. Returns ok=false when the
// cache is disabled, missing, stale, or for a different version.
func freshUpgradeResultFromCache(current string) (upgrade.CheckResult, bool) {
	path := upgradeActionCachePath()
	if path == "" {
		return upgrade.CheckResult{}, false
	}
	entry, ok, err := upgrade.ReadFreshCache(path, current, time.Now(), upgrade.CacheTTL)
	if err != nil || !ok {
		return upgrade.CheckResult{}, false
	}
	return upgrade.CheckResult{
		CurrentVersion: entry.CurrentVersion,
		LatestVersion:  entry.LatestVersion,
		ReleaseURL:     entry.ReleaseURL,
		Available:      upgrade.CompareStableVersions(entry.CurrentVersion, entry.LatestVersion) == upgrade.CompareOlder,
	}, true
}

var executablePath = os.Executable

var installUpgrade = func(ctx context.Context, plan upgrade.InstallPlan) error {
	return upgrade.Installer{}.Install(ctx, plan)
}

var brewUpgradeBossanova = func(ctx context.Context, binDir string) (string, error) {
	brew := brewExecutableForBinDir(binDir)
	// #nosec G204 -- brew upgrade runs the brew binary discovered from binDir; args are literal, operator/local-trust not attacker-controlled.
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	cmd := exec.CommandContext(ctx, brew, "upgrade", "bossanova-dev/tap/bossanova")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("brew upgrade bossanova-dev/tap/bossanova failed: %w\noutput:\n%s\nRun manually: brew upgrade bossanova-dev/tap/bossanova", err, strings.TrimSpace(string(out)))
	}

	// #nosec G204 -- brew --prefix runs the same discovered brew binary; literal args, local-trust.
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	prefixCmd := exec.CommandContext(ctx, brew, "--prefix", "bossanova-dev/tap/bossanova")
	out, err := prefixCmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("brew --prefix bossanova-dev/tap/bossanova failed: %w\noutput:\n%s\nRun manually: brew --prefix bossanova-dev/tap/bossanova", err, strings.TrimSpace(string(out)))
	}
	prefix := strings.TrimSpace(string(out))
	if prefix == "" {
		return "", fmt.Errorf("brew --prefix bossanova-dev/tap/bossanova returned empty prefix")
	}
	return filepath.Join(prefix, "libexec", "plugins"), nil
}

// verifyUpgradeVersion confirms a user-supplied --version actually exists on
// GitHub. Indirected through a var so tests can stub it without hitting the
// network.
var verifyUpgradeVersion = func(ctx context.Context, version string) error {
	return upgrade.VerifyReleaseTag(ctx, nil, "", version, upgrade.ResolveGitHubToken(ctx))
}

var restartDaemon = daemon.Restart

// restartHomebrewDaemon runs the boss executable selected by Homebrew after an
// upgrade. The current process can still be executing from the old Cellar
// version, so restarting in-process would restage the old adjacent bossd.
var restartHomebrewDaemon = func(ctx context.Context, executable string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, executable, "daemon", "restart")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("restart daemon with upgraded boss: %w\noutput:\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// restartDaemonAfterUpgrade stops a running daemon and waits for its socket to
// close before bootstrapping the upgraded daemon. launchctl bootout returns
// before bossd has necessarily finished draining, so daemon.Restart can race
// bootstrap against the old process.
var restartDaemonAfterUpgrade = func() error {
	socketPath, err := defaultSocketPath()
	if err != nil {
		return fmt.Errorf("restart daemon after upgrade: socket path: %w", err)
	}
	st, err := daemonGetStatus()
	if err != nil {
		return fmt.Errorf("restart daemon after upgrade: daemon status: %w", err)
	}
	start := func(string) error { return restartDaemon() }
	if !st.Installed {
		// A standalone daemon deliberately has no LaunchAgent. Restart would
		// create one on macOS, changing the user's service mode during upgrade.
		start = daemonEnsureRunning
	}
	if err := restartReachableDaemonForSettingsReloadWith(
		socketPath,
		func() (*daemon.Status, error) { return st, nil },
		daemonStop,
		start,
		terminateCurrentProfileBossd,
		waitForCurrentProfileBossdExit,
		waitForDaemonSocketGone,
	); err != nil {
		return fmt.Errorf("restart daemon after upgrade: %w", err)
	}
	if err := waitForDaemonRestartReady(socketPath); err != nil {
		return fmt.Errorf("restart daemon after upgrade: %w", err)
	}
	return nil
}

var runProviderStartupIfNeeded = views.RunProviderStartupIfNeeded

var defaultSocketPath = client.DefaultSocketPath

var daemonSocketReachable = daemon.IsSocketReachable

var daemonGetStatus = daemon.GetStatus

var daemonEnsureRunning = daemon.EnsureRunning

var daemonStop = daemon.Stop

var terminateAllBossdProcesses = terminateBossdProcesses

// terminateCurrentProfileBossd only stops the standalone daemon recorded for
// this profile. Upgrade restart must not kill daemons serving other profiles.
var terminateCurrentProfileBossd = func() (int, error) {
	profile, err := currentDaemonProfile()
	if err != nil {
		return 0, err
	}
	return terminateStandaloneCurrentProfile(profile)
}

var terminateStandaloneCurrentProfile = func(profile daemonProfile) (int, error) {
	return terminateProfileBossdProcess(profile.AppDataDir, func(pid int) (processSignaler, error) {
		return os.FindProcess(pid)
	})
}

var terminateAllPluginProcesses = terminateAllBossdPluginProcesses

var waitForDaemonSocketGone = waitForSocketGone

// waitForStandaloneBossdExit waits until the current standalone daemon has
// released its singleton lock. Its state record is removed before lock release
// so shutdown cannot delete metadata written by a replacement; acquiring this
// lock is therefore the handoff signal that prevents a detached-start race.
var waitForStandaloneBossdExit = waitForDaemonLockRelease

// daemonRestartReadyTimeout and daemonRestartPollInterval bound the wait for a
// freshly-restarted bossd to start accepting connections. They default to the
// shared daemon lifecycle budgets and are overridable in tests.
var (
	daemonRestartReadyTimeout = daemon.LifecycleStartupTimeout
	daemonRestartPollInterval = daemon.LifecyclePollInterval
)

var loadSettings = config.Load

var saveSettings = config.Save

var discoverPlugins = config.DiscoverPlugins

var upgradeLockPath = defaultUpgradeLockPath

var installedPluginNames = []string{
	"claude",
	"codex",
	"dependabot",
	"linear",
	"repair",
	"sentry",
}

func launchSettings(now time.Time) config.Settings {
	settings, err := loadSettings()
	if err != nil {
		return settings
	}
	if initialized, changed := settings.EnsureInstalledAt(now); changed {
		settings = initialized
		_ = saveSettings(settings)
	}
	return settings
}

func currentExecutableDir() (string, error) {
	path, err := executablePath()
	if err != nil {
		return "", fmt.Errorf("executable path: %w", err)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("absolute executable path: %w", err)
	}
	resolvedPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return "", fmt.Errorf("resolve executable path: %w", err)
	}
	return filepath.Dir(resolvedPath), nil
}

func defaultPluginDir(goos string) (string, error) {
	switch goos {
	case "darwin", "linux":
		return config.UserPluginDir()
	default:
		return "", fmt.Errorf("unsupported plugin install platform %s", goos)
	}
}

func upgradePluginDir(goos, binDir string) (string, error) {
	settings, err := loadSettings()
	if err != nil {
		return "", fmt.Errorf("load settings: %w", err)
	}
	homebrewPluginDir, homebrewInstall := homebrewPluginDirForBinDir(binDir)
	if dir, ok, err := commonEnabledPluginDir(settings.Plugins, homebrewPluginDir); err != nil {
		return "", err
	} else if ok {
		return dir, nil
	}
	if dir, ok, err := commonEnabledPluginDir(discoverPlugins(), homebrewPluginDir); err != nil {
		return "", err
	} else if ok {
		if homebrewInstall && isHomebrewCellarPluginDir(dir) {
			return homebrewPluginDir, nil
		}
		if isCWDDevPluginDir(dir) {
			return defaultPluginDir(goos)
		}
		return dir, nil
	}
	return defaultPluginDir(goos)
}

func homebrewPluginDirForBinDir(binDir string) (string, bool) {
	formulaPrefix, _, ok := homebrewPrefixesForBinDir(binDir)
	if !ok {
		return "", false
	}
	return filepath.Join(formulaPrefix, "libexec", "plugins"), true
}

func brewExecutableForBinDir(binDir string) string {
	_, homebrewPrefix, ok := homebrewPrefixesForBinDir(binDir)
	if ok && homebrewPrefix != "" {
		candidate := filepath.Join(homebrewPrefix, "bin", "brew")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return "brew"
}

func homebrewPrefixesForBinDir(binDir string) (string, string, bool) {
	clean := filepath.Clean(binDir)
	parts := strings.Split(filepath.ToSlash(clean), "/")
	for i := 0; i+3 < len(parts); i++ {
		if parts[i] == "Cellar" && parts[i+1] == "bossanova" && parts[i+3] == "bin" {
			return filepath.FromSlash(strings.Join(parts[:i+3], "/")), filepath.FromSlash(strings.Join(parts[:i], "/")), true
		}
	}
	return "", "", false
}

func isHomebrewCellarBinDir(binDir string) bool {
	_, ok := homebrewPluginDirForBinDir(binDir)
	return ok
}

func isCWDDevPluginDir(dir string) bool {
	wd, err := os.Getwd()
	if err != nil {
		return false
	}
	wd, err = filepath.Abs(wd)
	if err != nil {
		return false
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return false
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	for current := wd; ; current = filepath.Dir(current) {
		devBin := filepath.Join(current, "bin")
		if resolved, err := filepath.EvalSymlinks(devBin); err == nil {
			devBin = resolved
		}
		if filepath.Clean(dir) == filepath.Clean(devBin) {
			return true
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
	}
}

func commonEnabledPluginDir(plugins []config.PluginConfig, currentHomebrewPluginDir string) (string, bool, error) {
	dirs := make(map[string]struct{})
	homebrewDirs := make(map[string]struct{})
	customDirs := make(map[string]struct{})
	for _, plugin := range plugins {
		if !plugin.Enabled || plugin.Path == "" {
			continue
		}
		pluginDir := filepath.Clean(filepath.Dir(plugin.Path))
		dirs[pluginDir] = struct{}{}
		if currentHomebrewPluginDir != "" && isHomebrewCellarPluginDir(pluginDir) {
			homebrewDirs[pluginDir] = struct{}{}
		} else {
			customDirs[pluginDir] = struct{}{}
		}
	}
	if len(dirs) == 0 {
		return "", false, nil
	}
	if currentHomebrewPluginDir != "" && len(homebrewDirs) > 0 && len(customDirs) == 0 {
		return currentHomebrewPluginDir, true, nil
	}
	if len(dirs) == 1 {
		for dir := range dirs {
			return dir, true, nil
		}
	}
	first, second := firstTwoDirs(dirs)
	return "", false, fmt.Errorf("enabled plugin paths span multiple directories (%s and %s); upgrade via your package manager or configure plugins in one directory", first, second)
}

func isHomebrewCellarPluginDir(dir string) bool {
	parts := strings.Split(filepath.ToSlash(filepath.Clean(dir)), "/")
	for i := 0; i+4 < len(parts); i++ {
		if parts[i] == "Cellar" &&
			parts[i+1] == "bossanova" &&
			parts[i+3] == "libexec" &&
			parts[i+4] == "plugins" {
			return true
		}
	}
	return false
}

func firstTwoDirs(dirs map[string]struct{}) (string, string) {
	first := ""
	for dir := range dirs {
		if first == "" {
			first = dir
			continue
		}
		return first, dir
	}
	return first, first
}

func runUpgrade(cmd *cobra.Command, opts upgradeOptions) error {
	ctx := cmd.Context()
	current := upgradeCurrentVersion()

	targetVersion := ""
	if opts.Version != "" {
		version, ok, dev := upgrade.NormalizeVersion(opts.Version)
		if dev || !ok {
			return fmt.Errorf("invalid upgrade version %q", opts.Version)
		}
		if semver.Prerelease(version) != "" {
			return fmt.Errorf("prerelease upgrade version %q requires an explicit prerelease channel", opts.Version)
		}
		targetVersion = version
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "upgrade target: %s\n", targetVersion)
		if opts.CheckOnly {
			if err := verifyUpgradeVersion(ctx, targetVersion); err != nil {
				return fmt.Errorf("verify upgrade version: %w", err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "release %s exists on GitHub\n", targetVersion)
			return nil
		}
	} else {
		if _, ok, dev := upgrade.NormalizeVersion(current); dev || !ok {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "boss upgrade checks require a stable release version (current: %s)\n", current)
			return nil
		}

		result, ok := freshUpgradeResultFromCache(current)
		if !ok {
			var err error
			result, err = checkUpgrade(ctx, current)
			if err != nil {
				var rlErr *upgrade.RateLimitError
				if errors.As(err, &rlErr) {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "cannot check for upgrades: %s\n", rlErr.Error())
					return nil
				}
				return fmt.Errorf("check upgrade: %w", err)
			}
		}
		if !result.Available {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "boss is up to date (%s)\n", current)
			return nil
		}
		targetVersion = result.LatestVersion
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "upgrade available: %s -> %s\n", current, targetVersion)
		if opts.CheckOnly {
			return nil
		}
	}
	if !opts.Yes {
		return fmt.Errorf("refusing interactive upgrade without --yes")
	}
	binDir, err := currentExecutableDir()
	if err != nil {
		return err
	}
	if isHomebrewCellarBinDir(binDir) {
		if opts.Version != "" {
			return fmt.Errorf("homebrew installs upgrade through the tap; exact --version installs are not supported. Run manually: brew upgrade bossanova-dev/tap/bossanova")
		}
		return withUpgradeLock(func() error {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "upgrading bossanova with Homebrew")
			pluginDir, err := brewUpgradeBossanova(ctx, binDir)
			if err != nil {
				return err
			}
			if pluginDir != "" {
				if err := persistInstalledPluginPaths(pluginDir); err != nil {
					return fmt.Errorf("persist plugin settings: %w", err)
				}
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Homebrew upgrade completed")
			if opts.NoRestart {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "daemon restart skipped (--no-restart)")
			} else {
				upgradedBoss := filepath.Join(filepath.Dir(filepath.Dir(pluginDir)), "bin", "boss")
				if err := restartHomebrewDaemon(ctx, upgradedBoss); err != nil {
					return fmt.Errorf("restart daemon: %w", err)
				}
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "daemon restarted")
			}
			return nil
		})
	}
	pluginDir, err := upgradePluginDir(runtime.GOOS, binDir)
	if err != nil {
		return err
	}
	plan := upgrade.InstallPlan{
		Version:    targetVersion,
		ReleaseURL: "https://github.com/bossanova-dev/bossanova/releases/download/" + targetVersion,
		GOOS:       runtime.GOOS,
		GOARCH:     runtime.GOARCH,
		BinDir:     binDir,
		PluginDir:  pluginDir,
	}
	if err := plan.Validate(); err != nil {
		return err
	}
	return withUpgradeLock(func() error {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "installing %s assets into %s\n", targetVersion, plan.BinDir)
		if err := installUpgrade(ctx, plan); err != nil {
			return fmt.Errorf("install upgrade: %w", err)
		}
		if err := persistInstalledPluginPaths(plan.PluginDir); err != nil {
			return fmt.Errorf("persist plugin settings: %w", err)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "upgrade installed %s\n", targetVersion)
		if opts.NoRestart {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "daemon restart skipped (--no-restart)")
		} else {
			if err := restartDaemonAfterUpgrade(); err != nil {
				return fmt.Errorf("restart daemon: %w", err)
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "daemon restarted")
		}
		return nil
	})
}

func defaultUpgradeLockPath() (string, error) {
	dir, err := config.DefaultAppDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "upgrade.lock"), nil
}

func withUpgradeLock(fn func() error) error {
	path, err := upgradeLockPath()
	if err != nil {
		return fmt.Errorf("upgrade lock path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create upgrade lock dir: %w", err)
	}
	// #nosec G304 -- internally-derived upgrade-lock path (upgradeLockPath); O_EXCL single-writer lock, no operator input.
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("upgrade already in progress")
		}
		return fmt.Errorf("acquire upgrade lock: %w", err)
	}
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	defer func() {
		_ = f.Close()
		_ = os.Remove(path)
	}()
	return fn()
}

func persistInstalledPluginPaths(pluginDir string) error {
	settings, err := loadSettings()
	if err != nil {
		return err
	}
	rewriteInstalledPluginPaths(&settings, pluginDir)
	return saveSettings(settings)
}

func rewriteInstalledPluginPaths(settings *config.Settings, pluginDir string) {
	seen := make(map[string]struct{}, len(settings.Plugins))
	for i := range settings.Plugins {
		seen[settings.Plugins[i].Name] = struct{}{}
		if installedPluginName(settings.Plugins[i].Name) {
			settings.Plugins[i].Path = filepath.Join(pluginDir, "bossd-plugin-"+settings.Plugins[i].Name)
		}
	}
	for _, name := range installedPluginNames {
		if _, ok := seen[name]; ok {
			continue
		}
		settings.Plugins = append(settings.Plugins, config.PluginConfig{
			Name:    name,
			Path:    filepath.Join(pluginDir, "bossd-plugin-"+name),
			Enabled: true,
		})
	}
}

func installedPluginName(name string) bool {
	for _, pluginName := range installedPluginNames {
		if pluginName == name {
			return true
		}
	}
	return false
}

func runLS(cmd *cobra.Command) error {
	asJSON, _ := cmd.Flags().GetBool(jsonFlagName)

	c, err := newClient(cmd)
	if err != nil {
		return emitJSONFailure(cmd, asJSON, err)
	}

	repoID, _ := cmd.Flags().GetString("repo")
	archived, _ := cmd.Flags().GetBool("archived")
	stateStrs, _ := cmd.Flags().GetStringSlice("state")

	// Parse state filters.
	var states []pb.SessionState
	for _, s := range stateStrs {
		key := "SESSION_STATE_" + strings.ToUpper(s)
		if val, ok := pb.SessionState_value[key]; ok {
			states = append(states, pb.SessionState(val))
		} else {
			return emitJSONFailure(cmd, asJSON, fmt.Errorf("unknown state: %s", s))
		}
	}

	req := &pb.ListSessionsRequest{
		IncludeArchived: archived,
		States:          states,
	}
	if repoID != "" {
		req.RepoId = &repoID
	}

	ctx := context.Background()
	sessions, err := c.ListSessions(ctx, req, client.SessionReadOptions{})
	if err != nil {
		return emitJSONFailure(cmd, asJSON, fmt.Errorf("list sessions: %w", err))
	}

	if asJSON {
		// The empty case emits `{"sessions": []}`, not the human "no sessions"
		// line and not `null` — a driver decodes one shape either way.
		rows := make([]sessionJSON, 0, len(sessions))
		for _, sess := range sessions {
			rows = append(rows, newSessionRowJSON(sess))
		}
		return emitJSON(cmd, sessionListJSON{Sessions: rows})
	}

	if len(sessions) == 0 {
		fmt.Println("No sessions found.")
		return nil
	}

	ids := make([]string, len(sessions))
	titles := make([]string, len(sessions))
	stateStrs2 := make([]string, len(sessions))
	branchStrs := make([]string, len(sessions))
	prStrs := make([]string, len(sessions))
	for i, sess := range sessions {
		ids[i] = sess.Id
		t := sess.Title
		t = truncateString(t, 30)
		titles[i] = t
		stateStrs2[i] = views.StateLabel(sess.State)
		branchStrs[i] = sess.BranchName
		if sess.PrNumber != nil {
			prText := fmt.Sprintf("#%d", *sess.PrNumber)
			if sess.PrUrl != nil {
				prStrs[i] = lipgloss.NewStyle().Hyperlink(*sess.PrUrl).Render(prText)
			} else {
				prStrs[i] = prText
			}
		} else {
			prStrs[i] = "-"
		}
	}

	cols := []table.Column{
		{Title: "ID", Width: views.MaxColWidth("ID", ids, 0)},
		{Title: "TITLE", Width: views.MaxColWidth("TITLE", titles, 30)},
		{Title: "STATE", Width: views.MaxColWidth("STATE", stateStrs2, 14)},
		{Title: "BRANCH", Width: views.MaxColWidth("BRANCH", branchStrs, 40)},
		{Title: "PR", Width: views.MaxColWidth("PR", prStrs, 8)},
	}

	rows := make([]table.Row, len(sessions))
	for i := range sessions {
		rows[i] = table.Row{ids[i], titles[i], stateStrs2[i], branchStrs[i], prStrs[i]}
	}

	t := table.New(
		table.WithColumns(cols),
		table.WithRows(rows),
		table.WithHeight(len(rows)+1),
		table.WithWidth(views.CLIColumnsWidth(cols)),
		table.WithStyles(views.CLITableStyles()),
		table.WithFocused(false),
	)
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), t.View())
	return nil
}

func runNew(cmd *cobra.Command) error {
	agentName, _ := cmd.Flags().GetString("agent")
	repo, _ := cmd.Flags().GetString("repo")
	prompt, _ := cmd.Flags().GetString("prompt")
	title, _ := cmd.Flags().GetString("title")
	model, _ := cmd.Flags().GetString("model")
	account, _ := cmd.Flags().GetString("account")
	// Thread flag PRESENCE, not just the value: an explicit `--account=` (even
	// empty) is an opt-out to account 0, distinct from omitting the flag (the
	// daemon then applies its default-account policy). nil = absent.
	var accountArg *string
	if cmd.Flags().Changed("account") {
		a := account
		accountArg = &a
	}
	// --detach / --no-attach are deliberately NOT read. The scripting path below
	// always detaches — runNewDetach's predecessor already took the value as `_`
	// — and the interactive path launches the TUI regardless, so the flags are
	// inert on both. They stay registered because they are a documented surface
	// callers already pass; `--detach`'s help text is what says it is a no-op
	// here and points at --tmux-unattended for the durable-pane behaviour.
	tmuxUnattended, _ := cmd.Flags().GetBool("tmux-unattended")
	deferPR, _ := cmd.Flags().GetBool("defer-pr")
	quickChat, _ := cmd.Flags().GetBool("quick-chat")
	trackerID, _ := cmd.Flags().GetString("tracker-id")
	trackerSource, _ := cmd.Flags().GetString("tracker-source")
	trackerURL, _ := cmd.Flags().GetString("tracker-url")
	asJSON, _ := cmd.Flags().GetBool(jsonFlagName)

	// Reject an unknown --tracker-source before any RPC is issued, on BOTH the
	// interactive and the scripting path. The daemon neither validates nor
	// stores the field — there is no tracker_source column, and server.go reads
	// it exactly once, to label a plan derived from a tracker_issue — so an
	// unrecognised value is silently ignored rather than refused. Failing here
	// turns a typo into an error the caller can see, instead of a tracker
	// binding that quietly never happens.
	if err := validateTrackerSource(trackerSource); err != nil {
		return emitJSONFailure(cmd, asJSON, err)
	}

	// --quick-chat and --defer-pr are contradictory, so refuse the pair here —
	// before any RPC — rather than sending a request whose two fields ask the
	// daemon for incompatible shapes. A quick chat has no worktree, no branch and
	// no up-front draft PR (server.go routes it to StartQuickChatSession, which
	// never enters StartSession), so there is nothing for --defer-pr to defer:
	// the daemon would silently honour quick_chat and drop defer_pr, leaving the
	// caller believing it asked for something it did not get.
	if quickChat && deferPR {
		return emitJSONFailure(cmd, asJSON, codedError(codeInvalidArgument, fmt.Errorf(
			"--quick-chat and --defer-pr are mutually exclusive: a quick chat has no "+
				"worktree, branch, or draft PR to defer; use --defer-pr for an "+
				"unattended run that may produce commits")))
	}

	// Non-interactive path: --repo and --prompt both provided.
	if repo != "" && prompt != "" {
		return runNewDetach(cmd, newSessionOpts{
			RepoID:         repo,
			Prompt:         prompt,
			Title:          title,
			AgentName:      agentName,
			Model:          model,
			Account:        accountArg,
			TmuxUnattended: tmuxUnattended,
			DeferPR:        deferPR,
			QuickChat:      quickChat,
			TrackerID:      trackerID,
			TrackerSource:  trackerSource,
			TrackerURL:     trackerURL,
		}, asJSON)
	}

	return launchTUI(cmd, func(app *views.App) {
		app.SetInitialView(views.ViewNewSession)
		if agentName != "" {
			app.SetInitialAgent(agentName)
		}
		if accountArg != nil {
			app.SetInitialAccount(*accountArg)
		}
	})
}

// runNewDetach creates a session non-interactively, drains the setup stream,
// and prints the session-id and chat-id (agent_session_id) before returning.
// When detach is false the user can still pipe this output, but the session
// starts running in the background either way.
// newDetachRequest builds the CreateSessionRequest for the non-interactive
// `boss new --repo --prompt` scripting path. Detach is always true here: the
// prompt runs as a headless agent pass (codex exec / claude --print) on the
// daemon. The local --detach flag only governs whether this CLI waits/attaches
// after creating the session; interactive TUI sessions leave Detach false and
// start the agent on attach instead. agentName and model are optional
// (empty → server/agent default) and stay unset in the request when empty.
// account is optional and carries flag PRESENCE: nil omits account_id (the
// daemon applies its default-account policy), a non-nil pointer (even to "")
// binds explicitly — "" is the account-0 opt-out that skips the policy.
//
// It takes a named options struct rather than a positional parameter list:
// the list was already at six arguments and the tracker/tmux flags would have
// taken it to ten same-typed strings and bools, where a transposed pair is
// invisible at the call site and silently sends the URL as the id.
func newDetachRequest(opts newSessionOpts) *pb.CreateSessionRequest {
	req := &pb.CreateSessionRequest{
		RepoId: opts.RepoID,
		// Verbatim. The daemon only auto-submits an unattended prompt when it is
		// one trimmed line starting with `/` or `$`, so the CLI must not wrap,
		// prefix, trim or reflow it on the way in.
		Plan:  opts.Prompt,
		Title: opts.Title,
		// Detach and IsTmuxUnattended are orthogonal, not alternatives: the
		// daemon carries both on StartSessionOpts and mints a finalize hook
		// token when EITHER is set. Detach means "run the initial agent pass
		// headlessly"; is_tmux_unattended means "host it in a durable tmux pane
		// that survives a daemon restart". --tmux-unattended therefore adds to
		// this scripting path's always-true Detach rather than replacing it.
		Detach:           true,
		IsTmuxUnattended: opts.TmuxUnattended,
		// Plain (non-optional) proto bools, so they are set unconditionally from
		// the flag value — false is the wire default and means "not requested",
		// with no presence-pointer distinction to preserve like --account's.
		// Neither may disturb Detach above: Detach is what makes the daemon mint
		// the finalize hook token, and finalize is what opens --defer-pr's PR
		// when the run did produce commits. That hook drives finalize on the
		// durable tmux-hosted path; a paneless headless run reaches the same
		// finalize through the daemon's run-completion poller instead.
		DeferPr:     opts.DeferPR,
		IsQuickChat: opts.QuickChat,
	}
	if opts.AgentName != "" {
		req.AgentName = &opts.AgentName
	}
	if opts.Model != "" {
		req.Model = &opts.Model
	}
	if opts.Account != nil {
		req.AccountId = opts.Account
	}
	// The tracker fields are `optional` in the proto, so an omitted flag must
	// leave the field nil: a present-but-empty pointer is a different wire
	// signal, and the daemon's tracker-id dedup keys on presence.
	if opts.TrackerID != "" {
		req.TrackerId = &opts.TrackerID
	}
	if opts.TrackerSource != "" {
		req.TrackerSource = &opts.TrackerSource
	}
	if opts.TrackerURL != "" {
		req.TrackerUrl = &opts.TrackerURL
	}
	return req
}

// newSessionOpts carries the `boss new` non-interactive flag set through to the
// CreateSessionRequest builder. Named fields, not positional arguments — see
// newDetachRequest.
type newSessionOpts struct {
	RepoID    string
	Prompt    string
	Title     string
	AgentName string
	Model     string
	// Account carries flag PRESENCE: nil = absent, non-nil (even "") = explicit.
	Account *string
	// TmuxUnattended requests a durable tmux-hosted pane that survives a daemon
	// restart. Independent of Detach — see newDetachRequest.
	TmuxUnattended bool
	// DeferPR suppresses the up-front draft PR: the session still gets a worktree
	// and branch, and a PR is opened at finalize only if the run committed work.
	// This is the flag for an unattended run that may legitimately change
	// nothing, since a no-op then finalizes benignly instead of leaving an empty
	// PR behind.
	DeferPR bool
	// QuickChat creates a worktree-less, branch-less, PR-less session in the repo
	// checkout. The daemon never enters StartSession for one, so no agent runs at
	// create time and the returned session carries no chat id — runNewDetach says
	// so on stderr and in the --json envelope's next_action. Mutually exclusive
	// with DeferPR, which runNew rejects before any RPC.
	QuickChat bool
	// Tracker* bind the session to an external issue. Empty = flag absent, which
	// leaves the corresponding optional proto field unset.
	TrackerID     string
	TrackerSource string
	TrackerURL    string
}

// trackerSources is the tracker_source vocabulary the proto documents
// (proto/bossanova/v1/daemon.proto: `optional string tracker_source = 13; //
// "linear" | "sentry"`).
var trackerSources = []string{"linear", "sentry"}

// validateTrackerSource rejects an unknown --tracker-source. The daemon does
// not validate this field — it stores it and uses it only to format a plan from
// a tracker issue — so an unrecognised value is persisted verbatim and the
// binding it was meant to establish silently does not happen. Catching it here
// keeps the failure at the call the caller can still fix.
//
// An empty value is valid: it means the flag was omitted.
func validateTrackerSource(source string) error {
	if source == "" || slices.Contains(trackerSources, source) {
		return nil
	}
	return codedError(codeInvalidArgument, fmt.Errorf(
		"invalid --tracker-source %q: want one of %s",
		source, strings.Join(trackerSources, ", ")))
}

// deriveSessionTitle builds a session title from the prompt for the
// non-interactive `boss new` path, mirroring the interactive TUI which always
// supplies a client-side title (newsession.go). It takes the first non-empty
// line, collapses internal whitespace runs, and caps the length on a rune
// boundary. A whitespace-only prompt falls back to a stable default so the
// server's "title is required" invariant is always satisfied.
func deriveSessionTitle(prompt string) string {
	const maxTitleRunes = 72
	const fallback = "New session"
	var line string
	for _, l := range strings.Split(prompt, "\n") {
		if strings.TrimSpace(l) != "" {
			line = l
			break
		}
	}
	line = strings.Join(strings.Fields(line), " ")
	if line == "" {
		return fallback
	}
	runes := []rune(line)
	if len(runes) > maxTitleRunes {
		return strings.TrimSpace(string(runes[:maxTitleRunes])) + "…"
	}
	return line
}

func runNewDetach(cmd *cobra.Command, opts newSessionOpts, asJSON bool) error {
	c, err := newClient(cmd)
	if err != nil {
		return emitJSONFailure(cmd, asJSON, err)
	}

	ctx := context.Background()

	// Resolve --repo to a repo_id.
	repos, err := c.ListRepos(ctx)
	if err != nil {
		return emitJSONFailure(cmd, asJSON, fmt.Errorf("list repos: %w", err))
	}
	repoID, err := resolveRepoArg(opts.RepoID, repos)
	if err != nil {
		return emitJSONFailure(cmd, asJSON, err)
	}
	opts.RepoID = repoID

	// The server requires a non-empty title. Mirror the interactive TUI and
	// derive one client-side from the prompt when --title is absent. Only the
	// TITLE is derived — opts.Prompt itself is never rewritten, so Plan reaches
	// the daemon byte-identical.
	if strings.TrimSpace(opts.Title) == "" {
		opts.Title = deriveSessionTitle(opts.Prompt)
	}

	req := newDetachRequest(opts)

	stream, err := c.CreateSession(ctx, req)
	if err != nil {
		return emitJSONFailure(cmd, asJSON, fmt.Errorf("create session: %w", err))
	}
	defer func() { _ = stream.Close() }()

	// Drain the stream; print setup output inline and capture the final session.
	// Setup output goes to stderr on both paths, so stdout under --json carries
	// exactly one JSON object.
	var session *pb.Session
	for stream.Receive() {
		msg := stream.Msg()
		if so := msg.GetSetupOutput(); so != nil {
			_, _ = fmt.Fprint(cmd.ErrOrStderr(), so.Text)
		}
		if sc := msg.GetSessionCreated(); sc != nil {
			session = sc.Session
		}
	}
	if err := stream.Err(); err != nil {
		return emitJSONFailure(cmd, asJSON, fmt.Errorf("create session: %w", err))
	}
	if session == nil {
		return emitJSONFailure(cmd, asJSON, fmt.Errorf("daemon did not return a session"))
	}

	// A session that came back without a chat id started no agent — the
	// --quick-chat shape, which the daemon creates idle and only runs once
	// somebody attaches. Left unsaid that is a silent no-op, so name it. It goes
	// to STDERR on BOTH paths: the two lines below are a frozen scripting
	// surface, and under --json stdout must stay exactly one JSON object (setup
	// output already uses stderr for the same reason). The --json envelope
	// carries the machine-readable next_action in addition.
	if session.GetAgentSessionId() == "" {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), idleSessionNotice)
	}

	if asJSON {
		return emitJSON(cmd, newSessionJSON(session))
	}
	// Byte-identical to what this path printed before --json existed: it is a
	// documented scripting surface and existing callers parse it positionally.
	// The chat-id line is printed even when empty, for the same reason.
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "session-id: %s\nchat-id:    %s\n",
		session.GetId(), session.GetAgentSessionId())
	return nil
}

// idleSessionNotice is the stderr warning for a create that launched no agent,
// and nextActionIdleSession its machine-readable counterpart in the --json
// envelope. Both mirror the MCP create_session surface's existing precedent
// (lib/bossalib/bossmcp/tools_mutating.go), so a caller reading either surface
// is told the same thing about the same outcome.
const (
	idleSessionNotice = "notice: this session is idle awaiting attach — no agent was launched, " +
		"so the prompt has not run yet. Attach the session to start it, or re-create " +
		"without --quick-chat (adding --defer-pr) to run the prompt unattended."
	nextActionIdleSession = "session is idle awaiting attach; no agent was launched. Attach the " +
		"session to start it, or re-create without --quick-chat (adding --defer-pr) to run " +
		"the prompt unattended."
)

// newSessionEnvelope is the `boss new --json` success envelope. It nests under
// "session" like `boss merge --json` so a driver reads one shape across the
// CLI, and it names the chat id `chat_id` — the same field the human two-line
// output labels `chat-id` — rather than leaking the proto's agent_session_id.
type newSessionEnvelope struct {
	Session newSessionBody `json:"session"`
}

type newSessionBody struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	ChatID string `json:"chat_id"`
	// NextAction is set only when the create launched no agent (empty chat id).
	// `omitempty` is load-bearing: it keeps the envelope byte-identical for every
	// existing caller on the ordinary path, so the key's mere presence is the
	// signal that this session needs attaching before anything runs.
	NextAction string `json:"next_action,omitempty"`
}

func newSessionJSON(session *pb.Session) newSessionEnvelope {
	body := newSessionBody{
		ID:     session.GetId(),
		Title:  session.GetTitle(),
		ChatID: session.GetAgentSessionId(),
	}
	if body.ChatID == "" {
		body.NextAction = nextActionIdleSession
	}
	return newSessionEnvelope{Session: body}
}

// resolveRepoArg resolves a --repo argument (id, unique id prefix, display
// name, or local path) to a repo_id. Precedence favors EXACT matches over the
// non-exact id prefix so a value that unambiguously identifies a repo is never
// shadowed by a coincidental prefix collision: exact full id wins first (a full
// id is never treated as a prefix of itself), then an exact display name, then
// an exact local path, and only then a unique id prefix (making the id printed
// by `boss repo ls` directly usable). Ordering the prefix last matters because
// ids are hex: a pure-hex display name like "cafe" could otherwise be a prefix
// of an unrelated repo's id and silently resolve to the wrong repo — exactly the
// silent-mispick this ticket removes. A duplicated display name or an ambiguous
// id prefix returns an error listing the candidates instead of picking one.
func resolveRepoArg(arg string, repos []*pb.Repo) (string, error) {
	// Exact id wins first — a full id must never be treated as an ambiguous prefix.
	for _, r := range repos {
		if r.Id == arg {
			return r.Id, nil
		}
	}
	// Exact display name — error on duplicates (the "silently picks a broken
	// duplicate" bug). Checked before the id prefix so an exactly-named repo is
	// never shadowed by another repo whose id merely starts with the same chars.
	var nameMatches []*pb.Repo
	for _, r := range repos {
		if r.DisplayName == arg {
			nameMatches = append(nameMatches, r)
		}
	}
	if len(nameMatches) == 1 {
		return nameMatches[0].Id, nil
	}
	if len(nameMatches) > 1 {
		return "", fmt.Errorf("repo name %q is ambiguous; matches: %s; pass the full id", arg, repoCandidates(nameMatches))
	}
	// Exact local path (also before the id prefix, for the same reason — though a
	// path starts with '/' and can never collide with a hex id prefix).
	for _, r := range repos {
		if r.LocalPath == arg {
			return r.Id, nil
		}
	}
	// Unique id prefix (this is what makes the printed short id usable).
	var idMatches []*pb.Repo
	for _, r := range repos {
		if strings.HasPrefix(r.Id, arg) {
			idMatches = append(idMatches, r)
		}
	}
	if len(idMatches) == 1 {
		return idMatches[0].Id, nil
	}
	if len(idMatches) > 1 {
		return "", fmt.Errorf("repo %q is an ambiguous id prefix; matches: %s; pass the full id", arg, repoCandidates(idMatches))
	}
	return "", fmt.Errorf("repo %q not found; register it first with 'boss repo add'", arg)
}

// repoCandidates renders a repo disambiguation list, one candidate per entry as
// "<full-id> (<name>, <path>)". IDs are never truncated — surfacing a usable
// identifier is the whole point.
func repoCandidates(repos []*pb.Repo) string {
	parts := make([]string, len(repos))
	for i, r := range repos {
		parts[i] = fmt.Sprintf("%s (%s, %s)", r.Id, r.DisplayName, r.LocalPath)
	}
	return strings.Join(parts, "; ")
}

func runAttach(cmd *cobra.Command, sessionID string) error {
	return launchTUIWithOptions(cmd, launchTUIOptions{
		attachSessionID: sessionID,
		configure: func(app *views.App) {
			app.SetInitialView(views.ViewAttach)
			app.SetAttachSession(sessionID, "")
		},
	})
}

func runRepoAdd(cmd *cobra.Command) error {
	return launchTUI(cmd, func(app *views.App) {
		app.SetInitialView(views.ViewRepoAdd)
	})
}

func runRepoLS(cmd *cobra.Command) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}

	ctx := context.Background()
	repos, err := c.ListRepos(ctx)
	if err != nil {
		return fmt.Errorf("list repos: %w", err)
	}

	if len(repos) == 0 {
		fmt.Println("No repositories registered.")
		return nil
	}

	ids := make([]string, len(repos))
	names := make([]string, len(repos))
	paths := make([]string, len(repos))
	branches := make([]string, len(repos))
	setups := make([]string, len(repos))
	for i, repo := range repos {
		ids[i] = repo.Id
		names[i] = repo.DisplayName
		paths[i] = repo.LocalPath
		branches[i] = repo.DefaultBaseBranch
		if repo.SetupScript != nil {
			setups[i] = *repo.SetupScript
		} else {
			setups[i] = "-"
		}
	}

	cols := []table.Column{
		{Title: "ID", Width: views.MaxColWidth("ID", ids, 0)},
		{Title: "NAME", Width: views.MaxColWidth("NAME", names, 30)},
		{Title: "PATH", Width: views.MaxColWidth("PATH", paths, 60)},
		{Title: "BRANCH", Width: views.MaxColWidth("BRANCH", branches, 30)},
		{Title: "SETUP", Width: views.MaxColWidth("SETUP", setups, 40)},
	}

	rows := make([]table.Row, len(repos))
	for i := range repos {
		rows[i] = table.Row{ids[i], names[i], paths[i], branches[i], setups[i]}
	}

	t := table.New(
		table.WithColumns(cols),
		table.WithRows(rows),
		table.WithHeight(len(rows)+1),
		table.WithWidth(views.CLIColumnsWidth(cols)),
		table.WithStyles(views.CLITableStyles()),
		table.WithFocused(false),
	)
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), t.View())
	return nil
}

// runPluginList renders the daemon's view of every plugin it tried to
// load this run. Disabled and load-failed entries are included so an
// operator with a typo'd path or a stale binary on disk can spot the
// problem without grepping daemon logs.
func runPluginList(cmd *cobra.Command) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}

	plugins, err := c.ListPlugins(context.Background())
	if err != nil {
		return fmt.Errorf("list plugins: %w", err)
	}

	if len(plugins) == 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No plugins configured.")
		return nil
	}

	names := make([]string, len(plugins))
	statuses := make([]string, len(plugins))
	enabled := make([]string, len(plugins))
	caps := make([]string, len(plugins))
	paths := make([]string, len(plugins))
	errs := make([]string, len(plugins))
	hasError := false
	for i, p := range plugins {
		names[i] = p.GetName()
		statuses[i] = pluginStatusLabel(p.GetStatus())
		enabled[i] = boolLabel(p.GetEnabled())
		if c := p.GetCapabilities(); len(c) > 0 {
			caps[i] = strings.Join(c, ",")
		} else {
			caps[i] = "-"
		}
		paths[i] = p.GetPath()
		errs[i] = p.GetError()
		if errs[i] != "" {
			hasError = true
		}
	}

	cols := []table.Column{
		{Title: "NAME", Width: views.MaxColWidth("NAME", names, 24)},
		{Title: "STATUS", Width: views.MaxColWidth("STATUS", statuses, 14)},
		{Title: "ENABLED", Width: views.MaxColWidth("ENABLED", enabled, 8)},
		{Title: "CAPABILITIES", Width: views.MaxColWidth("CAPABILITIES", caps, 28)},
		{Title: "PATH", Width: views.MaxColWidth("PATH", paths, 60)},
	}
	if hasError {
		cols = append(cols, table.Column{Title: "ERROR", Width: views.MaxColWidth("ERROR", errs, 60)})
	}

	rows := make([]table.Row, len(plugins))
	for i := range plugins {
		row := table.Row{names[i], statuses[i], enabled[i], caps[i], paths[i]}
		if hasError {
			row = append(row, errs[i])
		}
		rows[i] = row
	}

	t := table.New(
		table.WithColumns(cols),
		table.WithRows(rows),
		table.WithHeight(len(rows)+1),
		table.WithWidth(views.CLIColumnsWidth(cols)),
		table.WithStyles(views.CLITableStyles()),
		table.WithFocused(false),
	)
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), t.View())
	return nil
}

// pluginStatusLabel maps the proto enum to a short, human-readable label
// suitable for the STATUS column.
func pluginStatusLabel(s pb.InstalledPlugin_Status) string {
	switch s {
	case pb.InstalledPlugin_STATUS_LOADED:
		return "loaded"
	case pb.InstalledPlugin_STATUS_DISABLED:
		return "disabled"
	case pb.InstalledPlugin_STATUS_LOAD_FAILED:
		return "failed"
	default:
		return "unknown"
	}
}

func boolLabel(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// agentLister is the narrow client seam `boss agents` needs. Naming it here —
// rather than taking the whole client.BossClient — is what lets the command be
// tested against a stub without standing up a daemon.
type agentLister interface {
	ListAgents(ctx context.Context) ([]client.AgentInfo, error)
}

// agentsJSON is the `boss agents --json` success envelope. The object wrapper
// (rather than a bare array) leaves room for future top-level fields without
// breaking a driver that already parses it.
type agentsJSON struct {
	Agents []agentJSON `json:"agents"`
}

type agentJSON struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	// Always an array, never null: a driver iterating an agent's settings
	// should not have to null-check a fleet member that exposes none.
	UserSettings []userSettingJSON `json:"user_settings"`
}

type userSettingJSON struct {
	Key          string `json:"key"`
	Label        string `json:"label"`
	Description  string `json:"description"`
	DefaultValue string `json:"default_value"`
	// Type is the enum NAME, not client.SettingType's integer. The int's
	// ordering is an implementation detail of this package; emitting it would
	// couple every caller to it.
	Type string `json:"type"`
	// Always an array, never null, for the same reason as UserSettings.
	AllowedValues []string `json:"allowed_values"`
}

// settingTypeName maps the package-local SettingType to its stable wire
// spelling. Anything unrecognised reads as UNSPECIFIED rather than a number, so
// a new enum member added upstream degrades to "opaque/text" instead of leaking
// an integer into the contract.
func settingTypeName(t client.SettingType) string {
	switch t {
	case client.SettingTypeBool:
		return "BOOL"
	case client.SettingTypeString:
		return "STRING"
	case client.SettingTypeEnum:
		return "ENUM"
	default:
		return "UNSPECIFIED"
	}
}

// runAgents lists the agent runners the daemon actually loaded — the plugins
// satisfying AgentRunnerService, which is a strictly narrower set than
// `boss plugin list` reports. A preflight asking "can I request this agent?"
// wants this list, not the plugin inventory.
func runAgents(cmd *cobra.Command) error {
	asJSON, _ := cmd.Flags().GetBool(jsonFlagName)
	c, err := newClient(cmd)
	if err != nil {
		return emitJSONFailure(cmd, asJSON, err)
	}
	return renderAgents(cmd, c, asJSON)
}

// renderAgents is runAgents with the transport hoisted out, so the rendering
// contract can be exercised against a stub client.
func renderAgents(cmd *cobra.Command, c agentLister, asJSON bool) error {
	agents, err := c.ListAgents(context.Background())
	if err != nil {
		return emitJSONFailure(cmd, asJSON, fmt.Errorf("list agents: %w", err))
	}

	if asJSON {
		return emitJSON(cmd, newAgentsJSON(agents))
	}

	if len(agents) == 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No agent runners loaded.")
		return nil
	}

	names := make([]string, len(agents))
	versions := make([]string, len(agents))
	settings := make([]string, len(agents))
	for i, a := range agents {
		names[i] = orDash(a.Name)
		versions[i] = orDash(a.Version)
		settings[i] = strconv.Itoa(len(a.UserSettings))
	}

	cols := []table.Column{
		{Title: "NAME", Width: views.MaxColWidth("NAME", names, 24)},
		{Title: "VERSION", Width: views.MaxColWidth("VERSION", versions, 20)},
		{Title: "SETTINGS", Width: views.MaxColWidth("SETTINGS", settings, 8)},
	}
	rows := make([]table.Row, len(agents))
	for i := range agents {
		rows[i] = table.Row{names[i], versions[i], settings[i]}
	}

	t := table.New(
		table.WithColumns(cols),
		table.WithRows(rows),
		table.WithHeight(len(rows)+1),
		table.WithWidth(views.CLIColumnsWidth(cols)),
		table.WithStyles(views.CLITableStyles()),
		table.WithFocused(false),
	)
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), t.View())
	return nil
}

// newAgentsJSON converts the client types into the wire envelope.
//
// Every slice here is allocated with make rather than grown from nil: a nil
// slice marshals as `null`, and each of these three lists is something a driver
// iterates. `[]` is the honest empty answer — "this agent has no settings", not
// "the settings are unknown".
func newAgentsJSON(agents []client.AgentInfo) agentsJSON {
	out := make([]agentJSON, 0, len(agents))
	for _, a := range agents {
		settings := make([]userSettingJSON, 0, len(a.UserSettings))
		for _, s := range a.UserSettings {
			allowed := make([]string, 0, len(s.AllowedValues))
			allowed = append(allowed, s.AllowedValues...)
			settings = append(settings, userSettingJSON{
				Key:           s.Key,
				Label:         s.Label,
				Description:   s.Description,
				DefaultValue:  s.DefaultValue,
				Type:          settingTypeName(s.Type),
				AllowedValues: allowed,
			})
		}
		out = append(out, agentJSON{
			Name:         a.Name,
			Version:      a.Version,
			UserSettings: settings,
		})
	}
	return agentsJSON{Agents: out}
}

func runRepoRemove(cmd *cobra.Command, id string) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if err := c.RemoveRepo(ctx, id); err != nil {
		return fmt.Errorf("remove repo: %w", err)
	}
	fmt.Printf("Repository %s removed.\n", id)
	return nil
}

// mergeTargetDescription names what a merge is about to act on, so the
// confirmation prompt catches a mistyped prefix that resolved to a real (and
// possibly archived) session. A session with a linked PR merges that PR; one
// without takes the daemon's local-only-branch merge path.
func mergeTargetDescription(sess *pb.Session) string {
	if sess.PrNumber != nil {
		return fmt.Sprintf("PR #%d %q", sess.GetPrNumber(), sess.GetTitle())
	}
	base := sess.GetBaseBranch()
	if base == "" {
		base = "its base branch"
	}
	return fmt.Sprintf("local branch %s into %s", sess.GetBranchName(), base)
}

// runMerge is a thin transport over the daemon's MergeSession RPC. The daemon
// owns the merge gate, the per-repo merge serialization, the already-merged
// short-circuit, and the merge-strategy resolution — none of that is mirrored
// here. In particular there is deliberately no client-side gate: the daemon's
// own `merge blocked: gate=<slug>; <detail>` message names the actual gate and
// is a better outcome than a CLI guess. Merges wait on the repo merge lock and
// the provider, so the context is unbounded like runArchive's (the procedure is
// already in the client's no-short-deadline set).
func runMerge(cmd *cobra.Command, sessionID string) error {
	asJSON, _ := cmd.Flags().GetBool("json")

	c, err := newClient(cmd)
	if err != nil {
		return emitJSONFailure(cmd, asJSON, err)
	}
	ctx := context.Background()
	sessionID, err = resolveSessionID(c, ctx, sessionID)
	if err != nil {
		return emitJSONFailure(cmd, asJSON, err)
	}

	yes, _ := cmd.Flags().GetBool("yes")
	switch {
	case asJSON && !yes:
		// --json is a machine contract, and the confirmation prompt is not part
		// of it. Prompting here would either block on a stdin nobody attached,
		// or read EOF and print "Cancelled." with exit 0 — a merge that reads
		// as declined though it was never offered. Refusing loudly is the safe
		// direction, and it travels as an envelope like every other failure.
		return emitJSONFailure(cmd, asJSON, codedError(codeConfirmationRequired,
			errors.New("merge: --json requires --yes (refusing to prompt for confirmation on a machine-readable invocation)")))
	case !yes:
		sess, err := c.GetSession(ctx, sessionID, client.SessionReadOptions{})
		if err != nil {
			return fmt.Errorf("get session: %w", err)
		}
		fmt.Printf("Merge session %s — %s? [y/N] ", sessionID, mergeTargetDescription(sess))
		var answer string
		if _, err := fmt.Scanln(&answer); err != nil || (answer != "y" && answer != "Y") {
			fmt.Println("Cancelled.")
			return nil
		}
	}

	sess, detail, err := c.MergeSession(ctx, sessionID)
	if err != nil {
		return emitJSONFailure(cmd, asJSON, fmt.Errorf("merge session: %w", err))
	}
	if asJSON {
		return emitJSON(cmd, newMergeJSON(sess, detail))
	}
	fmt.Printf("Session %s merged (%s).\n", sess.GetId(), sess.GetTitle())
	if detail != "" {
		fmt.Printf("Note: %s\n", detail)
	}
	return nil
}

// mergeJSON is the `boss merge --json` success envelope.
type mergeJSON struct {
	Session mergeSessionJSON `json:"session"`
	PR      *mergePRJSON     `json:"pr,omitempty"`
	// Detail is the daemon's note about how the merge was performed (e.g. a
	// merge-strategy substitution), empty when it ran exactly as configured.
	// Always emitted, never omitted: over --remote it is always "" because
	// ProxyMergeSessionResponse carries no detail field, so a caller has to be
	// able to tell "the daemon said nothing" from "this transport cannot say".
	// (--host is not affected: it tunnels to a real local client and carries the
	// daemon's detail through unchanged.)
	Detail string `json:"detail"`
}

type mergeSessionJSON struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// State is the protobuf enum name (e.g. SESSION_STATE_MERGED) — the same
	// vocabulary the wire uses, so it needs no client-side mapping table and
	// does not drift when a display label changes.
	//
	// It is the daemon's value verbatim, which on a successful merge can still
	// be the PRE-merge state: the daemon's handler reads the session before its
	// own deferred display refresh applies the Merged transition (see
	// services/bossd/internal/server/server.go, MergeSession). Treat this as
	// "the state as of the merge call", not as the merge's outcome — the
	// outcome is the envelope itself.
	State string `json:"state"`
}

type mergePRJSON struct {
	Number int32  `json:"number"`
	URL    string `json:"url"`
}

// newMergeJSON builds the success envelope. The pr object is omitted entirely
// unless the session carries a number or a URL: emitting it full of zero values
// would make a local-only-branch merge look like it merged PR #0.
//
// There is deliberately no already_merged field. The daemon's short-circuit
// returns an ordinary success plus a detail string, and MergeSessionResponse
// carries nothing else a client could key on — deriving the flag from detail
// text would reintroduce exactly the message matching this envelope removes.
// session.state is not a substitute: it can still read pre-merge on a genuine
// merge (see mergeSessionJSON.State), so a caller that branches on it would
// mistake a successful merge for a failed one. A caller that needs the settled
// state must re-read the session.
func newMergeJSON(sess *pb.Session, detail string) mergeJSON {
	env := mergeJSON{
		Session: mergeSessionJSON{
			ID:    sess.GetId(),
			Title: sess.GetTitle(),
			State: sess.GetState().String(),
		},
		Detail: detail,
	}
	if sess.PrNumber != nil || sess.PrUrl != nil {
		env.PR = &mergePRJSON{Number: sess.GetPrNumber(), URL: sess.GetPrUrl()}
	}
	return env
}

func runArchive(cmd *cobra.Command, sessionID string) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}
	ctx := context.Background()
	sessionID, err = resolveSessionID(c, ctx, sessionID)
	if err != nil {
		return err
	}
	sess, err := c.ArchiveSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("archive session: %w", err)
	}
	fmt.Printf("Session %s archived (%s).\n", sess.Id, sess.Title)
	return nil
}

func runRename(cmd *cobra.Command, sessionID, title string) error {
	title = strings.TrimSpace(title)
	if title == "" {
		return fmt.Errorf("rename: new title must not be empty")
	}
	c, err := newClient(cmd)
	if err != nil {
		return err
	}
	ctx := context.Background()
	sessionID, err = resolveSessionID(c, ctx, sessionID)
	if err != nil {
		return err
	}
	sess, err := c.UpdateSession(ctx, &pb.UpdateSessionRequest{Id: sessionID, Title: &title})
	if err != nil {
		return fmt.Errorf("rename session: %w", err)
	}
	fmt.Printf("Session %s renamed to %q.\n", sess.Id, sess.Title)
	return nil
}

func runResurrect(cmd *cobra.Command, sessionID string) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}
	ctx := context.Background()
	// Resolve prefix among archived sessions only.
	sessionID, err = resolveArchivedSessionID(c, ctx, sessionID)
	if err != nil {
		return err
	}
	sess, err := c.ResurrectSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("resurrect session: %w", err)
	}
	fmt.Printf("Session %s resurrected (%s).\n", sess.Id, sess.Title)
	return nil
}

func runSessionLinkPR(cmd *cobra.Command, sessionID, prRef string) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}
	ctx := context.Background()
	sessionID, err = resolveSessionID(c, ctx, sessionID)
	if err != nil {
		return err
	}
	sess, err := c.LinkSessionPR(ctx, sessionID, prRef)
	if err != nil {
		return fmt.Errorf("link PR: %w", err)
	}
	if sess.PrUrl != nil && *sess.PrUrl != "" {
		fmt.Printf("Session %s linked to PR %s.\n", sess.Id, *sess.PrUrl)
		return nil
	}
	if sess.PrNumber != nil {
		fmt.Printf("Session %s linked to PR #%d.\n", sess.Id, *sess.PrNumber)
		return nil
	}
	fmt.Printf("Session %s linked to PR.\n", sess.Id)
	return nil
}

func runTrashEmpty(cmd *cobra.Command) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}

	req := &pb.EmptyTrashRequest{}

	olderThan, _ := cmd.Flags().GetString("older-than")
	if olderThan != "" {
		d, err := parseDuration(olderThan)
		if err != nil {
			return fmt.Errorf("invalid --older-than: %w", err)
		}
		cutoff := time.Now().Add(-d)
		ts := timestamppb.New(cutoff)
		req.OlderThan = ts
	}

	ctx := context.Background()
	count, err := c.EmptyTrash(ctx, req)
	if err != nil {
		return fmt.Errorf("empty trash: %w", err)
	}
	if count == 0 {
		fmt.Println("No archived sessions to delete.")
	} else {
		fmt.Printf("Deleted %d archived session(s).\n", count)
	}
	return nil
}

// --- Daemon Management ---

func runDaemonInstall(cmd *cobra.Command) error {
	bossdPath, err := daemon.ResolveBossdPath()
	if err != nil {
		return err
	}

	force, _ := cmd.Flags().GetBool("force")
	if err := daemon.Install(bossdPath, force); err != nil {
		return fmt.Errorf("install daemon failed: %w", err)
	}
	installedPath, err := daemon.EnsureStaged(bossdPath)
	if err != nil {
		return fmt.Errorf("resolve installed daemon path: %w", err)
	}

	st, _ := daemon.GetStatus()
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintln(out, "Daemon installed and started.")
	_, _ = fmt.Fprint(out, daemonInstallBossdLine(installedPath, bossdPath))
	if st != nil && st.ServicePath != "" {
		_, _ = fmt.Fprintf(out, "  service: %s\n", st.ServicePath)
	}
	return nil
}

func daemonInstallBossdLine(installedPath, sourcePath string) string {
	if filepath.Clean(installedPath) == filepath.Clean(sourcePath) {
		return fmt.Sprintf("  bossd:   %s\n", sourcePath)
	}
	return fmt.Sprintf("  bossd:   %s (staged from %s)\n", installedPath, sourcePath)
}

func runDaemonUninstall(_ *cobra.Command) error {
	if err := daemon.Uninstall(); err != nil {
		return fmt.Errorf("uninstall daemon failed: %w", err)
	}
	fmt.Println("Daemon uninstalled.")
	return nil
}

func runDaemonStatus(_ *cobra.Command) error {
	st, err := daemonGetStatus()
	if err != nil {
		return fmt.Errorf("daemon status: %w", err)
	}
	profile, profileErr := currentDaemonProfile()

	if !st.Installed {
		fmt.Println("Daemon is not installed.")
		fmt.Println("  Run 'boss daemon install' to set up the daemon.")
	} else if st.Running {
		fmt.Println("Daemon is running.")
		if st.PID > 0 {
			fmt.Printf("  PID:     %d\n", st.PID)
		}
	} else {
		fmt.Println("Daemon is installed but not running.")
	}
	if st.ServicePath != "" {
		fmt.Printf("  service: %s\n", st.ServicePath)
	}
	if profileErr == nil {
		fmt.Printf("  settings: %s\n", profile.SettingsPath)
		fmt.Printf("  app data: %s\n", profile.AppDataDir)
		fmt.Printf("  socket:   %s\n", profile.SocketPath)
		fmt.Printf("  socket reachable: %t\n", daemonSocketReachable(profile.SocketPath))
		if metadata, err := daemonstate.Read(profile.AppDataDir); err == nil {
			fmt.Printf("  standalone PID: %d\n", metadata.PID)
			if metadata.ExecutablePath != "" {
				fmt.Printf("  standalone executable: %s\n", metadata.ExecutablePath)
			}
		}
	} else {
		fmt.Printf("  profile: unavailable (%v)\n", profileErr)
	}
	return nil
}

func runDaemonStart(_ *cobra.Command) error {
	socketPath, err := defaultSocketPath()
	if err != nil {
		return fmt.Errorf("daemon start: %w", err)
	}
	if daemonSocketReachable(socketPath) {
		fmt.Println("Daemon is already running.")
		return nil
	}
	if err := daemonEnsureRunning(socketPath); err != nil {
		return fmt.Errorf("start daemon failed: %w", err)
	}
	fmt.Println("Daemon started.")
	return nil
}

func runDaemonStop(cmd *cobra.Command) error {
	st, err := daemonGetStatus()
	if err != nil {
		return fmt.Errorf("daemon stop: %w", err)
	}
	profile, err := currentDaemonProfile()
	if err != nil {
		return fmt.Errorf("daemon stop: %w", err)
	}
	stopAllStandalone := false
	if cmd != nil {
		stopAllStandalone, _ = cmd.Flags().GetBool("all-standalone")
	}

	if stopAllStandalone {
		n, err := terminateAllBossdProcesses()
		if err != nil {
			return fmt.Errorf("stop standalone bossd failed: %w", err)
		}
		// BOS-349: this --all-standalone branch is the ONLY CLI path allowed to
		// signal plugin processes. It matches bossd-plugin-* by binary name/path
		// across profiles BY DESIGN — the explicit crash-cleanup escape hatch for
		// orphaned plugins. Normal stop/restart must never sweep: bossd's
		// Host.Stop (services/bossd/internal/plugin/host.go) owns reaping its own
		// children, and path-matching cannot attribute a plugin to a profile, so a
		// second daemon sharing the same binaries would lose its live plugins
		// (the BOS-349 mass-SIGTERM incident).
		pluginCount, err := terminateAllPluginProcesses(profile)
		if err != nil {
			return fmt.Errorf("stop plugin processes failed: %w", err)
		}
		if n == 0 && pluginCount == 0 {
			fmt.Println("No standalone bossd process is running.")
			return nil
		}
		if n > 0 && !waitForDaemonSocketGone(profile.SocketPath) {
			return fmt.Errorf("timed out waiting for daemon socket to stop after %s", daemon.LifecycleShutdownTimeout)
		}
		if n > 0 {
			fmt.Printf("Stopped %d bossd process(es) across all profiles.\n", n)
		}
		printPluginCleanup(pluginCount)
		return nil
	}

	if !st.Installed {
		n, err := terminateProfileBossdProcess(profile.AppDataDir, func(pid int) (processSignaler, error) {
			return os.FindProcess(pid)
		})
		if err != nil {
			return fmt.Errorf("stop standalone bossd failed: %w", err)
		}
		if n == 0 {
			fmt.Println("Daemon is not installed and no standalone bossd is running.")
			return nil
		}
		if !waitForDaemonSocketGone(profile.SocketPath) {
			return fmt.Errorf("timed out waiting for standalone bossd to stop after %s", daemon.LifecycleShutdownTimeout)
		}
		fmt.Println("Stopped standalone bossd for current profile.")
		return nil
	}
	if !st.Running {
		n, err := terminateProfileBossdProcess(profile.AppDataDir, func(pid int) (processSignaler, error) {
			return os.FindProcess(pid)
		})
		if err != nil {
			return fmt.Errorf("stop standalone bossd failed: %w", err)
		}
		if n > 0 {
			if !waitForDaemonSocketGone(profile.SocketPath) {
				return fmt.Errorf("timed out waiting for standalone bossd to stop after %s", daemon.LifecycleShutdownTimeout)
			}
			fmt.Println("Stopped standalone bossd for current profile.")
			return nil
		}
		fmt.Println("Daemon is already stopped.")
		return nil
	}
	if err := daemonStop(); err != nil {
		return fmt.Errorf("stop daemon failed: %w", err)
	}
	// Confirm the socket actually went away — bootout returns before the
	// process has fully exited on busy systems, so polling avoids misleading
	// "stopped" output while the old bossd is still draining.
	if !waitForDaemonSocketGone(profile.SocketPath) {
		return fmt.Errorf("timed out waiting for daemon socket to stop after %s", daemon.LifecycleShutdownTimeout)
	}
	fmt.Println("Daemon stopped.")
	return nil
}

func runDaemonRestart(_ *cobra.Command) error {
	st, err := daemonGetStatus()
	if err != nil {
		return fmt.Errorf("daemon restart: %w", err)
	}
	profile, err := currentDaemonProfile()
	if err != nil {
		return fmt.Errorf("daemon restart: %w", err)
	}
	socketPath := profile.SocketPath

	if !st.Installed {
		n, err := terminateStandaloneCurrentProfile(profile)
		if err != nil {
			return fmt.Errorf("restart standalone bossd failed: %w", err)
		}
		if n > 0 && !waitForDaemonSocketGone(socketPath) {
			return fmt.Errorf("timed out waiting for standalone bossd to stop after %s", daemon.LifecycleShutdownTimeout)
		}
		if n > 0 && !waitForStandaloneBossdExit(profile.AppDataDir) {
			return fmt.Errorf("timed out waiting for standalone bossd to exit after %s", daemon.LifecycleShutdownTimeout)
		}
		if err := daemonEnsureRunning(socketPath); err != nil {
			return fmt.Errorf("restart standalone bossd failed: %w", err)
		}
		if n > 0 {
			fmt.Println("Restarted standalone bossd for current profile.")
		} else {
			fmt.Println("Started standalone bossd.")
		}
		return nil
	}
	if st.Running {
		if err := daemonStop(); err != nil {
			return fmt.Errorf("stop daemon failed: %w", err)
		}
		if !waitForDaemonSocketGone(socketPath) {
			return fmt.Errorf("timed out waiting for daemon socket to stop after %s", daemon.LifecycleShutdownTimeout)
		}
	} else {
		n, err := terminateCurrentProfileBossd()
		if err != nil {
			return fmt.Errorf("restart standalone bossd failed: %w", err)
		}
		if n > 0 && !waitForDaemonSocketGone(socketPath) {
			return fmt.Errorf("timed out waiting for standalone bossd to stop after %s", daemon.LifecycleShutdownTimeout)
		}
	}
	if err := restartDaemon(); err != nil {
		return fmt.Errorf("restart daemon failed: %w", err)
	}
	if err := waitForDaemonRestartReady(socketPath); err != nil {
		return fmt.Errorf("daemon restarted but %w", err)
	}
	fmt.Println("Daemon restarted.")
	return nil
}

// waitForDaemonRestartReady waits for the replacement daemon to accept
// connections so restart callers cannot report success for the draining daemon.
func waitForDaemonRestartReady(socketPath string) error {
	deadline := time.Now().Add(daemonRestartReadyTimeout)
	for time.Now().Before(deadline) {
		if daemonSocketReachable(socketPath) {
			return nil
		}
		time.Sleep(daemonRestartPollInterval)
	}
	return fmt.Errorf("socket did not become reachable after %s; check 'boss daemon status'", daemonRestartReadyTimeout)
}

func restartSocketPath(socketPath string, err error) (string, error) {
	if err != nil {
		return "", fmt.Errorf("daemon restart: %w", err)
	}
	if socketPath == "" {
		return "", fmt.Errorf("daemon restart: could not resolve daemon socket path")
	}
	return socketPath, nil
}

// waitForSocketGone blocks until the unix socket at path stops accepting
// connections or the timeout elapses. Used after stop/restart so we don't
// print "stopped" while the old bossd is still draining.
func waitForSocketGone(path string) bool {
	// Let the user know we may block: bossd drains cron and shuts down its
	// server before removing the socket, which can take several seconds, and
	// silence for that long reads like a hang.
	if daemon.IsSocketReachable(path) {
		fmt.Println("Waiting for bossd to shut down…")
	}
	deadline := time.Now().Add(daemon.LifecycleShutdownTimeout)
	for time.Now().Before(deadline) {
		if !daemon.IsSocketReachable(path) {
			return true
		}
		time.Sleep(daemon.LifecyclePollInterval)
	}
	return false
}

func waitForCurrentProfileBossdExit() bool {
	profile, err := currentDaemonProfile()
	if err != nil {
		return false
	}
	return waitForStandaloneBossdExit(profile.AppDataDir)
}

func waitForDaemonLockRelease(appDataDir string) bool {
	deadline := time.Now().Add(daemon.LifecycleShutdownTimeout)
	lockPath := filepath.Join(appDataDir, "bossd.lock")
	for time.Now().Before(deadline) {
		// #nosec G304 -- fixed singleton-lock filename beneath the active profile's configured app-data directory; non-secret local state.
		// owner=@recurser review-by=2027-01-18 issue=BOS-28
		lock, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return false
		}
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		_ = lock.Close()
		if err == nil {
			return true
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return false
		}
		time.Sleep(daemon.LifecyclePollInterval)
	}
	return false
}

type daemonProfile struct {
	SettingsPath string
	AppDataDir   string
	SocketPath   string
}

func currentDaemonProfile() (daemonProfile, error) {
	settingsPath, err := config.Path()
	if err != nil {
		return daemonProfile{}, fmt.Errorf("settings path: %w", err)
	}
	settings, err := config.LoadFrom(settingsPath)
	if err != nil {
		return daemonProfile{}, fmt.Errorf("load settings: %w", err)
	}
	appDataDir, err := appDataDirForSettings(settings)
	if err != nil {
		return daemonProfile{}, err
	}
	socketPath, err := defaultSocketPath()
	if err != nil {
		return daemonProfile{}, fmt.Errorf("socket path: %w", err)
	}
	return daemonProfile{
		SettingsPath: settingsPath,
		AppDataDir:   appDataDir,
		SocketPath:   socketPath,
	}, nil
}

func appDataDirForSettings(settings config.Settings) (string, error) {
	if dir, ok, err := config.ConfiguredAppDataDir(settings); err != nil {
		return "", err
	} else if ok {
		return dir, nil
	}
	dir, err := config.DefaultAppDataDir()
	if err != nil {
		return "", fmt.Errorf("resolve app data dir: %w", err)
	}
	return dir, nil
}

func terminateProfileBossdProcess(appDataDir string, findProcess func(int) (processSignaler, error)) (int, error) {
	metadata, err := daemonstate.Read(appDataDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	if metadata.PID <= 0 {
		return 0, nil
	}
	if !metadataMatchesRunningProcess(metadata) {
		return 0, nil
	}
	return signalBossdProcesses([]int{metadata.PID}, findProcess)
}

var bossdProcessCommandLine = processCommandLine

func metadataMatchesRunningProcess(metadata daemonstate.Metadata) bool {
	if metadata.ExecutablePath == "" {
		return true
	}
	commandLine, err := bossdProcessCommandLine(metadata.PID)
	if err != nil {
		return false
	}
	return commandLineStartsWithExecutable(commandLine, metadata.ExecutablePath)
}

func processCommandLine(pid int) (string, error) {
	// #nosec G204 -- ps -p <pid> -o command=; const argv plus an int pid, no shell.
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// terminateBossdProcesses SIGTERMs every running `bossd` process (exact name
// match — does not touch `bossd-plugin-*` children, which their parent will
// reap on its way down). Returns the number of processes signalled. Safe to
// call when nothing is running.
func terminateBossdProcesses() (int, error) {
	pids, err := findBossdPIDs()
	if err != nil {
		return 0, err
	}
	return signalBossdProcesses(pids, func(pid int) (processSignaler, error) {
		return os.FindProcess(pid)
	})
}

type processSignaler interface {
	Signal(os.Signal) error
}

func signalBossdProcesses(pids []int, findProcess func(int) (processSignaler, error)) (int, error) {
	signalled := 0
	var errs []error
	for _, pid := range pids {
		p, err := findProcess(pid)
		if err != nil {
			errs = append(errs, fmt.Errorf("find bossd pid %d: %w", pid, err))
			continue
		}
		if err := p.Signal(syscall.SIGTERM); err != nil {
			if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
				continue
			}
			errs = append(errs, fmt.Errorf("signal bossd pid %d: %w", pid, err))
			continue
		}
		signalled++
	}
	return signalled, errors.Join(errs...)
}

// terminateAllBossdPluginProcesses backs `boss daemon stop --all-standalone`,
// the only CLI path allowed to signal plugin processes (BOS-349). It SIGTERMs
// every bossd-plugin-* process this user owns, matched by binary name/path
// across profiles — a deliberate broad sweep for crash-orphan cleanup, not a
// per-profile scoped teardown.
func terminateAllBossdPluginProcesses(profile daemonProfile) (int, error) {
	settings, err := config.LoadFrom(profile.SettingsPath)
	if err != nil {
		return 0, err
	}
	pids, err := findBossdPluginPIDs()
	if err != nil {
		return 0, err
	}
	matches := bossdPluginProcessMatcher(settings.Plugins)
	return signalBossdPluginProcesses(pids, matches, func(pid int) (processSignaler, error) {
		return os.FindProcess(pid)
	})
}

func printPluginCleanup(n int) {
	if n > 0 {
		fmt.Printf("Stopped %d plugin process(es).\n", n)
	}
}

var findBossdPluginPIDs = findBossdPluginPIDsFromPgrep

func findBossdPluginPIDsFromPgrep() ([]int, error) {
	// #nosec G204 -- pgrep -u <euid> -f bossd-plugin-; const args plus an int euid, no shell.
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	out, err := exec.Command("pgrep", bossdPluginPgrepArgs()...).Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("pgrep bossd-plugin: %w", err)
	}
	return parsePgrepOutput(string(out)), nil
}

func bossdPluginPgrepArgs() []string {
	return []string{"-u", strconv.Itoa(os.Geteuid()), "-f", "bossd-plugin-"}
}

// bossdPluginProcessMatcher builds the predicate used by the --all-standalone
// broad sweep (BOS-349): it matches a process command line if it starts with a
// configured plugin binary path, or — since the escape hatch cleans crash
// orphans from any profile — if it starts with any bossd-plugin-* executable.
func bossdPluginProcessMatcher(plugins []config.PluginConfig) func(string) bool {
	configuredPaths := make(map[string]struct{}, len(plugins))
	for _, plugin := range plugins {
		if plugin.Path == "" {
			continue
		}
		clean := filepath.Clean(plugin.Path)
		configuredPaths[clean] = struct{}{}
		if resolved, err := filepath.EvalSymlinks(clean); err == nil {
			configuredPaths[filepath.Clean(resolved)] = struct{}{}
		}
	}
	return func(commandLine string) bool {
		for configuredPath := range configuredPaths {
			if !strings.HasPrefix(filepath.Base(configuredPath), "bossd-plugin-") {
				continue
			}
			if commandLineStartsWithExecutable(commandLine, configuredPath) {
				return true
			}
		}
		return commandLineStartsWithBossdPluginExecutable(commandLine)
	}
}

func commandLineStartsWithExecutable(commandLine, executable string) bool {
	return daemon.CommandLineStartsWithExecutable(commandLine, executable)
}

func commandLineStartsWithBossdPluginExecutable(commandLine string) bool {
	commandLine = strings.TrimSpace(commandLine)
	if commandLine == "" {
		return false
	}
	firstToken := commandLineFirstToken(commandLine)
	if strings.HasPrefix(filepath.Base(filepath.Clean(firstToken)), "bossd-plugin-") {
		return true
	}
	if commandLine[0] != '/' && commandLine[0] != '.' {
		return false
	}
	if firstToken != "" {
		cleanFirstToken := filepath.Clean(firstToken)
		if info, err := os.Stat(cleanFirstToken); err == nil && !info.IsDir() &&
			!strings.HasPrefix(filepath.Base(cleanFirstToken), "bossd-plugin-") {
			return false
		}
	}
	marker := strings.Index(commandLine, "/bossd-plugin-")
	if marker < 0 {
		return false
	}
	prefix := commandLine[:marker+1]
	if strings.ContainsAny(prefix, " \t\r\n") && !strings.HasSuffix(prefix, "/plugins/") {
		return false
	}
	end := len(commandLine)
	for i := marker + 1; i < len(commandLine); i++ {
		if commandLine[i] == ' ' || commandLine[i] == '\t' || commandLine[i] == '\r' || commandLine[i] == '\n' {
			end = i
			break
		}
	}
	return strings.HasPrefix(filepath.Base(filepath.Clean(commandLine[:end])), "bossd-plugin-")
}

func commandLineFirstToken(commandLine string) string {
	for i, r := range commandLine {
		switch r {
		case ' ', '\t', '\r', '\n':
			return commandLine[:i]
		}
	}
	return commandLine
}

func signalBossdPluginProcesses(pids []int, matches func(string) bool, findProcess func(int) (processSignaler, error)) (int, error) {
	signalled := 0
	var errs []error
	for _, pid := range pids {
		commandLine, err := bossdProcessCommandLine(pid)
		if err != nil {
			continue
		}
		if !matches(commandLine) {
			continue
		}
		p, err := findProcess(pid)
		if err != nil {
			errs = append(errs, fmt.Errorf("find bossd plugin pid %d: %w", pid, err))
			continue
		}
		if err := p.Signal(syscall.SIGTERM); err != nil {
			if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
				continue
			}
			errs = append(errs, fmt.Errorf("signal bossd plugin pid %d: %w", pid, err))
			continue
		}
		signalled++
	}
	return signalled, errors.Join(errs...)
}

// findBossdPIDs returns PIDs of running processes owned by this effective user
// whose program name is exactly "bossd". Uses pgrep, which is available on
// macOS and Linux.
func findBossdPIDs() ([]int, error) {
	// #nosec G204 -- pgrep -u <euid> -x bossd; const args plus an int euid, no shell.
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	out, err := exec.Command("pgrep", bossdPgrepArgs()...).Output()
	if err != nil {
		// pgrep exits 1 when there are no matches — treat as empty result.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("pgrep bossd: %w", err)
	}
	return parsePgrepOutput(string(out)), nil
}

func bossdPgrepArgs() []string {
	return []string{"-u", strconv.Itoa(os.Geteuid()), "-x", "bossd"}
}

// parsePgrepOutput converts pgrep's newline-separated PID output into ints.
// Malformed lines are skipped silently — pgrep can legitimately emit blank
// trailing lines, and any non-numeric line is not a usable PID anyway.
func parsePgrepOutput(s string) []int {
	var pids []int
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var pid int
		if _, err := fmt.Sscanf(line, "%d", &pid); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids
}

// resolveSessionID resolves a (possibly prefix) session ID to a full session ID.
// If the prefix is at least 32 characters (full UUID length), it is used directly.
// Otherwise, it searches all sessions (including archived) for a unique prefix match.
func resolveSessionID(c client.BossClient, ctx context.Context, prefix string) (string, error) {
	if len(prefix) >= 32 {
		return prefix, nil
	}
	sessions, err := c.ListSessions(ctx, &pb.ListSessionsRequest{IncludeArchived: true}, client.SessionReadOptions{})
	if err != nil {
		return "", fmt.Errorf("list sessions: %w", err)
	}
	var matches []string
	for _, s := range sessions {
		if strings.HasPrefix(s.Id, prefix) {
			matches = append(matches, s.Id)
		}
	}
	switch len(matches) {
	case 0:
		// Tagged, not wrapped: codedError leaves the message byte-identical,
		// so the human output is unchanged while --json can still classify
		// this as NOT_FOUND. Resolution happens entirely client-side, so
		// there is no connect code to derive one from.
		return "", codedError(codeNotFound, fmt.Errorf("no session found matching prefix %q", prefix))
	case 1:
		return matches[0], nil
	default:
		return "", codedError(codeAmbiguousPrefix, fmt.Errorf("ambiguous prefix %q matches %d sessions", prefix, len(matches)))
	}
}

// resolveArchivedSessionID is like resolveSessionID but only matches archived sessions.
func resolveArchivedSessionID(c client.BossClient, ctx context.Context, prefix string) (string, error) {
	sessions, err := c.ListSessions(ctx, &pb.ListSessionsRequest{IncludeArchived: true}, client.SessionReadOptions{})
	if err != nil {
		return "", fmt.Errorf("list sessions: %w", err)
	}
	var matches []string
	for _, s := range sessions {
		if s.ArchivedAt != nil && strings.HasPrefix(s.Id, prefix) {
			matches = append(matches, s.Id)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no archived session found matching prefix %q", prefix)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("ambiguous prefix %q matches %d archived sessions", prefix, len(matches))
	}
}

// accountShowLabel renders the bound-account line for `boss show`. It prefers
// the server-computed account_label (one source of truth, matching the web
// SessionDetail badge); when the daemon has not populated a label it falls back
// to the raw account id, and to the unmanaged label for an unbound session (empty
// account id = the daemon's default-account policy applied).
func accountShowLabel(accountID, accountLabel string) string {
	if accountLabel != "" {
		return accountLabel
	}
	if accountID == "" {
		return views.UnmanagedLocalCredentialsLabel
	}
	return accountID
}

// printSessionShowHeader writes the key-value header block for `boss show` to
// stdout. Kept separate from runShow so its formatting (including the Account
// line) is unit-testable without a live daemon client.
func printSessionShowHeader(sess *pb.Session) {
	fmt.Printf("  ID:       %s\n", sess.Id)
	fmt.Printf("  Title:    %s\n", sess.Title)
	fmt.Printf("  Repo:     %s\n", sess.RepoDisplayName)
	fmt.Printf("  Branch:   %s\n", sess.BranchName)
	fmt.Printf("  State:    %s\n", views.StateLabel(sess.State))
	fmt.Printf("  Account:  %s\n", accountShowLabel(sess.GetAccountId(), sess.GetAccountLabel()))
	if sess.PrNumber != nil {
		fmt.Printf("  PR:       #%d\n", *sess.PrNumber)
	}
	if sess.GetWorktreePath() != "" {
		fmt.Printf("  Worktree: %s\n", sess.GetWorktreePath())
	}
	if sess.CreatedAt != nil {
		fmt.Printf("  Created:  %s\n", views.RelativeTime(sess.CreatedAt.AsTime()))
	}
	if sess.ArchivedAt != nil {
		fmt.Printf("  Archived: %s\n", views.RelativeTime(sess.ArchivedAt.AsTime()))
	}
}

func runShow(cmd *cobra.Command, sessionID string) error {
	asJSON, _ := cmd.Flags().GetBool(jsonFlagName)

	c, err := newClient(cmd)
	if err != nil {
		return emitJSONFailure(cmd, asJSON, err)
	}

	ctx := context.Background()
	sessionID, err = resolveSessionID(c, ctx, sessionID)
	if err != nil {
		return emitJSONFailure(cmd, asJSON, err)
	}

	sess, err := c.GetSession(ctx, sessionID, client.SessionReadOptions{})
	if err != nil {
		return emitJSONFailure(cmd, asJSON, fmt.Errorf("get session: %w", err))
	}

	if asJSON {
		// Session detail only — the chats table below is `boss chats`' shape,
		// which has to join in per-chat status, so it stays out of this
		// envelope rather than shipping a half-populated version of it.
		return emitJSON(cmd, sessionShowJSON{Session: newSessionDetailJSON(sess)})
	}

	// Print key-value header.
	printSessionShowHeader(sess)

	// Last repair-attempt diagnostics, when present. last_repair_started_at
	// is the "an attempt was recorded" sentinel — it stays nil for sessions
	// that the repair plugin has never run, so we suppress the line entirely
	// in that case instead of showing "Last repair: -" noise. The count is
	// a *consecutive-failure* tally (clean runs reset it to zero), which is
	// why the "(N×)" annotation only appears on the failure branches.
	switch {
	case sess.GetLastRepairRunnerError() != "":
		fmt.Printf("  Last repair: ✗ runner failed (%d×): %s\n",
			sess.GetLastRepairAttemptCount(), sess.GetLastRepairRunnerError())
	case sess.GetLastRepairExitError() != "":
		fmt.Printf("  Last repair: ✗ agent exited error (%d×): %s\n",
			sess.GetLastRepairAttemptCount(), sess.GetLastRepairExitError())
	case sess.GetLastRepairStartedAt() != nil:
		fmt.Printf("  Last repair: ✓ ok %s\n",
			views.RelativeTime(sess.GetLastRepairStartedAt().AsTime()))
	}

	// Non-fatal setup-script failure flag: the session was created in a
	// degraded state because its repo setup script failed during worktree
	// creation. Empty for clean runs.
	if sess.GetSetupError() != "" {
		fmt.Printf("  Setup:    ⚠ setup script failed: %s\n", sess.GetSetupError())
	}

	// List chats as a table.
	chats, err := c.ListChats(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("list chats: %w", err)
	}
	if len(chats) == 0 {
		fmt.Println("\n  No chats.")
		return nil
	}

	statuses, statusErr := fetchChatStatuses(ctx, c, sessionID)
	if statusErr != nil {
		warnChatStatusUnavailable(cmd, statusErr)
	}

	fmt.Println()
	printChatsTable(cmd, chats, statuses, statusErr == nil)
	return nil
}

func runChats(cmd *cobra.Command, sessionID string) error {
	asJSON, _ := cmd.Flags().GetBool(jsonFlagName)

	c, err := newClient(cmd)
	if err != nil {
		return emitJSONFailure(cmd, asJSON, err)
	}

	ctx := context.Background()
	sessionID, err = resolveSessionID(c, ctx, sessionID)
	if err != nil {
		return emitJSONFailure(cmd, asJSON, err)
	}

	chats, err := c.ListChats(ctx, sessionID)
	if err != nil {
		return emitJSONFailure(cmd, asJSON, fmt.Errorf("list chats: %w", err))
	}

	// The status read is what makes this command answerable at all: a chat row
	// without a status says nothing about whether the chat has settled. It is
	// fetched before the empty-list shortcut so the two output modes agree on
	// when status is unavailable regardless of how many chats came back.
	statuses, statusErr := fetchChatStatuses(ctx, c, sessionID)
	if statusErr != nil {
		if asJSON {
			// Fail loudly rather than emit rows with an absent status. Degrading
			// here produces rows that read `"status": "UNSPECIFIED"` — byte
			// identical to chats that genuinely have no cached status — so a
			// driver cannot tell "the read failed" from "this chat has not
			// reported yet", and one forgotten null check merges on a WORKING
			// chat. Refusing to answer is what makes "a missing status degrades
			// to a stall" true rather than aspirational.
			return emitJSONFailure(cmd, asJSON, codedError(codeChatStatusUnavailable,
				fmt.Errorf("get chat statuses: %w", statusErr)))
		}
		warnChatStatusUnavailable(cmd, statusErr)
	}

	if asJSON {
		return emitJSON(cmd, newChatsJSON(chats, statuses))
	}

	if len(chats) == 0 {
		fmt.Println("No chats found.")
		return nil
	}

	printChatsTable(cmd, chats, statuses, statusErr == nil)
	return nil
}

// chatStatusReader is the one client method the chat-status join needs. Naming
// it keeps fetchChatStatuses testable with a stub without dragging in the whole
// client surface.
type chatStatusReader interface {
	GetChatStatuses(ctx context.Context, sessionID string) ([]*pb.ChatStatusEntry, error)
}

// fetchChatStatuses reads the session's per-chat statuses and keys them by
// agent_session_id for the left join onto ListChats.
//
// Every error is reported to the caller, including connect.CodeUnimplemented.
// chatWaitTick (chat.go) tolerates Unimplemented because it has a transcript
// read to fall back on; this command has none. --remote is no longer a source of
// Unimplemented here (RemoteClient.GetChatStatuses proxies the read through
// ProxyGetChatStatuses since BOS-824), but a daemon older than that RPC still
// answers Unimplemented, and swallowing it would turn "this transport cannot
// tell you" into a silent "nothing is working".
//
// get_session_statuses is deliberately NOT a fallback here: it answers a
// different question (session aggregate vs the tracked chat), and a
// plausible-looking wrong answer on this path causes a wrong merge.
func fetchChatStatuses(ctx context.Context, c chatStatusReader, sessionID string) (map[string]*pb.ChatStatusEntry, error) {
	entries, err := c.GetChatStatuses(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	byChat := make(map[string]*pb.ChatStatusEntry, len(entries))
	for _, e := range entries {
		byChat[e.GetAgentSessionId()] = e
	}
	return byChat, nil
}

// warnChatStatusUnavailable prints the single stderr line the human table mode
// owes a reader whose STATUS cells all read "?".
func warnChatStatusUnavailable(cmd *cobra.Command, err error) {
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
		"boss: chat status unavailable (%v); STATUS and LAST OUTPUT show ? — no chat below can be read as settled\n", err)
}

// chatStatusName renders the enum as its bare suffix (IDLE, WORKING, WAITING,
// …), mirroring displayStatusName's treatment of DisplayStatus. A chat the
// daemon holds no cached status for stays UNSPECIFIED rather than being mapped
// to anything friendlier: only the caller can decide, and every safe caller
// treats UNSPECIFIED as not-settled.
//
// A value this build's enum does not know — a newer daemon reporting a status
// added after this binary was compiled — also renders UNSPECIFIED. The guard is
// load-bearing: protobuf-go's generated String() falls back to the *decimal
// number* for any value missing from ChatStatus_name, so TrimPrefix alone would
// leave a bare integer in the STATUS column and in the JSON `status` field.
// UNSPECIFIED is the only honest rendering, because it is the one value already
// documented as "not settled, do not merge on it".
func chatStatusName(s pb.ChatStatus) string {
	if _, known := pb.ChatStatus_name[int32(s)]; !known {
		return "UNSPECIFIED"
	}
	return strings.TrimPrefix(s.String(), "CHAT_STATUS_")
}

// chatsJSON is the `boss chats --json` success envelope.
type chatsJSON struct {
	Chats []chatJSON `json:"chats"`
}

// chatJSON is one chat row. Every field is always emitted — never omitempty:
// an absent field is precisely what a settled-green driver must not have to
// guess about.
type chatJSON struct {
	AgentSessionID string `json:"agent_session_id"`
	Title          string `json:"title"`
	CreatedAt      string `json:"created_at"`
	// Status is the trimmed enum string ("IDLE"), not the numeric value: a
	// driver comparing against "IDLE" is the intended use, and a number would
	// invite off-by-one coupling to the enum.
	Status string `json:"status"`
	// LastOutputAt is the proto's last_output_at verbatim, and is named after
	// it. For a WORKING chat it is effectively fetch-time — every WORKING chat
	// in one fetch shares it to the nanosecond — so it says nothing about
	// staleness until the chat is IDLE. "IDLE and stale" is a sound settled
	// gate; "stale" alone is not.
	LastOutputAt string `json:"last_output_at"`
	// WaitingReason explains a WAITING chat's block; empty for every other
	// status.
	WaitingReason string `json:"waiting_reason"`
}

// newChatsJSON left-joins the statuses onto the chats by agent_session_id.
func newChatsJSON(chats []*pb.ClaudeChat, statuses map[string]*pb.ChatStatusEntry) chatsJSON {
	out := chatsJSON{Chats: make([]chatJSON, 0, len(chats))}
	for _, chat := range chats {
		st := statuses[chat.GetAgentSessionId()]
		out.Chats = append(out.Chats, chatJSON{
			AgentSessionID: chat.GetAgentSessionId(),
			Title:          chat.GetTitle(),
			CreatedAt:      rfc3339OrEmpty(chat.GetCreatedAt()),
			Status:         chatStatusName(st.GetStatus()),
			LastOutputAt:   rfc3339OrEmpty(st.GetLastOutputAt()),
			WaitingReason:  st.GetWaitingReason(),
		})
	}
	return out
}

// chatStatusCellMax bounds the STATUS cell. A WAITING chat appends its
// waiting_reason inline, and reasons are free text from the daemon, so the same
// bound is applied to the cell text and to the column width — otherwise one long
// reason widens the table past a terminal for every other row.
const chatStatusCellMax = 44

// printChatsTable renders the human table. statusAvailable false means the
// status read failed: STATUS and LAST OUTPUT render "?" so no cell can be
// mistaken for a settled chat.
func printChatsTable(cmd *cobra.Command, chats []*pb.ClaudeChat, statuses map[string]*pb.ChatStatusEntry, statusAvailable bool) {
	ids := make([]string, len(chats))
	titles := make([]string, len(chats))
	createds := make([]string, len(chats))
	statusCells := make([]string, len(chats))
	lastOutputs := make([]string, len(chats))
	for i, chat := range chats {
		ids[i] = chat.AgentSessionId
		t := chat.Title
		if t == "" {
			t = "New chat"
		}
		t = truncateString(t, 50)
		titles[i] = t
		if chat.CreatedAt != nil {
			createds[i] = views.RelativeTime(chat.CreatedAt.AsTime())
		} else {
			createds[i] = "-"
		}

		if !statusAvailable {
			statusCells[i], lastOutputs[i] = "?", "?"
			continue
		}
		st := statuses[chat.GetAgentSessionId()]
		cell := chatStatusName(st.GetStatus())
		if reason := st.GetWaitingReason(); reason != "" {
			// waiting_reason exists precisely to explain the wait, so the human
			// table shows it inline rather than making a reader run a second
			// command to find out what the chat is blocked on.
			cell = truncateString(cell+" ("+reason+")", chatStatusCellMax)
		}
		statusCells[i] = cell
		if lo := st.GetLastOutputAt(); lo != nil {
			lastOutputs[i] = views.RelativeTime(lo.AsTime())
		} else {
			lastOutputs[i] = "-"
		}
	}

	// The new columns are appended rather than inserted: ID / TITLE / CREATED
	// keep the positions they have always had, so a reader (or a script) that
	// already slices this table by field index is widened, not silently shifted
	// onto a different column.
	cols := []table.Column{
		{Title: "ID", Width: views.MaxColWidth("ID", ids, 0)},
		{Title: "TITLE", Width: views.MaxColWidth("TITLE", titles, 50)},
		{Title: "CREATED", Width: views.MaxColWidth("CREATED", createds, 12)},
		{Title: "STATUS", Width: views.MaxColWidth("STATUS", statusCells, chatStatusCellMax)},
		{Title: "LAST OUTPUT", Width: views.MaxColWidth("LAST OUTPUT", lastOutputs, 12)},
	}

	rows := make([]table.Row, len(chats))
	for i := range chats {
		rows[i] = table.Row{ids[i], titles[i], createds[i], statusCells[i], lastOutputs[i]}
	}

	t := table.New(
		table.WithColumns(cols),
		table.WithRows(rows),
		table.WithHeight(len(rows)+1),
		table.WithWidth(views.CLIColumnsWidth(cols)),
		table.WithStyles(views.CLITableStyles()),
		table.WithFocused(false),
	)
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), t.View())
}

// truncateString truncates a string to maxRunes runes, appending "..." if truncated.
func truncateString(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes-3]) + "..."
}

// parseDuration parses a human-friendly duration like "30d", "2w", "1h".
func parseDuration(s string) (time.Duration, error) {
	if len(s) < 2 {
		return 0, fmt.Errorf("invalid duration: %s", s)
	}

	unit := s[len(s)-1]
	numStr := s[:len(s)-1]
	var n int
	if _, err := fmt.Sscanf(numStr, "%d", &n); err != nil {
		return 0, fmt.Errorf("invalid duration number: %s", numStr)
	}

	switch unit {
	case 'h':
		return time.Duration(n) * time.Hour, nil
	case 'd':
		return time.Duration(n) * 24 * time.Hour, nil
	case 'w':
		return time.Duration(n) * 7 * 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("unknown duration unit: %c (use h, d, or w)", unit)
	}
}

// --- Trash ---

func runTrashLS(cmd *cobra.Command) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}

	ctx := context.Background()
	sessions, err := c.ListSessions(ctx, &pb.ListSessionsRequest{IncludeArchived: true}, client.SessionReadOptions{})
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}

	// Filter to archived only.
	var archived []*pb.Session
	for _, s := range sessions {
		if s.ArchivedAt != nil {
			archived = append(archived, s)
		}
	}

	if len(archived) == 0 {
		fmt.Println("Trash is empty.")
		return nil
	}

	ids := make([]string, len(archived))
	titles := make([]string, len(archived))
	repos := make([]string, len(archived))
	prStrs := make([]string, len(archived))
	archiveds := make([]string, len(archived))
	for i, sess := range archived {
		ids[i] = sess.Id
		t := sess.Title
		t = truncateString(t, 30)
		titles[i] = t
		repos[i] = sess.RepoDisplayName
		if sess.PrNumber != nil {
			prStrs[i] = fmt.Sprintf("#%d", *sess.PrNumber)
		} else {
			prStrs[i] = "-"
		}
		archiveds[i] = views.RelativeTime(sess.ArchivedAt.AsTime())
	}

	cols := []table.Column{
		{Title: "ID", Width: views.MaxColWidth("ID", ids, 0)},
		{Title: "TITLE", Width: views.MaxColWidth("TITLE", titles, 30)},
		{Title: "REPO", Width: views.MaxColWidth("REPO", repos, 20)},
		{Title: "PR", Width: views.MaxColWidth("PR", prStrs, 8)},
		{Title: "ARCHIVED", Width: views.MaxColWidth("ARCHIVED", archiveds, 12)},
	}

	rows := make([]table.Row, len(archived))
	for i := range archived {
		rows[i] = table.Row{ids[i], titles[i], repos[i], prStrs[i], archiveds[i]}
	}

	t := table.New(
		table.WithColumns(cols),
		table.WithRows(rows),
		table.WithHeight(len(rows)+1),
		table.WithWidth(views.CLIColumnsWidth(cols)),
		table.WithStyles(views.CLITableStyles()),
		table.WithFocused(false),
	)
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), t.View())
	return nil
}

func runTrashDelete(cmd *cobra.Command, sessionID string) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}

	ctx := context.Background()
	sessionID, err = resolveArchivedSessionID(c, ctx, sessionID)
	if err != nil {
		return err
	}

	yes, _ := cmd.Flags().GetBool("yes")
	if !yes {
		fmt.Printf("Permanently delete session %s? [y/N] ", sessionID)
		var answer string
		if _, err := fmt.Scanln(&answer); err != nil || (answer != "y" && answer != "Y") {
			fmt.Println("Cancelled.")
			return nil
		}
	}

	if err := c.RemoveSession(ctx, sessionID); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	fmt.Printf("Session %s permanently deleted.\n", sessionID)
	return nil
}

// --- Repo Update ---

func runRepoUpdate(cmd *cobra.Command, repoID string) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}

	req := &pb.UpdateRepoRequest{Id: repoID}
	anyChanged := false

	if cmd.Flags().Changed("name") {
		v, _ := cmd.Flags().GetString("name")
		req.DisplayName = &v
		anyChanged = true
	}
	if cmd.Flags().Changed("setup-script") {
		v, _ := cmd.Flags().GetString("setup-script")
		req.SetupScript = &v
		anyChanged = true
	}
	if cmd.Flags().Changed("merge-strategy") {
		v, _ := cmd.Flags().GetString("merge-strategy")
		switch v {
		case "merge", "rebase", "squash":
			req.MergeStrategy = &v
		default:
			return fmt.Errorf("invalid merge strategy %q (use merge, rebase, or squash)", v)
		}
		anyChanged = true
	}

	// Boolean flag pairs.
	boolPairs := []struct {
		enable, disable string
		setter          func(v bool)
	}{
		{"auto-merge", "no-auto-merge", func(v bool) { req.CanAutoMerge = &v }},
		{"auto-merge-dependabot", "no-auto-merge-dependabot", func(v bool) { req.CanAutoMergeDependabot = &v }},
		{"auto-repair", "no-auto-repair", func(v bool) { req.CanAutoRepair = &v }},
		{"delete-branches", "no-delete-branches", func(v bool) { req.CanAutoDeleteBranches = &v }},
		{"keep-branches-current", "no-keep-branches-current", func(v bool) { req.ShouldKeepBranchesCurrent = &v }},
	}
	for _, bp := range boolPairs {
		enableChanged := cmd.Flags().Changed(bp.enable)
		disableChanged := cmd.Flags().Changed(bp.disable)
		if enableChanged && disableChanged {
			return fmt.Errorf("cannot use both --%s and --%s", bp.enable, bp.disable)
		}
		if enableChanged {
			bp.setter(true)
			anyChanged = true
		}
		if disableChanged {
			bp.setter(false)
			anyChanged = true
		}
	}

	if !anyChanged {
		return fmt.Errorf("no flags provided — use --name, --setup-script, --merge-strategy, or boolean flags")
	}

	ctx := context.Background()
	repo, err := c.UpdateRepo(ctx, req)
	if err != nil {
		return fmt.Errorf("update repo: %w", err)
	}

	fmt.Printf("Repository updated.\n")
	fmt.Printf("  ID:       %s\n", repo.Id)
	fmt.Printf("  Name:     %s\n", repo.DisplayName)
	fmt.Printf("  Strategy: %s\n", repo.MergeStrategy)
	if repo.SetupScript != nil {
		fmt.Printf("  Setup:    %s\n", *repo.SetupScript)
	}
	fmt.Printf("  Mark ready on green:    %v\n", repo.CanAutoMerge)
	fmt.Printf("  Auto-merge Dependabot:  %v\n", repo.CanAutoMergeDependabot)
	fmt.Printf("  Automatic repair:       %v\n", repo.CanAutoRepair)
	fmt.Printf("  Delete branches:        %v\n", repo.CanAutoDeleteBranches)
	fmt.Printf("  Keep branches current:  %v\n", repo.ShouldKeepBranchesCurrent)
	return nil
}

// --- Settings ---

func runSettings(cmd *cobra.Command) error {
	s, err := config.Load()
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}

	// If no flags provided, display current settings.
	anyChanged := cmd.Flags().Changed("skip-permissions") ||
		cmd.Flags().Changed("no-skip-permissions") ||
		cmd.Flags().Changed("managed-accounts") ||
		cmd.Flags().Changed("no-managed-accounts") ||
		cmd.Flags().Changed("rotation") ||
		cmd.Flags().Changed("no-rotation") ||
		cmd.Flags().Changed("worktree-dir") ||
		cmd.Flags().Changed("default-agent") ||
		cmd.Flags().Changed("poll-interval")

	if !anyChanged {
		fmt.Printf("  Skip permissions: %v\n", config.PluginConfigBool(&s, "claude", "dangerously_skip_permissions"))
		fmt.Printf("  Managed accounts: %v\n", s.ManagedAccounts.ManagedAccountsEnabled())
		fmt.Printf("  Failover proxy:   %v\n", s.ManagedAccounts.FailoverProxyEnabled())
		fmt.Printf("  Worktree dir:     %s\n", s.WorktreeBaseDir)
		fmt.Printf("  Default agent:    %s\n", s.DefaultAgent)
		interval := "30 (default)"
		if s.PollIntervalSeconds > 0 {
			interval = strconv.Itoa(s.PollIntervalSeconds)
		}
		fmt.Printf("  Poll interval:    %s seconds\n", interval)
		return nil
	}

	// Apply changes.
	if cmd.Flags().Changed("skip-permissions") && cmd.Flags().Changed("no-skip-permissions") {
		return fmt.Errorf("cannot use both --skip-permissions and --no-skip-permissions")
	}
	if cmd.Flags().Changed("skip-permissions") {
		config.SetPluginConfigBool(&s, "claude", "dangerously_skip_permissions", true)
	}
	if cmd.Flags().Changed("no-skip-permissions") {
		config.SetPluginConfigBool(&s, "claude", "dangerously_skip_permissions", false)
	}
	enableFlag := cmd.Flags().Changed("managed-accounts") || cmd.Flags().Changed("rotation")
	disableFlag := cmd.Flags().Changed("no-managed-accounts") || cmd.Flags().Changed("no-rotation")
	if enableFlag && disableFlag {
		return fmt.Errorf("cannot both enable and disable managed accounts")
	}
	if enableFlag {
		v := true
		s.ManagedAccounts.Enabled = &v
	}
	if disableFlag {
		v := false
		s.ManagedAccounts.Enabled = &v
	}
	if cmd.Flags().Changed("worktree-dir") {
		v, _ := cmd.Flags().GetString("worktree-dir")
		if v == "" {
			return fmt.Errorf("worktree-dir cannot be empty")
		}
		s.WorktreeBaseDir = v
	}
	if cmd.Flags().Changed("default-agent") {
		v, _ := cmd.Flags().GetString("default-agent")
		if v == "" {
			return fmt.Errorf("default-agent cannot be empty")
		}
		s.DefaultAgent = v
	}
	if cmd.Flags().Changed("poll-interval") {
		v, _ := cmd.Flags().GetInt("poll-interval")
		if v < 0 {
			return fmt.Errorf("poll-interval must be non-negative")
		}
		s.PollIntervalSeconds = v
	}

	if err := config.Save(s); err != nil {
		return fmt.Errorf("save settings: %w", err)
	}

	fmt.Println("Settings updated.")
	return nil
}

// --- Config Init ---

func runConfigInit(cmd *cobra.Command) error {
	pluginDir, _ := cmd.Flags().GetString("plugin-dir")

	var foundPlugins map[string]string // name -> path

	if pluginDir != "" {
		// Explicit --plugin-dir provided: validate and scan it.
		info, err := os.Stat(pluginDir)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("plugin directory not found: %s", pluginDir)
			}
			return fmt.Errorf("cannot access plugin directory: %w", err)
		}
		if !info.IsDir() {
			return fmt.Errorf("plugin-dir must be a directory: %s", pluginDir)
		}

		absPluginDir, err := filepath.Abs(pluginDir)
		if err != nil {
			return fmt.Errorf("resolve plugin directory: %w", err)
		}

		entries, err := os.ReadDir(absPluginDir)
		if err != nil {
			return fmt.Errorf("read plugin directory: %w", err)
		}

		foundPlugins = make(map[string]string)
		for _, e := range entries {
			if e.IsDir() || !strings.HasPrefix(e.Name(), "bossd-plugin-") {
				continue
			}
			foundPlugins[e.Name()] = filepath.Join(absPluginDir, e.Name())
		}
	} else {
		// No --plugin-dir: try auto-discovery relative to binary.
		discovered := config.DiscoverPlugins()
		if len(discovered) > 0 {
			foundPlugins = make(map[string]string)
			for _, p := range discovered {
				foundPlugins["bossd-plugin-"+p.Name] = p.Path
			}
		}
	}

	if len(foundPlugins) == 0 {
		if pluginDir != "" {
			fmt.Fprintf(os.Stderr, "Warning: no plugin binaries found in %s\n", pluginDir)
		} else {
			fmt.Fprintf(os.Stderr, "Warning: no plugin binaries found (use --plugin-dir to specify location)\n")
		}
		return nil
	}

	// Load existing settings
	s, err := config.Load()
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}

	// Create or update plugin entries
	pluginMap := make(map[string]int)
	for i := range s.Plugins {
		pluginMap[s.Plugins[i].Name] = i
	}

	for name, path := range foundPlugins {
		// Extract plugin name from binary name (bossd-plugin-foo -> foo)
		pluginName := strings.TrimPrefix(name, "bossd-plugin-")

		if idx, ok := pluginMap[pluginName]; ok {
			// Update existing entry path and version, but preserve Enabled state
			// so we don't re-enable plugins the user explicitly disabled.
			s.Plugins[idx].Path = path
			s.Plugins[idx].Version = buildinfo.Version
		} else {
			// Add new entry (default to enabled for newly discovered plugins)
			newPlugin := config.PluginConfig{
				Name:    pluginName,
				Path:    path,
				Enabled: true,
				Version: buildinfo.Version,
			}
			s.Plugins = append(s.Plugins, newPlugin)
			pluginMap[pluginName] = len(s.Plugins) - 1
		}
	}

	// Save settings
	if err := config.Save(s); err != nil {
		return fmt.Errorf("save settings: %w", err)
	}

	settingsPath, _ := config.Path()
	fmt.Printf("Configured %d plugins in %s\n", len(foundPlugins), settingsPath)
	return nil
}
