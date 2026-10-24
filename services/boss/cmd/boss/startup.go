package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/recurser/boss/internal/adapter/cloudrpc"

	"github.com/recurser/boss/internal/domain/rpc"

	"github.com/recurser/boss/internal/adapter/sshhost"

	"github.com/recurser/boss/api/tui/home"

	"github.com/recurser/boss/api/tui/repos"

	"github.com/recurser/boss/api/cli/cliutil"

	tea "charm.land/bubbletea/v2"
	settingscli "github.com/recurser/boss/api/cli/settings"
	daemonrpc "github.com/recurser/boss/internal/adapter/daemonrpc"
	"github.com/spf13/cobra"

	"github.com/recurser/boss/api/tui"
	"github.com/recurser/boss/api/tui/login"
	daemon "github.com/recurser/boss/internal/daemonlife"
	"github.com/recurser/boss/internal/preflight"
	"github.com/recurser/bossalib/config"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// newClient picks the transport: the cloud orchestrator (--remote), a bossd on
// another machine over an ssh tunnel (--host), or the local daemon. Only the
// local branch may start a daemon.
func newDaemonClient(cmd *cobra.Command) (*daemonrpc.LocalClient, error) {
	if host := hostDestination(cmd); host != "" {
		return newHostClient(cmd, host)
	}
	socketPath, err := daemonrpc.DefaultSocketPath()
	if err != nil {
		return nil, fmt.Errorf("socket path: %w", err)
	}

	// Skip auto-start when socket is explicitly provided (test mode). Routed
	// through the package var so a test can assert the --host branch never
	// reaches it.
	if os.Getenv("BOSS_SOCKET") == "" {
		if err := daemonEnsureRunning(socketPath); err != nil {
			return nil, fmt.Errorf("daemon failed to start: %w\nRun 'boss daemon install' to set up automatic startup, or start bossd manually", err)
		}
	}

	return daemonrpc.NewLocal(socketPath), nil
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
	// The elision is inside a shell command in a code span, not prose: ellipsis: literal-dots ok
	return fmt.Errorf("boss daemon commands manage the local bossd and cannot target a remote (--host %s) daemon; run `ssh %s boss daemon ...` on that host instead", host, host)
}

