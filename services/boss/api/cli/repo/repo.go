package repo

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	update.Flags().String("serve-script", "", "Set serve script (empty string to clear)")
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
	update.Flags().Bool("refresh-origin", false, "Re-read the checkout's origin remote and store it as the repository's origin (after a rename or transfer)")
	update.Flags().Bool("trusted", false, "Trust this repository's own configuration: Codex project trust, Claude project configuration, and proof secrets for its sessions (--trusted=false revokes)")

	repo.AddCommand(
		&cobra.Command{
			Use:   "add [path]",
			Short: "Register a repository (opens the add wizard; with a path, starts from that checkout)",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				var path string
				if len(args) == 1 {
					var err error
					path, err = resolveRepoAddPath(cmd, args[0])
					if err != nil {
						return err
					}
				}
				return g.options.LaunchRepoAdd(cmd, path)
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

func resolveRepoAddPath(cmd *cobra.Command, raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", errors.New("repository path must not be empty")
	}
	if remoteURL(cmd) != "" || hostDestination(cmd) != "" {
		return raw, nil
	}
	expanded, err := expandTilde(raw)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("repository path not found: %s", abs)
		}
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("repository path is not a directory: %s", abs)
	}
	return abs, nil
}

func expandTilde(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}
