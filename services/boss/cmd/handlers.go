package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"
	"golang.org/x/mod/semver"
	"google.golang.org/protobuf/types/known/timestamppb"

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

// newClient creates either a local or remote client depending on the --remote flag.
// For local connections, it ensures the daemon is running first.
func newClient(cmd *cobra.Command) (client.BossClient, error) {
	remote, _ := cmd.Root().Flags().GetString("remote")
	if remote != "" {
		return newRemoteClient(cmd, remote)
	}
	socketPath, err := client.DefaultSocketPath()
	if err != nil {
		return nil, fmt.Errorf("socket path: %w", err)
	}

	// Skip auto-start when socket is explicitly provided (test mode).
	if os.Getenv("BOSS_SOCKET") == "" {
		if err := daemon.EnsureRunning(socketPath); err != nil {
			return nil, fmt.Errorf("daemon failed: %w\nRun 'boss daemon install' to set up automatic startup, or start bossd manually", err)
		}
	}

	return client.NewLocal(socketPath), nil
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
	if issue := preflight.CheckTmux(); issue != nil {
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
	p := tea.NewProgram(app)
	_, err = p.Run()
	return err
}

type agentPreflightClient interface {
	ResolveContext(ctx context.Context, workingDir string) (*pb.ResolveContextResponse, error)
	GetSession(ctx context.Context, id string) (*pb.Session, error)
	ListAgents(ctx context.Context) ([]client.AgentInfo, error)
}

var checkAgentResolvableForPreflight = preflight.CheckAgentResolvable

var cliBackedAgentProviders = map[string]bool{
	"claude": true,
	"codex":  true,
}

func runAgentPreflights(ctx context.Context, cmd *cobra.Command, c agentPreflightClient, settings config.Settings, sessionID string) error {
	if remoteURL(cmd) != "" {
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
	return remoteURL(cmd) == "" && os.Getenv("BOSS_SOCKET") == ""
}

func agentPreflightTarget(ctx context.Context, c agentPreflightClient, sessionID string) (string, string, error) {
	if sessionID != "" {
		sess, err := c.GetSession(ctx, sessionID)
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

// waitForDaemon decides what to do when the initial newClient call fails.
// For a local socket it shows the auto-recovering daemon-wait screen and
// dials again once the socket is back. For remote/--remote or socket-path
// failures (which don't auto-recover) it falls back to the static preflight
// screen and propagates the original error.
func waitForDaemon(cmd *cobra.Command, origErr error) (client.BossClient, error) {
	issue := *preflight.DaemonIssue(origErr)
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
		waitForSocketGone,
	)
}

func restartReachableDaemonForSettingsReloadWith(
	socketPath string,
	getStatus func() (*daemon.Status, error),
	stop func() error,
	ensureRunning func(string) error,
	terminateStandalone func() (int, error),
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
			return fmt.Errorf("timed out waiting for standalone bossd to stop")
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
		return fmt.Errorf("timed out waiting for daemon socket to stop")
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
	return upgrade.Checker{}.Check(ctx, current)
}

var executablePath = os.Executable

var installUpgrade = func(ctx context.Context, plan upgrade.InstallPlan) error {
	return upgrade.Installer{}.Install(ctx, plan)
}

var brewUpgradeBossanova = func(ctx context.Context, binDir string) (string, error) {
	brew := brewExecutableForBinDir(binDir)
	cmd := exec.CommandContext(ctx, brew, "upgrade", "bossanova-dev/tap/bossanova")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("brew upgrade bossanova-dev/tap/bossanova failed: %w\noutput:\n%s\nRun manually: brew upgrade bossanova-dev/tap/bossanova", err, strings.TrimSpace(string(out)))
	}

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
	return upgrade.VerifyReleaseTag(ctx, nil, "", version)
}

var restartDaemon = daemon.Restart

var runProviderStartupIfNeeded = views.RunProviderStartupIfNeeded

var defaultSocketPath = client.DefaultSocketPath

var daemonSocketReachable = daemon.IsSocketReachable

var daemonGetStatus = daemon.GetStatus

var daemonEnsureRunning = daemon.EnsureRunning

var daemonStop = daemon.Stop

var terminateAllBossdProcesses = terminateBossdProcesses

var waitForDaemonSocketGone = waitForSocketGone

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

		result, err := checkUpgrade(ctx, current)
		if err != nil {
			return fmt.Errorf("check upgrade: %w", err)
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
				if err := restartDaemon(); err != nil {
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
			if err := restartDaemon(); err != nil {
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
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create upgrade lock dir: %w", err)
	}
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
	c, err := newClient(cmd)
	if err != nil {
		return err
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
			return fmt.Errorf("unknown state: %s", s)
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
	sessions, err := c.ListSessions(ctx, req)
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
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
		id := sess.Id
		if len(id) > 8 {
			id = id[:8]
		}
		ids[i] = id
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
		{Title: "ID", Width: views.MaxColWidth("ID", ids, 8)},
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
	return launchTUI(cmd, func(app *views.App) {
		app.SetInitialView(views.ViewNewSession)
		if agentName != "" {
			app.SetInitialAgent(agentName)
		}
	})
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
		id := repo.Id
		if len(id) > 8 {
			id = id[:8]
		}
		ids[i] = id
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
		{Title: "ID", Width: views.MaxColWidth("ID", ids, 8)},
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

	st, _ := daemon.GetStatus()
	fmt.Printf("Daemon installed and started.\n")
	fmt.Printf("  bossd:   %s\n", bossdPath)
	if st != nil && st.ServicePath != "" {
		fmt.Printf("  service: %s\n", st.ServicePath)
	}
	return nil
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
		if n == 0 {
			fmt.Println("No standalone bossd process is running.")
			return nil
		}
		if !waitForDaemonSocketGone(profile.SocketPath) {
			return fmt.Errorf("timed out waiting for daemon socket to stop")
		}
		fmt.Printf("Stopped %d bossd process(es) across all profiles.\n", n)
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
			return fmt.Errorf("timed out waiting for standalone bossd to stop")
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
				return fmt.Errorf("timed out waiting for standalone bossd to stop")
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
		return fmt.Errorf("timed out waiting for daemon socket to stop")
	}
	fmt.Println("Daemon stopped.")
	return nil
}

func runDaemonRestart(_ *cobra.Command) error {
	st, err := daemon.GetStatus()
	if err != nil {
		return fmt.Errorf("daemon restart: %w", err)
	}
	socketPath, err := restartSocketPath(client.DefaultSocketPath())
	if err != nil {
		return err
	}

	if !st.Installed {
		profile, err := currentDaemonProfile()
		if err != nil {
			return fmt.Errorf("daemon restart: %w", err)
		}
		n, err := terminateProfileBossdProcess(profile.AppDataDir, func(pid int) (processSignaler, error) {
			return os.FindProcess(pid)
		})
		if err != nil {
			return fmt.Errorf("restart standalone bossd failed: %w", err)
		}
		if n > 0 && !waitForSocketGone(socketPath) {
			return fmt.Errorf("timed out waiting for standalone bossd to stop")
		}
		if err := daemon.EnsureRunning(socketPath); err != nil {
			return fmt.Errorf("restart standalone bossd failed: %w", err)
		}
		if n > 0 {
			fmt.Println("Restarted standalone bossd for current profile.")
		} else {
			fmt.Println("Started standalone bossd.")
		}
		return nil
	}
	if err := daemon.Restart(); err != nil {
		return fmt.Errorf("restart daemon failed: %w", err)
	}
	// Wait for the new bossd to come up so the next command doesn't race.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if daemon.IsSocketReachable(socketPath) {
			fmt.Println("Daemon restarted.")
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("daemon restarted but socket did not become reachable; check 'boss daemon status'")
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
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !daemon.IsSocketReachable(path) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
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
	fields := strings.Fields(commandLine)
	if len(fields) == 0 {
		return false
	}
	got := filepath.Clean(fields[0])
	want := filepath.Clean(metadata.ExecutablePath)
	return got == want
}

func processCommandLine(pid int) (string, error) {
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

// findBossdPIDs returns PIDs of running processes owned by this effective user
// whose program name is exactly "bossd". Uses pgrep, which is available on
// macOS and Linux.
func findBossdPIDs() ([]int, error) {
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
	sessions, err := c.ListSessions(ctx, &pb.ListSessionsRequest{IncludeArchived: true})
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
		return "", fmt.Errorf("no session found matching prefix %q", prefix)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("ambiguous prefix %q matches %d sessions", prefix, len(matches))
	}
}

// resolveArchivedSessionID is like resolveSessionID but only matches archived sessions.
func resolveArchivedSessionID(c client.BossClient, ctx context.Context, prefix string) (string, error) {
	sessions, err := c.ListSessions(ctx, &pb.ListSessionsRequest{IncludeArchived: true})
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

func runShow(cmd *cobra.Command, sessionID string) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}

	ctx := context.Background()
	sessionID, err = resolveSessionID(c, ctx, sessionID)
	if err != nil {
		return err
	}

	sess, err := c.GetSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}

	// Print key-value header.
	id := sess.Id
	if len(id) > 8 {
		id = id[:8]
	}
	fmt.Printf("  ID:       %s\n", id)
	fmt.Printf("  Title:    %s\n", sess.Title)
	fmt.Printf("  Repo:     %s\n", sess.RepoDisplayName)
	fmt.Printf("  Branch:   %s\n", sess.BranchName)
	fmt.Printf("  State:    %s\n", views.StateLabel(sess.State))
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

	// List chats as a table.
	chats, err := c.ListChats(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("list chats: %w", err)
	}
	if len(chats) == 0 {
		fmt.Println("\n  No chats.")
		return nil
	}

	fmt.Println()
	printChatsTable(cmd, chats)
	return nil
}

func runChats(cmd *cobra.Command, sessionID string) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}

	ctx := context.Background()
	sessionID, err = resolveSessionID(c, ctx, sessionID)
	if err != nil {
		return err
	}

	chats, err := c.ListChats(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("list chats: %w", err)
	}
	if len(chats) == 0 {
		fmt.Println("No chats found.")
		return nil
	}

	printChatsTable(cmd, chats)
	return nil
}

