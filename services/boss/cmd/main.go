// Package main is the entry point for the boss CLI.
package main

import (
	"fmt"
	"os"

	"github.com/recurser/bossalib/buildinfo"
	"github.com/spf13/cobra"
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
		newCmd(),
		attachCmd(),
		repoCmd(),
		archiveCmd(),
		resurrectCmd(),
		trashCmd(),
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
		Use:   "resurrect <session-id>",
		Short: "Resurrect an archived session",
		Args:  cobra.ExactArgs(1),
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

func trashCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trash",
		Short: "Manage archived sessions",
	}

	empty := &cobra.Command{
		Use:   "empty",
		Short: "Permanently delete archived sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTrashEmpty(cmd)
		},
	}
	empty.Flags().String("older-than", "", "Only delete sessions archived longer than this duration (e.g. 30d)")
	cmd.AddCommand(empty)

	return cmd
}
