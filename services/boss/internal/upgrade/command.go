package upgrade

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/mod/semver"

	"github.com/recurser/bossalib/contract/daemon/daemonbin"
	"github.com/recurser/bossalib/kit/buildinfo"
	"github.com/recurser/bossalib/kit/config"
)

// checkUpgradeExplicitly always queries GitHub because the user asked: a cached
// "up to date" can predate a release by up to CacheTTL. A successful check
// refreshes the shared cache, preserving any snooze. Only a rate-limited check
// falls back to a fresh cache entry, and it says so. Snooze state never
// suppresses the result. ok=false means there is no result to act on.
func (g *Runner) checkUpgradeExplicitly(ctx context.Context, out io.Writer, current string) (CheckResult, bool, error) {
	path := g.upgradeActionCachePath()
	now := time.Now()
	result, err := g.checkUpgrade(ctx, current)
	if err == nil {
		if path != "" {
			_, _ = RecordCheck(path, result, now)
		}
		return result, true, nil
	}
	var rlErr *RateLimitError
	if !errors.As(err, &rlErr) {
		return CheckResult{}, false, fmt.Errorf("check upgrade: %w", err)
	}
	_, _ = fmt.Fprintf(out, "cannot check for upgrades: %s\n", rlErr.Error())
	if path == "" {
		return CheckResult{}, false, nil
	}
	entry, ok, readErr := ReadFreshCache(path, current, now, CacheTTL)
	if readErr != nil || !ok {
		return CheckResult{}, false, nil
	}
	_, _ = fmt.Fprintf(out, "using the result cached at %s\n", entry.CheckedAt.Local().Format("2006-01-02 15:04 MST"))
	return CheckResult{
		CurrentVersion: entry.CurrentVersion,
		LatestVersion:  entry.LatestVersion,
		ReleaseURL:     entry.ReleaseURL,
		Available:      CompareStableVersions(entry.CurrentVersion, entry.LatestVersion) == CompareOlder,
	}, true, nil
}

var installedPluginNames = []string{
	"claude",
	"codex",
	"dependabot",
	"linear",
	"repair",
	"sentry",
}

