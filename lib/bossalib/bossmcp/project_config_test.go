package bossmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/projectconfig"
	"google.golang.org/protobuf/proto"
)

const (
	projectMarker   = "boss-project:widget"
	projectOrigin   = "https://github.com/acme/widget"
	projectCheckout = "/home/me/src/widget"
	projectOrg      = "org-1"
)

// pinHome pins the home directory the project tools expand ~/ against.
func pinHome(t *testing.T) {
	t.Helper()
	prev := userHomeDir
	userHomeDir = func() (string, error) { return "/home/me", nil }
	t.Cleanup(func() { userHomeDir = prev })
}

// --- Local fake --------------------------------------------------------------

// projectDaemon is an in-memory bossd for the project config tools: it stores
// repos and cron jobs the way the daemon does (marker echoed) and records
// every call, reads included, in one ordered log.
type projectDaemon struct {
	fakeBackend
	repos  []*pb.Repo
	jobs   []*pb.CronJob
	valid  map[string]*pb.ValidateRepoPathResponse
	calls  []string
	nextID int

	repoUpdates []*pb.UpdateRepoRequest
}

func newProjectDaemon() *projectDaemon {
	return &projectDaemon{valid: map[string]*pb.ValidateRepoPathResponse{
		projectCheckout: {IsValid: true, OriginUrl: "git@github.com:acme/widget.git", DefaultBranch: "main"},
	}}
}

func (d *projectDaemon) call(s string) { d.calls = append(d.calls, s) }

func (d *projectDaemon) id(prefix string) string {
	d.nextID++
	return fmt.Sprintf("%s-%d", prefix, d.nextID)
}

// writes is every write call, in order.
func (d *projectDaemon) writes() []string {
	var out []string
	for _, c := range d.calls {
		for _, p := range []string{"register ", "update-repo", "create-cron ", "update-cron ", "delete-cron "} {
			if strings.HasPrefix(c, p) {
				out = append(out, c)
			}
		}
	}
	return out
}

func (d *projectDaemon) ResolveContext(_ context.Context, dir string) (*pb.ResolveContextResponse, error) {
	d.call("resolve " + dir)
	for _, r := range d.repos {
		if dir == r.GetLocalPath() || strings.HasPrefix(dir, r.GetLocalPath()+"/") {
			return &pb.ResolveContextResponse{Repo: proto.CloneOf(r)}, nil
		}
	}
	return &pb.ResolveContextResponse{}, nil
}

func (d *projectDaemon) ListRepos(context.Context) ([]*pb.Repo, error) {
	d.call("list-repos")
	out := make([]*pb.Repo, len(d.repos))
	for i, r := range d.repos {
		out[i] = proto.CloneOf(r)
	}
	return out, nil
}

func (d *projectDaemon) ValidateRepoPath(_ context.Context, p string) (*pb.ValidateRepoPathResponse, error) {
	d.call("validate " + p)
	if v, ok := d.valid[p]; ok {
		return proto.CloneOf(v), nil
	}
	return &pb.ValidateRepoPathResponse{ErrorMessage: "not a git repository"}, nil
}

func (d *projectDaemon) RegisterRepo(_ context.Context, req *pb.RegisterRepoRequest) (*pb.Repo, error) {
	d.call("register " + req.GetLocalPath())
	r := &pb.Repo{Id: d.id("repo"), LocalPath: req.GetLocalPath(), DisplayName: req.GetDisplayName(),
		DefaultBaseBranch: req.GetDefaultBaseBranch(), OriginUrl: d.valid[req.GetLocalPath()].GetOriginUrl()}
	d.repos = append(d.repos, r)
	return proto.CloneOf(r), nil
}

func (d *projectDaemon) UpdateRepo(_ context.Context, req *pb.UpdateRepoRequest) (*pb.Repo, error) {
	d.call("update-repo " + req.GetId())
	d.repoUpdates = append(d.repoUpdates, proto.CloneOf(req))
	for _, r := range d.repos {
		if r.GetId() == req.GetId() {
			if req.CanAutoRepair != nil {
				r.CanAutoRepair = req.GetCanAutoRepair()
			}
			return proto.CloneOf(r), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, errors.New("no such repo"))
}

func (d *projectDaemon) ListCronJobs(context.Context) ([]*pb.CronJob, error) {
	d.call("list-crons")
	out := make([]*pb.CronJob, len(d.jobs))
	for i, j := range d.jobs {
		out[i] = proto.CloneOf(j)
	}
	return out, nil
}

func (d *projectDaemon) CreateCronJob(_ context.Context, req *pb.CreateCronJobRequest) (*pb.CronJob, error) {
	d.call("create-cron " + req.GetName())
	j := &pb.CronJob{Id: d.id("cj"), RepoId: req.GetRepoId(), Name: req.GetName(), Prompt: req.GetPrompt(), Schedule: req.GetSchedule(),
		Timezone: req.GetTimezone(), IsEnabled: req.GetIsEnabled(), AgentName: req.GetAgentName(), Model: req.GetModel(),
		GateCommand: req.GetGateCommand(), ManagedBy: req.GetManagedBy(), ShouldRunSetupCommand: req.ShouldRunSetupCommand == nil || req.GetShouldRunSetupCommand()}
	d.jobs = append(d.jobs, j)
	return proto.CloneOf(j), nil
}

func (d *projectDaemon) UpdateCronJob(_ context.Context, req *pb.UpdateCronJobRequest) (*pb.CronJob, error) {
	for _, j := range d.jobs {
		if j.GetId() != req.GetId() {
			continue
		}
		d.call("update-cron " + j.GetName())
		if req.Name != nil {
			j.Name = req.GetName()
		}
		if req.Prompt != nil {
			j.Prompt = req.GetPrompt()
		}
		if req.Schedule != nil {
			j.Schedule = req.GetSchedule()
		}
		if req.GateCommand != nil {
			j.GateCommand = req.GetGateCommand()
		}
		if req.AgentName != nil {
			j.AgentName = req.GetAgentName()
		}
		if req.IsEnabled != nil {
			j.IsEnabled = req.GetIsEnabled()
		}
		if req.ManagedBy != nil {
			j.ManagedBy = req.GetManagedBy()
		}
		return proto.CloneOf(j), nil
	}
	return nil, connect.NewError(connect.CodeNotFound, errors.New("no such cron job"))
}

func (d *projectDaemon) DeleteCronJob(_ context.Context, id string) error {
	d.call("delete-cron " + id)
	return nil
}

func (d *projectDaemon) job(name string) *pb.CronJob {
	for _, j := range d.jobs {
		if j.GetName() == name {
			return j
		}
	}
	return nil
}

// projectLocalDaemon is a projectDaemon that also implements
// ProjectLocalBackend, as the local MCP server's backend does.
type projectLocalDaemon struct {
	*projectDaemon
	identity projectconfig.DaemonIdentity
	warning  string
	branch   string
	detached bool
}

var _ ProjectLocalBackend = (*projectLocalDaemon)(nil)

func newProjectLocal() *projectLocalDaemon {
	return &projectLocalDaemon{projectDaemon: newProjectDaemon(), identity: projectconfig.DaemonIdentity{ID: "d-1", DisplayName: "laptop"}, branch: "main"}
}

func (d *projectLocalDaemon) ProjectDaemonIdentity(context.Context) (projectconfig.DaemonIdentity, string, error) {
	d.call("identity")
	return d.identity, d.warning, nil
}

func (d *projectLocalDaemon) CheckoutBranch(_ context.Context, dir string) (string, bool) {
	d.call("branch " + dir)
	if d.detached {
		return "", false
	}
	return d.branch, true
}

// --- Calling the tools -------------------------------------------------------

// projectToolResult is the JSON every project config tool returns.
type projectToolResult struct {
	Scope     string             `json:"scope"`
	Content   string             `json:"content"`
	Plan      projectconfig.Plan `json:"plan"`
	Converged bool               `json:"converged"`
	Warnings  []string           `json:"warnings"`
	Applied   *bool              `json:"applied"`
	Result    json.RawMessage    `json:"result"`
	Error     string             `json:"error"`
}

func callProject(t *testing.T, backend Backend, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	return callNoteTool(t, newConnectedClient(t, backend, Options{}), name, args)
}

func callProjectOK(t *testing.T, backend Backend, name string, args map[string]any) projectToolResult {
	t.Helper()
	res := callProject(t, backend, name, args)
	if res.IsError {
		t.Fatalf("%s returned an error result: %s", name, textOf(t, res))
	}
	var out projectToolResult
	if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
		t.Fatalf("%s result: %v\n%s", name, err, textOf(t, res))
	}
	return out
}

