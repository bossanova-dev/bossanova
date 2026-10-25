package repo

import (
	"context"
	"errors"

	"github.com/recurser/boss/internal/adapter/cloudrpc"

	"github.com/recurser/boss/api/cli/cliutil"
	"github.com/recurser/boss/api/cli/skills"
	daemonrpc "github.com/recurser/boss/internal/adapter/daemonrpc"
	"github.com/recurser/bossalib/kit/projectconfig"
	"github.com/spf13/cobra"
)

// Client is the daemon surface consumed by repository and project commands.
type Client interface {
	skills.Client
	projectconfig.LocalReader
	projectconfig.LocalWriter
	RemoveRepo(context.Context, string) error
}

// DaemonProfile identifies the local daemon's persisted state and socket.
type DaemonProfile struct{ SettingsPath, AppDataDir, SocketPath string }

// Options connects command groups to process-owned dependencies.
type Options struct {
	Profile       func() (DaemonProfile, error)
	CloudRemote   func(*cobra.Command, error) (*cloudrpc.RemoteClient, error)
	LaunchRepoAdd func(*cobra.Command) error
}

// Group owns independent repository and project command trees.
type Group struct {
	newClient           cliutil.ClientFactory[Client]
	options             Options
	newProjectExportEnv func() projectExportEnv
	newProjectApplyEnv  func() projectApplyEnv
}

// New constructs repository and project commands with caller-owned factories.
func New(factory cliutil.ClientFactory[Client], options Options) *Group {
	g := &Group{newClient: factory, options: options}
	g.newProjectExportEnv = g.defaultProjectExportEnv
	g.newProjectApplyEnv = g.defaultProjectApplyEnv
	return g
}

// ExitStatus reads the project command's explicit status without treating arbitrary process errors as project statuses.
func ExitStatus(err error) int {
	var coded *projectExitError
	if errors.As(err, &coded) {
		return coded.code
	}
	return 1
}

func remoteURL(cmd *cobra.Command) string {
	if cmd == nil || cmd.Root() == nil {
		return ""
	}
	v, _ := cmd.Root().Flags().GetString("remote")
	return v
}
func hostDestination(cmd *cobra.Command) string {
	if cmd == nil || cmd.Root() == nil {
		return ""
	}
	v, _ := cmd.Root().Flags().GetString("host")
	return v
}

var _ Client = (*daemonrpc.LocalClient)(nil)
