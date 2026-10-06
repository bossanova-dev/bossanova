package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/daemon"
	"github.com/recurser/boss/internal/hermes"
	"github.com/recurser/bossalib/bossmcp"
	"github.com/recurser/bossalib/buildinfo"
	"github.com/recurser/bossalib/config"
	libskillinstall "github.com/recurser/bossalib/skillinstall"
)

// hermesHomeEnv is Hermes Agent's own home/profile selector. Both Hermes and
// libskillinstall.AgentHermes read it.
const hermesHomeEnv = "HERMES_HOME"

// hermesCommandTimeout bounds one `hermes` subprocess. Enabling a plugin or
// listing plugins is a config-file edit, so anything slower is a hang.
const hermesCommandTimeout = 30 * time.Second

// hermesRunner runs the hermes CLI at bin with args, adding env to the current
// environment, and returns its combined output.
type hermesRunner func(ctx context.Context, bin string, env []string, args ...string) ([]byte, error)

// Test seams, following the mcp.go convention: every host interaction `boss
// hermes` makes goes through a package var so tests never need Hermes, a
// daemon, a service manager or an MCP binary.
var (
	hermesLookPath                     = exec.LookPath
	hermesRun             hermesRunner = runHermesCommand
	hermesResolveMcpPath               = daemon.ResolveMcpPath
	hermesDaemonStatus                 = daemon.GetStatus
	hermesSocketReachable              = func(socketPath string) bool {
		conn, err := net.DialTimeout("unix", socketPath, 500*time.Millisecond)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}
	hermesToolDefinitions = bossmcp.ToolDefinitions
)

func runHermesCommand(ctx context.Context, bin string, env []string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, hermesCommandTimeout)
	defer cancel()
	c := exec.CommandContext(ctx, bin, args...)
	c.Env = append(os.Environ(), env...)
	c.WaitDelay = 2 * time.Second
	return c.CombinedOutput()
}

// hermesFlags are the explicit selectors shared by `install` and `status`.
type hermesFlags struct {
	settings   string
	socket     string
	hermesHome string
}

func (f *hermesFlags) register(c *cobra.Command) {
	c.Flags().StringVar(&f.settings, "settings", "", "bossanova settings file (default: $BOSS_SETTINGS_PATH, else the OS default)")
	c.Flags().StringVar(&f.socket, "socket", "", "bossd socket (default: $BOSS_SOCKET, else socket_path in the settings file, else the app data dir)")
	c.Flags().StringVar(&f.hermesHome, "hermes-home", "", "Hermes home directory (default: $HERMES_HOME, else ~/.hermes)")
}

// hermesTargets is where one `boss hermes` invocation points: the bossanova
// profile, its daemon, and the Hermes home the plugin goes into.
type hermesTargets struct {
	// SettingsPath is the resolved settings file, always absolute.
	SettingsPath string
	// DefaultProfile is true when SettingsPath is bossanova's OS default, so
	// nothing needs to carry it: boss_install.json omits it and the daemon
	// hint needs no BOSS_SETTINGS_PATH prefix.
	DefaultProfile bool
	Socket         string
	HermesHome     string
}

func (t hermesTargets) pluginDir() string {
	return filepath.Join(t.HermesHome, "plugins", hermes.PluginName)
}

// installSettingsPath is the value boss_install.json carries.
func (t hermesTargets) installSettingsPath() string {
	if t.DefaultProfile {
		return ""
	}
	return t.SettingsPath
}