func callProjectErr(t *testing.T, backend Backend, name string, args map[string]any) string {
	t.Helper()
	res := callProject(t, backend, name, args)
	if !res.IsError {
		t.Fatalf("%s succeeded, want an error result: %s", name, textOf(t, res))
	}
	return textOf(t, res)
}

// projectConfig decodes a project file into the map the config argument
// carries.
func projectConfig(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("config fixture: %v\n%s", err, body)
	}
	return m
}

// localProjectConfig is a one-daemon file whose daemons entry pins d-1.
func localProjectConfig(t *testing.T, crons string) map[string]any {
	t.Helper()
	return projectConfig(t, `{"version": 1, "project": "widget",
	  "repo": {"originUrl": "`+projectOrigin+`"},
	  "daemons": [{"name": "laptop", "daemonId": "d-1", "path": "~/src/widget"}],
	  "crons": `+crons+`}`)
}

const nightlyCron = `{"name": "nightly", "prompt": "/nightly", "schedule": "0 3 * * *"}`

func changeIn(t *testing.T, p projectconfig.Plan, r projectconfig.Resource, identity string) projectconfig.Change {
	t.Helper()
	var found []projectconfig.Change
	for _, c := range p.Changes {
		if c.Resource == r && c.Identity == identity {
			found = append(found, c)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want one %s change %q, got %d in\n%s", r, identity, len(found), p.Text())
	}
	return found[0]
}

func applied(r projectToolResult) bool { return r.Applied != nil && *r.Applied }

// --- Local apply ---------------------------------------------------------------

func TestApplyProjectConfigTool(t *testing.T) {
	pinHome(t)

	t.Run("without dry_run:false it only plans", func(t *testing.T) {
		d := newProjectLocal()
		out := callProjectOK(t, d, "apply_project_config", map[string]any{"config": localProjectConfig(t, `[`+nightlyCron+`]`), "working_dir": projectCheckout})
		if out.Scope != "local" || applied(out) || out.Applied == nil || out.Converged {
			t.Fatalf("result %+v", out)
		}
		if c := changeIn(t, out.Plan, projectconfig.ResourceRepo, projectOrigin); c.Action != projectconfig.ActionCreate {
			t.Fatalf("repo: %+v", c)
		}
		if c := changeIn(t, out.Plan, projectconfig.ResourceCron, "nightly"); c.Action != projectconfig.ActionCreate || !c.NeedsConfirmation {
			t.Fatalf("cron: %+v", c)
		}
		if w := d.writes(); len(w) != 0 {
			t.Fatalf("a plan-only apply wrote %q", w)
		}
		if slices.ContainsFunc(d.calls, func(c string) bool { return strings.HasPrefix(c, "branch ") }) {
			t.Fatalf("a dry run read the branch: %q", d.calls)
		}
		out = callProjectOK(t, d, "apply_project_config", map[string]any{"config": localProjectConfig(t, `[`+nightlyCron+`]`), "working_dir": projectCheckout, "dry_run": true})
		if applied(out) || len(d.writes()) != 0 {
			t.Fatalf("dry_run:true wrote: %+v %q", out, d.writes())
		}
	})

	t.Run("dry_run:false on the default branch performs exactly the plan's writes, then converges", func(t *testing.T) {
		d := newProjectLocal()
		cfg := localProjectConfig(t, `[`+nightlyCron+`]`)
		out := callProjectOK(t, d, "apply_project_config", map[string]any{"config": cfg, "working_dir": projectCheckout, "dry_run": false})
		if !applied(out) || out.Error != "" {
			t.Fatalf("result %+v", out)
		}
		if w := d.writes(); !slices.Equal(w, []string{"register " + projectCheckout, "create-cron nightly"}) {
			t.Fatalf("writes %q", w)
		}
		if j := d.job("nightly"); j.GetManagedBy() != projectMarker || j.GetRepoId() != d.repos[0].GetId() {
			t.Fatalf("created cron %+v", j)
		}
		var res projectconfig.LocalResult
		if err := json.Unmarshal(out.Result, &res); err != nil || len(res.Applied) != 2 || len(res.Failed) != 0 {
			t.Fatalf("engine result %s (%v)", out.Result, err)
		}
		again := callProjectOK(t, d, "diff_project_config", map[string]any{"config": cfg, "working_dir": projectCheckout})
		if !again.Converged {
			t.Fatalf("second plan not converged:\n%s", again.Plan.Text())
		}
		before := len(d.writes())
		if out := callProjectOK(t, d, "apply_project_config", map[string]any{"config": cfg, "working_dir": projectCheckout, "dry_run": false}); !applied(out) || len(d.writes()) != before {
			t.Fatalf("a converged apply wrote: %q", d.writes()[before:])
		}
	})

	t.Run("the default-branch rule", func(t *testing.T) {
		cases := []struct {
			name        string
			branch      string
			detached    bool
			allowBranch bool
			wantWrites  bool
		}{
			{name: "off the default branch is refused", branch: "feature"},
			{name: "allow_branch overrides", branch: "feature", allowBranch: true, wantWrites: true},
			{name: "detached HEAD is refused", detached: true},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				d := newProjectLocal()
				d.branch, d.detached = c.branch, c.detached
				args := map[string]any{"config": localProjectConfig(t, `[`+nightlyCron+`]`), "working_dir": projectCheckout, "dry_run": false}
				if c.allowBranch {
					args["allow_branch"] = true
				}
				if !c.wantWrites {
					msg := callProjectErr(t, d, "apply_project_config", args)
					if !strings.Contains(msg, "default branch") || !strings.Contains(msg, "nothing was written") {
						t.Fatalf("refusal %q", msg)
					}
					if w := d.writes(); len(w) != 0 {
						t.Fatalf("a refused apply wrote %q", w)
					}
					return
				}
				if out := callProjectOK(t, d, "apply_project_config", args); !applied(out) || len(d.writes()) == 0 {
					t.Fatalf("allow_branch did not write: %+v", out)
				}
			})
		}
	})

	t.Run("a config that does not decode or validate is refused with zero backend calls", func(t *testing.T) {
		bad := map[string]string{
			"newer version": `{"version": 2, "project": "widget", "repo": {"originUrl": "` + projectOrigin + `"}}`,
			"literal secret": `{"version": 1, "project": "widget", "repo": {"originUrl": "` + projectOrigin + `",
			  "secrets": {"linearApiKey": "lin_api_pasted_literal"}}}`,
			"absolute gate": `{"version": 1, "project": "widget", "repo": {"originUrl": "` + projectOrigin + `"},
			  "daemons": [{"name": "laptop"}], "crons": [{"name": "g", "prompt": "/g", "schedule": "0 3 * * *", "gateCommand": "/usr/local/bin/gate"}]}`,
			"dot-dot gate": `{"version": 1, "project": "widget", "repo": {"originUrl": "` + projectOrigin + `"},
			  "daemons": [{"name": "laptop"}], "crons": [{"name": "g", "prompt": "/g", "schedule": "0 3 * * *", "gateCommand": "./../gate.sh"}]}`,
		}
		paths := map[string]string{"newer version": "version", "literal secret": "repo.secrets.linearApiKey", "absolute gate": "crons[0].gateCommand", "dot-dot gate": "crons[0].gateCommand"}
		for name, body := range bad {
			t.Run(name, func(t *testing.T) {
				for _, tool := range []string{"apply_project_config", "diff_project_config"} {
					d := newProjectLocal()
					args := map[string]any{"config": projectConfig(t, body), "working_dir": projectCheckout}
					if tool == "apply_project_config" {
						args["dry_run"] = false
					}
					msg := callProjectErr(t, d, tool, args)
					if !strings.Contains(msg, paths[name]) {
						t.Fatalf("%s: error %q does not name %s", tool, msg, paths[name])
					}
					if strings.Contains(msg, "lin_api_pasted_literal") {
						t.Fatalf("%s: the error echoed a pasted secret: %q", tool, msg)
					}
					if len(d.calls) != 0 {
						t.Fatalf("%s: backend calls %q", tool, d.calls)
					}
				}
			})
		}
		d := newProjectLocal()
		if msg := callProjectErr(t, d, "diff_project_config", map[string]any{"working_dir": projectCheckout}); !strings.Contains(msg, "config") || len(d.calls) != 0 {
			t.Fatalf("missing config: %q calls %q", msg, d.calls)
		}
	})

	t.Run("an unknown key is a warning, not an error", func(t *testing.T) {
		d := newProjectLocal()
		cfg := localProjectConfig(t, `[`+nightlyCron+`]`)
		cfg["futureSetting"] = true
		out := callProjectOK(t, d, "diff_project_config", map[string]any{"config": cfg, "working_dir": projectCheckout})
		if !slices.ContainsFunc(out.Warnings, func(w string) bool { return strings.Contains(w, "futureSetting") }) {
			t.Fatalf("warnings %q", out.Warnings)
		}
	})

	t.Run("factory crons: no row is a create-via-cli skip, an unmarked /boss-build row is adopted with its gate untouched", func(t *testing.T) {
		d := newProjectLocal()
		d.repos = []*pb.Repo{{Id: "repo-1", LocalPath: projectCheckout, OriginUrl: projectOrigin, DefaultBaseBranch: "main"}}
		const handGate = `node "$HOME/.claude/skills/boss-build/toolbox/cron-gates/boss-build.mjs"`
		d.jobs = []*pb.CronJob{{Id: "cj-hand", RepoId: "repo-1", Name: "my build", Prompt: "/boss-build", Schedule: "*/15 * * * *", IsEnabled: true, AgentName: "claude", GateCommand: handGate}}
		cfg := localProjectConfig(t, `[{"name": "factory-plan", "factoryStage": "plan"}, {"name": "factory-build", "factoryStage": "build"}]`)
		out := callProjectOK(t, d, "apply_project_config", map[string]any{"config": cfg, "working_dir": projectCheckout, "dry_run": false})
		if c := changeIn(t, out.Plan, projectconfig.ResourceCron, "factory-plan"); c.Action != projectconfig.ActionSkip || c.Reason != projectconfig.ReasonCreateViaCLI {
			t.Fatalf("factory-plan: %+v", c)
		}
		if c := changeIn(t, out.Plan, projectconfig.ResourceCron, "factory-build"); c.Action != projectconfig.ActionAdopt {
			t.Fatalf("factory-build: %+v", c)
		}
		if w := d.writes(); !slices.Equal(w, []string{"update-cron my build"}) {
			t.Fatalf("writes %q", w)
		}
		if j := d.job("factory-build"); j.GetManagedBy() != projectMarker || j.GetGateCommand() != handGate {
			t.Fatalf("adopted row %+v", j)
		}
	})

	t.Run("another project's marker on a same-name cron is a conflict and never written", func(t *testing.T) {
		d := newProjectLocal()
		d.repos = []*pb.Repo{{Id: "repo-1", LocalPath: projectCheckout, OriginUrl: projectOrigin, DefaultBaseBranch: "main"}}
		d.jobs = []*pb.CronJob{{Id: "cj-1", RepoId: "repo-1", Name: "nightly", Prompt: "/other", Schedule: "0 1 * * *", IsEnabled: true, ManagedBy: "boss-project:other"}}
		out := callProjectOK(t, d, "apply_project_config", map[string]any{"config": localProjectConfig(t, `[`+nightlyCron+`]`), "working_dir": projectCheckout, "dry_run": false})
		if c := changeIn(t, out.Plan, projectconfig.ResourceCron, "nightly"); c.Action != projectconfig.ActionConflict || c.Reason != projectconfig.ReasonManagedByOther {
			t.Fatalf("nightly: %+v", c)
		}
		if w := d.writes(); len(w) != 0 {
			t.Fatalf("writes %q", w)
		}
	})

	t.Run("a backend that cannot identify its daemon skips every cron and refuses to write", func(t *testing.T) {
		d := newProjectDaemon()
		args := map[string]any{"config": localProjectConfig(t, `[`+nightlyCron+`]`), "working_dir": projectCheckout}
		out := callProjectOK(t, d, "apply_project_config", args)
		if c := changeIn(t, out.Plan, projectconfig.ResourceCron, "nightly"); c.Action != projectconfig.ActionSkip || c.Reason != projectconfig.ReasonDaemonUnmatched {
			t.Fatalf("nightly: %+v", c)
		}
		args["dry_run"] = false
		if msg := callProjectErr(t, d, "apply_project_config", args); !strings.Contains(msg, "default branch") {
			t.Fatalf("refusal %q", msg)
		}
		if w := d.writes(); len(w) != 0 {
			t.Fatalf("writes %q", w)
		}
	})

	t.Run("a worktree of the registered repo stands for its main checkout", func(t *testing.T) {
		d := newProjectLocal()
		d.repos = []*pb.Repo{{Id: "repo-1", LocalPath: projectCheckout, OriginUrl: projectOrigin, DefaultBaseBranch: "main"}}
		// No daemons entry places the repo, so working_dir does.
		cfg := projectConfig(t, `{"version": 1, "project": "widget", "repo": {"originUrl": "`+projectOrigin+`"}}`)
		out := callProjectOK(t, d, "diff_project_config", map[string]any{"config": cfg, "working_dir": projectCheckout + "/.worktrees/feature"})
		if c := changeIn(t, out.Plan, projectconfig.ResourceRepo, projectOrigin); c.Action != projectconfig.ActionUnchanged {
			t.Fatalf("repo: %+v", c)
		}
	})
}

