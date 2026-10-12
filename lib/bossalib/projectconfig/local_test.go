package projectconfig

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

const (
	localMarker     = "boss-project:widget"
	localTarget     = "/home/me/src/widget"
	localBuildGate  = `node "$HOME/.claude/skills/boss-build/toolbox/cron-gates/boss-build.mjs"`
	localSecretVal  = "lin_value_never_rendered"
	localSecretVal2 = "lin_other_value"
)

func localBuildFactory() FactoryCron {
	return FactoryCron{Skill: "boss-build", Agent: "claude", Prompt: "/boss-build", GateCommand: localBuildGate, DefaultSchedule: "*/15 * * * *"}
}

// localCfg is a one-daemon file with a plain cron and a factory cron.
func localCfg(edit func(*Config)) *Config {
	cfg := &Config{
		Version: SchemaVersion, Project: "widget",
		Repo:    Repo{OriginURL: "git@github.com:acme/widget.git"},
		Daemons: []Daemon{{Name: "laptop", DaemonID: ptr("d-1"), Path: ptr("~/src/widget")}},
		Crons: []Cron{
			{Name: "nightly", Prompt: ptr("/nightly"), Schedule: ptr("0 3 * * *")},
			{Name: "factory-build", FactoryStage: ptr("build")},
		},
	}
	if edit != nil {
		edit(cfg)
	}
	return cfg
}

func localRepo() *pb.Repo {
	return &pb.Repo{Id: "repo-1", LocalPath: localTarget, OriginUrl: "https://github.com/acme/widget", DisplayName: "widget", DefaultBaseBranch: "main", CanAutoRepair: true}
}

// localState is a daemon holding repo (nil: unregistered) and jobs at the
// file's path, with the build stage resolving cleanly.
func localState(repo *pb.Repo, jobs ...*pb.CronJob) *LocalState {
	st := &LocalState{
		TargetPath: localTarget, TargetCheckout: localTarget,
		Validation:    &pb.ValidateRepoPathResponse{IsValid: true, OriginUrl: "https://github.com/acme/widget.git", DefaultBranch: "main"},
		RepoCheckouts: map[string]string{},
		CronJobs:      jobs,
		Factory:       map[string]FactoryResolution{"factory-build": {Cron: localBuildFactory()}},
	}
	if repo != nil {
		st.Repos = []*pb.Repo{repo}
		st.RepoCheckouts[repo.GetId()] = repo.GetLocalPath()
	}
	return st
}

func localOpts(env map[string]string) LocalOptions {
	return LocalOptions{
		Daemon:       DaemonIdentity{ID: "d-1", DisplayName: "laptop"},
		CheckoutPath: "/work/widget",
		HomeDir:      "/home/me",
		Secrets:      EnvResolver(func(k string) string { return env[k] }),
	}
}

func nightlyJob(marker string) *pb.CronJob {
	return &pb.CronJob{Id: "c-nightly", Name: "nightly", Prompt: "/nightly", Schedule: "0 3 * * *", IsEnabled: true, ManagedBy: marker}
}

func buildJob(name, marker string) *pb.CronJob {
	return &pb.CronJob{Id: "c-" + name, Name: name, Prompt: "/boss-build", Schedule: "*/15 * * * *", IsEnabled: true, AgentName: "claude", GateCommand: localBuildGate, ManagedBy: marker}
}

