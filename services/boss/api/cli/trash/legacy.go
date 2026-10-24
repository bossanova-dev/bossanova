package trash

import (
	"github.com/recurser/boss/api/cli/cliutil"

	"github.com/spf13/cobra"
)

func LegacyRestore(factory cliutil.ClientFactory[Client]) *cobra.Command {
	g := &group{newClient: factory}
	return &cobra.Command{
		Use:        "resurrect <session-id>",
		Short:      "Resurrect an archived session",
		Deprecated: "use 'boss trash restore' instead",
		Args:       cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runResurrect(cmd, args[0])
		},
	}
}