// --- Secrets -----------------------------------------------------------------

func TestProjectConfigToolsNeverResolveSecrets(t *testing.T) {
	pinHome(t)

	t.Run("local: a resolvable secret reference is still a skip and never written", func(t *testing.T) {
		t.Setenv("BOSS_LINEAR_API_KEY", "lin_value_in_the_environment")
		d := newProjectLocal()
		d.repos = []*pb.Repo{{Id: "repo-1", LocalPath: projectCheckout, OriginUrl: projectOrigin, DefaultBaseBranch: "main"}}
		cfg := projectConfig(t, `{"version": 1, "project": "widget",
		  "repo": {"originUrl": "`+projectOrigin+`", "settings": {"canAutoRepair": true},
		    "secrets": {"linearApiKey": {"env": "BOSS_LINEAR_API_KEY"}}},
		  "daemons": [{"name": "laptop", "daemonId": "d-1", "path": "~/src/widget"}]}`)
		out := callProjectOK(t, d, "apply_project_config", map[string]any{"config": cfg, "working_dir": projectCheckout, "dry_run": false})
		if c := changeIn(t, out.Plan, projectconfig.ResourceRepo, "secrets.linearApiKey"); c.Action != projectconfig.ActionSkip || c.Reason != projectconfig.ReasonSecretUnresolved {
			t.Fatalf("secret: %+v", c)
		}
		if len(d.repoUpdates) != 1 {
			t.Fatalf("want the settings update only, got %d repo updates", len(d.repoUpdates))
		}
		for _, u := range d.repoUpdates {
			if u.LinearKey != nil || u.SentryKey != nil {
				t.Fatalf("a repo update carried a secret: %+v", u)
			}
		}
		if strings.Contains(fmt.Sprint(out), "lin_value_in_the_environment") {
			t.Fatal("the secret value reached the result")
		}
	})

	t.Run("cloud: minting and secret-referencing creates are skips and send nothing", func(t *testing.T) {
		t.Setenv("BOSS_HOOK_SECRET", "hook_value_in_the_environment")
		c := newProjectCloud()
		cfg := cloudProjectConfig(t, `{"organization": "`+projectOrg+`",
		  "triggers": [{"name": "deploy", "type": "http", "launch": {"promptTemplate": "Deploy"}, "placement": {"mode": "first-available"}}],
		  "webhooks": [
		    {"url": "https://hooks.example.com/minted", "eventTypes": ["session.merged"]},
		    {"url": "https://hooks.example.com/ref", "eventTypes": ["session.merged"], "secret": {"env": "BOSS_HOOK_SECRET"}}]}`, "")
		out := callProjectOK(t, c, "apply_project_config", map[string]any{"config": cfg, "dry_run": false})
		if ch := changeIn(t, out.Plan, projectconfig.ResourceTrigger, "deploy"); ch.Action != projectconfig.ActionSkip || ch.Reason != projectconfig.ReasonCreateViaCLI {
			t.Fatalf("http trigger: %+v", ch)
		}
		if ch := changeIn(t, out.Plan, projectconfig.ResourceWebhook, "https://hooks.example.com/minted"); ch.Action != projectconfig.ActionSkip || ch.Reason != projectconfig.ReasonCreateViaCLI {
			t.Fatalf("secret-less webhook: %+v", ch)
		}
		if ch := changeIn(t, out.Plan, projectconfig.ResourceWebhook, "https://hooks.example.com/ref"); ch.Action != projectconfig.ActionSkip || ch.Reason != projectconfig.ReasonSecretUnresolved {
			t.Fatalf("secret-referencing webhook: %+v", ch)
		}
		if w := c.writes(); len(w) != 0 {
			t.Fatalf("writes %q", w)
		}
		if out.Error != "" || strings.Contains(textOfResult(t, out), "hook_value_in_the_environment") {
			t.Fatalf("result %+v", out)
		}
	})
}

