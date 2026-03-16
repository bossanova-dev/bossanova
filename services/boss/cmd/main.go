// Package main is the entry point for the boss CLI.
package main

import (
	"fmt"
	"os"

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

	root.AddCommand(
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
	)

	return root
}

// --- Subcommands ---

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
