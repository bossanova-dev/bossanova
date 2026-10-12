package bossmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/projectconfig"
	"github.com/recurser/bossalib/vcs"
)

// Project config tools: export_project_config, diff_project_config and
// apply_project_config run the same projectconfig engine as `boss project
// export|diff|apply`, with the file's contents passed as the `config`
// argument. They are registered in tools.go (export, diff) and
// tools_mutating.go (apply), where the docs gates read tool names; this file
// holds their arguments, handlers and the adapters that turn a Backend into
// the engine's consumer-declared interfaces.
//
// Scope is chosen by the backend, never by an argument: a backend that
// implements ProjectCloudBackend (the hosted gateway) plans the cloud leg (the
// caller's triggers, the organization's session webhooks, crons on the
// caller's daemons); any other backend plans the local leg (this daemon's
// repo registration, settings and crons).
//
// MCP never resolves a secret reference and never performs a write that would
// mint a secret: the local leg plans with a nil SecretResolver, the cloud leg
// with CanMintSecrets and RotateSecrets off, so each such change is a skip
// the CLI can apply. Nothing is ever pruned over MCP.

// Project config scopes, reported in every result.
const (
	projectScopeLocal = "local"
	projectScopeCloud = "cloud"
)

// userHomeDir expands ~/ in daemons[].path and renders export paths; a
// package var so tests can pin it.
var userHomeDir = os.UserHomeDir

// ExportProjectConfigArgs is the typed argument struct for
// export_project_config.
type ExportProjectConfigArgs struct {
	WorkingDir     string `json:"working_dir,omitempty" jsonschema:"local: a directory in the repo's checkout (required)"`
	RepoOriginURL  string `json:"repo_origin_url,omitempty" jsonschema:"gateway: the repo's origin URL (required)"`
	Project        string `json:"project,omitempty" jsonschema:"project slug (default: the repo name)"`
	OrganizationID string `json:"organization_id,omitempty" jsonschema:"gateway: organization (required)"`
}

// DiffProjectConfigArgs is the typed argument struct for diff_project_config.
type DiffProjectConfigArgs struct {
	Config         map[string]any `json:"config" jsonschema:"the .boss-project.json contents"`
	WorkingDir     string         `json:"working_dir,omitempty" jsonschema:"local: the checkout used when no daemons[] entry matches"`
	OrganizationID string         `json:"organization_id,omitempty" jsonschema:"gateway: organization (default: cloud.organization)"`
}

// ApplyProjectConfigArgs is the typed argument struct for
// apply_project_config. Without dry_run:false it only plans.
type ApplyProjectConfigArgs struct {
	Config         map[string]any `json:"config" jsonschema:"the .boss-project.json contents"`
	WorkingDir     string         `json:"working_dir,omitempty" jsonschema:"local: the checkout used when no daemons[] entry matches"`
	OrganizationID string         `json:"organization_id,omitempty" jsonschema:"gateway: organization (default: cloud.organization)"`
	DryRun         *bool          `json:"dry_run,omitempty" jsonschema:"false writes; omitted or true only plans"`
	AllowBranch    bool           `json:"allow_branch,omitempty" jsonschema:"local: write from a checkout off the default branch"`
}

// exportProjectConfigResult is export_project_config's result. Content is the
// exact file the CLI exporter writes for the same state.
type exportProjectConfigResult struct {
	Scope    string   `json:"scope"`
	Content  string   `json:"content"`
	Warnings []string `json:"warnings"`
}

// projectConfigResult is diff_project_config's and apply_project_config's
// result. Plan is the engine plan as `boss project diff --json` renders it;
// Warnings are about the call (decode, identity, scope) rather than a change.
// Applied is set by apply only: true once the plan's writes ran. Result is
// the engine's apply result, and Error its failure, which is reported here
// rather than as a tool error because some writes may have landed.
type projectConfigResult struct {
	Scope     string             `json:"scope"`
	Plan      projectconfig.Plan `json:"plan"`
	Converged bool               `json:"converged"`
	Warnings  []string           `json:"warnings"`
	Applied   *bool              `json:"applied,omitempty"`
	Result    any                `json:"result,omitempty"`
	Error     string             `json:"error,omitempty"`
}

