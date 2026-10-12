package projectconfig

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files")

// stageClassifier is a fake FactoryClassifier: exact prompt -> stage.
type stageClassifier map[string]string

func (c stageClassifier) Classify(job *pb.CronJob) (string, bool) {
	s, ok := c[job.GetPrompt()]
	return s, ok
}

var testClassifier = stageClassifier{"/boss-plan": "plan", "/boss-build": "build", "$boss-build": "build"}

func ptrTo[T any](v T) *T { return &v }

func exportRepoFixture() *pb.Repo {
	return &pb.Repo{
		Id:                              "repo-1",
		DisplayName:                     "Example",
		LocalPath:                       "/Users/a/code/example",
		OriginUrl:                       "git@github.com:acme/example.git",
		DefaultBaseBranch:               "main",
		SetupScript:                     ptrTo("make setup"),
		CanAutoMerge:                    true,
		MergeStrategy:                   "squash",
		CanAutoRepair:                   true,
		ShouldArchiveSessionsAfterMerge: true,
		CanAutoDeleteBranches:           true,
	}
}

func exportJobsFixture(home, node string) []*pb.CronJob {
	gate := func(skill string) string {
		return node + ` "` + home + "/.claude/skills/" + skill + "/toolbox/cron-gates/" + skill + `.mjs"`
	}
	return []*pb.CronJob{
		{Id: "j1", Name: "factory-plan", Prompt: "/boss-plan", Schedule: "*/15 * * * *", IsEnabled: true, AgentName: "claude", GateCommand: gate("boss-plan"), ShouldRunSetupCommand: true, ConcurrencyPolicy: pb.CronJobConcurrencyPolicy_CRON_JOB_CONCURRENCY_POLICY_SKIP},
		{Id: "j2", Name: "factory-build", Prompt: "/boss-build", Schedule: "*/15 * * * *", IsEnabled: true, GateCommand: gate("boss-build"), ShouldRunSetupCommand: true, ConcurrencyPolicy: pb.CronJobConcurrencyPolicy_CRON_JOB_CONCURRENCY_POLICY_SKIP},
		{Id: "j3", Name: "nightly-report", Prompt: "Summarise yesterday's merges.", Schedule: "0 6 * * *", Timezone: "Europe/London", IsEnabled: false, AgentName: "codex", Model: "gpt-5", GateCommand: "node scripts/has-work.mjs", ShouldRunSetupCommand: false, IsZeroOutput: true, ConcurrencyPolicy: pb.CronJobConcurrencyPolicy_CRON_JOB_CONCURRENCY_POLICY_ALLOW_CONCURRENT},
	}
}

func baseExportInput() ExportInput {
	return ExportInput{
		Repo:         exportRepoFixture(),
		CronJobs:     exportJobsFixture("$HOME", "node"),
		Daemon:       DaemonIdentity{ID: "d-laptop", DisplayName: "laptop"},
		CheckoutPath: "/Users/a/code/example",
		HomeDir:      "/Users/a",
		Classifier:   testClassifier,
	}
}

func mustExport(t *testing.T, in ExportInput) (*Config, []string, []byte) {
	t.Helper()
	cfg, warnings, err := Export(in)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	return cfg, warnings, mustEncode(t, cfg)
}

