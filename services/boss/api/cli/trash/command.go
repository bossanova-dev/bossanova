package trash

import (
	"context"

	"github.com/recurser/boss/internal/domain/rpc"

	"github.com/recurser/boss/api/cli/cliutil"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"

	"github.com/spf13/cobra"
)

func (g *group) trashCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trash",
		Short: "Manage archived sessions",
	}

	ls := &cobra.Command{
		Use:   "ls",
		Short: "List archived sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runTrashLS(cmd)
		},
	}

	restore := &cobra.Command{
		Use:   "restore <session-id>",
		Short: "Restore an archived session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runResurrect(cmd, args[0])
		},
	}

	del := &cobra.Command{
		Use:   "delete <session-id>",
		Short: "Permanently delete an archived session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runTrashDelete(cmd, args[0])
		},
	}
	del.Flags().BoolP("yes", "y", false, "Skip confirmation prompt")

	empty := &cobra.Command{
		Use:   "empty",
		Short: "Permanently delete all archived sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runTrashEmpty(cmd)
		},
	}
	empty.Flags().String("older-than", "", "Only delete sessions archived longer than this duration (e.g. 30d)")

	cmd.AddCommand(ls, restore, del, empty)

	return cmd
}

// Client is the daemon surface consumed by this command group.
type Client interface {
	ListSessions(context.Context, *pb.ListSessionsRequest, rpc.SessionReadOptions) ([]*pb.Session, error)
	ResurrectSession(context.Context, string) (rpc.ResurrectSessionStream, error)
	EmptyTrash(context.Context, *pb.EmptyTrashRequest) (int32, error)
	RemoveSession(context.Context, string) error
}
type group struct{ newClient cliutil.ClientFactory[Client] }

// New constructs an independent command group.
func New(factory cliutil.ClientFactory[Client]) *cobra.Command {
	g := &group{newClient: factory}
	return g.trashCmd()
}