// projectJSONResult renders a project config result without HTML escaping,
// so a secret field reads as the literal "<secret>" placeholder, as in the
// CLI's --json output.
func projectJSONResult(v any) (*mcp.CallToolResult, any, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, nil, fmt.Errorf("marshal result: %w", err)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.TrimRight(b.String(), "\n")}}}, nil, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// decodeProjectConfig decodes and validates the config argument exactly as
// the CLI reads the file. It runs before any backend call.
func decodeProjectConfig(raw map[string]any) (*projectconfig.Config, []string, error) {
	if raw == nil {
		return nil, nil, errors.New("config is required: pass the .boss-project.json contents as a JSON object")
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("config: %w", err)
	}
	cfg, warnings, err := projectconfig.Decode(data)
	if err != nil {
		return nil, warnings, fmt.Errorf("config: %w", err)
	}
	if err := projectconfig.Validate(cfg); err != nil {
		return nil, warnings, fmt.Errorf("config is not a valid project config:\n%w", err)
	}
	return cfg, warnings, nil
}

// --- Handlers ----------------------------------------------------------------

func exportProjectConfigHandler(backend Backend) mcp.ToolHandlerFor[ExportProjectConfigArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args ExportProjectConfigArgs) (*mcp.CallToolResult, any, error) {
		var (
			scope    = projectScopeLocal
			data     []byte
			warnings []string
			err      error
		)
		if cloud, ok := backend.(ProjectCloudBackend); ok {
			scope = projectScopeCloud
			data, warnings, err = exportCloudProjectConfig(ctx, cloud, args)
		} else {
			data, warnings, err = exportLocalProjectConfig(ctx, backend, args)
		}
		if err != nil {
			return errorResult(err), nil, nil
		}
		return projectJSONResult(exportProjectConfigResult{Scope: scope, Content: string(data), Warnings: nonNil(warnings)})
	}
}

func diffProjectConfigHandler(backend Backend) mcp.ToolHandlerFor[DiffProjectConfigArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args DiffProjectConfigArgs) (*mcp.CallToolResult, any, error) {
		cfg, warnings, err := decodeProjectConfig(args.Config)
		if err != nil {
			return errorResult(err), nil, nil
		}
		run, err := planProjectConfig(ctx, backend, cfg, args.WorkingDir, args.OrganizationID)
		if err != nil {
			return errorResult(err), nil, nil
		}
		return projectJSONResult(run.result(warnings))
	}
}

func applyProjectConfigHandler(backend Backend) mcp.ToolHandlerFor[ApplyProjectConfigArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args ApplyProjectConfigArgs) (*mcp.CallToolResult, any, error) {
		cfg, warnings, err := decodeProjectConfig(args.Config)
		if err != nil {
			return errorResult(err), nil, nil
		}
		run, err := planProjectConfig(ctx, backend, cfg, args.WorkingDir, args.OrganizationID)
		if err != nil {
			return errorResult(err), nil, nil
		}
		out := run.result(warnings)
		applied := false
		out.Applied = &applied
		if args.DryRun == nil || *args.DryRun {
			return projectJSONResult(out)
		}
		if err := run.checkBranch(ctx, args.AllowBranch); err != nil {
			return errorResult(err), nil, nil
		}
		result, err := run.apply(ctx)
		applied = true
		out.Result = result
		if err != nil {
			out.Error = err.Error()
		}
		return projectJSONResult(out)
	}
}

// --- Planning ----------------------------------------------------------------

// projectRun is one planned project config call, on exactly one scope.
type projectRun struct {
	warnings []string
	local    *localProjectRun
	cloud    *cloudProjectRun
}

type localProjectRun struct {
	writer    projectconfig.LocalWriter
	identity  ProjectLocalBackend // nil when the backend cannot say
	cfg       *projectconfig.Config
	state     *projectconfig.LocalState
	plan      projectconfig.LocalPlan
	branchDir string
}

type cloudProjectRun struct {
	adapter projectCloudAdapter
	plan    projectconfig.CloudPlan
}

func planProjectConfig(ctx context.Context, backend Backend, cfg *projectconfig.Config, workingDir, orgArg string) (*projectRun, error) {
	if cloud, ok := backend.(ProjectCloudBackend); ok {
		return planCloudProjectConfig(ctx, cloud, cfg, orgArg)
	}
	return planLocalProjectConfig(ctx, backend, cfg, workingDir)
}

func (r *projectRun) result(decodeWarnings []string) projectConfigResult {
	out := projectConfigResult{Warnings: nonNil(append(append([]string(nil), decodeWarnings...), r.warnings...))}
	if r.cloud != nil {
		out.Scope, out.Plan = projectScopeCloud, r.cloud.plan.Plan
	} else {
		out.Scope, out.Plan = projectScopeLocal, r.local.plan.Plan
	}
	out.Converged = out.Plan.Converged()
	return out
}

