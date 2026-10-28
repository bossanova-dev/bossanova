package repo

import (
	"context"
	"fmt"
	"io"

	"github.com/recurser/boss/api/cli/cliutil"
	"github.com/recurser/boss/internal/adapter/cloudrpc"

	"charm.land/bubbles/v2/table"
	"github.com/recurser/boss/internal/domain/display"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

func (g *Group) runRepoLS(cmd *cobra.Command) error {
	c, err := g.newClient(cmd)
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
	trusted := make([]string, len(repos))
	for i, repo := range repos {
		trusted[i] = "no"
		if repo.IsTrusted {
			trusted[i] = "yes"
		}
		ids[i] = repo.Id
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
		{Title: "ID", Width: display.MaxColWidth("ID", ids, 0)},
		{Title: "NAME", Width: display.MaxColWidth("NAME", names, 30)},
		{Title: "PATH", Width: display.MaxColWidth("PATH", paths, 60)},
		{Title: "BRANCH", Width: display.MaxColWidth("BRANCH", branches, 30)},
		{Title: "TRUSTED", Width: 7},
		{Title: "SETUP", Width: display.MaxColWidth("SETUP", setups, 40)},
	}

	rows := make([]table.Row, len(repos))
	for i := range repos {
		rows[i] = table.Row{ids[i], names[i], paths[i], branches[i], trusted[i], setups[i]}
	}

	t := table.New(
		table.WithColumns(cols),
		table.WithRows(rows),
		table.WithHeight(len(rows)+1),
		table.WithWidth(display.CLIColumnsWidth(cols)),
		table.WithStyles(display.CLITableStyles()),
		table.WithFocused(false),
	)
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), t.View())
	return nil
}

func (g *Group) runRepoRemove(cmd *cobra.Command, id string) error {
	c, err := g.newClient(cmd)
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

func (g *Group) runRepoUpdate(cmd *cobra.Command, repoID string) error {
	req, err := buildRepoUpdateRequest(cmd, repoID)
	if err != nil {
		return err
	}

	c, err := g.newClient(cmd)
	if err != nil {
		return err
	}

	ctx := context.Background()
	repo, err := c.UpdateRepo(ctx, req)
	if err != nil {
		return fmt.Errorf("update repo: %w", err)
	}

	printUpdatedRepo(cmd.OutOrStdout(), c, repo)
	return nil
}

func (g *Group) runRepoShow(cmd *cobra.Command, repoID string) error {
	c, err := g.newClient(cmd)
	if err != nil {
		return err
	}
	repos, err := c.ListRepos(cmd.Context())
	if err != nil {
		return fmt.Errorf("list repos: %w", err)
	}
	for _, r := range repos {
		if r.Id == repoID {
			asJSON, _ := cmd.Flags().GetBool(cliutil.JSONFlagName)
			if asJSON {
				return cliutil.EmitJSON(cmd, repoSettingsFor(c, r))
			}
			printRepoSettings(cmd.OutOrStdout(), c, r)
			return nil
		}
	}
	return fmt.Errorf("repository %q not found; run boss repo ls to see registered repositories", repoID)
}

// printUpdatedRepo writes the `boss repo update` summary. The Trusted line is
// omitted for a remote client: the cloud proxy deliberately carries no trust
// (a cloud caller must not see or flip a daemon's local trust), so its repo
// always decodes as untrusted and printing it would misreport a trusted repo.
func printUpdatedRepo(w io.Writer, c Client, repo *pb.Repo) {
	_, _ = fmt.Fprintf(w, "Repository updated.\n")
	printRepoSettings(w, c, repo)
}

func printRepoSettings(w io.Writer, c Client, repo *pb.Repo) {
	_, _ = fmt.Fprintf(w, "  ID:       %s\n", repo.Id)
	_, _ = fmt.Fprintf(w, "  Name:     %s\n", repo.DisplayName)
	_, _ = fmt.Fprintf(w, "  Path:     %s\n  Origin:   %s\n  Default base branch: %s\n", repo.LocalPath, repo.OriginUrl, repo.DefaultBaseBranch)
	_, _ = fmt.Fprintf(w, "  Strategy: %s\n", repo.MergeStrategy)
	if repo.SetupScript != nil {
		_, _ = fmt.Fprintf(w, "  Setup:    %s\n", *repo.SetupScript)
	}
	_, _ = fmt.Fprintf(w, "  Worktree base:          %s\n", repo.WorktreeBaseDir)
	_, _ = fmt.Fprintf(w, "  Mark ready on green:    %v\n", repo.CanAutoMerge)
	_, _ = fmt.Fprintf(w, "  Auto-merge Dependabot:  %v\n", repo.CanAutoMergeDependabot)
	_, _ = fmt.Fprintf(w, "  Automatic repair:       %v\n", repo.CanAutoRepair)
	_, _ = fmt.Fprintf(w, "  Delete branches:        %v\n", repo.CanAutoDeleteBranches)
	_, _ = fmt.Fprintf(w, "  Archive after merge:   %v\n", repo.ShouldArchiveSessionsAfterMerge)
	_, _ = fmt.Fprintf(w, "  Sentry org:            %s\n  Linear API key:        %s\n  Sentry auth token:     %s\n", repo.SentryOrg, secretPresence(repo.LinearApiKey), secretPresence(repo.SentryApiKey))
	_, _ = fmt.Fprintf(w, "  Keep branches current:  %v\n", repo.ShouldKeepBranchesCurrent)
	if _, remote := c.(*cloudrpc.RemoteClient); !remote {
		_, _ = fmt.Fprintf(w, "  Trusted:                %v\n", repo.IsTrusted)
		_, _ = fmt.Fprintln(w, "    Codex trust is written per worktree when a chat or run launches; Claude loads project MCP servers and settings only when trusted")
	}
}

// buildRepoUpdateRequest maps the `boss repo update` flags that were actually
// set onto an UpdateRepoRequest, so the mapping is testable without a daemon.
// It errors when no flag was set or when a flag pair conflicts. Path validation
// for --worktree-base-dir is the daemon's (it must be absolute).
func buildRepoUpdateRequest(cmd *cobra.Command, repoID string) (*pb.UpdateRepoRequest, error) {
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
			return nil, fmt.Errorf("invalid merge strategy %q (use merge, rebase, or squash)", v)
		}
		anyChanged = true
	}
	if cmd.Flags().Changed("worktree-base-dir") {
		v, _ := cmd.Flags().GetString("worktree-base-dir")
		req.WorktreeBaseDir = &v
		anyChanged = true
	}
	if cmd.Flags().Changed("trusted") {
		v, _ := cmd.Flags().GetBool("trusted")
		req.IsTrusted = &v
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
		{"delete-branches", "no-delete-branches", func(v bool) { req.CanAutoDeleteBranches = &v }},
		{"keep-branches-current", "no-keep-branches-current", func(v bool) { req.ShouldKeepBranchesCurrent = &v }},
	}
	for _, bp := range boolPairs {
		enableChanged := cmd.Flags().Changed(bp.enable)
		disableChanged := cmd.Flags().Changed(bp.disable)
		if enableChanged && disableChanged {
			return nil, fmt.Errorf("cannot use both --%s and --%s", bp.enable, bp.disable)
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
		return nil, fmt.Errorf("no flags provided — use --name, --setup-script, --merge-strategy, --worktree-base-dir, --trusted, or boolean flags; run \"boss repo show %s\" to see the current settings", repoID)
	}
	return req, nil
}
