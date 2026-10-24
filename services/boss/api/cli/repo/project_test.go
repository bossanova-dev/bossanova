package repo

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/recurser/boss/api/cli/skills"

	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/boss/internal/tuitest"
	"github.com/recurser/bossalib/daemonstate"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/projectconfig"
)

const projectTestLinearKey = "lin_live_do_not_print"

type projectFixture struct {
	dir, main, appData string
	daemon             *tuitest.MockDaemon
	env                projectExportEnv
}

func newProjectFixture(t *testing.T) *projectFixture {
	t.Helper()
	dir := initGitRepo(t)
	main, err := skills.MainCheckout(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	d := tuitest.NewMockDaemon(t)
	t.Setenv("BOSS_SOCKET", d.SocketPath())
	d.AddRepo(&pb.Repo{
		Id: "repo-1", LocalPath: main, OriginUrl: "git@github.com:acme/widget.git", DisplayName: "widget",
		DefaultBaseBranch: "main", CanAutoRepair: true, LinearApiKey: projectTestLinearKey,
	})
	for _, st := range projectconfig.DefaultFactoryStages() {
		d.AddCronJob(&pb.CronJob{
			Id: st.JobName, RepoId: "repo-1", Name: st.JobName, Prompt: st.Prompt("claude"), Schedule: st.DefaultSchedule,
			IsEnabled: true, AgentName: "claude", ShouldRunSetupCommand: true,
			GateCommand:       `node "$HOME/.claude/skills/` + st.Skill + "/toolbox/cron-gates/" + st.Skill + ".mjs" + `"`,
			ConcurrencyPolicy: pb.CronJobConcurrencyPolicy_CRON_JOB_CONCURRENCY_POLICY_SKIP,
		})
	}
	f := &projectFixture{dir: dir, main: main, appData: t.TempDir(), daemon: d}
	f.writeIdentity(t, d.SocketPath())
	home := filepath.Dir(main)
	f.env = projectExportEnv{
		newClient: testClient,
		profile: func() (DaemonProfile, error) {
			return DaemonProfile{AppDataDir: f.appData, SocketPath: d.SocketPath()}, nil
		},
		hostname: func() (string, error) { return "host-x", nil },
		homeDir:  func() (string, error) { return home, nil },
		cloud: func(*cobra.Command) (CloudClient, error) {
			return nil, errors.New("cloud read without --cloud")
		},
	}
	return f
}

func (f *projectFixture) writeIdentity(t *testing.T, socket string) {
	t.Helper()
	if err := daemonstate.Write(f.appData, daemonstate.Metadata{PID: 1, SocketPath: socket, DaemonID: "d-test", DisplayName: "test-daemon"}); err != nil {
		t.Fatal(err)
	}
}

func (f *projectFixture) run(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	g := newTestGroup()
	g.newProjectExportEnv = func() projectExportEnv { return f.env }
	cmd := g.ProjectCommand()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs(append([]string{"export", "--dir", f.dir}, args...))
	err := cmd.ExecuteContext(context.Background())
	return stdout.String(), stderr.String(), err
}

// fakeProjectCloud is an in-memory CloudClient.
type fakeProjectCloud struct {
	mapping     *pb.RepoOrganizationMapping
	triggers    []*pb.Trigger
	webhooks    []*pb.SessionWebhook
	listedOrgs  []string
	mappedRepos []string
}

func (f *fakeProjectCloud) ListOrganizations(context.Context) ([]*pb.Organization, error) {
	return []*pb.Organization{{Id: "org-only"}}, nil
}

func (f *fakeProjectCloud) GetRepoOrganization(_ context.Context, origin string) (*pb.RepoOrganizationMapping, error) {
	f.mappedRepos = append(f.mappedRepos, origin)
	return f.mapping, nil
}

func (f *fakeProjectCloud) ListTriggers(_ context.Context, req *pb.ListTriggersRequest) ([]*pb.Trigger, error) {
	f.listedOrgs = append(f.listedOrgs, req.GetOrganizationId())
	return f.triggers, nil
}

func (f *fakeProjectCloud) ListSessionWebhooks(_ context.Context, org string) ([]*pb.SessionWebhook, error) {
	f.listedOrgs = append(f.listedOrgs, org)
	return f.webhooks, nil
}

func decodeExport(t *testing.T, out string) *projectconfig.Config {
	t.Helper()
	cfg, warnings, err := projectconfig.Decode([]byte(out))
	if err != nil || len(warnings) != 0 {
		t.Fatalf("export output does not decode cleanly: %v %v\n%s", err, warnings, out)
	}
	if err := projectconfig.Validate(cfg); err != nil {
		t.Fatalf("export output does not validate: %v", err)
	}
	again, err := projectconfig.Encode(cfg)
	if err != nil || string(again) != out {
		t.Fatalf("export output is not canonical: %v\n%s", err, out)
	}
	return cfg
}

func warningLines(stderr string) []string {
	var out []string
	for line := range strings.SplitSeq(stderr, "\n") {
		if strings.HasPrefix(line, "warning: ") {
			out = append(out, line)
		}
	}
	return out
}

func TestProjectExport(t *testing.T) {
	t.Run("prints the canonical file", func(t *testing.T) {
		f := newProjectFixture(t)
		out, stderr, err := f.run(t)
		if err != nil {
			t.Fatalf("export: %v\n%s", err, stderr)
		}
		if stderr != "" {
			t.Fatalf("unexpected stderr: %s", stderr)
		}
		cfg := decodeExport(t, out)
		if cfg.Project != "widget" || cfg.Repo.OriginURL != "https://github.com/acme/widget" {
			t.Fatalf("repo identity: %+v", cfg.Repo)
		}
		if len(cfg.Daemons) != 1 || cfg.Daemons[0].Name != "test-daemon" || cfg.Daemons[0].DaemonID == nil || *cfg.Daemons[0].DaemonID != "d-test" {
			t.Fatalf("daemon entry not pinned: %+v", cfg.Daemons)
		}
		if len(cfg.Crons) != len(projectconfig.DefaultFactoryStages()) {
			t.Fatalf("crons: %+v", cfg.Crons)
		}
		for _, c := range cfg.Crons {
			if !c.IsFactory() || c.Prompt != nil || c.GateCommand != nil {
				t.Fatalf("factory job not symbolic: %+v", c)
			}
		}
		if strings.Contains(out, projectTestLinearKey) || !strings.Contains(out, projectconfig.LinearAPIKeyEnv) {
			t.Fatal("secret handling wrong: want a BOSS_* reference and never the value")
		}
		if _, err := os.Stat(filepath.Join(f.main, projectconfig.FileName)); !os.IsNotExist(err) {
			t.Fatal("stdout export wrote a file")
		}
	})

	t.Run("--cloud without a login names boss login before any request", func(t *testing.T) {
		f := newProjectFixture(t)
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(srv.Close)
		mgr := auth.NewManager(emptyTokenStore{}, auth.Config{})
		f.env.cloud = func(cmd *cobra.Command) (CloudClient, error) {
			return projectCloudRemote(cmd.Context(), mgr, srv.URL)
		}
		out, _, err := f.run(t, "--cloud")
		if err == nil || !strings.Contains(err.Error(), "boss login") {
			t.Fatalf("want the boss login error, got %v", err)
		}
		if out != "" || calls.Load() != 0 {
			t.Fatalf("output %q or %d cloud request(s) without a login", out, calls.Load())
		}
	})

	t.Run("--cloud exports the repository's triggers and the organization's webhooks", func(t *testing.T) {
		f := newProjectFixture(t)
		launch := &pb.TriggerLaunchSettings{PromptTemplate: "go"}
		fake := &fakeProjectCloud{
			mapping: &pb.RepoOrganizationMapping{OrganizationId: "org-9"},
			triggers: []*pb.Trigger{
				{Name: "mine", TriggerType: "github", RepoOriginUrl: "https://github.com/acme/widget", Launch: launch, IsEnabled: true,
					TypeConfig: &pb.Trigger_Github{Github: &pb.GitHubTriggerConfig{EventTypes: []string{"push"}}},
					Placement:  &pb.TriggerPlacement{Mode: pb.TriggerPlacementMode_TRIGGER_PLACEMENT_MODE_SPECIFIC_DAEMON, DaemonId: "d-test"}},
				{Name: "theirs", TriggerType: "github", RepoOriginUrl: "https://github.com/acme/other", Launch: launch, IsEnabled: true,
					Placement: &pb.TriggerPlacement{Mode: pb.TriggerPlacementMode_TRIGGER_PLACEMENT_MODE_FIRST_AVAILABLE}},
			},
			webhooks: []*pb.SessionWebhook{{Url: "https://hooks.example.com/x", EventTypes: []string{"session.passing"}, IsEnabled: true}},
		}
		f.env.cloud = func(*cobra.Command) (CloudClient, error) { return fake, nil }
		out, stderr, err := f.run(t, "--cloud")
		if err != nil {
			t.Fatalf("export --cloud: %v\n%s", err, stderr)
		}
		cfg := decodeExport(t, out)
		if cfg.Cloud == nil || deref(cfg.Cloud.Organization) != "org-9" || len(cfg.Cloud.Triggers) != 1 || len(cfg.Cloud.Webhooks) != 1 {
			t.Fatalf("cloud section: %+v", cfg.Cloud)
		}
		if p := cfg.Cloud.Triggers[0].Placement; deref(p.Daemon) != "test-daemon" {
			t.Fatalf("local pin not named: %+v", p)
		}
		if len(fake.listedOrgs) != 2 || fake.listedOrgs[0] != "org-9" || fake.mappedRepos[0] != "https://github.com/acme/widget" {
			t.Fatalf("org resolution: listed %v mapped %v", fake.listedOrgs, fake.mappedRepos)
		}

		fake.mapping, fake.listedOrgs = nil, nil
		if _, _, err := f.run(t, "--cloud"); err != nil || fake.listedOrgs[0] != "org-only" {
			t.Fatalf("sole-org fallback: %v %v", err, fake.listedOrgs)
		}
		fake.listedOrgs = nil
		if _, _, err := f.run(t, "--cloud", "--org", "org-flag"); err != nil || fake.listedOrgs[0] != "org-flag" {
			t.Fatalf("--org ignored: %v %v", err, fake.listedOrgs)
		}
	})

	t.Run("an unregistered checkout names boss init --register", func(t *testing.T) {
		f := newProjectFixture(t)
		f.dir = initGitRepo(t)
		_, _, err := f.run(t, "--write")
		if err == nil || !strings.Contains(err.Error(), "boss init --register") {
			t.Fatalf("want the register hint, got %v", err)
		}
		if _, err := os.Stat(filepath.Join(f.dir, projectconfig.FileName)); !os.IsNotExist(err) {
			t.Fatal("file written for an unregistered checkout")
		}
	})

	t.Run("--write refuses an existing file without --force", func(t *testing.T) {
		f := newProjectFixture(t)
		target := filepath.Join(f.main, projectconfig.FileName)
		sentinel := []byte("{\"keep\": true}\n")
		if err := os.WriteFile(target, sentinel, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := f.run(t, "--write"); err == nil || !strings.Contains(err.Error(), "--force") {
			t.Fatalf("want a --force refusal, got %v", err)
		}
		if got, _ := os.ReadFile(target); !bytes.Equal(got, sentinel) {
			t.Fatal("existing file changed without --force")
		}
		out, _, err := f.run(t, "--write", "--force")
		if err != nil || !strings.Contains(out, "Wrote "+target) {
			t.Fatalf("--force: %v %q", err, out)
		}
		written, _ := os.ReadFile(target)
		decodeExport(t, string(written))
		if _, _, err := f.run(t, "--force"); err == nil {
			t.Fatal("--force without --write accepted")
		}
	})

	t.Run("--write warns when git ignores the file", func(t *testing.T) {
		f := newProjectFixture(t)
		if err := os.WriteFile(filepath.Join(f.main, ".gitignore"), []byte(projectconfig.FileName+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, stderr, err := f.run(t, "--write")
		if err != nil || !strings.Contains(stderr, "ignored by git") {
			t.Fatalf("no ignore warning: %v %q", err, stderr)
		}
	})

	t.Run("a re-export keeps the existing project slug", func(t *testing.T) {
		f := newProjectFixture(t)
		if _, _, err := f.run(t, "--write", "--project", "custom-slug"); err != nil {
			t.Fatal(err)
		}
		out, _, err := f.run(t)
		if err != nil || decodeExport(t, out).Project != "custom-slug" {
			t.Fatalf("slug not kept: %v", err)
		}
	})

	t.Run("daemonstate for another socket is ignored", func(t *testing.T) {
		f := newProjectFixture(t)
		f.writeIdentity(t, filepath.Join(t.TempDir(), "other.sock"))
		out, stderr, err := f.run(t)
		if err != nil {
			t.Fatal(err)
		}
		cfg := decodeExport(t, out)
		if cfg.Daemons[0].DaemonID != nil || cfg.Daemons[0].Name != "host-x" {
			t.Fatalf("foreign identity trusted: %+v", cfg.Daemons[0])
		}
		if w := warningLines(stderr); len(w) != 1 || !strings.Contains(w[0], "by name only") {
			t.Fatalf("want one identity warning, got %q", stderr)
		}
	})

	t.Run("the classifier matches exact stage prompts only", func(t *testing.T) {
		c := factoryStageClassifier{}
		for _, st := range projectconfig.DefaultFactoryStages() {
			for _, agent := range []string{"claude", "codex"} {
				if got, ok := c.Classify(&pb.CronJob{Prompt: st.Prompt(agent), AgentName: agent}); !ok || got != st.Stage {
					t.Fatalf("%s/%s not classified: %q %v", st.Stage, agent, got, ok)
				}
			}
		}
		if _, ok := c.Classify(&pb.CronJob{Prompt: "/boss-plan"}); !ok {
			t.Fatal("empty agent is claude")
		}
		for _, job := range []*pb.CronJob{
			{Prompt: "/boss-build BOS-1", AgentName: "claude"},
			{Prompt: "$boss-plan", AgentName: "claude"},
			{Prompt: "summarise", AgentName: "claude"},
		} {
			if stage, ok := c.Classify(job); ok {
				t.Fatalf("%q classified as %s", job.GetPrompt(), stage)
			}
		}
	})
}

func TestFactoryStagesClassifiesRetro(t *testing.T) {
	c := factoryStageClassifier{}
	for _, job := range []*pb.CronJob{
		{Name: "boss-retro", Prompt: "/boss-retro", AgentName: "claude"},
		{Name: "weekly", Prompt: "/boss-retro"},
		{Name: "factory-retro", Prompt: "$boss-retro", AgentName: "codex"},
	} {
		if stage, ok := c.Classify(job); !ok || stage != "retro" {
			t.Fatalf("%q (agent %q) classified %q %v, want retro", job.GetPrompt(), job.GetAgentName(), stage, ok)
		}
	}
	for _, job := range []*pb.CronJob{
		{Prompt: "/boss-retro --dry-run", AgentName: "claude"},
		{Prompt: "$boss-retro", AgentName: "claude"},
	} {
		if stage, ok := c.Classify(job); ok {
			t.Fatalf("%q classified as %s; it exports as a plain cron", job.GetPrompt(), stage)
		}
	}
	if got, ok := c.DefaultSchedule("retro"); !ok || got != "23 9 * * 1" {
		t.Fatalf("retro default schedule %q %v", got, ok)
	}

	f := newProjectFixture(t)
	out, _, err := f.run(t)
	if err != nil {
		t.Fatal(err)
	}
	var retro *projectconfig.Cron
	cfg := decodeExport(t, out)
	for i := range cfg.Crons {
		if cfg.Crons[i].Name == "factory-retro" {
			retro = &cfg.Crons[i]
		}
	}
	if retro == nil || retro.FactoryStage == nil || *retro.FactoryStage != "retro" || retro.Prompt != nil || retro.GateCommand != nil ||
		retro.Schedule == nil || *retro.Schedule != "23 9 * * 1" {
		t.Fatalf("live retro job not exported as a symbolic factoryStage retro: %+v\n%s", retro, out)
	}
}