// resolveHermesTargets applies the documented precedence:
//
//	settings:    --settings > BOSS_SETTINGS_PATH > OS default
//	socket:      --socket > BOSS_SOCKET > socket_path (or app_data_dir) in THAT settings file > <app data dir>/bossd.sock
//	hermes home: --hermes-home > HERMES_HOME (absolute) > ~/.hermes
//
// The socket is read from the chosen settings file, never from the ambient
// profile, so --settings really selects the profile.
func resolveHermesTargets(f hermesFlags) (hermesTargets, error) {
	var t hermesTargets

	switch {
	case f.settings != "":
		if !filepath.IsAbs(f.settings) {
			return t, fmt.Errorf("--settings must be absolute: %q", f.settings)
		}
		t.SettingsPath = filepath.Clean(f.settings)
		t.DefaultProfile = isDefaultSettingsPath(t.SettingsPath)
	case os.Getenv(daemon.ServiceSettingsPathEnv) != "":
		p, err := config.Path()
		if err != nil {
			return t, err
		}
		t.SettingsPath = p
		t.DefaultProfile = isDefaultSettingsPath(p)
	default:
		p, err := config.Path()
		if err != nil {
			return t, err
		}
		t.SettingsPath = p
		// Not unconditionally true: under XDG_CONFIG_HOME the OS default can
		// differ for the Hermes-spawned MCP binary, so the path must be pinned.
		t.DefaultProfile = isDefaultSettingsPath(p)
	}

	switch {
	case f.socket != "":
		p, err := filepath.Abs(f.socket)
		if err != nil {
			return t, fmt.Errorf("resolve --socket: %w", err)
		}
		t.Socket = p
	case os.Getenv("BOSS_SOCKET") != "":
		p, err := filepath.Abs(os.Getenv("BOSS_SOCKET"))
		if err != nil {
			return t, fmt.Errorf("resolve BOSS_SOCKET: %w", err)
		}
		t.Socket = p
	default:
		settings, err := config.LoadFrom(t.SettingsPath)
		if err != nil {
			return t, fmt.Errorf("load settings %s: %w", t.SettingsPath, err)
		}
		p, ok, err := settingsSocketPath(settings)
		if err != nil {
			return t, fmt.Errorf("settings %s: %w", t.SettingsPath, err)
		}
		if !ok {
			dir, err := config.DefaultAppDataDir()
			if err != nil {
				return t, fmt.Errorf("resolve app data dir: %w", err)
			}
			p = filepath.Join(dir, "bossd.sock")
		}
		t.Socket = p
	}

	switch env := os.Getenv(hermesHomeEnv); {
	case f.hermesHome != "":
		p, err := filepath.Abs(f.hermesHome)
		if err != nil {
			return t, fmt.Errorf("resolve --hermes-home: %w", err)
		}
		t.HermesHome = p
	case filepath.IsAbs(env):
		t.HermesHome = filepath.Clean(env)
	default:
		// A relative HERMES_HOME is ignored, as libskillinstall.DirForAgent
		// ignores it, so the plugin and the skills land in the same home.
		home, err := os.UserHomeDir()
		if err != nil {
			return t, fmt.Errorf("resolve home directory: %w", err)
		}
		t.HermesHome = filepath.Join(home, ".hermes")
	}
	return t, nil
}

// isDefaultSettingsPath mirrors the daemon's service-settings rule: an explicit
// path naming the OS default is the default profile, unless XDG_CONFIG_HOME is
// set, because a process without this shell's XDG override (the Hermes-spawned
// MCP binary, say) could derive a different default.
func isDefaultSettingsPath(p string) bool {
	if os.Getenv("XDG_CONFIG_HOME") != "" {
		return false
	}
	dir, err := config.DefaultAppDataDir()
	if err != nil {
		return false
	}
	return filepath.Join(dir, "settings.json") == filepath.Clean(p)
}

// settingsSocketPath is config.ConfiguredSocketPath without its side effect:
// for an app_data_dir-only profile ConfiguredSocketPath creates the directory,
// and resolution here must write nothing (--dry-run, status).
func settingsSocketPath(s config.Settings) (string, bool, error) {
	if s.SocketPath == "" && s.AppDataDir != "" {
		if !filepath.IsAbs(s.AppDataDir) {
			return "", false, fmt.Errorf("app_data_dir must be absolute: %q", s.AppDataDir)
		}
		return filepath.Join(filepath.Clean(s.AppDataDir), "bossd.sock"), true, nil
	}
	return config.ConfiguredSocketPath(s)
}

// hermesMcpBin resolves the MCP binary the plugin spawns, made absolute.
func hermesMcpBin() (string, error) {
	p, err := hermesResolveMcpPath()
	if err != nil {
		return "", err
	}
	return filepath.Abs(p)
}

// renderHermesPlugin renders the plugin file set `install` would write now.
func renderHermesPlugin(ctx context.Context, t hermesTargets, mcpBin string) (map[string][]byte, error) {
	tools, err := hermesToolDefinitions(ctx, bossmcp.Options{})
	if err != nil {
		return nil, fmt.Errorf("list MCP tool definitions: %w", err)
	}
	return hermes.Render(hermes.RenderOptions{
		Version: buildinfo.Version,
		Tools:   tools,
		Install: hermes.InstallConfig{
			McpBin:       mcpBin,
			Socket:       t.Socket,
			SettingsPath: t.installSettingsPath(),
		},
	})
}

