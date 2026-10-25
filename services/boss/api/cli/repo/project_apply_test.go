package repo

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/recurser/boss/api/cli/skills"
	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/adapter/repotest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/projectconfig"
)

const projectApplyMarker = "boss-project:widget"

type applyFixture struct {
	dir, main string
	daemon    *repotest.RepoFake
	env       projectApplyEnv
	terminal  bool
	answer    bool
	asked     int
	vars      map[string]string
}

func newApplyFixture(t *testing.T) *applyFixture {
	t.Helper()
	dir := initGitRepo(t)
	main, err := skills.MainCheckout(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	d := repotest.New(t)
	t.Setenv("BOSS_SOCKET", d.SocketPath())
	d.SetValidateRepoPathResult(&pb.ValidateRepoPathResponse{IsValid: true, IsGithub: true, OriginUrl: "git@github.com:acme/widget.git", DefaultBranch: "main"})
	f := &applyFixture{dir: dir, main: main, daemon: d, vars: map[string]string{}}
	home := filepath.Dir(main)
	f.env = projectApplyEnv{
		identity: func(projectExportEnv) (projectconfig.DaemonIdentity, string) {
			return projectconfig.DaemonIdentity{ID: "d-test", DisplayName: "test-daemon"}, ""
		},
		export: projectExportEnv{
			newClient: testClient,
			profile:   func() (DaemonProfile, error) { return DaemonProfile{}, nil },
			hostname:  func() (string, error) { return "host-x", nil },
			homeDir:   func() (string, error) { return home, nil },
			cloud:     func(*cobra.Command) (CloudClient, error) { return nil, nil },
		},
		isTerminal: func() bool { return f.terminal },
		confirm: func(*cobra.Command, string) (bool, error) {
			f.asked++
			return f.answer, nil
		},
		getenv:     func(k string) string { return f.vars[k] },
		branch:     gitCurrentBranch,
		originHead: func(context.Context, string) string { return "main" },
		cronEnv:    func() skills.CronEnv { return skills.CronEnv{} },
	}
	return f
}

// projectFile renders a one-daemon file placing crons on this checkout.
func (f *applyFixture) projectFile(crons string, extraRepo string) string {
	repo := `"originUrl": "https://github.com/acme/widget"`
	if extraRepo != "" {
		repo += ", " + extraRepo
	}
	out := `{"version": 1, "project": "widget", "repo": {` + repo + `},
  "daemons": [{"name": "test-daemon", "daemonId": "d-test", "path": "` + f.main + `"}]`
	if crons != "" {
		out += `, "crons": [` + crons + `]`
	}
	return out + "}\n"
}

const twoCrons = `{"name": "nightly", "prompt": "/nightly", "schedule": "0 3 * * *"},
  {"name": "weekly", "prompt": "/weekly", "schedule": "0 4 * * 1", "gateCommand": "node scripts/gate.mjs"}`

func (f *applyFixture) write(t *testing.T, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, projectconfig.FileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *applyFixture) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	g := newTestGroup()
	g.newProjectApplyEnv = func() projectApplyEnv { return f.env }
	cmd := g.ProjectCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs(append(args, "--dir", f.dir))
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

// writes counts every daemon write the fake recorded.
func (f *applyFixture) writes() int {
	return len(f.daemon.RegisterRepoCalls()) + len(f.daemon.UpdateRepoCalls()) + len(f.daemon.CreateCronJobCalls()) +
		len(f.daemon.UpdateCronJobCalls()) + f.daemon.DeleteCronJobCallCount()
}

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	return ExitStatus(err)
}

func TestProjectApplyConverges(t *testing.T) {
	f := newApplyFixture(t)
	f.write(t, f.projectFile(twoCrons, ""))

	out, err := f.run(t, "apply", "--yes")
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	if regs := f.daemon.RegisterRepoCalls(); len(regs) != 1 || regs[0].GetLocalPath() != f.main {
		t.Fatalf("registrations %+v", regs)
	}
	creates := f.daemon.CreateCronJobCalls()
	if len(creates) != 2 {
		t.Fatalf("creates %+v\n%s", creates, out)
	}
	for _, c := range creates {
		if c.GetManagedBy() != projectApplyMarker || c.GetRepoId() == "" {
			t.Fatalf("create %+v", c)
		}
	}
	if creates[1].GetName() != "weekly" || creates[1].GetGateCommand() != "node scripts/gate.mjs" {
		t.Fatalf("weekly %+v", creates[1])
	}

	out, err = f.run(t, "diff", "--exit-code")
	if code := exitCodeOf(err); code != 0 || !strings.Contains(out, "nothing to change") {
		t.Fatalf("diff after apply exits %d: %v\n%s", code, err, out)
	}

	before := f.writes()
	if out, err := f.run(t, "apply", "--yes"); err != nil || f.writes() != before {
		t.Fatalf("second apply wrote %d: %v\n%s", f.writes()-before, err, out)
	}
}

