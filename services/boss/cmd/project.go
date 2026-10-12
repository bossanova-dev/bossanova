package main

import "github.com/spf13/cobra"

// `boss project` works with the committed project config file
// (lib/bossalib/projectconfig): a repository's boss setup written down so it
// can be reviewed in a pull request and replayed on another machine.
func projectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project",
		Short: "Work with the committed project config file (.boss-project.json)",
	}
	cmd.AddCommand(projectExportCmd(), projectDiffCmd(), projectApplyCmd())
	return cmd
}
