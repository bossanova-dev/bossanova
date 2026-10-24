package repo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/recurser/boss/api/cli/cliutil"

	"github.com/spf13/cobra"

	"github.com/recurser/boss/api/cli/skills"
	daemonrpc "github.com/recurser/boss/internal/adapter/daemonrpc"
	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/bossalib/config"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/projectconfig"
	"github.com/recurser/bossalib/vcs"
)

// CloudClient is the cloud surface `boss project export --cloud`
// reads. *cloudrpc.RemoteClient satisfies it; tests substitute a fake.
type CloudClient interface {
	cliutil.OrganizationLister
	GetRepoOrganization(ctx context.Context, repoOriginURL string) (*pb.RepoOrganizationMapping, error)
	ListTriggers(ctx context.Context, req *pb.ListTriggersRequest) ([]*pb.Trigger, error)
	ListSessionWebhooks(ctx context.Context, organizationID string) ([]*pb.SessionWebhook, error)
}

// errProjectNotLoggedIn is the failure for --cloud with no usable login.
var errProjectNotLoggedIn = errors.New("exporting cloud state needs a cloud login: run 'boss login' first")

// projectExportEnv is every host dependency of export. Tests replace it whole
// through newProjectExportEnv.
type projectExportEnv struct {
	newClient cliutil.ClientFactory[Client]
	// profile names the daemon this CLI talks to: its app data dir (where
	// bossd records its identity) and its socket.
	profile  func() (DaemonProfile, error)
	hostname func() (string, error)
	homeDir  func() (string, error)
	cloud    func(cmd *cobra.Command) (CloudClient, error)
}

func (g *Group) defaultProjectExportEnv() projectExportEnv {
	return projectExportEnv{
		newClient: g.newClient,
		profile:   g.options.Profile,
		hostname:  os.Hostname,
		homeDir:   os.UserHomeDir,
		cloud: func(cmd *cobra.Command) (CloudClient, error) {
			remote, err := g.options.CloudRemote(cmd, errProjectNotLoggedIn)
			if err != nil {
				return nil, err
			}
			return remote, nil
		},
	}
}

// projectCloudRemote dials bosso with mgr's access token; every login failure
// surfaces as errProjectNotLoggedIn before any request is sent.
func projectCloudRemote(ctx context.Context, mgr *auth.Manager, url string) (CloudClient, error) {
	remote, err := cliutil.DialCloudRemote(ctx, mgr, url, errProjectNotLoggedIn)
	if err != nil {
		return nil, err
	}
	return remote, nil
}

func (g *Group) projectExportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Print this checkout's boss setup as a project config file",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runProjectExport(cmd, g.newProjectExportEnv())
		},
	}
	cmd.Flags().String("dir", ".", "Checkout to export (any worktree of the repository)")
	cmd.Flags().Bool("cloud", false, "Include cloud triggers and session webhooks (needs boss login)")
	cmd.Flags().String("org", "", "Organization id for --cloud (default: the repository's organization, else your only one)")
	cmd.Flags().String("project", "", "Project slug (default: the existing file's, else the repository name)")
	cmd.Flags().Bool("write", false, "Write the file to the main checkout instead of printing it")
	cmd.Flags().Bool("force", false, "With --write, overwrite an existing file")
	return cmd
}

func runProjectExport(cmd *cobra.Command, env projectExportEnv) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	flags := cmd.Flags()
	write, _ := flags.GetBool("write")
	force, _ := flags.GetBool("force")
	withCloud, _ := flags.GetBool("cloud")
	if force && !write {
		return errors.New("--force requires --write")
	}
	if remoteURL(cmd) != "" || hostDestination(cmd) != "" {
		return errors.New("boss project export reads the local daemon's registration of a local checkout; --remote and --host are not supported")
	}

	// A missing login fails before anything else is read.
	var cloud CloudClient
	if withCloud {
		c, err := env.cloud(cmd)
		if err != nil {
			return err
		}
		cloud = c
	}

	dir, _ := flags.GetString("dir")
	main, err := skills.MainCheckout(ctx, dir)
	if err != nil {
		return err
	}
	c, err := env.newClient(cmd)
	if err != nil {
		return errors.New("could not connect to local bossd")
	}
	repo, err := skills.FindInitRepo(ctx, c, main)
	if err != nil {
		return err
	}
	if repo == nil {
		return fmt.Errorf("%s is not registered with the local daemon; run 'boss init --register' there first", main)
	}
	jobs, err := c.ListCronJobs(ctx, repo.GetId())
	if err != nil {
		return errors.New("could not list the repository's cron jobs")
	}

	var warnings []string
	target := filepath.Join(main, projectconfig.FileName)
	project, _ := flags.GetString("project")
	existing, existingWarning := existingProjectSlug(target)
	if existingWarning != "" {
		warnings = append(warnings, existingWarning)
	}
	home, _ := env.homeDir()
	in := projectconfig.ExportInput{
		Project:         project,
		ExistingProject: existing,
		Repo:            repo,
		CronJobs:        jobs,
		Daemon:          localDaemonIdentity(env),
		CheckoutPath:    main,
		HomeDir:         home,
		Classifier:      factoryStageClassifier{},
	}
	if cloud != nil {
		in.Cloud, err = readProjectCloud(cmd, cloud, repo)
		if err != nil {
			return err
		}
	}

	cfg, exportWarnings, err := projectconfig.Export(in)
	warnings = append(warnings, exportWarnings...)
	if err != nil {
		printProjectWarnings(cmd.ErrOrStderr(), warnings)
		return err
	}
	data, err := projectconfig.Encode(cfg)
	if err != nil {
		return err
	}
	if !write {
		printProjectWarnings(cmd.ErrOrStderr(), warnings)
		_, err := cmd.OutOrStdout().Write(data)
		return err
	}

	if _, err := os.Stat(target); err == nil && !force {
		printProjectWarnings(cmd.ErrOrStderr(), warnings)
		return fmt.Errorf("%s already exists; pass --force to overwrite it", target)
	}
	if err := os.WriteFile(target, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", target, err)
	}
	if gitIgnores(ctx, main, projectconfig.FileName) {
		warnings = append(warnings, fmt.Sprintf("%s is ignored by git, so it will not be committed; remove it from your ignore rules to share it", target))
	}
	printProjectWarnings(cmd.ErrOrStderr(), warnings)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s\n", target)
	return nil
}