// checkBranch is the local trust rule the CLI applies before any write: the
// applied checkout (the matched daemons[] entry's path, else working_dir)
// must be on the repo's default branch unless allowBranch. A detached or
// unreadable checkout, or an unknown default branch, counts as off it. The
// gateway has no checkout, so its cloud leg has no branch rule (as the CLI's
// --cloud leg checks only the local checkout). A plan that writes nothing
// passes.
func (r *projectRun) checkBranch(ctx context.Context, allowBranch bool) error {
	l := r.local
	if l == nil || allowBranch || !l.plan.HasWrites() {
		return nil
	}
	want := l.state.DefaultBranch(l.cfg)
	have, ok := "", false
	if l.identity != nil && l.branchDir != "" {
		have, ok = l.identity.CheckoutBranch(ctx, l.branchDir)
	}
	if ok && have != "" && want != "" && have == want {
		return nil
	}
	where := fmt.Sprintf("on branch %q", have)
	if !ok || have == "" {
		where = "not on a known branch"
	}
	return fmt.Errorf("refusing to apply: the checkout %s is %s, not the repo's default branch %q; nothing was written (pass allow_branch:true to apply anyway)", l.branchDir, where, want)
}

// apply runs the plan's writes. The error is a write failure inside the
// engine; the result says which changes landed.
func (r *projectRun) apply(ctx context.Context) (any, error) {
	if r.cloud != nil {
		sink := &refusingSecretSink{}
		res, err := projectconfig.ApplyCloud(ctx, r.cloud.adapter, sink, r.cloud.plan)
		if len(sink.minted) > 0 {
			err = errors.Join(err, fmt.Errorf("bosso minted a signing secret MCP cannot show (%s); rotate it with `boss project apply --cloud --rotate-secrets`", strings.Join(sink.minted, ", ")))
		}
		return &res, err
	}
	res, err := projectconfig.ApplyLocal(ctx, r.local.writer, r.local.plan)
	return &res, err
}

// refusingSecretSink is the cloud leg's SecretSink. With CanMintSecrets off
// no write mints a secret, so it is never called; if one ever is, the label
// (never the value) is recorded and the apply reports an error.
type refusingSecretSink struct{ minted []string }

func (s *refusingSecretSink) Secret(label, _ string) { s.minted = append(s.minted, label) }

func planLocalProjectConfig(ctx context.Context, backend Backend, cfg *projectconfig.Config, workingDir string) (*projectRun, error) {
	var warnings []string
	var identity projectconfig.DaemonIdentity
	lb, _ := backend.(ProjectLocalBackend)
	if lb != nil {
		id, warning, err := lb.ProjectDaemonIdentity(ctx)
		if err != nil {
			return nil, fmt.Errorf("read this daemon's identity: %w", err)
		}
		identity = id
		if warning != "" {
			warnings = append(warnings, warning)
		}
	} else {
		warnings = append(warnings, "this server cannot identify its daemon, so no daemons[] entry matches it and no cron is planned here")
	}
	checkout := workingDir
	if workingDir != "" {
		// A worktree of a registered repo stands for its main checkout, as
		// the CLI's --dir does.
		rc, err := backend.ResolveContext(ctx, workingDir)
		if err == nil && rc.GetRepo().GetLocalPath() != "" && vcs.CanonicalRepoURL(rc.GetRepo().GetOriginUrl()) == vcs.CanonicalRepoURL(cfg.Repo.OriginURL) {
			checkout = rc.GetRepo().GetLocalPath()
		}
	}
	home, _ := userHomeDir()
	opts := projectconfig.LocalOptions{
		Daemon:          identity,
		CheckoutPath:    checkout,
		HomeDir:         home,
		HostFreeFactory: true,
	}
	adapter := projectLocalAdapter{Backend: backend}
	st, err := projectconfig.ObserveLocal(ctx, cfg, adapter, projectconfig.HostFreeFactory(projectconfig.DefaultFactoryStages()), opts)
	if err != nil {
		return nil, err
	}
	branchDir := st.TargetPath
	if branchDir == checkout {
		branchDir = workingDir
	}
	return &projectRun{
		warnings: warnings,
		local: &localProjectRun{
			writer: adapter, identity: lb, cfg: cfg, state: st,
			plan: projectconfig.PlanLocal(cfg, st, opts), branchDir: branchDir,
		},
	}, nil
}