// newRemoteClient creates a RemoteClient with a JWT from the keychain.
func newRemoteClient(cmd *cobra.Command, baseURL string) (*cloudrpc.RemoteClient, error) {
	mgr, err := settingscli.NewAuthManager(cmd)
	if err != nil {
		return nil, fmt.Errorf("auth: %w (run 'boss login' first)", err)
	}
	ctx := context.Background()
	token, err := mgr.AccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("access token: %w (run 'boss login' first)", err)
	}
	c := cloudrpc.NewRemote(baseURL, token)
	if err := settingscli.RequireActiveCloudAccess(ctx, c); err != nil {
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
func launchTUI(cmd *cobra.Command, configure func(*tui.App)) error {
	return launchTUIWithOptions(cmd, launchTUIOptions{configure: configure})
}

type launchTUIOptions struct {
	configure       func(*tui.App)
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
	// that enable the same input-reporting modes, and home.RunDaemonWait is an
	// explicitly indefinite unattended wait — precisely the walked-away-and-the-
	// SSH-dropped case this ticket is about. The tmux options are not known yet,
	// so the cleanup reads them from tmuxRestore, which is filled in below; the
	// handler itself stays armed from here to teardown. See BOS-650.
	var tmuxRestore atomic.Pointer[func()]
	defer installHangupRescueFn(hangupCleanup(&tmuxRestore))()
	if needsLocalTmux(cmd) {
		if issue := preflight.CheckTmux(); issue != nil {
			return home.RunPreflight(*issue)
		}
	}
	if issue := preflight.CheckTerminal(); issue != nil {
		return home.RunPreflight(*issue)
	}
	if needsLocalDaemonStartup(cmd) {
		if issue := preflight.CheckShellTools(); issue != nil {
			return home.RunPreflight(*issue)
		}
		if err := runLocalProviderStartupBeforeClient(); err != nil {
			if home.IsPreflightCancelled(err) {
				return nil
			}
			return err
		}
	}
	// e2e-only: stage the --host tunnel-dropped screen before the client dials,
	// so a proof scenario can capture the reconnecting state on a harness with no
	// network. Compiles to an unconditional nil in production builds.
	if err := sshhost.RunE2EReconnectSeed(); err != nil {
		if home.IsPreflightCancelled(err) {
			return nil
		}
		return err
	}
	// e2e-only: put the TUI into a --host remote context without a tunnel, so a
	// proof scenario can capture the views that degrade under --host. Compiles to
	// an empty function in production builds.
	sshhost.ApplyE2EAttachSeed()
	c, err := newTUIClient(cmd)
	if err != nil {
		c, err = handleClientStartupError(cmd, err, func() (startupClient, error) {
			return waitForDaemon(cmd, err)
		})
		if err != nil {
			if home.IsPreflightCancelled(err) {
				return nil
			}
			return err
		}
	}
	// e2e-only: make the home list read as a cross-organization union, so a
	// proof scenario can capture the union and the one-organization-short
	// partial read without a cloud backend. Returns the client unchanged in
	// production builds and in every scenario that asked for no seed.
	c = ApplyE2ECrossOrganizationSessions(c)
	authMgr := settingscli.NewOptionalAuthManager(cmd)
	settings := launchSettings(time.Now())
	if err := runAgentPreflights(context.Background(), cmd, c, settings, opts.attachSessionID); err != nil {
		if home.IsPreflightCancelled(err) {
			return nil
		}
		return err
	}
	app := tui.NewApp(c, authMgr)
	app.WithSettings(settings)
	// BOS-1520: the stderr unsupervised-daemon warning is printed before the
	// alt screen takes over, so the TUI carries its own health line. nil under
	// --remote/--host/BOSS_SOCKET, where Home stays silent.
	app.WithDaemonSupervisionProbe(tuiDaemonSupervisionProbe(cmd))
	if settingscli.ResolveE2ELoginEmail() != "" || settingscli.ResolveE2ECloudAccessClient() != nil {
		tui.DisableExternalBrowserForE2E()
	}
	if interval := settingscli.E2ECloudRefreshInterval(); interval > 0 {
		login.SetSubscriptionPollIntervalOverride(interval)
		repos.SetGitHubAppInstallPollIntervalOverride(interval)
	}
	app.WithTelemetry(commandTelemetryClient(cmd))
	if authMgr != nil {
		if cloud := cliutil.CloudURL(cmd); cloud != "" {
			app.WithCloudAccessClient(settingscli.NewAuthCloudAccessClient(authMgr, cloud))
		}
		app.WithCheckoutURLs(settingscli.CloudReturnURL(), settingscli.CloudCancelURL())
		// Unscoped on purpose: the cloud client is only being constructed here, so
		// no CloudAccessStatus has been read and there is no organization id to
		// scope the URL to yet.
		app.WithSubscriptionURL(settingscli.CloudSubscribeURL(""))
	}
	if opts.configure != nil {
		opts.configure(&app)
	}
	// Under tmux, enable the session options boss needs to deliver desktop
	// notifications (allow-passthrough) and receive focus events (focus-events),
	// restoring them on exit. No-op outside tmux or when notifications are off.
	restoreTmux := home.EnableTmuxNotificationOptions(config.NotificationsEnabled(settings))
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
	GetSession(ctx context.Context, id string, opts rpc.SessionReadOptions) (*pb.Session, error)
	ListAgents(ctx context.Context) ([]rpc.AgentInfo, error)
}

// checkAgentsResolvableForPreflight probes every enabled agent under a single
// startup budget — see preflight.CheckAgentsResolvable, which runs them
// concurrently precisely because this call blocks boss's first frame.
var checkAgentsResolvableForPreflight = preflight.CheckAgentsResolvable

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
	agents := enabledAgentProviders(settings, loadedAgents, agentName)
	if issue := checkAgentsResolvableForPreflight(settings.LoginShell, agents, worktree); issue != nil {
		return home.RunPreflight(*issue)
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
// attach reaches it as `ssh -t <destination> tmux ...` (tui.buildAttachCommand),
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
		sess, err := c.GetSession(ctx, sessionID, rpc.SessionReadOptions{})
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

func enabledAgentProviders(settings config.Settings, loadedAgents []rpc.AgentInfo, agentName string) []string {
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

func handleClientStartupError(cmd *cobra.Command, err error, wait func() (startupClient, error)) (startupClient, error) {
	if settingscli.IsCloudGateError(err) {
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

// waitForDaemon decides what to do when the initial newClient call fails.
// For a local socket it shows the auto-recovering daemon-wait screen and
// dials again once the socket is back. For remote/--remote or socket-path
// failures (which don't auto-recover) it falls back to the static preflight
// screen and propagates the original error.
//
// A --host failure gets its own auto-recovering screen: the supervised ssh
// tunnel redials on its own, so killing it mid-session must not kill boss.
func waitForDaemon(cmd *cobra.Command, origErr error) (*daemonrpc.LocalClient, error) {
	issue := *preflight.DaemonIssue(origErr)
	if host := hostDestination(cmd); host != "" {
		return sshhost.WaitForDaemon(host, origErr)
	}
	remote, _ := cmd.Root().Flags().GetString("remote")
	if remote != "" {
		return nil, home.RunPreflight(issue)
	}
	socketPath, pathErr := daemonrpc.DefaultSocketPath()
	if pathErr != nil {
		return nil, home.RunPreflight(issue)
	}
	check := func() bool { return daemon.IsSocketReachable(socketPath) }
	if err := home.RunDaemonWait(issue, check); err != nil {
		return nil, err
	}
	return daemonrpc.NewLocal(socketPath), nil
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

var runProviderStartupIfNeeded = tui.RunProviderStartupIfNeeded

var loadSettings = config.Load

var saveSettings = config.Save

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

// --- Daemon Management ---

var daemonEnsureRunning = daemon.EnsureRunning

var defaultSocketPath = daemonrpc.DefaultSocketPath
var daemonSocketReachable = daemon.IsSocketReachable

// requireSingleTransport rejects --host together with --remote. They select
// different daemons entirely — an ssh tunnel to another machine's bossd versus
// the cloud orchestrator — so silently preferring one would drive the wrong
// machine. It runs at the top of the root's PersistentPreRunE, before any dial.
func requireSingleTransport(cmd *cobra.Command) error {
	if hostDestination(cmd) != "" && remoteURL(cmd) != "" {
		return errors.New("--host and --remote select different daemons and cannot be combined; pass only one")
	}
	return nil
}

var newHostClient = sshhost.NewClient

// newTUIClient resolves the transport for the router's screen contracts.
func newTUIClient(cmd *cobra.Command) (startupClient, error) {
	if remote := remoteURL(cmd); remote != "" {
		return newRemoteClient(cmd, remote)
	}
	return newDaemonClient(cmd)
}

// startupClient is the router contract plus the probes owned by startup wiring.
type startupClient interface {
	tui.Client
	agentPreflightClient
}
