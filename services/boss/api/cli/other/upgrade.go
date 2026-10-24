package other

import (
	"github.com/recurser/boss/internal/upgrade"
	"github.com/spf13/cobra"
)

func (g *group) upgradeCmd() *cobra.Command {
	var checkOnly bool
	var yes bool
	var version string
	var noRestart bool

	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Check for and install Bossanova upgrades",
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.upgrade.Run(cmd, upgrade.Options{
				CheckOnly: checkOnly,
				Yes:       yes,
				Version:   version,
				NoRestart: noRestart,
			})
		},
	}
	cmd.Flags().BoolVar(&checkOnly, "check", false, "check for an upgrade without installing")
	cmd.Flags().BoolVar(&yes, "yes", false, "install without interactive confirmation")
	cmd.Flags().StringVar(&version, "version", "", "install a specific stable release tag (prereleases are not supported)")
	cmd.Flags().BoolVar(&noRestart, "no-restart", false, "do not restart the daemon after upgrade")
	return cmd
}