func (g *Runner) currentExecutableDir() (string, error) {
	path, err := g.executablePath()
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

func (g *Runner) upgradePluginDir(goos, binDir string) (string, error) {
	settings, err := g.loadSettings()
	if err != nil {
		return "", fmt.Errorf("load settings: %w", err)
	}
	homebrewPluginDir, homebrewInstall := homebrewPluginDirForBinDir(binDir)
	if dir, ok, err := commonEnabledPluginDir(settings.Plugins, homebrewPluginDir); err != nil {
		return "", err
	} else if ok {
		return dir, nil
	}
	if dir, ok, err := commonEnabledPluginDir(g.discoverPlugins(), homebrewPluginDir); err != nil {
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

// homebrewPrefixesForBinDir delegates to daemonbin so the Cellar shape has a
// single definition shared with the staleness classifier (BOS-864). This
// helper shares that definition with upgrade installation.
func homebrewPrefixesForBinDir(binDir string) (string, string, bool) {
	return daemonbin.HomebrewCellarPrefixes(binDir)
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

func (g *Runner) Run(cmd *cobra.Command, opts Options) error {
	ctx := cmd.Context()
	current := g.upgradeCurrentVersion()

	targetVersion := ""
	if opts.Version != "" {
		version, ok, dev := NormalizeVersion(opts.Version)
		if dev || !ok {
			return fmt.Errorf("invalid upgrade version %q", opts.Version)
		}
		if semver.Prerelease(version) != "" {
			return fmt.Errorf("prerelease upgrade version %q requires an explicit prerelease channel", opts.Version)
		}
		targetVersion = version
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "upgrade target: %s\n", targetVersion)
		if opts.CheckOnly {
			if err := g.verifyUpgradeVersion(ctx, targetVersion); err != nil {
				return fmt.Errorf("verify upgrade version: %w", err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "release %s exists on GitHub\n", targetVersion)
			return nil
		}
	} else {
		if _, ok, dev := NormalizeVersion(current); dev || !ok {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "boss upgrade checks require a stable release version (current: %s)\n", current)
			return nil
		}

		result, ok, err := g.checkUpgradeExplicitly(ctx, cmd.OutOrStdout(), current)
		if err != nil || !ok {
			return err
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
	binDir, err := g.currentExecutableDir()
	if err != nil {
		return err
	}
	if isHomebrewCellarBinDir(binDir) {
		if opts.Version != "" {
			return fmt.Errorf("homebrew installs upgrade through the tap; exact --version installs are not supported. Run manually: brew upgrade bossanova-dev/tap/bossanova")
		}
		return g.withUpgradeLock(func() error {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "upgrading bossanova with Homebrew")
			pluginDir, err := g.brewUpgradeBossanova(ctx, binDir)
			if err != nil {
				return err
			}
			if pluginDir != "" {
				if err := g.persistInstalledPluginPaths(pluginDir); err != nil {
					return fmt.Errorf("persist plugin settings: %w", err)
				}
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Homebrew upgrade completed")
			if opts.NoRestart {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "daemon restart skipped (--no-restart)")
			} else {
				upgradedBoss := filepath.Join(filepath.Dir(filepath.Dir(pluginDir)), "bin", "boss")
				if err := g.restartHomebrewDaemon(ctx, upgradedBoss); err != nil {
					return fmt.Errorf("restart daemon: %w", err)
				}
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "daemon restarted")
			}
			return nil
		})
	}
	pluginDir, err := g.upgradePluginDir(runtime.GOOS, binDir)
	if err != nil {
		return err
	}
	plan := InstallPlan{
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
	return g.withUpgradeLock(func() error {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "installing %s assets into %s\n", targetVersion, plan.BinDir)
		if err := g.installUpgrade(ctx, plan); err != nil {
			return fmt.Errorf("install upgrade: %w", err)
		}
		if err := g.persistInstalledPluginPaths(plan.PluginDir); err != nil {
			return fmt.Errorf("persist plugin settings: %w", err)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "upgrade installed %s\n", targetVersion)
		if opts.NoRestart {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "daemon restart skipped (--no-restart)")
		} else {
			if err := g.restartDaemonAfterUpgrade(); err != nil {
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

func (g *Runner) withUpgradeLock(fn func() error) error {
	path, err := g.upgradeLockPath()
	if err != nil {
		return fmt.Errorf("upgrade lock path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create upgrade lock dir: %w", err)
	}
	// #nosec G304 -- internally-derived upgrade-lock path (g.upgradeLockPath); O_EXCL single-writer lock, no operator input.
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

func (g *Runner) persistInstalledPluginPaths(pluginDir string) error {
	settings, err := g.loadSettings()
	if err != nil {
		return err
	}
	rewriteInstalledPluginPaths(&settings, pluginDir)
	return g.saveSettings(settings)
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

// Options selects the explicit upgrade action.
type Options struct {
	CheckOnly bool
	Yes       bool
	Version   string
	NoRestart bool
}

// Runner owns the upgrade dependencies for a single command group.
type Runner struct {
	upgradeCurrentVersion     func() string
	checkUpgrade              func(context.Context, string) (CheckResult, error)
	upgradeActionCachePath    func() string
	executablePath            func() (string, error)
	installUpgrade            func(context.Context, InstallPlan) error
	brewUpgradeBossanova      func(context.Context, string) (string, error)
	verifyUpgradeVersion      func(context.Context, string) error
	restartHomebrewDaemon     func(context.Context, string) error
	discoverPlugins           func() []config.PluginConfig
	upgradeLockPath           func() (string, error)
	loadSettings              func() (config.Settings, error)
	saveSettings              func(config.Settings) error
	restartDaemonAfterUpgrade func() error
}

// NewRunner constructs the upgrade action with an injected daemon restart.
func NewRunner(restart func() error) *Runner {
	g := &Runner{}
	g.upgradeCurrentVersion = func() string {
		return buildinfo.Version
	}
	g.checkUpgrade = func(ctx context.Context, current string) (CheckResult, error) {
		return Checker{Token: ResolveGitHubToken(ctx)}.Check(ctx, current)
	}
	g.upgradeActionCachePath = DefaultCachePath
	g.executablePath = os.Executable
	g.installUpgrade = func(ctx context.Context, plan InstallPlan) error {
		return Installer{}.Install(ctx, plan)
	}
	g.brewUpgradeBossanova = func(ctx context.Context, binDir string) (string, error) {
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
	g.verifyUpgradeVersion = func(ctx context.Context, version string) error {
		return VerifyReleaseTag(ctx, nil, "", version, ResolveGitHubToken(ctx))
	}
	g.restartHomebrewDaemon = func(ctx context.Context, executable string) error {
		if ctx == nil {
			ctx = context.Background()
		}
		cmd := exec.CommandContext(ctx, executable, "daemon", "restart")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("restart daemon with upgraded boss: %w\noutput:\n%s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	g.discoverPlugins = config.DiscoverPlugins
	g.upgradeLockPath = defaultUpgradeLockPath
	g.loadSettings = config.Load
	g.saveSettings = config.Save
	g.restartDaemonAfterUpgrade = restart
	return g
}