// hermesEnableCommand is the command that enables the plugin for home.
func hermesEnableCommand(home string) string {
	return fmt.Sprintf("%s=%s hermes plugins enable %s", hermesHomeEnv, shellQuote(home), hermes.PluginName)
}

// hermesReinstallCommand is the `boss hermes install` remedy carrying the
// resolved targets, so following it repairs the same Hermes home, socket and
// profile that status inspected rather than the ambient defaults.
func hermesReinstallCommand(t hermesTargets) string {
	cmd := "boss hermes install"
	if !t.DefaultProfile {
		cmd += " --settings " + shellQuote(t.SettingsPath)
	}
	return cmd + " --socket " + shellQuote(t.Socket) + " --hermes-home " + shellQuote(t.HermesHome)
}

// hermesDaemonInstallCommand is the per-user login registration remedy,
// carrying the profile when it is not the default.
func hermesDaemonInstallCommand(t hermesTargets) string {
	if t.DefaultProfile {
		return "boss daemon install"
	}
	return fmt.Sprintf("%s=%s boss daemon install", daemon.ServiceSettingsPathEnv, shellQuote(t.SettingsPath))
}

func hermesCmd() *cobra.Command {
	h := &cobra.Command{
		Use:   "hermes",
		Short: "Wire Hermes Agent to the local bossd as a native plugin",
	}

	var installFlags hermesFlags
	var noEnable, force, dryRun bool
	install := &cobra.Command{
		Use:   "install",
		Short: "Install the bossanova Hermes plugin and skills for this OS user",
		Long: "Install the native bossanova Hermes plugin into <hermes home>/plugins/bossanova, " +
			"pointed at this user's bossd socket and settings file, install the boss skills into " +
			"Hermes, and enable the plugin with `hermes plugins enable bossanova` when hermes is on PATH. " +
			"Run it once per OS user; the tools load in the next Hermes session.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runHermesInstall(c, installFlags, hermesInstallOptions{noEnable: noEnable, force: force, dryRun: dryRun})
		},
	}
	installFlags.register(install)
	install.Flags().BoolVar(&noEnable, "no-enable", false, "Do not run `hermes plugins enable bossanova`")
	install.Flags().BoolVar(&force, "force", false, "Replace an existing plugins/bossanova directory that boss did not install")
	install.Flags().BoolVar(&dryRun, "dry-run", false, "Print the resolved values and rendered files; write and run nothing")

	var statusFlags hermesFlags
	var asJSON bool
	status := &cobra.Command{
		Use:   "status",
		Short: "Report whether the installed bossanova Hermes plugin is current",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runHermesStatus(c, statusFlags, asJSON)
		},
	}
	statusFlags.register(status)
	status.Flags().BoolVar(&asJSON, jsonFlagName, false, "Emit the report as JSON")

	h.AddCommand(install, status)
	return h
}

type hermesInstallOptions struct {
	noEnable bool
	force    bool
	dryRun   bool
}