func cronByName(t *testing.T, cfg *Config, name string) Cron {
	t.Helper()
	for _, c := range cfg.Crons {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no cron %q", name)
	return Cron{}
}

func cloudExportInput() *CloudInput {
	launch := &pb.TriggerLaunchSettings{PromptTemplate: "Review it.", ShouldRunSetupCommand: ptrTo(true), IsZeroOutput: ptrTo(false)}
	return &CloudInput{
		OrganizationID: "org-1",
		Triggers: []*pb.Trigger{
			{Id: "t1", Name: "local-pin", TriggerType: "github", RepoOriginUrl: "https://github.com/acme/example", TypeConfig: &pb.Trigger_Github{Github: &pb.GithubTriggerConfig{EventTypes: []string{"pull_request.opened"}}}, Launch: launch, Placement: &pb.TriggerPlacement{Mode: pb.TriggerPlacementMode_TRIGGER_PLACEMENT_MODE_SPECIFIC_DAEMON, DaemonId: "d-laptop"}, IsEnabled: true, EndpointPath: "/ignored", ConfigVersion: 1},
			{Id: "t2", Name: "remote-pin", TriggerType: "http", RepoOriginUrl: "https://github.com/acme/example.git", TypeConfig: &pb.Trigger_Http{Http: &pb.HttpTriggerConfig{}}, Launch: launch, Placement: &pb.TriggerPlacement{Mode: pb.TriggerPlacementMode_TRIGGER_PLACEMENT_MODE_SPECIFIC_DAEMON, DaemonId: "d-other"}, IsEnabled: false, ConcurrencyPolicy: pb.TriggerConcurrencyPolicy_TRIGGER_CONCURRENCY_POLICY_CANCEL_IN_PROGRESS, CooldownSeconds: 30, Filters: []*pb.TriggerFilter{{Field: "body.env", Operator: pb.TriggerFilterOperator_TRIGGER_FILTER_OPERATOR_EQUALS, Values: []string{"prod"}}}, PayloadFields: []string{"body.ref"}, Gate: &pb.TriggerGate{ScriptPath: "scripts/gate.sh"}},
			{Id: "t3", Name: "anywhere", TriggerType: "github", RepoOriginUrl: "https://github.com/acme/example", TypeConfig: &pb.Trigger_Github{Github: &pb.GithubTriggerConfig{EventTypes: []string{"push"}}}, Launch: launch, Placement: &pb.TriggerPlacement{Mode: pb.TriggerPlacementMode_TRIGGER_PLACEMENT_MODE_FIRST_AVAILABLE}, IsEnabled: true},
			{Id: "t4", Name: "other-repo", TriggerType: "github", RepoOriginUrl: "https://github.com/acme/other", Launch: launch, Placement: &pb.TriggerPlacement{Mode: pb.TriggerPlacementMode_TRIGGER_PLACEMENT_MODE_FIRST_AVAILABLE}},
		},
		Webhooks: []*pb.SessionWebhook{
			{Id: "w1", Url: "https://hooks.example.com/b", EventTypes: []string{"session.passing", "session.failed"}, IsEnabled: true, Format: "raw"},
			{Id: "w2", Url: "https://hooks.example.com/a", Description: "Team", EventTypes: []string{"session.merged"}, IsEnabled: false, Format: "slack"},
		},
	}
}

func TestExport(t *testing.T) {
	t.Run("golden file", func(t *testing.T) {
		in := baseExportInput()
		in.Cloud = cloudExportInput()
		cfg, warnings, got := mustExport(t, in)
		golden := filepath.Join("testdata", "export-golden.json")
		if *updateGolden {
			if err := os.WriteFile(golden, got, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if want := readFixture(t, "export-golden.json"); !bytes.Equal(got, want) {
			t.Fatalf("export differs from golden:\n%s", got)
		}
		back, decodeWarnings := mustDecode(t, readFixture(t, "export-golden.json"))
		if len(decodeWarnings) != 0 || Validate(back) != nil {
			t.Fatalf("golden does not load cleanly: %v %v", decodeWarnings, Validate(back))
		}
		if len(cfg.Crons) != 3 || len(cfg.Cloud.Triggers) != 3 || len(cfg.Cloud.Webhooks) != 2 {
			t.Fatalf("rows missing: %+v", cfg)
		}
		if !slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, `"slack"`) }) {
			t.Fatalf("webhook format loss not warned: %v", warnings)
		}
	})

	t.Run("factory crons are symbolic", func(t *testing.T) {
		cfg, _, _ := mustExport(t, baseExportInput())
		for _, name := range []string{"factory-plan", "factory-build"} {
			c := cronByName(t, cfg, name)
			if !c.IsFactory() || c.Prompt != nil || c.GateCommand != nil || deref(c.Agent) != "claude" {
				t.Fatalf("%s not symbolic: %+v", name, c)
			}
		}
	})

	t.Run("the same factory job on two hosts exports identically", func(t *testing.T) {
		a := baseExportInput()
		b := baseExportInput()
		b.CronJobs = exportJobsFixture("/home/b", "/opt/homebrew/bin/node")
		b.CronJobs[0].AgentName = ""
		b.HomeDir, b.CheckoutPath = "/home/b", "/home/b/src/example"
		ca, _, _ := mustExport(t, a)
		cb, _, _ := mustExport(t, b)
		for _, name := range []string{"factory-plan", "factory-build"} {
			ja, _ := json.Marshal(cronByName(t, ca, name))
			jb, _ := json.Marshal(cronByName(t, cb, name))
			if !bytes.Equal(ja, jb) {
				t.Fatalf("%s differs across hosts:\n%s\n%s", name, ja, jb)
			}
		}
	})

	t.Run("plain cron rows", func(t *testing.T) {
		in := baseExportInput()
		in.CronJobs = append(in.CronJobs, &pb.CronJob{
			Name: "build-one", Prompt: "/boss-build BOS-1", Schedule: "0 * * * *", IsEnabled: true, ShouldRunSetupCommand: true,
			GateCommand:       `node "$HOME/.claude/skills/boss-build/toolbox/cron-gates/boss-build.mjs"`,
			ConcurrencyPolicy: pb.CronJobConcurrencyPolicy_CRON_JOB_CONCURRENCY_POLICY_SKIP,
		})
		cfg, warnings, out := mustExport(t, in)
		c := cronByName(t, cfg, "build-one")
		if c.IsFactory() || deref(c.Prompt) != "/boss-build BOS-1" || c.GateCommand != nil {
			t.Fatalf("not a plain cron without gate: %+v", c)
		}
		if !slices.ContainsFunc(warnings, func(w string) bool {
			return strings.HasPrefix(w, "crons[build-one].gateCommand") && strings.Contains(w, "host paths")
		}) {
			t.Fatalf("no host-path warning: %v", warnings)
		}
		if c.ConcurrencyPolicy != nil || c.Enabled != nil {
			t.Fatalf("defaults exported: %+v", c)
		}
		n := cronByName(t, cfg, "nightly-report")
		if deref(n.Enabled) || n.Enabled == nil || deref(n.GateCommand) != "node scripts/has-work.mjs" || deref(n.ConcurrencyPolicy) != "allow-concurrent" {
			t.Fatalf("plain cron fields lost: %+v", n)
		}
		if bytes.Contains(out, []byte(`"concurrencyPolicy": "skip"`)) {
			t.Fatal("SKIP exported")
		}
	})

	t.Run("two daemons sharing a display name pin distinct ids", func(t *testing.T) {
		var got []string
		for _, id := range []string{"d1", "d2"} {
			in := baseExportInput()
			in.Daemon = DaemonIdentity{ID: id, DisplayName: "mac"}
			cfg, _, _ := mustExport(t, in)
			if len(cfg.Daemons) != 1 || cfg.Daemons[0].Name != "mac" || cfg.Daemons[0].Path == nil || *cfg.Daemons[0].Path != "~/code/example" {
				t.Fatalf("daemon entry: %+v", cfg.Daemons)
			}
			got = append(got, deref(cfg.Daemons[0].DaemonID))
		}
		if !slices.Equal(got, []string{"d1", "d2"}) {
			t.Fatalf("pinned ids %v", got)
		}
	})

	t.Run("an unknown daemon id exports a name-only entry with one warning", func(t *testing.T) {
		in := baseExportInput()
		in.Daemon = DaemonIdentity{DisplayName: "mac"}
		cfg, warnings, _ := mustExport(t, in)
		if cfg.Daemons[0].DaemonID != nil {
			t.Fatal("daemonId exported without identity")
		}
		if len(warnings) != 1 || !strings.Contains(warnings[0], "match daemons by name only") {
			t.Fatalf("want one identity warning, got %v", warnings)
		}
	})

	t.Run("cloud triggers and webhooks", func(t *testing.T) {
		in := baseExportInput()
		in.Cloud = cloudExportInput()
		cfg, _, out := mustExport(t, in)
		byName := map[string]Trigger{}
		for _, tr := range cfg.Cloud.Triggers {
			byName[tr.Name] = tr
		}
		if _, ok := byName["other-repo"]; ok {
			t.Fatal("trigger on another origin exported")
		}
		if p := byName["local-pin"].Placement; deref(p.Daemon) != "laptop" || p.DaemonID != nil {
			t.Fatalf("local pin not named: %+v", p)
		}
		if p := byName["remote-pin"].Placement; deref(p.DaemonID) != "d-other" || p.Daemon != nil {
			t.Fatalf("remote pin lost: %+v", p)
		}
		if p := byName["anywhere"].Placement; p.Mode != PlacementFirstAvailable {
			t.Fatalf("first-available lost: %+v", p)
		}
		if deref(byName["remote-pin"].ConcurrencyPolicy) != "cancel-in-progress" || deref(byName["remote-pin"].Enabled) {
			t.Fatalf("trigger fields lost: %+v", byName["remote-pin"])
		}
		for _, w := range cfg.Cloud.Webhooks {
			if w.Secret != nil {
				t.Fatal("webhook secret exported")
			}
		}
		for _, outputOnly := range []string{"/ignored", "configVersion", `"t1"`} {
			if bytes.Contains(out, []byte(outputOnly)) {
				t.Fatalf("output-only field %s exported", outputOnly)
			}
		}
	})

	t.Run("project slug", func(t *testing.T) {
		cfg, _, _ := mustExport(t, baseExportInput())
		if cfg.Project != "example" {
			t.Fatalf("slug from origin: %q", cfg.Project)
		}
		in := baseExportInput()
		in.ExistingProject = "kept"
		if cfg, _, _ := mustExport(t, in); cfg.Project != "kept" {
			t.Fatalf("existing project not kept: %q", cfg.Project)
		}
		in.Project = "explicit"
		if cfg, _, _ := mustExport(t, in); cfg.Project != "explicit" {
			t.Fatalf("explicit project ignored: %q", cfg.Project)
		}
		in.Project = "Not Valid"
		if _, _, err := Export(in); err == nil {
			t.Fatal("invalid explicit project accepted")
		}
		if got := Slugify("My_Repo.Name"); got != "my-repo-name" {
			t.Fatalf("slugify: %q", got)
		}
	})

	t.Run("errors", func(t *testing.T) {
		in := baseExportInput()
		in.Repo = &pb.Repo{Id: "r"}
		if _, _, err := Export(in); err == nil {
			t.Fatal("repo without origin exported")
		}
		in.Repo = nil
		if _, _, err := Export(in); err == nil {
			t.Fatal("nil repo exported")
		}
		in = baseExportInput()
		in.CronJobs = append(in.CronJobs, &pb.CronJob{Name: "factory-plan", Prompt: "x", Schedule: "@daily", ShouldRunSetupCommand: true})
		if _, _, err := Export(in); err == nil || !strings.Contains(err.Error(), "duplicate cron name") {
			t.Fatalf("invalid live state exported: %v", err)
		}
	})

	t.Run("every export decodes, validates and re-encodes byte-identically", func(t *testing.T) {
		withCloud := baseExportInput()
		withCloud.Cloud = cloudExportInput()
		noID := baseExportInput()
		noID.Daemon = DaemonIdentity{}
		for _, in := range []ExportInput{baseExportInput(), withCloud, noID} {
			_, _, out := mustExport(t, in)
			cfg, warnings := mustDecode(t, out)
			if len(warnings) != 0 || Validate(cfg) != nil {
				t.Fatalf("export output does not load cleanly: %v %v", warnings, Validate(cfg))
			}
			if again := mustEncode(t, cfg); !bytes.Equal(again, out) {
				t.Fatalf("re-encode differs:\n%s\n%s", out, again)
			}
		}
	})
}

