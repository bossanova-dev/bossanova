package diagnostics

import "github.com/spf13/cobra"

func (deps *Group) repairCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repair",
		Short: "Auto-repair plugin operations",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "doctor",
		Short: "Health-check the auto-repair pipeline (plugin loaded, claude on PATH, recent logs, etc.)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return deps.runRepairDoctor(cmd)
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "start",
		Short: "(Re-)arm the auto-repair workflow (e.g. after the repair plugin was stopped or restarted)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return deps.runRepairStart(cmd)
		},
	})
	return cmd
}