func runHermesInstall(c *cobra.Command, flags hermesFlags, opts hermesInstallOptions) error {
	out := c.OutOrStdout()
	errOut := c.ErrOrStderr()
	ctx := c.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	t, err := resolveHermesTargets(flags)
	if err != nil {
		return err
	}
	mcpBin, err := hermesMcpBin()
	if err != nil {
		return err
	}
	files, err := renderHermesPlugin(ctx, t, mcpBin)
	if err != nil {
		return err
	}
	skillsDir := libskillinstall.HermesSkillsDir(t.HermesHome)
	pluginDir := t.pluginDir()

	if opts.dryRun {
		_, _ = fmt.Fprintln(out, "boss hermes install --dry-run: nothing written, nothing run.")
		printHermesTargets(out, t, mcpBin)
		_, _ = fmt.Fprintf(out, "  plugin:   %s\n", pluginDir)
		_, _ = fmt.Fprintf(out, "  skills:   %s\n", skillsDir)
		_, _ = fmt.Fprintf(out, "  digest:   %s\n", hermes.Digest(files))
		_, _ = fmt.Fprintln(out, "Files:")
		for _, name := range sortedFileNames(files) {
			_, _ = fmt.Fprintf(out, "  %s\n", name)
		}
		if opts.noEnable {
			_, _ = fmt.Fprintln(out, "Would not enable the plugin (--no-enable).")
		} else {
			_, _ = fmt.Fprintf(out, "Would run: %s\n", hermesEnableCommand(t.HermesHome))
		}
		return nil
	}

	if err := writeHermesPlugin(pluginDir, files, opts.force); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "Hermes plugin installed: %s\n", pluginDir)
	printHermesTargets(out, t, mcpBin)

	verb, err := installHermesSkills(skillsDir)
	if err != nil {
		return fmt.Errorf("install Hermes skills into %s: %w", skillsDir, err)
	}
	_, _ = fmt.Fprintf(out, "Hermes skills %s: %s\n", verb, skillsDir)

	var enableErr error
	switch hermesBin, lookErr := hermesLookPath("hermes"); {
	case opts.noEnable:
		_, _ = fmt.Fprintf(out, "Not enabling the plugin (--no-enable). Enable it with:\n  %s\n", hermesEnableCommand(t.HermesHome))
	case lookErr != nil:
		_, _ = fmt.Fprintf(out, "hermes is not on PATH; the plugin is in place. Enable it with:\n  %s\n", hermesEnableCommand(t.HermesHome))
	default:
		env := []string{hermesHomeEnv + "=" + t.HermesHome}
		if output, err := hermesRun(ctx, hermesBin, env, "plugins", "enable", hermes.PluginName); err != nil {
			enableErr = fmt.Errorf("hermes plugins enable %s: %w\n%s", hermes.PluginName, err, strings.TrimSpace(string(output)))
		} else {
			_, _ = fmt.Fprintf(out, "Enabled the %s plugin in Hermes.\n", hermes.PluginName)
		}
	}

	warnHermesDaemon(errOut, t)
	_, _ = fmt.Fprintln(out, "The bossanova tools load in the next Hermes session; check them there with /bossanova status.")
	return enableErr
}

func printHermesTargets(out io.Writer, t hermesTargets, mcpBin string) {
	settings := t.SettingsPath
	if t.DefaultProfile {
		settings += " (default)"
	}
	_, _ = fmt.Fprintf(out, "  settings: %s\n", settings)
	_, _ = fmt.Fprintf(out, "  socket:   %s\n", t.Socket)
	_, _ = fmt.Fprintf(out, "  hermes:   %s\n", t.HermesHome)
	_, _ = fmt.Fprintf(out, "  mcp:      %s\n", mcpBin)
}

// warnHermesDaemon warns, never fails, when the plugin has no daemon to talk
// to: an unreachable socket, or no per-user login registration.
func warnHermesDaemon(errOut io.Writer, t hermesTargets) {
	warned := false
	if !hermesSocketReachable(t.Socket) {
		_, _ = fmt.Fprintf(errOut, "Warning: bossd is not reachable at %s.\n", t.Socket)
		warned = true
	}
	// A status error says nothing either way, so it warns about nothing.
	if st, err := hermesDaemonStatus(); err == nil && st != nil && !st.Installed {
		_, _ = fmt.Fprintln(errOut, "Warning: no bossd login registration is installed for this user.")
		warned = true
	}
	if warned {
		_, _ = fmt.Fprintf(errOut, "Register and start the daemon with:\n  %s\n", hermesDaemonInstallCommand(t))
	}
}

func installHermesSkills(dir string) (string, error) {
	payload, err := skillPayload()
	if err != nil {
		return "", fmt.Errorf("select skill payload: %w", err)
	}
	record := skillPayloadRecord(payload, libskillinstall.WriterExplicit)
	if !libskillinstall.IsInstalled(dir) {
		if err := libskillinstall.ExtractRecorded(dir, payload.fsys, record); err != nil {
			return "", err
		}
		return "installed", nil
	}
	updated, err := libskillinstall.EnsureUpdatedRecorded(dir, payload.fsys, record)
	if err != nil {
		return "", err
	}
	if updated {
		return "updated", nil
	}
	return "up to date", nil
}

// errHermesPluginUnmanaged refuses to clobber a plugin directory boss did not
// write.
var errHermesPluginUnmanaged = errors.New("not installed by boss")