func changeFor(t *testing.T, p LocalPlan, r Resource, identity string) Change {
	t.Helper()
	var found []Change
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

func countActions(p LocalPlan, r Resource, a Action) int {
	n := 0
	for _, c := range p.Changes {
		if c.Resource == r && c.Action == a {
			n++
		}
	}
	return n
}

func fieldNames(c Change) []string {
	var out []string
	for _, f := range c.Fields {
		out = append(out, f.Field)
	}
	return out
}

func hasWarning(p LocalPlan, sub string) bool {
	return slices.ContainsFunc(p.Warnings, func(w string) bool { return strings.Contains(w, sub) })
}

func TestPlanLocal(t *testing.T) {
	origin := "https://github.com/acme/widget"
	tests := []struct {
		name  string
		cfg   *Config
		state *LocalState
		opts  func(*LocalOptions)
		check func(t *testing.T, p LocalPlan)
	}{
		{
			name:  "first apply on an empty daemon creates the repo and every cron, marked and needing confirmation",
			cfg:   localCfg(nil),
			state: localState(nil),
			check: func(t *testing.T, p LocalPlan) {
				repo := changeFor(t, p, ResourceRepo, origin)
				if repo.Action != ActionCreate || p.ops[0].register == nil || p.ops[0].register.GetLocalPath() != localTarget {
					t.Fatalf("repo %+v ops %+v", repo, p.ops)
				}
				for _, name := range []string{"nightly", "factory-build"} {
					c := changeFor(t, p, ResourceCron, name)
					if c.Action != ActionCreate || !c.NeedsConfirmation || c.Daemon != "laptop" {
						t.Fatalf("%s: %+v", name, c)
					}
				}
				var creates []*pb.CreateCronJobRequest
				for _, op := range p.ops {
					if op.createCron != nil {
						creates = append(creates, op.createCron)
						if !op.needsRepo {
							t.Fatal("a cron of a repo this plan registers must take its id")
						}
					}
				}
				if len(creates) != 2 || creates[0].GetName() != "factory-build" || creates[0].GetManagedBy() != localMarker ||
					creates[0].GetSchedule() != "*/15 * * * *" || creates[0].GetGateCommand() != localBuildGate || !creates[0].GetIsEnabled() ||
					creates[1].GetName() != "nightly" || creates[1].GetManagedBy() != localMarker {
					t.Fatalf("creates %+v", creates)
				}
				if !p.NeedsConfirmation() || p.Converged() {
					t.Fatal("first apply needs confirmation and is not converged")
				}
			},
		},
		{
			name:  "markers present and equal: every change unchanged and the plan converged",
			cfg:   localCfg(nil),
			state: localState(localRepo(), nightlyJob(localMarker), buildJob("factory-build", localMarker)),
			check: func(t *testing.T, p LocalPlan) {
				if !p.Converged() || p.HasWrites() {
					t.Fatalf("not converged:\n%s", p.Text())
				}
			},
		},
		{
			name: "one differing setting updates that field only",
			cfg: localCfg(func(c *Config) {
				c.Repo.Settings = &RepoSettings{CanAutoRepair: ptr(false)}
				c.Repo.DisplayName = ptr("widget")
			}),
			state: localState(localRepo(), nightlyJob(localMarker), buildJob("factory-build", localMarker)),
			check: func(t *testing.T, p LocalPlan) {
				c := changeFor(t, p, ResourceRepo, origin)
				if c.Action != ActionUpdate || !slices.Equal(fieldNames(c), []string{"settings.canAutoRepair"}) {
					t.Fatalf("%+v", c)
				}
				req := p.ops[0].updateRepo
				if req.GetId() != "repo-1" || req.CanAutoRepair == nil || req.DisplayName != nil || req.CanAutoMerge != nil {
					t.Fatalf("request %+v", req)
				}
			},
		},
		{
			name:  "defaultBaseBranch differing on an existing repo is a registration-only skip",
			cfg:   localCfg(func(c *Config) { c.Repo.DefaultBaseBranch = ptr("develop") }),
			state: localState(localRepo()),
			check: func(t *testing.T, p LocalPlan) {
				c := changeFor(t, p, ResourceRepo, "repo.defaultBaseBranch")
				if c.Action != ActionSkip || c.Reason != ReasonRegistrationOnly {
					t.Fatalf("%+v", c)
				}
			},
		},
		{
			name: "declared path missing: repo conflict, crons skipped",
			cfg:  localCfg(nil),
			state: func() *LocalState {
				st := localState(nil)
				st.Validation = &pb.ValidateRepoPathResponse{IsValid: false, ErrorMessage: "no such directory"}
				return st
			}(),
			check: func(t *testing.T, p LocalPlan) {
				c := changeFor(t, p, ResourceRepo, origin)
				if c.Action != ActionConflict || c.Reason != ReasonRepoPath || !strings.Contains(c.Detail, "no such directory") {
					t.Fatalf("%+v", c)
				}
				if countActions(p, ResourceCron, ActionSkip) != 2 || p.HasWrites() {
					t.Fatalf("crons must skip:\n%s", p.Text())
				}
			},
		},
		{
			name: "declared path is a clone of another origin: repo conflict, crons skipped",
			cfg:  localCfg(nil),
			state: func() *LocalState {
				st := localState(nil)
				st.Validation.OriginUrl = "git@github.com:other/thing.git"
				return st
			}(),
			check: func(t *testing.T, p LocalPlan) {
				if c := changeFor(t, p, ResourceRepo, origin); c.Action != ActionConflict || c.Reason != ReasonOriginMismatch {
					t.Fatalf("%+v", c)
				}
				if countActions(p, ResourceCron, ActionSkip) != 2 || p.HasWrites() {
					t.Fatalf("crons must skip:\n%s", p.Text())
				}
			},
		},
		{
			name: "same origin registered at another path is a conflict naming it",
			cfg:  localCfg(nil),
			state: func() *LocalState {
				st := localState(nil)
				other := localRepo()
				other.LocalPath = "/elsewhere/widget"
				st.Repos = []*pb.Repo{other}
				st.RepoCheckouts[other.GetId()] = other.GetLocalPath()
				return st
			}(),
			check: func(t *testing.T, p LocalPlan) {
				c := changeFor(t, p, ResourceRepo, origin)
				if c.Action != ActionConflict || c.Reason != ReasonRegisteredElsewhere || !strings.Contains(c.Detail, "/elsewhere/widget") {
					t.Fatalf("%+v", c)
				}
				if p.HasWrites() {
					t.Fatal("a conflict writes nothing")
				}
			},
		},
		{
			name: "two entries match: one conflict naming both, crons skipped, settings and secrets still converge at the checkout",
			cfg: localCfg(func(c *Config) {
				c.Daemons = []Daemon{{Name: "laptop", Path: ptr("~/a")}, {Name: "laptop-2", Path: ptr("~/b")}}
				c.Daemons[1].Name = "laptop"
				c.Repo.Settings = &RepoSettings{CanAutoMerge: ptr(true)}
				c.Repo.Secrets = &RepoSecrets{LinearAPIKey: &SecretRef{Env: LinearAPIKeyEnv}}
				for i := range c.Crons {
					c.Crons[i].Daemons = []string{"laptop"}
				}
			}),
			state: func() *LocalState {
				st := localState(nil)
				st.TargetPath, st.TargetCheckout = "/work/widget", "/work/widget"
				repo := localRepo()
				repo.LocalPath = "/work/widget"
				st.Repos = []*pb.Repo{repo}
				st.RepoCheckouts[repo.GetId()] = repo.GetLocalPath()
				return st
			}(),
			opts: func(o *LocalOptions) {
				o.Daemon.ID = ""
				o.Secrets = EnvResolver(func(string) string { return localSecretVal })
			},
			check: func(t *testing.T, p LocalPlan) {
				c := changeFor(t, p, ResourceRepo, "daemons")
				if c.Action != ActionConflict || c.Reason != ReasonAmbiguous || strings.Count(c.Detail, `"laptop"`) != 2 {
					t.Fatalf("%+v", c)
				}
				if countActions(p, ResourceCron, ActionSkip) != 2 {
					t.Fatalf("crons must skip:\n%s", p.Text())
				}
				if r := changeFor(t, p, ResourceRepo, origin); r.Action != ActionUpdate {
					t.Fatalf("settings must converge at the checkout: %+v", r)
				}
				if s := changeFor(t, p, ResourceRepo, "secrets.linearApiKey"); s.Action != ActionUpdate {
					t.Fatalf("secret must converge at the checkout: %+v", s)
				}
			},
		},
		{
			name: "unmatched machine: repo at the applied checkout, crons skipped, warning names the entry to add",
			cfg:  localCfg(nil),
			state: func() *LocalState {
				st := localState(nil)
				st.TargetPath, st.TargetCheckout = "/home/me/work/widget", "/home/me/work/widget"
				return st
			}(),
			opts: func(o *LocalOptions) {
				o.Daemon = DaemonIdentity{ID: "d-9", DisplayName: "desktop"}
				o.CheckoutPath = "/home/me/work/widget"
			},
			check: func(t *testing.T, p LocalPlan) {
				if c := changeFor(t, p, ResourceRepo, origin); c.Action != ActionCreate || p.ops[0].register.GetLocalPath() != "/home/me/work/widget" {
					t.Fatalf("%+v", c)
				}
				if countActions(p, ResourceCron, ActionSkip) != 2 {
					t.Fatalf("crons must skip:\n%s", p.Text())
				}
				if !hasWarning(p, `{"name": "desktop", "daemonId": "d-9", "path": "~/work/widget"}`) {
					t.Fatalf("warnings %q", p.Warnings)
				}
			},
		},
		{
			name:  "secret env set and differing is an update carrying no value",
			cfg:   localCfg(func(c *Config) { c.Repo.Secrets = &RepoSecrets{LinearAPIKey: &SecretRef{Env: LinearAPIKeyEnv}} }),
			state: localState(func() *pb.Repo { r := localRepo(); r.LinearApiKey = localSecretVal2; return r }()),
			opts:  func(o *LocalOptions) { o.Secrets = EnvResolver(func(string) string { return localSecretVal }) },
			check: func(t *testing.T, p LocalPlan) {
				c := changeFor(t, p, ResourceRepo, "secrets.linearApiKey")
				if c.Action != ActionUpdate || len(c.Fields) != 1 || !c.Fields[0].Secret {
					t.Fatalf("%+v", c)
				}
				data, err := p.JSON()
				if err != nil {
					t.Fatal(err)
				}
				for _, rendered := range []string{p.Text(), string(data), mustMarshal(t, p)} {
					if strings.Contains(rendered, localSecretVal) || strings.Contains(rendered, localSecretVal2) {
						t.Fatalf("a secret value was rendered:\n%s", rendered)
					}
				}
				var req *pb.UpdateRepoRequest
				for _, op := range p.ops {
					if op.secret {
						req = op.updateRepo
					}
				}
				if req.GetLinearKey().GetAction() != pb.SecretAction_SECRET_ACTION_SET || req.GetLinearKey().GetValue() != localSecretVal {
					t.Fatalf("secret op %+v", req)
				}
			},
		},
		{
			name:  "secret env set and equal is unchanged",
			cfg:   localCfg(func(c *Config) { c.Repo.Secrets = &RepoSecrets{LinearAPIKey: &SecretRef{Env: LinearAPIKeyEnv}} }),
			state: localState(func() *pb.Repo { r := localRepo(); r.LinearApiKey = localSecretVal; return r }()),
			opts:  func(o *LocalOptions) { o.Secrets = EnvResolver(func(string) string { return localSecretVal }) },
			check: func(t *testing.T, p LocalPlan) {
				if c := changeFor(t, p, ResourceRepo, "secrets.linearApiKey"); c.Action != ActionUnchanged {
					t.Fatalf("%+v", c)
				}
			},
		},
		{
			name:  "secret env unset with a stored value is unchanged with a warning",
			cfg:   localCfg(func(c *Config) { c.Repo.Secrets = &RepoSecrets{SentryAuthToken: &SecretRef{Env: SentryAuthTokenEnv}} }),
			state: localState(func() *pb.Repo { r := localRepo(); r.SentryApiKey = localSecretVal; return r }()),
			check: func(t *testing.T, p LocalPlan) {
				if c := changeFor(t, p, ResourceRepo, "secrets.sentryAuthToken"); c.Action != ActionUnchanged || !hasWarning(p, "BOSS_SENTRY_AUTH_TOKEN is not set") {
					t.Fatalf("%+v %q", c, p.Warnings)
				}
			},
		},
		{
			name:  "secret env unset with nothing stored is a skip",
			cfg:   localCfg(func(c *Config) { c.Repo.Secrets = &RepoSecrets{LinearAPIKey: &SecretRef{Env: LinearAPIKeyEnv}} }),
			state: localState(localRepo()),
			check: func(t *testing.T, p LocalPlan) {
				if c := changeFor(t, p, ResourceRepo, "secrets.linearApiKey"); c.Action != ActionSkip || c.Reason != ReasonSecretUnresolved {
					t.Fatalf("%+v", c)
				}
			},
		},
		{
			name:  "nil secret resolver skips every secret",
			cfg:   localCfg(func(c *Config) { c.Repo.Secrets = &RepoSecrets{LinearAPIKey: &SecretRef{Env: LinearAPIKeyEnv}} }),
			state: localState(localRepo()),
			opts:  func(o *LocalOptions) { o.Secrets = nil },
			check: func(t *testing.T, p LocalPlan) {
				if c := changeFor(t, p, ResourceRepo, "secrets.linearApiKey"); c.Action != ActionSkip || c.Reason != ReasonSecretUnresolved {
					t.Fatalf("%+v", c)
				}
			},
		},
		{
			name:  "unmanaged same-name row is adopted with the marker",
			cfg:   localCfg(nil),
			state: localState(localRepo(), nightlyJob(""), buildJob("factory-build", localMarker)),
			check: func(t *testing.T, p LocalPlan) {
				c := changeFor(t, p, ResourceCron, "nightly")
				if c.Action != ActionAdopt || c.NeedsConfirmation {
					t.Fatalf("%+v", c)
				}
				if req := p.ops[0].updateCron; req.GetId() != "c-nightly" || req.GetManagedBy() != localMarker || req.Prompt != nil {
					t.Fatalf("adopt request %+v", req)
				}
			},
		},
		{
			name:  "foreign-marker same-name row is a conflict",
			cfg:   localCfg(nil),
			state: localState(localRepo(), nightlyJob("boss-project:other"), buildJob("factory-build", localMarker)),
			check: func(t *testing.T, p LocalPlan) {
				if c := changeFor(t, p, ResourceCron, "nightly"); c.Action != ActionConflict || c.Reason != ReasonManagedByOther {
					t.Fatalf("%+v", c)
				}
				if p.HasWrites() {
					t.Fatal("a conflict writes nothing")
				}
			},
		},
		{
			name:  "hand-named /boss-build row is adopted with a rename, not duplicated",
			cfg:   localCfg(nil),
			state: localState(localRepo(), nightlyJob(localMarker), buildJob("my-builder", "")),
			check: func(t *testing.T, p LocalPlan) {
				c := changeFor(t, p, ResourceCron, "factory-build")
				if c.Action != ActionAdopt || !slices.Contains(fieldNames(c), "name") || countActions(p, ResourceCron, ActionCreate) != 0 {
					t.Fatalf("%+v\n%s", c, p.Text())
				}
				req := p.ops[0].updateCron
				if req.GetId() != "c-my-builder" || req.GetName() != "factory-build" || req.GetManagedBy() != localMarker {
					t.Fatalf("adopt request %+v", req)
				}
			},
		},
		{
			name: "$boss-build spelling also matches the factory fallback",
			cfg:  localCfg(nil),
			state: func() *LocalState {
				j := buildJob("codex-builder", "")
				j.Prompt, j.AgentName = "$boss-build", "codex"
				return localState(localRepo(), nightlyJob(localMarker), j)
			}(),
			check: func(t *testing.T, p LocalPlan) {
				c := changeFor(t, p, ResourceCron, "factory-build")
				if c.Action != ActionAdopt || !slices.Contains(fieldNames(c), "prompt") || !c.NeedsConfirmation {
					t.Fatalf("%+v", c)
				}
			},
		},
		{
			name:  "two hand-named /boss-build rows are a conflict",
			cfg:   localCfg(nil),
			state: localState(localRepo(), nightlyJob(localMarker), buildJob("b1", ""), buildJob("b2", "")),
			check: func(t *testing.T, p LocalPlan) {
				if c := changeFor(t, p, ResourceCron, "factory-build"); c.Action != ActionConflict || c.Reason != ReasonAmbiguous {
					t.Fatalf("%+v", c)
				}
			},
		},
		{
			name:  "own-marker row under another name is not adopted: new name created, old row disabled",
			cfg:   localCfg(func(c *Config) { c.Crons[0].Name = "nightly-v2" }),
			state: localState(localRepo(), nightlyJob(localMarker), buildJob("factory-build", localMarker)),
			check: func(t *testing.T, p LocalPlan) {
				if c := changeFor(t, p, ResourceCron, "nightly-v2"); c.Action != ActionCreate {
					t.Fatalf("%+v", c)
				}
				if c := changeFor(t, p, ResourceCron, "nightly"); c.Action != ActionDisable || !hasWarning(p, `cron "nightly" is no longer in the file`) {
					t.Fatalf("%+v %q", c, p.Warnings)
				}
				last := p.ops[len(p.ops)-1]
				if last.updateCron.GetId() != "c-nightly" || last.updateCron.IsEnabled == nil || *last.updateCron.IsEnabled {
					t.Fatalf("disable op %+v", last)
				}
			},
		},
		{
			name:  "with prune the dropped own row is deleted",
			cfg:   localCfg(func(c *Config) { c.Crons[0].Name = "nightly-v2" }),
			state: localState(localRepo(), nightlyJob(localMarker), buildJob("factory-build", localMarker)),
			opts:  func(o *LocalOptions) { o.Prune = true },
			check: func(t *testing.T, p LocalPlan) {
				if c := changeFor(t, p, ResourceCron, "nightly"); c.Action != ActionDelete || p.ops[len(p.ops)-1].deleteCron != "c-nightly" {
					t.Fatalf("%+v", c)
				}
				if countActions(p, ResourceCron, ActionDelete) != 1 {
					t.Fatal("exactly one delete")
				}
			},
		},
		{
			name: "own-marker row selected only for another daemon is disabled",
			cfg: localCfg(func(c *Config) {
				c.Daemons = append(c.Daemons, Daemon{Name: "server", Path: ptr("/srv/widget")})
				c.Crons[0].Daemons = []string{"server"}
				c.Crons[1].Daemons = []string{"laptop"}
			}),
			state: localState(localRepo(), nightlyJob(localMarker), buildJob("factory-build", localMarker)),
			check: func(t *testing.T, p LocalPlan) {
				if c := changeFor(t, p, ResourceCron, "nightly"); c.Action != ActionDisable {
					t.Fatalf("%+v", c)
				}
			},
		},
		{
			name:  "unmanaged row absent from the file is never touched, even with prune",
			cfg:   localCfg(func(c *Config) { c.Crons = c.Crons[1:] }),
			state: localState(localRepo(), nightlyJob(""), buildJob("factory-build", localMarker)),
			opts:  func(o *LocalOptions) { o.Prune = true },
			check: func(t *testing.T, p LocalPlan) {
				for _, c := range p.Changes {
					if c.Identity == "nightly" {
						t.Fatalf("unmanaged row planned: %+v", c)
					}
				}
				if p.HasWrites() {
					t.Fatalf("writes:\n%s", p.Text())
				}
			},
		},
		{
			name: "gate path rule: traversal, absolute and metacharacters conflict; a repo-relative path stores as ./path",
			cfg: localCfg(func(c *Config) {
				c.Crons = []Cron{
					{Name: "dotdot", Prompt: ptr("/x"), Schedule: ptr("0 * * * *"), GateCommand: ptr("../x.sh")},
					{Name: "absolute", Prompt: ptr("/x"), Schedule: ptr("0 * * * *"), GateCommand: ptr("/usr/bin/x")},
					{Name: "shell", Prompt: ptr("/x"), Schedule: ptr("0 * * * *"), GateCommand: ptr("scripts/x.sh;curl")},
					{Name: "bare", Prompt: ptr("/x"), Schedule: ptr("0 * * * *"), GateCommand: ptr("scripts/has-work.sh")},
					{Name: "live", Prompt: ptr("/x"), Schedule: ptr("0 * * * *"), GateCommand: ptr("scripts/has-work.sh")},
				}
			}),
			state: localState(localRepo(), &pb.CronJob{Id: "c-live", Name: "live", Prompt: "/x", Schedule: "0 * * * *", IsEnabled: true, GateCommand: "./scripts/has-work.sh", ManagedBy: localMarker}),
			check: func(t *testing.T, p LocalPlan) {
				for _, name := range []string{"dotdot", "absolute", "shell"} {
					if c := changeFor(t, p, ResourceCron, name); c.Action != ActionConflict || c.Reason != ReasonGatePath {
						t.Fatalf("%s: %+v", name, c)
					}
				}
				if c := changeFor(t, p, ResourceCron, "live"); c.Action != ActionUnchanged {
					t.Fatalf("live ./scripts/has-work.sh must compare equal: %+v", c)
				}
				if c := changeFor(t, p, ResourceCron, "bare"); c.Action != ActionCreate || p.ops[0].createCron.GetGateCommand() != "./scripts/has-work.sh" {
					t.Fatalf("%+v %+v", c, p.ops)
				}
			},
		},
		{
			name: "factory preflight failure creates the row disabled with the reason",
			cfg:  localCfg(nil),
			state: func() *LocalState {
				st := localState(localRepo(), nightlyJob(localMarker))
				fc := localBuildFactory()
				fc.PreflightFailures = []string{"gate script missing"}
				st.Factory["factory-build"] = FactoryResolution{Cron: fc}
				return st
			}(),
			check: func(t *testing.T, p LocalPlan) {
				c := changeFor(t, p, ResourceCron, "factory-build")
				if c.Action != ActionCreate || c.Reason != ReasonPreflightFailed || !strings.Contains(c.Detail, "gate script missing") || p.ops[0].createCron.GetIsEnabled() {
					t.Fatalf("%+v %+v", c, p.ops[0].createCron)
				}
			},
		},
		{
			name: "factory preflight failure on an existing differing row is a skip with the failures",
			cfg:  localCfg(nil),
			state: func() *LocalState {
				j := buildJob("factory-build", localMarker)
				j.Prompt = "/boss-build --old"
				st := localState(localRepo(), nightlyJob(localMarker), j)
				fc := localBuildFactory()
				fc.PreflightFailures = []string{"gate script missing"}
				st.Factory["factory-build"] = FactoryResolution{Cron: fc}
				return st
			}(),
			check: func(t *testing.T, p LocalPlan) {
				if c := changeFor(t, p, ResourceCron, "factory-build"); c.Action != ActionSkip || c.Reason != ReasonPreflightFailed || !strings.Contains(c.Detail, "gate script missing") {
					t.Fatalf("%+v", c)
				}
				if p.HasWrites() {
					t.Fatal("a skipped update writes nothing")
				}
			},
		},
		{
			name: "unknown factory stage is a skip",
			cfg:  localCfg(nil),
			state: func() *LocalState {
				st := localState(localRepo(), nightlyJob(localMarker))
				st.Factory["factory-build"] = FactoryResolution{Err: ErrUnknownFactoryStage}
				return st
			}(),
			check: func(t *testing.T, p LocalPlan) {
				if c := changeFor(t, p, ResourceCron, "factory-build"); c.Action != ActionSkip || c.Reason != ReasonUnknownStage {
					t.Fatalf("%+v", c)
				}
			},
		},
		{
			name: "nil factory resolver (nothing resolved) is a skip",
			cfg:  localCfg(nil),
			state: func() *LocalState {
				st := localState(localRepo(), nightlyJob(localMarker))
				st.Factory = map[string]FactoryResolution{}
				return st
			}(),
			check: func(t *testing.T, p LocalPlan) {
				if c := changeFor(t, p, ResourceCron, "factory-build"); c.Action != ActionSkip || c.Reason != ReasonFactoryUnavailable {
					t.Fatalf("%+v", c)
				}
			},
		},
		{
			name: "enabled omitted with the live row disabled leaves it disabled and warns",
			cfg:  localCfg(nil),
			state: func() *LocalState {
				j := nightlyJob(localMarker)
				j.IsEnabled = false
				return localState(localRepo(), j, buildJob("factory-build", localMarker))
			}(),
			check: func(t *testing.T, p LocalPlan) {
				if c := changeFor(t, p, ResourceCron, "nightly"); c.Action != ActionUnchanged || !hasWarning(p, `cron "nightly" is disabled on this daemon`) {
					t.Fatalf("%+v %q", c, p.Warnings)
				}
			},
		},
		{
			name: "host-free factory: no row is a create-via-cli skip, an unmarked row is adopted with its gate untouched",
			cfg: localCfg(func(c *Config) {
				c.Crons = append(c.Crons, Cron{Name: "factory-plan", FactoryStage: ptr("plan")})
			}),
			state: func() *LocalState {
				hand := buildJob("my build", "")
				hand.GateCommand = "node /opt/hand/gate.mjs"
				st := localState(localRepo(), nightlyJob(localMarker), hand)
				r := HostFreeFactory(DefaultFactoryStages())
				for _, name := range []string{"factory-build", "factory-plan"} {
					fc, err := r.Resolve(strings.TrimPrefix(name, "factory-"), "")
					st.Factory[name] = FactoryResolution{Cron: fc, Err: err}
				}
				return st
			}(),
			opts: func(o *LocalOptions) { o.HostFreeFactory = true },
			check: func(t *testing.T, p LocalPlan) {
				if c := changeFor(t, p, ResourceCron, "factory-plan"); c.Action != ActionSkip || c.Reason != ReasonCreateViaCLI {
					t.Fatalf("factory cron with no row: %+v", c)
				}
				c := changeFor(t, p, ResourceCron, "factory-build")
				if c.Action != ActionAdopt || slices.Contains(fieldNames(c), "gateCommand") {
					t.Fatalf("hand-made /boss-build row: %+v", c)
				}
				for _, op := range p.ops {
					if op.createCron != nil {
						t.Fatalf("a factory cron was created without its gate: %+v", op.createCron)
					}
					if op.updateCron != nil && op.updateCron.GateCommand != nil {
						t.Fatalf("a host-free plan touched a gate: %+v", op.updateCron)
					}
				}
			},
		},
		{
			name:  "enabled false with the live row enabled is an update",
			cfg:   localCfg(func(c *Config) { c.Crons[0].Enabled = ptr(false) }),
			state: localState(localRepo(), nightlyJob(localMarker), buildJob("factory-build", localMarker)),
			check: func(t *testing.T, p LocalPlan) {
				c := changeFor(t, p, ResourceCron, "nightly")
				if c.Action != ActionUpdate || !slices.Equal(fieldNames(c), []string{"enabled"}) || c.NeedsConfirmation {
					t.Fatalf("%+v", c)
				}
			},
		},
	}

	// Each field the file states differs from the live row: an update naming
	// it, needing confirmation only for prompt and gate.
	fieldCases := []struct {
		field   string
		edit    func(*Cron)
		confirm bool
	}{
		{"prompt", func(c *Cron) { c.Prompt = ptr("/nightly --deep") }, true},
		{"schedule", func(c *Cron) { c.Schedule = ptr("0 4 * * *") }, false},
		{"timezone", func(c *Cron) { c.Timezone = ptr("Europe/London") }, false},
		{"agent", func(c *Cron) { c.Agent = ptr("codex") }, false},
		{"model", func(c *Cron) { c.Model = ptr("opus") }, false},
		{"gateCommand", func(c *Cron) { c.GateCommand = ptr("./scripts/gate.sh") }, true},
		{"runSetupCommand", func(c *Cron) { c.RunSetupCommand = ptr(true) }, false},
		{"zeroOutput", func(c *Cron) { c.ZeroOutput = ptr(true) }, false},
		{"concurrencyPolicy", func(c *Cron) { c.ConcurrencyPolicy = ptr("allow-concurrent") }, false},
	}
	for _, fc := range fieldCases {
		tests = append(tests, struct {
			name  string
			cfg   *Config
			state *LocalState
			opts  func(*LocalOptions)
			check func(t *testing.T, p LocalPlan)
		}{
			name:  "cron field " + fc.field + " differs",
			cfg:   localCfg(func(c *Config) { fc.edit(&c.Crons[0]) }),
			state: localState(localRepo(), nightlyJob(localMarker), buildJob("factory-build", localMarker)),
			check: func(t *testing.T, p LocalPlan) {
				c := changeFor(t, p, ResourceCron, "nightly")
				if c.Action != ActionUpdate || !slices.Equal(fieldNames(c), []string{fc.field}) || c.NeedsConfirmation != fc.confirm {
					t.Fatalf("%+v", c)
				}
				if p.NeedsConfirmation() != fc.confirm {
					t.Fatalf("plan NeedsConfirmation = %v", p.NeedsConfirmation())
				}
			},
		})
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := localOpts(nil)
			if tc.opts != nil {
				tc.opts(&opts)
			}
			tc.check(t, PlanLocal(tc.cfg, tc.state, opts))
		})
	}
}