func TestProjectDiffExitCode(t *testing.T) {
	f := newApplyFixture(t)
	f.write(t, f.projectFile(twoCrons, ""))
	if out, err := f.run(t, "apply", "--yes"); err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	if _, err := f.run(t, "diff", "--exit-code"); exitCodeOf(err) != 0 {
		t.Fatalf("converged: %v", err)
	}

	f.write(t, f.projectFile(strings.Replace(twoCrons, "0 3 * * *", "0 5 * * *", 1), ""))
	out, err := f.run(t, "diff", "--exit-code")
	if exitCodeOf(err) != 1 || err.Error() != "" || !strings.Contains(out, `update cron "nightly"`) {
		t.Fatalf("one update: exit %d %v\n%s", exitCodeOf(err), err, out)
	}
	if _, err := f.run(t, "diff"); err != nil {
		t.Fatalf("without --exit-code a pending change exits 0: %v", err)
	}
	before := f.writes()
	if _, err := f.run(t, "diff", "--exit-code", "--json"); exitCodeOf(err) != 1 || f.writes() != before {
		t.Fatalf("diff is read-only and keeps its code under --json: %v", err)
	}

	if err := os.Remove(filepath.Join(f.dir, projectconfig.FileName)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.run(t, "diff", "--exit-code"); exitCodeOf(err) != 2 || !strings.Contains(err.Error(), "no .boss-project.json") {
		t.Fatalf("missing file: exit %d %v", exitCodeOf(err), err)
	}
}

func TestProjectApplyTrust(t *testing.T) {
	t.Run("registered default develop governs both branches", func(t *testing.T) {
		f := newApplyFixture(t)
		f.daemon.AddRepo(&pb.Repo{Id: "repo-1", LocalPath: f.main, OriginUrl: "https://github.com/acme/widget", DefaultBaseBranch: "develop"})
		f.write(t, f.projectFile(twoCrons, ""))
		out, err := f.run(t, "apply", "--yes")
		if err == nil || !strings.Contains(err.Error(), `"develop"`) || !strings.Contains(err.Error(), "--allow-branch") || f.writes() != 0 {
			t.Fatalf("writes %d err %v\n%s", f.writes(), err, out)
		}
		if out, err := exec.Command("git", "-C", f.dir, "checkout", "-q", "-b", "develop").CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		if out, err := f.run(t, "apply", "--yes"); err != nil || len(f.daemon.CreateCronJobCalls()) != 2 {
			t.Fatalf("creates %d err %v\n%s", len(f.daemon.CreateCronJobCalls()), err, out)
		}
	})
	t.Run("non-TTY cron create without --yes writes nothing", func(t *testing.T) {
		f := newApplyFixture(t)
		f.write(t, f.projectFile(twoCrons, ""))
		out, err := f.run(t, "apply")
		if err == nil || f.writes() != 0 || !strings.Contains(err.Error(), "--yes") {
			t.Fatalf("writes %d err %v\n%s", f.writes(), err, out)
		}
		if exitCodeOf(err) == 0 {
			t.Fatal("refusal must exit non-zero")
		}
	})
	t.Run("with --yes the same plan applies", func(t *testing.T) {
		f := newApplyFixture(t)
		f.write(t, f.projectFile(twoCrons, ""))
		if out, err := f.run(t, "apply", "--yes"); err != nil || len(f.daemon.CreateCronJobCalls()) != 2 {
			t.Fatalf("%v\n%s", err, out)
		}
	})
	t.Run("a declined TTY confirmation writes nothing", func(t *testing.T) {
		f := newApplyFixture(t)
		f.terminal, f.answer = true, false
		f.write(t, f.projectFile(twoCrons, ""))
		out, err := f.run(t, "apply")
		if err == nil || f.asked != 1 || f.writes() != 0 || !strings.Contains(err.Error(), "declined") {
			t.Fatalf("asked %d writes %d err %v\n%s", f.asked, f.writes(), err, out)
		}
	})
	t.Run("an accepted TTY confirmation applies", func(t *testing.T) {
		f := newApplyFixture(t)
		f.terminal, f.answer = true, true
		f.write(t, f.projectFile(twoCrons, ""))
		if out, err := f.run(t, "apply"); err != nil || f.asked != 1 || len(f.daemon.CreateCronJobCalls()) != 2 {
			t.Fatalf("asked %d err %v\n%s", f.asked, err, out)
		}
	})
	t.Run("a repo-settings-only plan applies non-TTY without --yes", func(t *testing.T) {
		f := newApplyFixture(t)
		f.daemon.AddRepo(&pb.Repo{Id: "repo-1", LocalPath: f.main, OriginUrl: "https://github.com/acme/widget", DefaultBaseBranch: "main"})
		f.write(t, f.projectFile("", `"settings": {"canAutoMerge": true}`))
		out, err := f.run(t, "apply")
		updates := f.daemon.UpdateRepoCalls()
		if err != nil || len(updates) != 1 || !updates[0].GetCanAutoMerge() || f.writes() != 1 {
			t.Fatalf("updates %+v err %v\n%s", updates, err, out)
		}
	})
	t.Run("a feature-branch checkout without --allow-branch writes nothing", func(t *testing.T) {
		f := newApplyFixture(t)
		if out, err := exec.Command("git", "-C", f.dir, "checkout", "-q", "-b", "feature").CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		f.write(t, f.projectFile(twoCrons, ""))
		out, err := f.run(t, "apply", "--yes")
		if err == nil || f.writes() != 0 || !strings.Contains(err.Error(), "--allow-branch") {
			t.Fatalf("writes %d err %v\n%s", f.writes(), err, out)
		}
		if out, err := f.run(t, "apply", "--yes", "--allow-branch"); err != nil || len(f.daemon.CreateCronJobCalls()) != 2 {
			t.Fatalf("--allow-branch: %v\n%s", err, out)
		}
	})
	t.Run("an unknown branch and default branch never match", func(t *testing.T) {
		f := newApplyFixture(t)
		f.daemon.SetValidateRepoPathResult(&pb.ValidateRepoPathResponse{IsValid: true, IsGithub: true, OriginUrl: "git@github.com:acme/widget.git"})
		f.env.branch = func(context.Context, string) string { return "" }
		f.env.originHead = func(context.Context, string) string { return "" }
		f.write(t, f.projectFile(twoCrons, ""))
		out, err := f.run(t, "apply", "--yes")
		if err == nil || f.writes() != 0 || !strings.Contains(err.Error(), "--allow-branch") {
			t.Fatalf("writes %d err %v\n%s", f.writes(), err, out)
		}
	})
	t.Run("--dry-run writes nothing regardless", func(t *testing.T) {
		f := newApplyFixture(t)
		f.write(t, f.projectFile(twoCrons, ""))
		out, err := f.run(t, "apply", "--dry-run", "--yes", "--allow-branch")
		if err != nil || f.writes() != 0 || !strings.Contains(out, "Dry run") || !strings.Contains(out, `create cron "nightly"`) {
			t.Fatalf("writes %d err %v\n%s", f.writes(), err, out)
		}
	})
	t.Run("prune: a dropped cron is one delete in a dry run, and disabled without --prune", func(t *testing.T) {
		f := newApplyFixture(t)
		f.write(t, f.projectFile(twoCrons, ""))
		if out, err := f.run(t, "apply", "--yes"); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		f.write(t, f.projectFile(`{"name": "nightly", "prompt": "/nightly", "schedule": "0 3 * * *"}`, ""))
		out, err := f.run(t, "apply", "--prune", "--dry-run")
		if err != nil || strings.Count(out, "delete ") != 1 || !strings.Contains(out, `delete cron "weekly"`) {
			t.Fatalf("%v\n%s", err, out)
		}
		before := f.writes()
		if out, err := f.run(t, "apply"); err != nil || f.writes() != before+1 {
			t.Fatalf("disable: %v\n%s", err, out)
		}
		for _, job := range f.daemon.CronJobs() {
			if job.GetName() == "weekly" && job.GetIsEnabled() {
				t.Fatal("the dropped cron is still enabled")
			}
		}
		if f.daemon.DeleteCronJobCallCount() != 0 {
			t.Fatal("apply without --prune deleted a cron")
		}
	})
	t.Run("--remote and --host are rejected", func(t *testing.T) {
		root := &cobra.Command{Use: "boss"}
		root.PersistentFlags().String("remote", "", "")
		root.PersistentFlags().String("host", "", "")
		root.AddCommand(newTestGroup().ProjectCommand())
		root.SetArgs([]string{"project", "apply", "--remote", "https://x"})
		root.SilenceUsage, root.SilenceErrors = true, true
		if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "--remote and --host are not supported") {
			t.Fatalf("%v", err)
		}
	})
}