// writeHermesPlugin writes files into dir atomically: the set is written into
// a temporary sibling, then renamed into place. An existing dir without
// boss_install.json (a hand-made plugin of the same name) is refused unless
// force is set.
func writeHermesPlugin(dir string, files map[string][]byte, force bool) error {
	if info, err := os.Lstat(dir); err == nil {
		managed := false
		if info.IsDir() {
			if _, err := os.Stat(filepath.Join(dir, hermes.InstallFile)); err == nil {
				managed = true
			}
		}
		if !managed && !force {
			return fmt.Errorf("%s exists but has no %s (%w); re-run with --force to replace it",
				dir, hermes.InstallFile, errHermesPluginUnmanaged)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("inspect %s: %w", dir, err)
	}

	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", parent, err)
	}
	tmp, err := os.MkdirTemp(parent, "."+filepath.Base(dir)+".tmp-")
	if err != nil {
		return fmt.Errorf("create staging dir: %w", err)
	}
	keepTmp := false
	defer func() {
		if !keepTmp {
			_ = os.RemoveAll(tmp)
		}
	}()
	if err := os.Chmod(tmp, 0o755); err != nil {
		return fmt.Errorf("chmod staging dir: %w", err)
	}
	for _, name := range sortedFileNames(files) {
		p := filepath.Join(tmp, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, files[name], 0o644); err != nil {
			return fmt.Errorf("write %s: %w", p, err)
		}
	}

	// Move any existing install aside rather than deleting it first, so a
	// failed swap can put it back.
	var old string
	if _, err := os.Lstat(dir); err == nil {
		old = filepath.Join(parent, "."+filepath.Base(dir)+".old-"+filepath.Base(tmp))
		if err := os.Rename(dir, old); err != nil {
			return fmt.Errorf("move existing %s aside: %w", dir, err)
		}
	}
	if err := os.Rename(tmp, dir); err != nil {
		if old != "" {
			_ = os.Rename(old, dir)
		}
		return fmt.Errorf("move plugin into %s: %w", dir, err)
	}
	keepTmp = true
	if old != "" {
		_ = os.RemoveAll(old)
	}
	return nil
}

func sortedFileNames(files map[string][]byte) []string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Plugin states `boss hermes status` reports.
const (
	hermesPluginCurrent   = "current"
	hermesPluginStale     = "stale"
	hermesPluginMissing   = "missing"
	hermesPluginUnmanaged = "unmanaged"
	hermesStateUnknown    = "unknown"
)

// hermesStatusReport is `boss hermes status --json`.
type hermesStatusReport struct {
	SettingsPath    string `json:"settings_path"`
	DefaultProfile  bool   `json:"default_profile"`
	Socket          string `json:"socket_path"`
	HermesHome      string `json:"hermes_home"`
	DaemonReachable bool   `json:"daemon_reachable"`
	McpBin          string `json:"mcp_bin,omitempty"`
	McpBinError     string `json:"mcp_bin_error,omitempty"`
	PluginDir       string `json:"plugin_dir"`
	// Plugin is current, stale, missing, unmanaged (no boss_install.json) or
	// unknown (the expected set could not be rendered).
	Plugin string `json:"plugin"`
	// Enabled is enabled, disabled, not-listed, or unknown (hermes absent or
	// its listing failed).
	Enabled   string `json:"enabled"`
	SkillsDir string `json:"skills_dir"`
	// Skills is current, stale, missing or unknown.
	Skills string `json:"skills"`
}

func runHermesStatus(c *cobra.Command, flags hermesFlags, asJSON bool) error {
	ctx := c.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	t, err := resolveHermesTargets(flags)
	if err != nil {
		return err
	}
	r := hermesStatusReport{
		SettingsPath:    t.SettingsPath,
		DefaultProfile:  t.DefaultProfile,
		Socket:          t.Socket,
		HermesHome:      t.HermesHome,
		DaemonReachable: hermesSocketReachable(t.Socket),
		PluginDir:       t.pluginDir(),
		Plugin:          hermesStateUnknown,
		Enabled:         hermesStateUnknown,
		Skills:          hermesStateUnknown,
	}

	mcpBin, mcpErr := hermesMcpBin()
	if mcpErr != nil {
		r.McpBinError = mcpErr.Error()
	} else {
		r.McpBin = mcpBin
	}
	r.Plugin = hermesPluginState(ctx, t, mcpBin, mcpErr)
	r.Enabled = hermesEnabledState(ctx, t.HermesHome)
	r.SkillsDir = libskillinstall.HermesSkillsDir(t.HermesHome)
	r.Skills = hermesSkillsState(r.SkillsDir)

	if asJSON {
		return emitJSON(c, r)
	}
	printHermesStatus(c.OutOrStdout(), t, r)
	return nil
}

