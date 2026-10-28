package repo

import (
	"github.com/recurser/boss/api/cli/cliutil"
	"github.com/spf13/cobra"
)

func (g *Group) RepoCommand() *cobra.Command {
	repo := &cobra.Command{
		Use:   "repo",
		Short: "Manage repositories",
	}

	show := &cobra.Command{Use: "show <repo-id>", Short: "Show repository settings", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error { return g.runRepoShow(cmd, args[0]) }}
	show.Flags().Bool(cliutil.JSONFlagName, false, "Print repository settings as JSON (secrets redacted)")

	update := &cobra.Command{
		Use:   "update <repo-id>",
		Short: "Update repository settings",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runRepoUpdate(cmd, args[0])
		},
	}
	update.Flags().String("name", "", "Set display name")
	update.Flags().String("setup-script", "", "Set setup script (empty string to clear)")
	update.Flags().String("merge-strategy", "", "Set merge strategy (merge, rebase, squash)")
	update.Flags().String("worktree-base-dir", "", "Set the absolute directory new session worktrees are created under (applies to new sessions)")
	update.Flags().Bool("auto-merge", false, "Enable auto-merge")
	update.Flags().Bool("no-auto-merge", false, "Disable auto-merge")
	update.Flags().Bool("auto-merge-dependabot", false, "Enable auto-merge for Dependabot PRs")
	update.Flags().Bool("no-auto-merge-dependabot", false, "Disable auto-merge for Dependabot PRs")
	update.Flags().Bool("auto-repair", false, "Enable automatic repair (failing checks, conflicts, review feedback)")
	update.Flags().Bool("no-auto-repair", false, "Disable automatic repair")
	update.Flags().Bool("delete-branches", false, "Enable deleting safe local branches after archiving")
	update.Flags().Bool("no-delete-branches", false, "Disable deleting local branches after archiving")
	update.Flags().Bool("keep-branches-current", false, "Enable proactively rebasing in-flight session branches when the base advances")
	update.Flags().Bool("no-keep-branches-current", false, "Disable proactively rebasing in-flight session branches when the base advances")
	update.Flags().Bool("trusted", false, "Trust this repository's own configuration: Codex project trust, Claude project configuration, and proof secrets for its sessions (--trusted=false revokes)")

	repo.AddCommand(
		&cobra.Command{
			Use:   "add",
			Short: "Register a repository",
			RunE: func(cmd *cobra.Command, args []string) error {
				return g.options.LaunchRepoAdd(cmd)
			},
		},
		&cobra.Command{
			Use:   "ls",
			Short: "List registered repositories",
			RunE: func(cmd *cobra.Command, args []string) error {
				return g.runRepoLS(cmd)
			},
		},
		&cobra.Command{
			Use:   "remove <repo-id>",
			Short: "Remove a registered repository",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return g.runRepoRemove(cmd, args[0])
			},
		},
		show,
		update,
	)

	return repo
}
