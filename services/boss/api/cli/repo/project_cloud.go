package repo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/recurser/boss/internal/adapter/cloudrpc"

	"github.com/recurser/boss/api/cli/cliutil"

	"github.com/spf13/cobra"

	"github.com/recurser/bossalib/kit/projectconfig"
	"github.com/recurser/bossalib/kit/vcs"
)

// The --cloud leg of `boss project diff|apply`: the caller's own cloud
// triggers, the organization's session webhooks, and the crons the file places
// on the caller's other daemons (through the cron proxies, never this daemon,
// which the local leg writes over its socket). The engine is
// projectconfig.ObserveCloud / PlanCloud / ApplyCloud; this file resolves the
// organization, dials bosso with the keychain login, and prints each secret
// bosso mints to stderr as it arrives.

// projectCloudApplyClient is the cloud surface `boss project diff|apply
// --cloud` uses. *cloudrpc.RemoteClient satisfies it; tests substitute a fake
// orchestrator behind a real RemoteClient.
type projectCloudApplyClient interface {
	CloudClient
	projectconfig.CloudApplyReader
	projectconfig.CloudWriter
}

var _ projectCloudApplyClient = (*cloudrpc.RemoteClient)(nil)

// errProjectApplyNotLoggedIn is the failure for diff/apply --cloud with no
// usable login.
var errProjectApplyNotLoggedIn = errors.New("applying cloud triggers and webhooks needs a cloud login: run 'boss login' first")

func (g *Group) defaultProjectCloudApply(cmd *cobra.Command) (projectCloudApplyClient, error) {
	remote, err := g.options.CloudRemote(cmd, errProjectApplyNotLoggedIn)
	if err != nil {
		return nil, err
	}
	return remote, nil
}

// projectCloudFlags are the --cloud options diff and apply share.
type projectCloudFlags struct {
	enabled       bool
	org           string
	rotateSecrets bool
}

func readProjectCloudFlags(cmd *cobra.Command) projectCloudFlags {
	var f projectCloudFlags
	f.enabled, _ = cmd.Flags().GetBool("cloud")
	f.org, _ = cmd.Flags().GetString("org")
	f.org = strings.TrimSpace(f.org)
	if cmd.Flags().Lookup("rotate-secrets") != nil {
		f.rotateSecrets, _ = cmd.Flags().GetBool("rotate-secrets")
	}
	return f
}

// checkProjectCloudFlags rejects flag combinations before any request: the
// cloud-only flags without --cloud, and an --org that disagrees with the
// file's cloud.organization.
func checkProjectCloudFlags(cfg *projectconfig.Config, f projectCloudFlags) error {
	if !f.enabled {
		if f.org != "" || f.rotateSecrets {
			return errors.New("--org and --rotate-secrets apply only with --cloud")
		}
		return nil
	}
	if fileOrg := projectFileOrg(cfg); fileOrg != "" && f.org != "" && fileOrg != f.org {
		return fmt.Errorf("--org %q disagrees with cloud.organization %q in %s; drop --org or fix the file", f.org, fileOrg, projectconfig.FileName)
	}
	return nil
}

func projectFileOrg(cfg *projectconfig.Config) string {
	if cfg.Cloud == nil || cfg.Cloud.Organization == nil {
		return ""
	}
	return strings.TrimSpace(*cfg.Cloud.Organization)
}

// resolveProjectCloudOrg picks the organization: the file's
// cloud.organization, else --org, else the repository's organization mapping,
// else the caller's only organization.
func resolveProjectCloudOrg(cmd *cobra.Command, c CloudClient, cfg *projectconfig.Config, f projectCloudFlags) (string, error) {
	if org := projectFileOrg(cfg); org != "" {
		return org, nil
	}
	if f.org != "" {
		return f.org, nil
	}
	mapping, err := c.GetRepoOrganization(commandContext(cmd), vcs.CanonicalRepoURL(cfg.Repo.OriginURL))
	if err != nil {
		return "", fmt.Errorf("read the repository's organization: %w", err)
	}
	if org := mapping.GetOrganizationId(); org != "" {
		return org, nil
	}
	return cliutil.ResolveOrganizationID(cmd, c)
}

// projectCloudRun is one planned cloud leg.
type projectCloudRun struct {
	c    projectCloudApplyClient
	plan projectconfig.CloudPlan
}

// planCloudProject dials bosso, resolves the organization and plans the
// file's cloud section. Its first request after resolving the organization
// is the paid-gate precheck (ListTriggers): any failure returns before the
// caller writes anything on any leg.
func planCloudProject(ctx context.Context, cmd *cobra.Command, env projectApplyEnv, proj *loadedProject, f projectCloudFlags, prune bool) (*projectCloudRun, error) {
	c, err := env.cloudApply(cmd)
	if err != nil {
		return nil, err
	}
	org, err := resolveProjectCloudOrg(cmd, c, proj.cfg, f)
	if err != nil {
		return nil, err
	}
	// The local leg's identity excludes this daemon from the cron fan-out;
	// its warning (an unknown id) is reported by the local leg.
	local, _ := env.identity(env.export)
	opts := projectconfig.CloudOptions{
		Org:            org,
		Prune:          prune,
		RotateSecrets:  f.rotateSecrets,
		CanMintSecrets: true,
		Secrets:        projectconfig.EnvResolver(env.getenv),
		LocalDaemon:    local,
		RemoteFactory:  projectconfig.HostFreeFactory(projectconfig.DefaultFactoryStages()),
	}
	st, err := projectconfig.ObserveCloud(ctx, proj.cfg, c, opts)
	if err != nil {
		return nil, err
	}
	return &projectCloudRun{c: c, plan: projectconfig.PlanCloud(proj.cfg, st, opts)}, nil
}

// combinedPlan renders the local and cloud legs as one plan.
func combinedPlan(local projectconfig.LocalPlan, cloud *projectCloudRun) projectconfig.Plan {
	out := projectconfig.Plan{Changes: append([]projectconfig.Change(nil), local.Changes...), Warnings: append([]string(nil), local.Warnings...)}
	if cloud != nil {
		out.Changes = append(out.Changes, cloud.plan.Changes...)
		out.Warnings = append(out.Warnings, cloud.plan.Warnings...)
	}
	return out
}

// stderrSecretSink prints each secret bosso mints the moment it arrives. It
// writes only to the command's stderr, never to stdout (where --json goes).
type stderrSecretSink struct{ w io.Writer }

func (s stderrSecretSink) Secret(label, value string) {
	_, _ = fmt.Fprintf(s.w, "%s signing secret (shown once, store it now): %s\n", label, value)
}

// projectCloudBlocked reports a cloud leg that could not converge: some
// change is a skip or a conflict.
func projectCloudBlocked(plan projectconfig.CloudPlan) error {
	var n int
	for _, c := range plan.Changes {
		if c.Action == projectconfig.ActionConflict || c.Action == projectconfig.ActionSkip {
			n++
		}
	}
	return fmt.Errorf("%d cloud change(s) were skipped or conflicted; see the plan above", n)
}