// projectCloudOrg is the organization the cloud leg applies to: the
// organization_id argument, else cloud.organization. Both present and
// different, or neither present, is an error before any request.
func projectCloudOrg(cfg *projectconfig.Config, orgArg string) (string, error) {
	orgArg = strings.TrimSpace(orgArg)
	fileOrg := ""
	if cfg.Cloud != nil && cfg.Cloud.Organization != nil {
		fileOrg = strings.TrimSpace(*cfg.Cloud.Organization)
	}
	switch {
	case orgArg != "" && fileOrg != "" && orgArg != fileOrg:
		return "", fmt.Errorf("organization_id %q disagrees with cloud.organization %q; drop one or make them match", orgArg, fileOrg)
	case orgArg != "":
		return orgArg, nil
	case fileOrg != "":
		return fileOrg, nil
	}
	return "", errors.New("organization_id is required (or set cloud.organization in the config)")
}

// cloudScopeWarning is the one warning every cloud-scope plan carries.
const cloudScopeWarning = "the hosted gateway plans only the cloud section and the crons the file places on your daemons; the repo registration, its settings and secrets are applied on each machine (boss project apply, or the local MCP server)"

func planCloudProjectConfig(ctx context.Context, backend ProjectCloudBackend, cfg *projectconfig.Config, orgArg string) (*projectRun, error) {
	org, err := projectCloudOrg(cfg, orgArg)
	if err != nil {
		return nil, err
	}
	opts := projectconfig.CloudOptions{
		Org:            org,
		CanMintSecrets: false,
		RotateSecrets:  false,
		Prune:          false,
		NoLocalDaemon:  true,
		RemoteFactory:  projectconfig.HostFreeFactory(projectconfig.DefaultFactoryStages()),
	}
	adapter := projectCloudAdapter{ProjectCloudBackend: backend}
	st, err := projectconfig.ObserveCloud(ctx, cfg, adapter, opts)
	if err != nil {
		return nil, err
	}
	return &projectRun{
		warnings: []string{cloudScopeWarning},
		cloud:    &cloudProjectRun{adapter: adapter, plan: projectconfig.PlanCloud(cfg, st, opts)},
	}, nil
}

// --- Export ------------------------------------------------------------------

// exportLocalProjectConfig exports this daemon's state for the repository
// working_dir is in, exactly as `boss project export` does for that checkout.
func exportLocalProjectConfig(ctx context.Context, backend Backend, args ExportProjectConfigArgs) ([]byte, []string, error) {
	if strings.TrimSpace(args.WorkingDir) == "" {
		return nil, nil, errors.New("working_dir is required: a directory in the repository's checkout")
	}
	rc, err := backend.ResolveContext(ctx, args.WorkingDir)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve %s: %w", args.WorkingDir, err)
	}
	repo := rc.GetRepo()
	if repo.GetId() == "" {
		return nil, nil, fmt.Errorf("%s is not in a repository registered with this daemon; register it first", args.WorkingDir)
	}
	repos, err := backend.ListRepos(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list repositories: %w", err)
	}
	for _, r := range repos {
		if r.GetId() == repo.GetId() {
			repo = r
		}
	}
	jobs, err := projectLocalAdapter{Backend: backend}.ListCronJobs(ctx, repo.GetId())
	if err != nil {
		return nil, nil, fmt.Errorf("list the repository's cron jobs: %w", err)
	}
	var identity projectconfig.DaemonIdentity
	if lb, ok := backend.(ProjectLocalBackend); ok {
		// An unknown id is reported by Export itself.
		if identity, _, err = lb.ProjectDaemonIdentity(ctx); err != nil {
			return nil, nil, fmt.Errorf("read this daemon's identity: %w", err)
		}
	}
	home, _ := userHomeDir()
	cfg, warnings, err := projectconfig.Export(projectconfig.ExportInput{
		Project:      args.Project,
		Repo:         repo,
		CronJobs:     jobs,
		Daemon:       identity,
		CheckoutPath: repo.GetLocalPath(),
		HomeDir:      home,
		Classifier:   projectconfig.DefaultFactoryStages(),
	})
	if err != nil {
		return nil, warnings, err
	}
	data, err := projectconfig.Encode(cfg)
	return data, warnings, err
}