func hermesPluginState(ctx context.Context, t hermesTargets, mcpBin string, mcpErr error) string {
	dir := t.pluginDir()
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return hermesPluginMissing
	}
	if _, err := os.Stat(filepath.Join(dir, hermes.InstallFile)); err != nil {
		return hermesPluginUnmanaged
	}
	if mcpErr != nil {
		return hermesStateUnknown
	}
	want, err := renderHermesPlugin(ctx, t, mcpBin)
	if err != nil {
		return hermesStateUnknown
	}
	// Compare only the rendered paths: Hermes importing the plugin leaves a
	// __pycache__ behind, which is not drift.
	got := make(map[string][]byte, len(want))
	for name := range want {
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			return hermesPluginStale
		}
		got[name] = data
	}
	if hermes.Digest(got) != hermes.Digest(want) {
		return hermesPluginStale
	}
	return hermesPluginCurrent
}

// hermesEnabledState asks Hermes whether the plugin is enabled, by reading the
// bossanova line of `hermes plugins list`.
func hermesEnabledState(ctx context.Context, home string) string {
	bin, err := hermesLookPath("hermes")
	if err != nil {
		return hermesStateUnknown
	}
	output, err := hermesRun(ctx, bin, []string{hermesHomeEnv + "=" + home}, "plugins", "list")
	if err != nil {
		return hermesStateUnknown
	}
	for _, line := range strings.Split(string(output), "\n") {
		lower := strings.ToLower(line)
		if !strings.Contains(lower, hermes.PluginName) {
			continue
		}
		switch {
		case strings.Contains(lower, "disabled"), strings.Contains(lower, "not enabled"):
			return "disabled"
		case strings.Contains(lower, "enabled"):
			return "enabled"
		}
		return hermesStateUnknown
	}
	return "not-listed"
}

func hermesSkillsState(dir string) string {
	if !libskillinstall.IsInstalled(dir) {
		return hermesPluginMissing
	}
	// The payload `install` would write, so a fresh install reads current.
	payload, err := skillPayload()
	if err != nil {
		return hermesStateUnknown
	}
	stale, err := libskillinstall.NeedsUpdate(dir, payload.fsys)
	if err != nil {
		return hermesStateUnknown
	}
	if stale {
		return hermesPluginStale
	}
	return hermesPluginCurrent
}

func printHermesStatus(out io.Writer, t hermesTargets, r hermesStatusReport) {
	settings := r.SettingsPath
	if r.DefaultProfile {
		settings += " (default)"
	}
	reach := "reachable"
	if !r.DaemonReachable {
		reach = "NOT reachable — " + hermesDaemonInstallCommand(t)
	}
	mcp := r.McpBin
	if r.McpBinError != "" {
		mcp = "not found (" + r.McpBinError + ")"
	}
	_, _ = fmt.Fprintf(out, "settings: %s\n", settings)
	_, _ = fmt.Fprintf(out, "socket:   %s (%s)\n", r.Socket, reach)
	_, _ = fmt.Fprintf(out, "hermes:   %s\n", r.HermesHome)
	_, _ = fmt.Fprintf(out, "mcp:      %s\n", mcp)
	_, _ = fmt.Fprintf(out, "plugin:   %s (%s)\n", r.PluginDir, r.Plugin)
	_, _ = fmt.Fprintf(out, "enabled:  %s\n", r.Enabled)
	_, _ = fmt.Fprintf(out, "skills:   %s (%s)\n", r.SkillsDir, r.Skills)

	switch r.Plugin {
	case hermesPluginMissing, hermesPluginStale:
		_, _ = fmt.Fprintf(out, "Run `%s` to (re)install the plugin.\n", hermesReinstallCommand(t))
	case hermesPluginUnmanaged:
		_, _ = fmt.Fprintf(out, "plugins/bossanova was not installed by boss; `%s --force` replaces it.\n", hermesReinstallCommand(t))
	}
	if r.Skills == hermesPluginMissing || r.Skills == hermesPluginStale {
		_, _ = fmt.Fprintf(out, "Run `%s` to refresh the Hermes skills.\n", hermesReinstallCommand(t))
	}
	if r.Enabled == "disabled" || r.Enabled == "not-listed" {
		_, _ = fmt.Fprintf(out, "Enable it with: %s\n", hermesEnableCommand(t.HermesHome))
	}
	_, _ = fmt.Fprintf(out, "To turn it off: %s=%s hermes plugins disable %s\n", hermesHomeEnv, shellQuote(t.HermesHome), hermes.PluginName)
}