func mustMarshal(t *testing.T, p LocalPlan) string {
	t.Helper()
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// fakeLocalReader serves a fixed daemon state.
type fakeLocalReader struct {
	repos      []*pb.Repo
	jobs       map[string][]*pb.CronJob
	validation *pb.ValidateRepoPathResponse
	validated  []string
}

func (f *fakeLocalReader) ListRepos(context.Context) ([]*pb.Repo, error) { return f.repos, nil }

func (f *fakeLocalReader) ListCronJobs(_ context.Context, repoID string) ([]*pb.CronJob, error) {
	return f.jobs[repoID], nil
}

func (f *fakeLocalReader) ValidateRepoPath(_ context.Context, p string) (*pb.ValidateRepoPathResponse, error) {
	f.validated = append(f.validated, p)
	return f.validation, nil
}

type fakeFactory map[string]FactoryCron

func (f fakeFactory) Resolve(stage, _ string) (FactoryCron, error) {
	fc, ok := f[stage]
	if !ok {
		return FactoryCron{}, ErrUnknownFactoryStage
	}
	return fc, nil
}

func TestObserveLocal(t *testing.T) {
	reader := &fakeLocalReader{
		repos:      []*pb.Repo{localRepo()},
		jobs:       map[string][]*pb.CronJob{"repo-1": {nightlyJob(localMarker)}},
		validation: &pb.ValidateRepoPathResponse{IsValid: true, OriginUrl: "https://github.com/acme/widget"},
	}
	cfg := localCfg(func(c *Config) {
		c.Crons = append(c.Crons, Cron{Name: "retro", FactoryStage: ptr("retro")})
	})
	st, err := ObserveLocal(context.Background(), cfg, reader, fakeFactory{"build": localBuildFactory()}, localOpts(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(reader.validated, []string{localTarget}) || st.Registration().GetId() != "repo-1" || len(st.CronJobs) != 1 {
		t.Fatalf("validated %q state %+v", reader.validated, st)
	}
	if st.Factory["factory-build"].Cron.Prompt != "/boss-build" || !errors.Is(st.Factory["retro"].Err, ErrUnknownFactoryStage) {
		t.Fatalf("factory %+v", st.Factory)
	}
	if st.DefaultBranch(cfg) != "main" {
		t.Fatalf("default branch %q", st.DefaultBranch(cfg))
	}
}