// TestProjectApplyFactoryRetro: a factoryStage retro cron resolves through the
// shared stage table, converges, and adopts a hand-named retro job.
func TestProjectApplyFactoryRetro(t *testing.T) {
	const retroCron = `{"name": "factory-retro", "factoryStage": "retro"}`
	withFactory := func(t *testing.T) *applyFixture {
		t.Helper()
		f := newApplyFixture(t)
		f.daemon.SetAgents([]*pb.AgentInfo{{Name: "claude"}})
		cron := newCronFixture(t, "claude")
		f.env.cronEnv = func() skills.CronEnv { return cron.env }
		if err := os.WriteFile(filepath.Join(f.main, skills.ConfigFilename), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		f.write(t, f.projectFile(retroCron, ""))
		return f
	}

	t.Run("creates, then re-applies unchanged", func(t *testing.T) {
		f := withFactory(t)
		out, err := f.run(t, "apply", "--yes")
		if err != nil {
			t.Fatalf("apply: %v\n%s", err, out)
		}
		creates := f.daemon.CreateCronJobCalls()
		if len(creates) != 1 {
			t.Fatalf("creates %+v\n%s", creates, out)
		}
		c := creates[0]
		if c.GetName() != "factory-retro" || c.GetManagedBy() != projectApplyMarker || c.GetPrompt() != "/boss-retro" ||
			c.GetSchedule() != "23 9 * * 1" || c.GetGateCommand() != retroGate(".claude") || !c.GetIsEnabled() {
			t.Fatalf("retro create %+v\n%s", c, out)
		}
		out, err = f.run(t, "diff", "--exit-code")
		if code := exitCodeOf(err); code != 0 || !strings.Contains(out, "nothing to change") {
			t.Fatalf("diff after apply exits %d: %v\n%s", code, err, out)
		}
		before := f.writes()
		if out, err := f.run(t, "apply", "--yes"); err != nil || f.writes() != before {
			t.Fatalf("second apply wrote %d: %v\n%s", f.writes()-before, err, out)
		}
	})

	t.Run("export then apply adopts a stage-default gate spelled absolute and unquoted", func(t *testing.T) {
		f := newApplyFixture(t)
		f.daemon.SetAgents([]*pb.AgentInfo{{Name: "claude"}})
		cron := newCronFixture(t, "claude")
		f.env.cronEnv = func() skills.CronEnv { return cron.env }
		// One home for the planner and the factory resolver, as on a real machine.
		f.env.export.homeDir = func() (string, error) { return cron.home, nil }
		// Export and apply resolve this daemon the same way.
		f.env.identity = localApplyIdentity
		if err := os.WriteFile(filepath.Join(f.main, skills.ConfigFilename), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		absGate := "node " + filepath.Join(cron.home, ".claude", "skills", "boss-retro", "toolbox", "cron-gates", "boss-retro.mjs")
		f.daemon.AddRepo(&pb.Repo{Id: "repo-1", LocalPath: f.main, OriginUrl: "https://github.com/acme/widget", DefaultBaseBranch: "main"})
		f.daemon.AddCronJob(&pb.CronJob{Id: "hand", RepoId: "repo-1", Name: "retro", Prompt: "/boss-retro", Schedule: "23 9 * * 1", IsEnabled: true, AgentName: "claude", GateCommand: absGate})

		g := newTestGroup()
		g.newProjectExportEnv = func() projectExportEnv { return f.env.export }
		export := g.ProjectCommand()
		var exported, exportErr bytes.Buffer
		export.SetOut(&exported)
		export.SetErr(&exportErr)
		export.SilenceUsage, export.SilenceErrors = true, true
		export.SetArgs([]string{"export", "--dir", f.dir})
		if err := export.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("export: %v\n%s", err, exportErr.String())
		}
		cfg, _, err := projectconfig.Decode(exported.Bytes())
		if err != nil {
			t.Fatalf("decode export: %v\n%s", err, exported.String())
		}
		if len(cfg.Crons) != 1 || cfg.Crons[0].Name != "retro" || cfg.Crons[0].FactoryStage == nil || *cfg.Crons[0].FactoryStage != "retro" {
			t.Fatalf("want one factoryStage retro row, got %+v\n%s", cfg.Crons, exported.String())
		}
		f.write(t, exported.String())

		f.terminal = false
		out, err := f.run(t, "apply")
		if err != nil {
			t.Fatalf("apply without --yes: %v\n%s", err, out)
		}
		if f.asked != 0 {
			t.Fatalf("apply asked %d times\n%s", f.asked, out)
		}
		for _, u := range f.daemon.UpdateCronJobCalls() {
			if u.GateCommand != nil {
				t.Fatalf("apply rewrote an equivalent gate: %+v\n%s", u, out)
			}
		}
		if job := f.daemon.CronJobs()["hand"]; job.GetGateCommand() != absGate || job.GetManagedBy() != projectApplyMarker {
			t.Fatalf("adopted job %+v\n%s", job, out)
		}
		out, err = f.run(t, "diff", "--exit-code")
		if code := exitCodeOf(err); code != 0 || !strings.Contains(out, "nothing to change") {
			t.Fatalf("diff after export and apply exits %d: %v\n%s", code, err, out)
		}
		t.Logf("diff after export and apply:\n%s", out)
	})

	t.Run("adopts the hand-named boss-retro job", func(t *testing.T) {
		f := withFactory(t)
		f.daemon.AddRepo(&pb.Repo{Id: "repo-1", LocalPath: f.main, OriginUrl: "https://github.com/acme/widget", DefaultBaseBranch: "main"})
		f.daemon.AddCronJob(&pb.CronJob{Id: "hand", RepoId: "repo-1", Name: "boss-retro", Prompt: "/boss-retro", Schedule: "0 9 * * 1", IsEnabled: true, AgentName: "claude", GateCommand: "node skills-toolbox/cron-gates/boss-retro.mjs"})
		out, err := f.run(t, "diff", "--json")
		if err != nil {
			t.Fatalf("diff: %v\n%s", err, out)
		}
		var plan projectconfig.Plan
		if err := json.Unmarshal([]byte(out), &plan); err != nil {
			t.Fatalf("diff --json: %v\n%s", err, out)
		}
		var crons []projectconfig.Change
		for _, c := range plan.Changes {
			if c.Resource == projectconfig.ResourceCron {
				crons = append(crons, c)
			}
		}
		if len(crons) != 1 {
			t.Fatalf("cron changes %+v", crons)
		}
		adopt := crons[0]
		if adopt.Action != projectconfig.ActionAdopt || !strings.Contains(adopt.Detail, `adopt "boss-retro" as "factory-retro"`) || !adopt.NeedsConfirmation {
			t.Fatalf("want a confirmed adopt with rename, got %+v", adopt)
		}
		if !slices.ContainsFunc(adopt.Fields, func(d projectconfig.FieldDiff) bool { return d.Field == "gateCommand" }) {
			t.Fatalf("adopt does not change the gate: %+v", adopt.Fields)
		}
		if len(f.daemon.CreateCronJobCalls()) != 0 {
			t.Fatal("diff wrote")
		}
		if out, err := f.run(t, "apply", "--yes"); err != nil || len(f.daemon.CreateCronJobCalls()) != 0 {
			t.Fatalf("apply created a duplicate: %v\n%s", err, out)
		}
		if job := f.daemon.CronJobs()["hand"]; job.GetName() != "factory-retro" || job.GetGateCommand() != retroGate(".claude") || job.GetManagedBy() != projectApplyMarker {
			t.Fatalf("adopted job %+v", job)
		}
	})
}

func TestProjectDefaultBranch(t *testing.T) {
	registration := &projectconfig.LocalState{
		Repos:          []*pb.Repo{{Id: "repo-1", DefaultBaseBranch: "develop"}},
		RepoCheckouts:  map[string]string{"repo-1": "/repo"},
		TargetCheckout: "/repo",
		Validation:     &pb.ValidateRepoPathResponse{DefaultBranch: "detected"},
	}
	for _, tt := range []struct {
		name, fileBranch, want string
		state                  *projectconfig.LocalState
	}{
		{name: "file without daemon", fileBranch: "release", want: "release"},
		{name: "origin HEAD without daemon", want: "main"},
		{name: "registration before detection and origin HEAD", state: registration, want: "develop"},
		{name: "file before registration", fileBranch: "release", state: registration, want: "release"},
		{name: "daemon detection before origin HEAD", state: &projectconfig.LocalState{Validation: &pb.ValidateRepoPathResponse{DefaultBranch: "detected"}}, want: "detected"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &projectconfig.Config{}
			if tt.fileBranch != "" {
				cfg.Repo.DefaultBaseBranch = &tt.fileBranch
			}
			proj := &loadedProject{cfg: cfg, checkout: "/repo"}
			env := projectApplyEnv{originHead: func(context.Context, string) string { return "main" }}
			if got := projectDefaultBranch(context.Background(), env, proj, tt.state); got != tt.want {
				t.Fatalf("default branch = %q, want %q", got, tt.want)
			}
		})
	}
}