// exportCloudProjectConfig exports the caller's triggers for one repository
// and the organization's session webhooks. daemons and crons are absent
// (unmanaged), so applying the file manages only the cloud classes. A webhook
// read refused by the owner check omits webhooks with a warning.
func exportCloudProjectConfig(ctx context.Context, backend ProjectCloudBackend, args ExportProjectConfigArgs) ([]byte, []string, error) {
	org := strings.TrimSpace(args.OrganizationID)
	if org == "" {
		return nil, nil, errors.New("organization_id is required on the hosted gateway")
	}
	origin := vcs.CanonicalRepoURL(args.RepoOriginURL)
	if origin == "" {
		return nil, nil, errors.New("repo_origin_url is required on the hosted gateway")
	}
	project := args.Project
	switch {
	case project == "":
		project = projectconfig.Slugify(origin[strings.LastIndex(origin, "/")+1:])
	case !projectconfig.ValidProject(project):
		return nil, nil, fmt.Errorf("invalid project %q: want a lowercase slug", project)
	}
	adapter := projectCloudAdapter{ProjectCloudBackend: backend}
	triggers, err := adapter.ListTriggers(ctx, &pb.ListTriggersRequest{OrganizationId: &org})
	if err != nil {
		return nil, nil, fmt.Errorf("list triggers: %w", err)
	}
	var warnings []string
	cloud := &projectconfig.Cloud{Organization: &org, Triggers: []projectconfig.Trigger{}}
	for _, t := range triggers {
		if vcs.CanonicalRepoURL(t.GetRepoOriginUrl()) != origin {
			continue
		}
		tr, w := projectconfig.ExportTrigger(t, nil)
		cloud.Triggers = append(cloud.Triggers, tr)
		warnings = append(warnings, w...)
	}
	webhooks, err := adapter.ListSessionWebhooks(ctx, org)
	switch {
	case connect.CodeOf(err) == connect.CodePermissionDenied:
		warnings = append(warnings, "cloud.webhooks: not exported; only an active owner of the organization can read its session webhooks")
	case err != nil:
		return nil, warnings, fmt.Errorf("list session webhooks: %w", err)
	default:
		cloud.Webhooks = []projectconfig.Webhook{}
		for _, wh := range webhooks {
			out, w := projectconfig.ExportWebhook(wh)
			cloud.Webhooks = append(cloud.Webhooks, out)
			warnings = append(warnings, w...)
		}
	}
	cfg := &projectconfig.Config{
		Version: projectconfig.SchemaVersion,
		Project: project,
		Repo:    projectconfig.Repo{OriginURL: origin},
		Cloud:   cloud,
	}
	if err := projectconfig.Validate(cfg); err != nil {
		return nil, warnings, fmt.Errorf("export: live state does not form a valid project config:\n%w", err)
	}
	data, err := projectconfig.Encode(cfg)
	return data, warnings, err
}

// --- Adapters ----------------------------------------------------------------

// projectLocalAdapter is a Backend as the local leg's LocalReader and
// LocalWriter. Backend.ListCronJobs lists every repo's jobs, so the
// repo-scoped read filters them; every other method passes through.
type projectLocalAdapter struct{ Backend }

func (a projectLocalAdapter) ListCronJobs(ctx context.Context, repoID string) ([]*pb.CronJob, error) {
	jobs, err := a.Backend.ListCronJobs(ctx)
	if err != nil {
		return nil, err
	}
	var out []*pb.CronJob
	for _, j := range jobs {
		if j.GetRepoId() == repoID {
			out = append(out, j)
		}
	}
	return out, nil
}

// projectCloudAdapter is a ProjectCloudBackend as the cloud leg's reader and
// writer: it unwraps the response types the hosted families return. Errors
// pass through unchanged, so bosso's paid-gate, owner and validation messages
// reach the caller as bosso wrote them.
type projectCloudAdapter struct{ ProjectCloudBackend }

func (a projectCloudAdapter) ListTriggers(ctx context.Context, req *pb.ListTriggersRequest) ([]*pb.Trigger, error) {
	resp, err := a.ProjectCloudBackend.ListTriggers(ctx, req)
	if err != nil {
		return nil, err
	}
	return resp.GetTriggers(), nil
}

func (a projectCloudAdapter) ListSessionWebhooks(ctx context.Context, organizationID string) ([]*pb.SessionWebhook, error) {
	resp, err := a.ProjectCloudBackend.ListSessionWebhooks(ctx, &pb.ListSessionWebhooksRequest{OrganizationId: organizationID})
	if err != nil {
		return nil, err
	}
	return resp.GetWebhooks(), nil
}

func (a projectCloudAdapter) DeleteSessionWebhook(ctx context.Context, organizationID, id string) error {
	return a.ProjectCloudBackend.DeleteSessionWebhook(ctx, &pb.DeleteSessionWebhookRequest{OrganizationId: organizationID, Id: id})
}

var (
	_ projectconfig.LocalReader      = projectLocalAdapter{}
	_ projectconfig.LocalWriter      = projectLocalAdapter{}
	_ projectconfig.CloudApplyReader = projectCloudAdapter{}
	_ projectconfig.CloudWriter      = projectCloudAdapter{}
)