// readProjectCloud resolves the organization (--org, else the repository's
// organization mapping, else the caller's only organization) and reads the
// caller's triggers and the organization's session webhooks.
func readProjectCloud(cmd *cobra.Command, c CloudClient, repo *pb.Repo) (*projectconfig.CloudInput, error) {
	ctx := cmd.Context()
	org, _ := cmd.Flags().GetString("org")
	org = strings.TrimSpace(org)
	if org == "" {
		mapping, err := c.GetRepoOrganization(ctx, vcs.CanonicalRepoURL(repo.GetOriginUrl()))
		if err != nil {
			return nil, fmt.Errorf("read the repository's organization: %w", err)
		}
		org = mapping.GetOrganizationId()
	}
	if org == "" {
		resolved, err := cliutil.ResolveOrganizationID(cmd, c)
		if err != nil {
			return nil, err
		}
		org = resolved
	}
	triggers, err := c.ListTriggers(ctx, &pb.ListTriggersRequest{OrganizationId: &org})
	if err != nil {
		return nil, fmt.Errorf("list triggers: %w", err)
	}
	webhooks, err := c.ListSessionWebhooks(ctx, org)
	if err != nil {
		return nil, fmt.Errorf("list session webhooks: %w", err)
	}
	return &projectconfig.CloudInput{OrganizationID: org, Triggers: triggers, Webhooks: webhooks}, nil
}

// localDaemonIdentity is the daemon this CLI talks to (see
// localApplyIdentity), without the warning: export reports an unknown id
// itself.
func localDaemonIdentity(env projectExportEnv) projectconfig.DaemonIdentity {
	id, _ := localApplyIdentity(env)
	return id
}

func fallbackDaemonName(env projectExportEnv, settingsPath string) string {
	hostname, _ := env.hostname()
	var settings config.Settings
	if settingsPath != "" {
		if s, err := config.LoadFrom(settingsPath); err == nil {
			settings = s
		}
	}
	return config.DaemonDisplayName(settings, hostname)
}

// existingProjectSlug reads the project slug of an existing file, so a
// re-export keeps it. A file that does not decode is reported, not fatal.
func existingProjectSlug(path string) (string, string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	cfg, _, err := projectconfig.Decode(data)
	if err != nil {
		return "", fmt.Sprintf("%s could not be read (%v); its project slug is not reused", path, err)
	}
	return cfg.Project, ""
}

// gitIgnores reports whether git ignores name inside repoDir.
func gitIgnores(ctx context.Context, repoDir, name string) bool {
	return exec.CommandContext(ctx, "git", "-C", repoDir, "check-ignore", "-q", name).Run() == nil
}

func printProjectWarnings(w io.Writer, warnings []string) {
	for _, warning := range warnings {
		_, _ = fmt.Fprintf(w, "warning: %s\n", warning)
	}
}

// factoryStageClassifier recognises a live job as a factory stage from the
// library stage table: the job's prompt must be exactly the stage prompt for
// the job's agent, so a prompt with extra arguments is not a factory job.
type factoryStageClassifier struct{}

func (factoryStageClassifier) Classify(job *pb.CronJob) (string, bool) {
	return projectconfig.DefaultFactoryStages().Classify(job)
}

// DefaultSchedule is a stage's schedule from the stage table, so the adapter
// is the core's projectconfig.FactoryStageReader.
func (factoryStageClassifier) DefaultSchedule(stage string) (string, bool) {
	return projectconfig.DefaultFactoryStages().DefaultSchedule(stage)
}

// The daemon client and the factory adapters satisfy the interfaces the
// project config engine declares by consumer.
var (
	_ projectconfig.FactoryStageReader = factoryStageClassifier{}
	_ projectconfig.FactoryResolver    = (*skills.FactoryCronResolver)(nil)
	_ projectconfig.RepoReader         = (*daemonrpc.LocalClient)(nil)
	_ projectconfig.LocalReader        = (*daemonrpc.LocalClient)(nil)
	_ projectconfig.LocalWriter        = (*daemonrpc.LocalClient)(nil)
)