func printChatsTable(cmd *cobra.Command, chats []*pb.ClaudeChat) {
	ids := make([]string, len(chats))
	titles := make([]string, len(chats))
	createds := make([]string, len(chats))
	for i, chat := range chats {
		id := chat.AgentSessionId
		if len(id) > 8 {
			id = id[:8]
		}
		ids[i] = id
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
	}

	cols := []table.Column{
		{Title: "ID", Width: views.MaxColWidth("ID", ids, 8)},
		{Title: "TITLE", Width: views.MaxColWidth("TITLE", titles, 50)},
		{Title: "CREATED", Width: views.MaxColWidth("CREATED", createds, 12)},
	}

	rows := make([]table.Row, len(chats))
	for i := range chats {
		rows[i] = table.Row{ids[i], titles[i], createds[i]}
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
	sessions, err := c.ListSessions(ctx, &pb.ListSessionsRequest{IncludeArchived: true})
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
		id := sess.Id
		if len(id) > 8 {
			id = id[:8]
		}
		ids[i] = id
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
		{Title: "ID", Width: views.MaxColWidth("ID", ids, 8)},
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
		id := sessionID
		if len(id) > 8 {
			id = id[:8]
		}
		fmt.Printf("Permanently delete session %s? [y/N] ", id)
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
		cmd.Flags().Changed("worktree-dir") ||
		cmd.Flags().Changed("default-agent") ||
		cmd.Flags().Changed("poll-interval")

	if !anyChanged {
		fmt.Printf("  Skip permissions: %v\n", config.PluginConfigBool(&s, "claude", "dangerously_skip_permissions"))
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
