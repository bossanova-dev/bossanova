package settings

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/recurser/bossalib/kit/buildinfo"
	"github.com/recurser/bossalib/kit/config"
	"github.com/spf13/cobra"
)

func (g *Group) SettingsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "settings",
		Short: "View or update global settings",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSettings(cmd)
		},
	}
	cmd.Flags().Bool("skip-permissions", false, "Enable Claude --dangerously-skip-permissions")
	cmd.Flags().Bool("no-skip-permissions", false, "Disable Claude --dangerously-skip-permissions")
	cmd.Flags().Bool("managed-accounts", false, "Enable managed accounts (bossd credential rotation)")
	cmd.Flags().Bool("no-managed-accounts", false, "Disable managed accounts (use the terminal's own login)")
	// Back-compat hidden aliases.
	cmd.Flags().Bool("rotation", false, "Deprecated alias for --managed-accounts")
	cmd.Flags().Bool("no-rotation", false, "Deprecated alias for --no-managed-accounts")
	_ = cmd.Flags().MarkHidden("rotation")
	_ = cmd.Flags().MarkHidden("no-rotation")
	cmd.Flags().String("worktree-dir", "", "Set worktree base directory")
	cmd.Flags().String("default-agent", "", "Set the default agent plugin (e.g. claude, opencode)")
	cmd.Flags().Int("poll-interval", 0, "Set poll interval in seconds (0 = default)")
	cmd.Flags().String("daemon-name", "", "Set the daemon display name (blank resets to the machine hostname; restart the daemon to apply)")
	return cmd
}
func (g *Group) ConfigCmd() *cobra.Command {
	cfg := &cobra.Command{
		Use:   "config",
		Short: "Manage configuration",
	}

	init := &cobra.Command{
		Use:   "init",
		Short: "Initialize bossd plugin settings in settings.json from a plugin directory",
		Long: "Initialize bossd plugin settings in settings.json from a plugin directory.\n\n" +
			"Unrelated to `boss init`, which writes the .boss-skills.json the boss skills read.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfigInit(cmd)
		},
	}
	init.Flags().String("plugin-dir", "", "Directory containing plugin binaries (auto-detected if omitted)")

	cfg.AddCommand(init)
	return cfg
}
func settingsDaemonNameLine(s config.Settings, hostname string) string {
	daemonName := config.DaemonDisplayName(s, hostname)
	if strings.TrimSpace(daemonName) == "" {
		// os.Hostname() failed and no override is set. A bare blank here
		// reads as a bug rather than as the prompt to set a name it is.
		return "(machine hostname unavailable — set one with --daemon-name)"
	}
	return daemonName
}
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
		cmd.Flags().Changed("poll-interval") ||
		cmd.Flags().Changed("daemon-name")

	if !anyChanged {
		fmt.Printf("  Skip permissions: %v\n", config.PluginConfigBool(&s, "claude", "dangerously_skip_permissions"))
		fmt.Printf("  Managed accounts: %v\n", s.ManagedAccounts.ManagedAgentAccountsEnabled())
		fmt.Printf("  Failover proxy:   %v\n", s.ManagedAccounts.FailoverProxyEnabled())
		fmt.Printf("  Worktree dir:     %s\n", s.WorktreeBaseDir)
		fmt.Printf("  Default agent:    %s\n", s.DefaultAgent)
		interval := "30 (default)"
		if s.PollIntervalSeconds > 0 {
			interval = strconv.Itoa(s.PollIntervalSeconds)
		}
		fmt.Printf("  Poll interval:    %s seconds\n", interval)
		hostname, _ := os.Hostname()
		fmt.Printf("  Daemon name:      %s\n", settingsDaemonNameLine(s, hostname))
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
	daemonNameChanged := cmd.Flags().Changed("daemon-name")
	if daemonNameChanged {
		v, _ := cmd.Flags().GetString("daemon-name")
		// Unlike --worktree-dir and --default-agent, a blank value here is the
		// documented reset to the machine hostname rather than an error. This
		// mirrors the TUI commit branch in services/boss/api/tui, and
		// `daemon_name` is json:omitempty so "" drops the key entirely.
		s.DaemonName = strings.TrimSpace(v)
	}

	if err := config.Save(s); err != nil {
		return fmt.Errorf("save settings: %w", err)
	}

	fmt.Println("Settings updated.")
	if daemonNameChanged {
		// bossd reads daemon_name only at startup, which is why the TUI row
		// carries a restart hint on every render.
		fmt.Println("Restart the daemon for the new name to take effect: 'boss daemon restart'.")
	}
	return nil
}
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
			// Add new entry. Newly discovered plugins default to enabled, except
			// experimental ones (BOS-1145): this scans the directory the official
			// installer populated, so it authors the very entry that made an
			// alpha runner load unasked. bossd gates it off at load either way,
			// but writing "enabled": true here would leave the installer's own
			// file contradicting the daemon.
			newPlugin := config.PluginConfig{
				Name:    pluginName,
				Path:    path,
				Enabled: !config.IsExperimentalPlugin(pluginName),
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

	// Name the experimental plugins the daemon will leave off. The count above
	// includes them, so on its own it reads as "all of these are on", and the
	// only other places that state surfaces are a bossd log line and the
	// settings reference — neither of which reaches the person who just ran this.
	// settings.json cannot carry a comment, so this line is the hint.
	//
	// Ask the gate rather than reading plugins[].enabled: that flag is inert for
	// a registry member, since ApplyExperimentalPluginGate overrides it in both
	// directions. Reading it directly would nag a user who has already opted in
	// (config init writes the entry disabled and never rewrites the flag) and
	// stay silent for the legacy "enabled": true entry the daemon is about to
	// force off — the very installed base this notice exists for.
	gated, _ := config.ApplyExperimentalPluginGate(s.Plugins, s.ExperimentalPlugins)
	var experimentalOff []string
	for i := range gated {
		p := gated[i]
		if _, discovered := foundPlugins["bossd-plugin-"+p.Name]; !discovered {
			continue
		}
		if config.IsExperimentalPlugin(p.Name) && !p.Enabled {
			experimentalOff = append(experimentalOff, p.Name)
		}
	}
	if len(experimentalOff) > 0 {
		fmt.Printf("Experimental, left off: %s. To load one, add its name to %q in %s.\n",
			strings.Join(experimentalOff, ", "), "experimental_plugins", settingsPath)
	}
	return nil
}
