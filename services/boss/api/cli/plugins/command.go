package plugins

import (
	"context"

	"github.com/recurser/boss/internal/domain/rpc"

	"github.com/recurser/boss/api/cli/cliutil"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

// Client contains the daemon operations used by the plugin and agent inventory commands.
type Client interface {
	ListPlugins(context.Context) ([]*pb.InstalledPlugin, error)
	ListAgents(context.Context) ([]rpc.AgentInfo, error)
}

// Group owns the dependencies for plugin and agent inventory commands.
type Group struct{ newClient cliutil.ClientFactory[Client] }

// New constructs a command group with the supplied daemon client factory.
func New(factory cliutil.ClientFactory[Client]) *Group { return &Group{newClient: factory} }

func (g *Group) PluginCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugin",
		Short: "Inspect installed plugins",
	}
	list := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List plugins the daemon attempted to load this run",
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runPluginList(cmd)
		},
	}
	cmd.AddCommand(list)
	return cmd
}

// AgentsCommand lists the agent runners the daemon loaded. It is deliberately not a
// filtered `plugin list`: the question it answers — "is this agent runner
// available to request?" — is narrower, and it carries each runner's
// user_settings, which the plugin inventory does not.
func (g *Group) AgentsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agents",
		Short: "List the agent runners the daemon loaded",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runAgents(cmd)
		},
	}
	cmd.Flags().Bool(cliutil.JSONFlagName, false, "Emit machine-readable JSON, including each agent's user settings")
	return cmd
}