func TestExportNeverLeaksSecrets(t *testing.T) {
	const linear, sentry = "lin_live_123", "sntrys_456"
	in := baseExportInput()
	in.Repo.LinearApiKey, in.Repo.SentryApiKey = linear, sentry
	in.Cloud = cloudExportInput()
	cfg, warnings, out := mustExport(t, in)
	if cfg.Repo.Secrets == nil || deref(cfg.Repo.Secrets.LinearAPIKey).Env != LinearAPIKeyEnv || deref(cfg.Repo.Secrets.SentryAuthToken).Env != SentryAuthTokenEnv {
		t.Fatalf("references missing: %+v", cfg.Repo.Secrets)
	}
	for _, literal := range []string{linear, sentry} {
		if bytes.Contains(out, []byte(literal)) {
			t.Fatalf("encoded output contains %s", literal)
		}
		for _, w := range warnings {
			if strings.Contains(w, literal) {
				t.Fatalf("warning contains %s", literal)
			}
		}
	}

	in.Repo.LinearApiKey = ""
	cfg, _, out = mustExport(t, in)
	if cfg.Repo.Secrets.LinearAPIKey != nil || bytes.Contains(out, []byte(LinearAPIKeyEnv)) {
		t.Fatal("empty Linear key exported a reference")
	}
	in.Repo.SentryApiKey = ""
	cfg, _, _ = mustExport(t, in)
	if cfg.Repo.Secrets != nil {
		t.Fatal("secrets section exported with no live secret")
	}
}

func TestExportWebhookWarnsOnEmbeddedCredential(t *testing.T) {
	cases := []struct {
		url  string
		warn bool
	}{
		{"https://hooks.example.com/in", false},
		{"https://hooks.example.com/in?token=s3cr3t", true},
		{"https://user:s3cr3t@hooks.example.com/in", true},
	}
	for _, tc := range cases {
		_, warnings := ExportWebhook(&pb.SessionWebhook{Url: tc.url, EventTypes: []string{"session.completed"}})
		got := slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, "may embed a credential") })
		if got != tc.warn {
			t.Errorf("%s: credential warning = %v, want %v (warnings %q)", tc.url, got, tc.warn, warnings)
		}
		for _, w := range warnings {
			if strings.Contains(w, "s3cr3t") {
				t.Errorf("%s: warning repeats the credential: %q", tc.url, w)
			}
		}
	}
}
