package repo

import "github.com/spf13/cobra"

// `boss project` works with the committed project config file
// (lib/bossalib/projectconfig): a repository's boss setup written down so it
// can be reviewed in a pull request and replayed on another machine.
func (g *Group) ProjectCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project",
		Short: "Work with the committed project config file (.boss-project.json)",
	}
	cmd.AddCommand(g.projectExportCmd(), g.projectDiffCmd(), g.projectApplyCmd())
	return cmd
}
