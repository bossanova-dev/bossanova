package settings

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/recurser/bossalib/kit/config"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func (g *Group) SettingsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "settings",
		Short: "View or update global settings",
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runSettingsWithRestriction(cmd)
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
	cmd.Flags().String("default-agent", "", "Set the default agent plugin (e.g. claude, codex, antigravity)")
	cmd.Flags().Int("poll-interval", 0, "Set poll interval in seconds (0 = default)")
	cmd.Flags().String("daemon-name", "", "Set the daemon display name (blank resets to the machine hostname; restart the daemon to apply)")
	cmd.Flags().String("cloud-control", "", "Cloud control: default, private, read-only or full (takes effect immediately; no restart needed)")
	cmd.Flags().StringArray("cloud-control-repo", nil, "Set a registered repo grant: <origin, display name or checkout>=<default|private|read-only|full> (repeatable)")
	cmd.Flags().StringArray("cloud-control-owner", nil, "Set each registered repo under <host/owner>=<default|private|read-only|full> (repeatable)")
	cmd.Flags().Bool("restrict-public-repos", false, "Suggest public repos and restrict them to read-only after confirmation")
	cmd.Flags().String("owner", "", "Limit public-repo restriction to <host/owner>")
	cmd.Flags().Bool("yes", false, "Confirm public-repo restriction without prompting")
	return cmd
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
func settingsCloudControlLine(raw string) string {
	mode := config.ResolveCloudControl(raw)
	switch strings.TrimSpace(raw) {
	case "":
		return string(mode) + " (default)"
	case string(config.CloudControlPrivate), string(config.CloudControlFull), string(config.CloudControlReadOnly):
		return string(mode)
	default:
		return fmt.Sprintf("%s (unrecognised value %q)", mode, raw)
	}
}
func applyCloudControlFlag(cmd *cobra.Command, s *config.Settings) error {
	if cmd.Flags().Changed("cloud-control") {
		v, _ := cmd.Flags().GetString("cloud-control")
		if v != "default" && v != string(config.CloudControlPrivate) && v != string(config.CloudControlFull) && v != string(config.CloudControlReadOnly) {
			return fmt.Errorf("cloud-control must be default, private, read-only or full")
		}
		if v == "default" {
			v = ""
		}
		s.CloudControl = v
	}

	return nil
}

func printCloudControlWarning(cmd *cobra.Command, s config.Settings) {
	if !cmd.Flags().Changed("cloud-control") {
		return
	}
	switch config.ResolveCloudControl(s.CloudControl) {
	case config.CloudControlFull:
		fmt.Println(config.FullCloudControlWarning)
	case config.CloudControlPrivate:
		fmt.Println("The web app will show sessions on this machine but not their transcripts, terminals, notes, cron prompts or callback prompts.")
	case config.CloudControlReadOnly:
	}
}

func printSettingsCloudControlWarnings(cmd *cobra.Command, s config.Settings, repoFull bool) {
	printCloudControlWarning(cmd, s)
	if repoFull && (!cmd.Flags().Changed("cloud-control") || s.CloudControl != "full") {
		fmt.Println(config.FullCloudControlWarning)
	}
}

func (g *Group) runSettingsWithRestriction(cmd *cobra.Command) error {
	restrict, _ := cmd.Flags().GetBool("restrict-public-repos")
	if restrict {
		var incompatible string
		cmd.Flags().Visit(func(flag *pflag.Flag) {
			if flag.Name != "restrict-public-repos" && flag.Name != "owner" && flag.Name != "yes" {
				incompatible = flag.Name
			}
		})
		if incompatible != "" {
			return fmt.Errorf("--restrict-public-repos cannot be combined with --%s", incompatible)
		}
		return g.restrictPublicRepos(cmd)
	}
	if cmd.Flags().Changed("owner") || cmd.Flags().Changed("yes") {
		return fmt.Errorf("--owner and --yes require --restrict-public-repos")
	}

	return g.runSettings(cmd)
}

func (g *Group) runSettings(cmd *cobra.Command) error {
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
		cmd.Flags().Changed("daemon-name") ||
		cmd.Flags().Changed("cloud-control") ||
		cmd.Flags().Changed("cloud-control-repo") || cmd.Flags().Changed("cloud-control-owner")

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
		fmt.Printf("  Cloud control:    %s\n", settingsCloudControlLine(s.CloudControl))
		g.printRepoCloudControls(cmd)
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

	if err := applyCloudControlFlag(cmd, &s); err != nil {
		return err
	}

	repoFull, err := g.applyRepoCloudControlFlags(cmd, &s)
	if err != nil {
		return err
	}
	if err := config.Save(s); err != nil {
		return fmt.Errorf("save settings: %w", err)
	}

	fmt.Println("Settings updated.")
	printSettingsCloudControlWarnings(cmd, s, repoFull)
	if daemonNameChanged {
		// bossd reads daemon_name only at startup, which is why the TUI row
		// carries a restart hint on every render.
		fmt.Println("Restart the daemon for the new name to take effect: 'boss daemon restart'.")
	}
	return nil
}