func textOfResult(t *testing.T, r projectToolResult) string {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// --- Cloud fake --------------------------------------------------------------

// projectCloud is an in-memory bosso for the gateway scope: triggers, the
// organization's webhooks and the caller's fleet, with every call recorded.
type projectCloud struct {
	fakeBackend
	triggers []*pb.Trigger
	webhooks []*pb.SessionWebhook
	own      []*pb.DaemonInfo
	peers    []*pb.DaemonRef
	repos    []*pb.AggregatedRepo
	cronRows []*pb.CronJobWithDaemon

	listTriggersErr error
	listWebhooksErr error

	calls  []string
	nextID int

	createdTriggers []*pb.CreateTriggerRequest
	createdCrons    map[string][]*pb.CreateCronJobRequest
}

var _ ProjectCloudBackend = (*projectCloud)(nil)

func newProjectCloud() *projectCloud {
	return &projectCloud{createdCrons: map[string][]*pb.CreateCronJobRequest{}}
}

func (c *projectCloud) call(s string) { c.calls = append(c.calls, s) }

func (c *projectCloud) writes() []string {
	var out []string
	for _, s := range c.calls {
		if !strings.HasPrefix(s, "list-") {
			out = append(out, s)
		}
	}
	return out
}

func (c *projectCloud) GetTriggerCatalog(context.Context) (*pb.TriggerCatalog, error) {
	return nil, errNotImpl
}

func (c *projectCloud) ListTriggers(_ context.Context, req *pb.ListTriggersRequest) (*pb.ListTriggersResponse, error) {
	c.call("list-triggers " + req.GetOrganizationId())
	if c.listTriggersErr != nil {
		return nil, c.listTriggersErr
	}
	return &pb.ListTriggersResponse{Triggers: c.triggers}, nil
}

func (c *projectCloud) GetTrigger(context.Context, string) (*pb.Trigger, error) {
	return nil, errNotImpl
}

func (c *projectCloud) ListTriggerInvocations(context.Context, *pb.ListTriggerInvocationsRequest) (*pb.ListTriggerInvocationsResponse, error) {
	return nil, errNotImpl
}

func (c *projectCloud) CreateTrigger(_ context.Context, req *pb.CreateTriggerRequest) (*pb.CreateTriggerResponse, error) {
	c.call("create-trigger " + req.GetName())
	c.createdTriggers = append(c.createdTriggers, proto.CloneOf(req))
	c.nextID++
	t := &pb.Trigger{Id: fmt.Sprintf("tr-%d", c.nextID), Name: req.GetName(), TriggerType: req.GetTriggerType(), RepoOriginUrl: req.GetRepoOriginUrl(), ManagedBy: req.GetManagedBy(), IsEnabled: req.GetIsEnabled()}
	c.triggers = append(c.triggers, t)
	return &pb.CreateTriggerResponse{Trigger: proto.CloneOf(t)}, nil
}

func (c *projectCloud) UpdateTrigger(_ context.Context, req *pb.UpdateTriggerRequest) (*pb.Trigger, error) {
	c.call("update-trigger " + req.GetId())
	return nil, errNotImpl
}

func (c *projectCloud) RotateTriggerSecret(_ context.Context, id string) (*pb.RotateTriggerSecretResponse, error) {
	c.call("rotate-trigger " + id)
	return nil, errNotImpl
}

func (c *projectCloud) TestTrigger(context.Context, *pb.TestTriggerRequest) (*pb.TriggerInvocation, error) {
	return nil, errNotImpl
}

func (c *projectCloud) DeleteTrigger(_ context.Context, id string) error {
	c.call("delete-trigger " + id)
	return errNotImpl
}

func (c *projectCloud) ListSessionWebhookEventTypes(context.Context, *pb.ListSessionWebhookEventTypesRequest) (*pb.ListSessionWebhookEventTypesResponse, error) {
	return nil, errNotImpl
}

func (c *projectCloud) ListSessionWebhooks(_ context.Context, req *pb.ListSessionWebhooksRequest) (*pb.ListSessionWebhooksResponse, error) {
	c.call("list-webhooks " + req.GetOrganizationId())
	if c.listWebhooksErr != nil {
		return nil, c.listWebhooksErr
	}
	return &pb.ListSessionWebhooksResponse{Webhooks: c.webhooks}, nil
}

func (c *projectCloud) ListSessionWebhookDeliveries(context.Context, *pb.ListSessionWebhookDeliveriesRequest) (*pb.ListSessionWebhookDeliveriesResponse, error) {
	return nil, errNotImpl
}

func (c *projectCloud) GetSessionWebhookDelivery(context.Context, *pb.GetSessionWebhookDeliveryRequest) (*pb.GetSessionWebhookDeliveryResponse, error) {
	return nil, errNotImpl
}

func (c *projectCloud) CreateSessionWebhook(_ context.Context, req *pb.CreateSessionWebhookRequest) (*pb.CreateSessionWebhookResponse, error) {
	c.call("create-webhook " + req.GetUrl())
	return nil, errNotImpl
}

func (c *projectCloud) UpdateSessionWebhook(_ context.Context, req *pb.UpdateSessionWebhookRequest) (*pb.SessionWebhook, error) {
	c.call("update-webhook " + req.GetId())
	return nil, errNotImpl
}

func (c *projectCloud) RotateSessionWebhookSecret(_ context.Context, req *pb.RotateSessionWebhookSecretRequest) (*pb.RotateSessionWebhookSecretResponse, error) {
	c.call("rotate-webhook " + req.GetId())
	return nil, errNotImpl
}

func (c *projectCloud) SendSessionWebhookTestEvent(context.Context, *pb.SendSessionWebhookTestEventRequest) (*pb.SendSessionWebhookTestEventResponse, error) {
	return nil, errNotImpl
}

func (c *projectCloud) DeleteSessionWebhook(_ context.Context, req *pb.DeleteSessionWebhookRequest) error {
	c.call("delete-webhook " + req.GetId())
	return errNotImpl
}

func (c *projectCloud) ListDaemons(_ context.Context, org string) ([]*pb.DaemonInfo, error) {
	c.call("list-daemons " + org)
	return c.own, nil
}

func (c *projectCloud) ListFleet(_ context.Context, org string) ([]*pb.DaemonInfo, []*pb.DaemonRef, error) {
	c.call("list-fleet " + org)
	return c.own, c.peers, nil
}

func (c *projectCloud) ListReposAggregated(_ context.Context, org string) ([]*pb.AggregatedRepo, error) {
	c.call("list-repos " + org)
	return c.repos, nil
}

func (c *projectCloud) ListCronJobsWithDaemon(_ context.Context, org string) ([]*pb.CronJobWithDaemon, error) {
	c.call("list-crons " + org)
	return c.cronRows, nil
}

func (c *projectCloud) CreateCronJobOn(_ context.Context, daemonID string, req *pb.CreateCronJobRequest) (*pb.CronJob, error) {
	c.call("create-cron " + daemonID + " " + req.GetName())
	c.createdCrons[daemonID] = append(c.createdCrons[daemonID], proto.CloneOf(req))
	c.nextID++
	return &pb.CronJob{Id: fmt.Sprintf("cj-%d", c.nextID), RepoId: req.GetRepoId(), Name: req.GetName(), ManagedBy: req.GetManagedBy()}, nil
}

func (c *projectCloud) UpdateCronJobOn(_ context.Context, daemonID string, req *pb.UpdateCronJobRequest) (*pb.CronJob, error) {
	c.call("update-cron " + daemonID + " " + req.GetId())
	return nil, errNotImpl
}

func (c *projectCloud) DeleteCronJobOn(_ context.Context, daemonID, id string) error {
	c.call("delete-cron " + daemonID + " " + id)
	return errNotImpl
}

// cloudProjectConfig is a project file with the given cloud section and,
// when crons is non-empty, a fleet of daemons entries and those crons.
func cloudProjectConfig(t *testing.T, cloud, crons string) map[string]any {
	t.Helper()
	body := `{"version": 1, "project": "widget",
	  "repo": {"originUrl": "git@github.com:acme/widget.git", "settings": {"canAutoRepair": true}},
	  "cloud": ` + cloud
	if crons != "" {
		body += `, "daemons": [{"name": "server", "daemonId": "d-server", "path": "~/w"}, {"name": "box", "daemonId": "d-box", "path": "~/w"},
		  {"name": "off", "daemonId": "d-off", "path": "~/w"}, {"name": "peer", "daemonId": "d-peer", "path": "~/w"}], "crons": ` + crons
	}
	return projectConfig(t, body+`}`)
}

const githubTrigger = `{"name": "on-push", "type": "github", "github": {"eventTypes": ["push"]}, "launch": {"promptTemplate": "go"}, "placement": {"mode": "first-available"}}`

// --- Cloud scope ---------------------------------------------------------------

func TestProjectConfigToolsCloud(t *testing.T) {
	t.Run("a gateway-style backend gets cloud scope and creates a marked GitHub trigger", func(t *testing.T) {
		c := newProjectCloud()
		cfg := cloudProjectConfig(t, `{"organization": "`+projectOrg+`", "triggers": [`+githubTrigger+`]}`, "")
		out := callProjectOK(t, c, "apply_project_config", map[string]any{"config": cfg, "dry_run": false})
		if out.Scope != "cloud" || !applied(out) || out.Error != "" {
			t.Fatalf("result %+v", out)
		}
		if ch := changeIn(t, out.Plan, projectconfig.ResourceTrigger, "on-push"); ch.Action != projectconfig.ActionCreate {
			t.Fatalf("trigger: %+v", ch)
		}
		for _, ch := range out.Plan.Changes {
			if ch.Resource == projectconfig.ResourceRepo || ch.Resource == projectconfig.ResourceCron {
				t.Fatalf("the cloud scope planned a local class: %+v", ch)
			}
		}
		if len(out.Warnings) != 1 || out.Warnings[0] != cloudScopeWarning {
			t.Fatalf("warnings %q", out.Warnings)
		}
		if len(c.createdTriggers) != 1 || c.createdTriggers[0].GetManagedBy() != projectMarker || c.createdTriggers[0].GetRepoOriginUrl() != projectOrigin || c.createdTriggers[0].GetOrganizationId() != projectOrg {
			t.Fatalf("created %+v", c.createdTriggers)
		}
	})

	t.Run("the paid gate refuses before anything is planned or written", func(t *testing.T) {
		c := newProjectCloud()
		c.listTriggersErr = connect.NewError(connect.CodePermissionDenied, errors.New("cloud features need a paid plan"))
		cfg := cloudProjectConfig(t, `{"triggers": [`+githubTrigger+`]}`, `[`+nightlyCronOn+`]`)
		msg := callProjectErr(t, c, "apply_project_config", map[string]any{"config": cfg, "organization_id": projectOrg, "dry_run": false})
		if !strings.Contains(msg, "cloud features need a paid plan") {
			t.Fatalf("error %q", msg)
		}
		if w := c.writes(); len(w) != 0 {
			t.Fatalf("writes %q", w)
		}
	})

	t.Run("a non-owner gets owner-required webhooks while triggers still converge", func(t *testing.T) {
		c := newProjectCloud()
		c.listWebhooksErr = connect.NewError(connect.CodePermissionDenied, errors.New("organization owner role required"))
		cfg := cloudProjectConfig(t, `{"organization": "`+projectOrg+`", "triggers": [`+githubTrigger+`],
		  "webhooks": [{"url": "https://hooks.example.com/a", "eventTypes": ["session.merged"], "secret": {"env": "BOSS_HOOK_SECRET"}}]}`, "")
		out := callProjectOK(t, c, "apply_project_config", map[string]any{"config": cfg, "dry_run": false})
		if ch := changeIn(t, out.Plan, projectconfig.ResourceWebhook, "https://hooks.example.com/a"); ch.Action != projectconfig.ActionSkip || ch.Reason != projectconfig.ReasonOwnerRequired {
			t.Fatalf("webhook: %+v", ch)
		}
		if w := c.writes(); !slices.Equal(w, []string{"create-trigger on-push"}) {
			t.Fatalf("writes %q", w)
		}
	})

	t.Run("crons fan out to the caller's own online daemons only", func(t *testing.T) {
		c := newProjectCloud()
		c.own = []*pb.DaemonInfo{
			{DaemonId: "d-server", Hostname: "server", Online: true},
			{DaemonId: "d-box", Hostname: "box", Online: true},
			{DaemonId: "d-off", Hostname: "off", Online: false},
		}
		c.peers = []*pb.DaemonRef{{DaemonId: "d-peer", Hostname: "peer"}}
		c.repos = []*pb.AggregatedRepo{{OriginUrl: projectOrigin, Daemons: []*pb.DaemonRepoRef{
			{DaemonId: "d-server", RepoId: "r-server"}, {DaemonId: "d-box", RepoId: "r-box"}, {DaemonId: "d-off", RepoId: "r-off"},
		}}}
		cfg := cloudProjectConfig(t, `{"organization": "`+projectOrg+`"}`, `[`+nightlyCronOn+`]`)
		out := callProjectOK(t, c, "apply_project_config", map[string]any{"config": cfg, "dry_run": false})
		for daemon, repoID := range map[string]string{"d-server": "r-server", "d-box": "r-box"} {
			reqs := c.createdCrons[daemon]
			if len(reqs) != 1 || reqs[0].GetName() != "nightly" || reqs[0].GetManagedBy() != projectMarker || reqs[0].GetRepoId() != repoID {
				t.Fatalf("%s: created %+v", daemon, reqs)
			}
		}
		want := map[string]string{"off": projectconfig.ReasonDaemonOffline, "peer": projectconfig.ReasonNotYourDaemon}
		for _, ch := range out.Plan.Changes {
			if r, ok := want[ch.Daemon]; ok && (ch.Action != projectconfig.ActionSkip || ch.Reason != r) {
				t.Fatalf("%s: %+v", ch.Daemon, ch)
			}
		}
		if len(c.createdCrons) != 2 {
			t.Fatalf("writes reached %d daemons: %q", len(c.createdCrons), c.writes())
		}
	})

	t.Run("organization: a mismatch or none at all is refused before any request", func(t *testing.T) {
		c := newProjectCloud()
		cfg := cloudProjectConfig(t, `{"organization": "`+projectOrg+`", "triggers": [`+githubTrigger+`]}`, "")
		if msg := callProjectErr(t, c, "diff_project_config", map[string]any{"config": cfg, "organization_id": "org-other"}); !strings.Contains(msg, "disagrees") {
			t.Fatalf("mismatch: %q", msg)
		}
		cfg = cloudProjectConfig(t, `{"triggers": [`+githubTrigger+`]}`, "")
		if msg := callProjectErr(t, c, "diff_project_config", map[string]any{"config": cfg}); !strings.Contains(msg, "organization_id is required") {
			t.Fatalf("none: %q", msg)
		}
		if len(c.calls) != 0 {
			t.Fatalf("calls %q", c.calls)
		}
	})
}

const nightlyCronOn = `{"name": "nightly", "prompt": "/nightly", "schedule": "0 3 * * *", "daemons": ["server", "box", "off", "peer"]}`

// --- Export --------------------------------------------------------------------

func TestExportProjectConfigTool(t *testing.T) {
	pinHome(t)
	repo := &pb.Repo{Id: "repo-1", LocalPath: projectCheckout, OriginUrl: "git@github.com:acme/widget.git", DisplayName: "widget",
		DefaultBaseBranch: "main", CanAutoRepair: true, LinearApiKey: "lin_stored_value"}
	jobs := []*pb.CronJob{
		{Id: "cj-1", RepoId: "repo-1", Name: "nightly", Prompt: "/nightly", Schedule: "0 3 * * *", IsEnabled: true, ShouldRunSetupCommand: true},
		{Id: "cj-2", RepoId: "repo-1", Name: "factory-build", Prompt: "/boss-build", Schedule: "*/15 * * * *", IsEnabled: true, AgentName: "claude",
			GateCommand: `node "$HOME/.claude/skills/boss-build/toolbox/cron-gates/boss-build.mjs"`, ShouldRunSetupCommand: true},
		{Id: "cj-3", RepoId: "repo-other", Name: "elsewhere", Prompt: "/x", Schedule: "0 1 * * *", IsEnabled: true},
	}
	local := func() *projectLocalDaemon {
		d := newProjectLocal()
		d.repos = []*pb.Repo{proto.CloneOf(repo)}
		for _, j := range jobs {
			d.jobs = append(d.jobs, proto.CloneOf(j))
		}
		return d
	}
	expected := func(t *testing.T, id projectconfig.DaemonIdentity) string {
		t.Helper()
		cfg, _, err := projectconfig.Export(projectconfig.ExportInput{
			Repo: repo, CronJobs: jobs[:2], Daemon: id, CheckoutPath: projectCheckout, HomeDir: "/home/me",
			Classifier: projectconfig.DefaultFactoryStages(),
		})
		if err != nil {
			t.Fatal(err)
		}
		data, err := projectconfig.Encode(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	t.Run("local: content is byte-equal to the CLI exporter's, with the factory cron symbolic", func(t *testing.T) {
		d := local()
		out := callProjectOK(t, d, "export_project_config", map[string]any{"working_dir": projectCheckout + "/sub"})
		if out.Scope != "local" {
			t.Fatalf("scope %q", out.Scope)
		}
		if want := expected(t, d.identity); out.Content != want {
			t.Fatalf("content differs from the CLI exporter's:\n--- got\n%s\n--- want\n%s", out.Content, want)
		}
		if !strings.Contains(out.Content, `"factoryStage": "build"`) || strings.Contains(out.Content, "/boss-build") || strings.Contains(out.Content, "elsewhere") {
			t.Fatalf("content:\n%s", out.Content)
		}
		if strings.Contains(out.Content, "lin_stored_value") || !strings.Contains(out.Content, "BOSS_LINEAR_API_KEY") {
			t.Fatalf("secrets must export as references only:\n%s", out.Content)
		}
		if w := d.writes(); len(w) != 0 {
			t.Fatalf("export wrote %q", w)
		}
	})

	t.Run("local: an unknown daemon id omits daemonId and warns", func(t *testing.T) {
		d := local()
		d.identity = projectconfig.DaemonIdentity{DisplayName: "laptop"}
		out := callProjectOK(t, d, "export_project_config", map[string]any{"working_dir": projectCheckout})
		if out.Content != expected(t, d.identity) || strings.Contains(out.Content, "daemonId") {
			t.Fatalf("content:\n%s", out.Content)
		}
		if !slices.ContainsFunc(out.Warnings, func(w string) bool { return strings.Contains(w, "daemon id is unknown") }) {
			t.Fatalf("warnings %q", out.Warnings)
		}
	})

	t.Run("local: a directory outside any registered repo is an error", func(t *testing.T) {
		d := local()
		if msg := callProjectErr(t, d, "export_project_config", map[string]any{"working_dir": "/tmp/elsewhere"}); !strings.Contains(msg, "not in a repository registered") {
			t.Fatalf("error %q", msg)
		}
	})

	t.Run("gateway: only the matching origin's triggers, webhooks omitted when the read is denied", func(t *testing.T) {
		c := newProjectCloud()
		c.triggers = []*pb.Trigger{
			{Id: "tr-1", Name: "mine", TriggerType: "github", RepoOriginUrl: projectOrigin, IsEnabled: true,
				TypeConfig: &pb.Trigger_Github{Github: &pb.GitHubTriggerConfig{EventTypes: []string{"push"}}},
				Launch:     &pb.TriggerLaunchSettings{PromptTemplate: "go"},
				Placement:  &pb.TriggerPlacement{Mode: pb.TriggerPlacementMode_TRIGGER_PLACEMENT_MODE_FIRST_AVAILABLE}},
			{Id: "tr-2", Name: "theirs", TriggerType: "github", RepoOriginUrl: "https://github.com/acme/other", IsEnabled: true,
				TypeConfig: &pb.Trigger_Github{Github: &pb.GitHubTriggerConfig{EventTypes: []string{"push"}}},
				Launch:     &pb.TriggerLaunchSettings{PromptTemplate: "go"},
				Placement:  &pb.TriggerPlacement{Mode: pb.TriggerPlacementMode_TRIGGER_PLACEMENT_MODE_FIRST_AVAILABLE}},
		}
		c.listWebhooksErr = connect.NewError(connect.CodePermissionDenied, errors.New("organization owner role required"))
		out := callProjectOK(t, c, "export_project_config", map[string]any{"repo_origin_url": "git@github.com:acme/widget.git", "organization_id": projectOrg})
		if out.Scope != "cloud" {
			t.Fatalf("scope %q", out.Scope)
		}
		cfg, _, err := projectconfig.Decode([]byte(out.Content))
		if err != nil {
			t.Fatalf("content does not decode: %v\n%s", err, out.Content)
		}
		if err := projectconfig.Validate(cfg); err != nil {
			t.Fatalf("content does not validate: %v", err)
		}
		if len(cfg.Cloud.Triggers) != 1 || cfg.Cloud.Triggers[0].Name != "mine" || cfg.Project != "widget" || *cfg.Cloud.Organization != projectOrg {
			t.Fatalf("exported %+v", cfg.Cloud)
		}
		if strings.Contains(out.Content, `"webhooks"`) || cfg.Daemons != nil || cfg.Crons != nil {
			t.Fatalf("content:\n%s", out.Content)
		}
		if !slices.ContainsFunc(out.Warnings, func(w string) bool { return strings.Contains(w, "cloud.webhooks") }) {
			t.Fatalf("warnings %q", out.Warnings)
		}
	})

	t.Run("gateway: a retire-only webhook is left out with a warning", func(t *testing.T) {
		c := newProjectCloud()
		c.webhooks = []*pb.SessionWebhook{
			{Id: "wh-1", Url: "https://hooks.example.com/a", EventTypes: []string{"session.merged"}, IsEnabled: true, Format: "raw"},
			// An orphaned endpoint bosso lists retire-only, without its URL (BOS-1593).
			{Id: "wh-orphan", EventTypes: []string{"session.merged"}, IsEnabled: true, Format: "raw", IsRetireOnly: true},
		}
		out := callProjectOK(t, c, "export_project_config", map[string]any{"repo_origin_url": projectOrigin, "organization_id": projectOrg})
		cfg, _, err := projectconfig.Decode([]byte(out.Content))
		if err != nil {
			t.Fatalf("content does not decode: %v\n%s", err, out.Content)
		}
		if len(cfg.Cloud.Webhooks) != 1 || cfg.Cloud.Webhooks[0].URL != "https://hooks.example.com/a" {
			t.Fatalf("exported webhooks %+v", cfg.Cloud.Webhooks)
		}
		if !slices.ContainsFunc(out.Warnings, func(w string) bool { return strings.Contains(w, "wh-orphan") }) {
			t.Fatalf("warnings %q", out.Warnings)
		}
	})

	t.Run("gateway: the origin and organization are required", func(t *testing.T) {
		c := newProjectCloud()
		if msg := callProjectErr(t, c, "export_project_config", map[string]any{"organization_id": projectOrg}); !strings.Contains(msg, "repo_origin_url") {
			t.Fatalf("no origin: %q", msg)
		}
		if msg := callProjectErr(t, c, "export_project_config", map[string]any{"repo_origin_url": projectOrigin}); !strings.Contains(msg, "organization_id") {
			t.Fatalf("no organization: %q", msg)
		}
		if len(c.calls) != 0 {
			t.Fatalf("calls %q", c.calls)
		}
	})
}

// --- Registration ------------------------------------------------------------

func TestProjectConfigToolRegistration(t *testing.T) {
	names := []string{"export_project_config", "diff_project_config", "apply_project_config"}
	present := func(tools map[string]*mcp.Tool) []string {
		var out []string
		for _, n := range names {
			if _, ok := tools[n]; ok {
				out = append(out, n)
			}
		}
		return out
	}
	if got := present(registeredTools(t, newProjectLocal(), Options{})); !slices.Equal(got, names) {
		t.Fatalf("Options{}: %q", got)
	}
	if got := present(registeredTools(t, newProjectLocal(), Options{ReadOnly: true})); !slices.Equal(got, names[:2]) {
		t.Fatalf("ReadOnly: %q", got)
	}
	if got := present(registeredTools(t, newProjectLocal(), Options{Only: map[string]bool{"diff_project_config": true}})); !slices.Equal(got, []string{"diff_project_config"}) {
		t.Fatalf("Only: %q", got)
	}
	tools := registeredTools(t, newProjectLocal(), Options{})
	for _, n := range names[:2] {
		if a := tools[n].Annotations; a == nil || !a.ReadOnlyHint {
			t.Errorf("%s: want ReadOnlyHint", n)
		}
	}
	if a := tools["apply_project_config"].Annotations; a == nil || a.ReadOnlyHint || a.DestructiveHint == nil || *a.DestructiveHint {
		t.Errorf("apply_project_config: want a non-destructive write tool")
	}

	pinHome(t)
	d := newProjectLocal()
	out := callProjectOK(t, d, "diff_project_config", map[string]any{"config": localProjectConfig(t, `[`+nightlyCron+`]`), "working_dir": projectCheckout})
	if out.Converged || out.Applied != nil {
		t.Fatalf("diff result %+v", out)
	}
	if w := d.writes(); len(w) != 0 {
		t.Fatalf("diff wrote %q", w)
	}
}
