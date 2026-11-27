package skills

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"

	"github.com/recurser/boss/internal/domain/rpc"

	"github.com/recurser/bossalib/contract/plugin/skillinstall"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/projectconfig"
	"github.com/spf13/cobra"
)

// Client is the daemon surface consumed by skills initialization.
type Client interface {
	ValidateRepoPath(context.Context, string) (*pb.ValidateRepoPathResponse, error)
	ListRepos(context.Context) ([]*pb.Repo, error)
	RegisterRepo(context.Context, *pb.RegisterRepoRequest) (*pb.Repo, error)
	UpdateRepo(context.Context, *pb.UpdateRepoRequest) (*pb.Repo, error)
	ListAgents(context.Context) ([]rpc.AgentInfo, error)
	ListCronJobs(context.Context, string) ([]*pb.CronJob, error)
	CreateCronJob(context.Context, *pb.CreateCronJobRequest) (*pb.CronJob, error)
	UpdateCronJob(context.Context, *pb.UpdateCronJobRequest) (*pb.CronJob, error)
	ListSetupChecks(context.Context, *pb.ListSetupChecksRequest) (*pb.ListSetupChecksResponse, error)
}
type ClientFactory func(*cobra.Command) (Client, error)

// Config connects init to the project command's existing application engine.
type Config struct {
	ShutdownHostTunnel func()

	LoadProject func(context.Context, string) (*Project, error)

	// NewSetupCheckClient builds the read-only client for `boss init check`
	// without auto-starting bossd. When nil, Group falls back to its default
	// client factory.
	NewSetupCheckClient ClientFactory
	// SetupCheckSocketPath resolves the local daemon socket path reported in
	// unreachable-daemon evidence.
	SetupCheckSocketPath func() (string, error)
	// SetupCheckServiceInstalled reports whether the bossd service is installed
	// on this host, choosing between `boss daemon start` and `boss daemon install`
	// when the daemon is unreachable.
	SetupCheckServiceInstalled func() bool
}
type Project struct {
	Path         string
	SelectsCrons func() bool
	CheckBranch  func(context.Context, *cobra.Command) (string, bool)
	Plan         func(context.Context, *cobra.Command) (ProjectRun, error)
}
type ProjectRun struct {
	WritePlan func(io.Writer, projectconfig.LocalPlan)

	Plan       projectconfig.LocalPlan
	CheckTrust func(projectconfig.LocalPlan, ProjectTrust) error
	Apply      func(projectconfig.LocalPlan) (projectconfig.LocalResult, error)
}
type ProjectTrust struct {
	Yes, AllowBranch bool
	Confirm          func(string) (bool, error)
}

var ErrProjectNotConfirmed = errors.New("project config not applied")

// Commands builds fresh skills and init command trees.
func (g *Group) Commands() []*cobra.Command { return []*cobra.Command{g.skillsCmd(), g.initCmd()} }
func (g *Group) MaybeInstallSkills() error  { return g.maybeInstallSkills() }
func (g *Group) SkillPayload() (fs.FS, skillinstall.PayloadRecord, error) {
	p, e := g.skillPayload()
	if e != nil {
		return nil, skillinstall.PayloadRecord{}, e
	}
	return p.fsys, g.skillPayloadRecord(p, skillinstall.WriterExplicit), nil
}
func ShellQuote(s string) string { return shellQuote(s) }
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
func commandContext(cmd *cobra.Command) context.Context {
	if cmd == nil {
		return context.Background()
	}
	return cmd.Context()
}
func fileExists(path string) bool { _, err := os.Stat(path); return err == nil }
