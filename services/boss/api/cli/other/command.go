package other

import (
	"fmt"

	"github.com/recurser/boss/internal/upgrade"
	"github.com/recurser/bossalib/kit/buildinfo"
	"github.com/spf13/cobra"
)

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("boss " + buildinfo.String())
		},
	}
}

type group struct{ upgrade *upgrade.Runner }

// New constructs version and upgrade commands.
func New(restart func() error) []*cobra.Command {
	g := &group{upgrade: upgrade.NewRunner(restart)}
	return []*cobra.Command{versionCmd(), g.upgradeCmd()}
}
