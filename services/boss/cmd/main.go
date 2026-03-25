// Package main is the entry point for the boss CLI.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/recurser/bossalib/buildinfo"
	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/skilldata"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "boss: %v\n", err)
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "boss",
		Short: "Bossanova — autonomous Claude coding sessions",
		Long:  "Boss manages Claude coding sessions with automatic PR creation, CI fix loops, and code review handling.",
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			return maybeInstallSkills()
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			// Default: launch interactive TUI home screen.
			return runTUI(cmd)
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().String("remote", "", "Connect to orchestrator URL instead of local daemon")

	root.AddCommand(
		versionCmd(),
		lsCmd(),
		showCmd(),
		chatsCmd(),
		newCmd(),
		attachCmd(),
		repoCmd(),
		archiveCmd(),
		resurrectCmd(),
		trashCmd(),
		settingsCmd(),
		loginCmd(),
		logoutCmd(),
		authStatusCmd(),
		daemonCmd(),
		autopilotCmd(),
	)

	return root
}

// --- Subcommands ---

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("boss " + buildinfo.String())
		},
	}
}

func lsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List sessions (non-interactive)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLS(cmd)
		},
	}
	cmd.Flags().String("repo", "", "Filter by repo ID")
	cmd.Flags().Bool("archived", false, "Include archived sessions")
	cmd.Flags().StringSlice("state", nil, "Filter by state(s)")
	return cmd
}

func showCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <session-id>",
		Short: "Show session details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runShow(cmd, args[0])
		},
	}
}

func chatsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "chats <session-id>",
		Short: "List chats in a session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runChats(cmd, args[0])
		},
	}
}

func newCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "new",
		Short: "Create a new coding session",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runNew(cmd)
		},
	}
}

func attachCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "attach <session-id>",
		Short: "Attach to a running session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAttach(cmd, args[0])
		},
	}
}

func repoCmd() *cobra.Command {
	repo := &cobra.Command{
		Use:   "repo",
		Short: "Manage repositories",
	}

	update := &cobra.Command{
		Use:   "update <repo-id>",
		Short: "Update repository settings",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRepoUpdate(cmd, args[0])
		},
	}
	update.Flags().String("name", "", "Set display name")
	update.Flags().String("setup-script", "", "Set setup script (empty string to clear)")
	update.Flags().String("merge-strategy", "", "Set merge strategy (merge, rebase, squash)")
	update.Flags().Bool("auto-merge", false, "Enable auto-merge")
	update.Flags().Bool("no-auto-merge", false, "Disable auto-merge")
	update.Flags().Bool("auto-merge-dependabot", false, "Enable auto-merge for Dependabot PRs")
	update.Flags().Bool("no-auto-merge-dependabot", false, "Disable auto-merge for Dependabot PRs")
	update.Flags().Bool("auto-address-reviews", false, "Enable auto-address review feedback")
	update.Flags().Bool("no-auto-address-reviews", false, "Disable auto-address review feedback")
	update.Flags().Bool("auto-resolve-conflicts", false, "Enable auto-resolve merge conflicts")
	update.Flags().Bool("no-auto-resolve-conflicts", false, "Disable auto-resolve merge conflicts")

	repo.AddCommand(
		&cobra.Command{
			Use:   "add",
			Short: "Register a repository",
			RunE: func(cmd *cobra.Command, args []string) error {
				return runRepoAdd(cmd)
			},
		},
		&cobra.Command{
			Use:   "ls",
			Short: "List registered repositories",
			RunE: func(cmd *cobra.Command, args []string) error {
				return runRepoLS(cmd)
			},
		},
		&cobra.Command{
			Use:   "remove <repo-id>",
			Short: "Remove a registered repository",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return runRepoRemove(cmd, args[0])
			},
		},
		update,
	)

	return repo
}

func archiveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "archive <session-id>",
		Short: "Archive a session (keep branch, remove worktree)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runArchive(cmd, args[0])
		},
	}
}

func resurrectCmd() *cobra.Command {
	return &cobra.Command{
		Use:        "resurrect <session-id>",
		Short:      "Resurrect an archived session",
		Deprecated: "use 'boss trash restore' instead",
		Args:       cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runResurrect(cmd, args[0])
		},
	}
}

func daemonCmd() *cobra.Command {
	d := &cobra.Command{
		Use:   "daemon",
		Short: "Manage the bossd daemon",
	}

	d.AddCommand(
		&cobra.Command{
			Use:   "install",
			Short: "Install bossd as a macOS LaunchAgent",
			RunE: func(cmd *cobra.Command, args []string) error {
				return runDaemonInstall(cmd)
			},
		},
		&cobra.Command{
			Use:   "uninstall",
			Short: "Uninstall the bossd LaunchAgent",
			RunE: func(cmd *cobra.Command, args []string) error {
				return runDaemonUninstall(cmd)
			},
		},
		&cobra.Command{
			Use:   "status",
			Short: "Show bossd daemon status",
			RunE: func(cmd *cobra.Command, args []string) error {
				return runDaemonStatus(cmd)
			},
		},
	)

	return d
}

func settingsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "settings",
		Short: "View or update global settings",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSettings(cmd)
		},
	}
	cmd.Flags().Bool("skip-permissions", false, "Enable Claude --dangerously-skip-permissions")
	cmd.Flags().Bool("no-skip-permissions", false, "Disable Claude --dangerously-skip-permissions")
	cmd.Flags().String("worktree-dir", "", "Set worktree base directory")
	cmd.Flags().Int("poll-interval", 0, "Set poll interval in seconds (0 = default)")
	return cmd
}

// maybeInstallSkills prompts the user to install boss skills into ~/.claude/skills/
// on first run. If skills are already installed, this is a no-op (the daemon handles updates).
func maybeInstallSkills() error {
	dir, err := skilldata.DefaultSkillsDir()
	if err != nil {
		return nil // non-fatal
	}
	settings, _ := config.Load()
	if skilldata.BossSkillsInstalled(dir) || settings.SkillsDeclined {
		return nil // already installed or user opted out
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil // non-interactive, skip silently
	}

	fmt.Fprintf(os.Stderr, "Install boss skills to %s? [Y/n] ", dir)
	var answer string
	if _, err := fmt.Scanln(&answer); err != nil {
		answer = "" // default to yes on read error
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	if answer == "n" || answer == "no" {
		settings.SkillsDeclined = true
		_ = config.Save(settings)
		return nil
	}
	if err := skilldata.ExtractSkills(dir, skilldata.SkillsFS); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to install skills: %v\n", err)
	} else {
		fmt.Fprintln(os.Stderr, "Boss skills installed.")
	}
	return nil
}

func trashCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trash",
		Short: "Manage archived sessions",
	}

	ls := &cobra.Command{
		Use:   "ls",
		Short: "List archived sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTrashLS(cmd)
		},
	}

	restore := &cobra.Command{
		Use:   "restore <session-id>",
		Short: "Restore an archived session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runResurrect(cmd, args[0])
		},
	}

	del := &cobra.Command{
		Use:   "delete <session-id>",
		Short: "Permanently delete an archived session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTrashDelete(cmd, args[0])
		},
	}
	del.Flags().BoolP("yes", "y", false, "Skip confirmation prompt")

	empty := &cobra.Command{
		Use:   "empty",
		Short: "Permanently delete all archived sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTrashEmpty(cmd)
		},
	}
	empty.Flags().String("older-than", "", "Only delete sessions archived longer than this duration (e.g. 30d)")

	cmd.AddCommand(ls, restore, del, empty)

	return cmd
}
