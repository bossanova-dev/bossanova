package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/client"
	"github.com/recurser/boss/internal/skillinstall"
	"github.com/recurser/boss/internal/tuitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

// factoryDocJob is one `boss cron add` line from docs/skills/factory.md.
type factoryDocJob struct{ name, schedule, prompt, gate string }

var factoryDocCronAdd = regexp.MustCompile(`^boss cron add --repo "\$REPO_ID" --name (\S+) --schedule '([^']*)' --prompt '([^']*)' --gate '([^']*)' --enabled=false$`)

// factoryDocJobs parses the literal cron command lines out of factory.md.
func factoryDocJobs(t *testing.T) []factoryDocJob {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	doc := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "docs", "skills", "factory.md")
	data, err := os.ReadFile(doc)
	if err != nil {
		t.Fatalf("read %s: %v", doc, err)
	}
	var jobs []factoryDocJob
	for line := range strings.SplitSeq(string(data), "\n") {
		if !strings.HasPrefix(line, "boss cron add ") {
			continue
		}
		m := factoryDocCronAdd.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("factory.md cron line has an unexpected shape: %s", line)
		}
		jobs = append(jobs, factoryDocJob{name: m[1], schedule: m[2], prompt: m[3], gate: m[4]})
	}
	if len(jobs) != 3 {
		t.Fatalf("factory.md: want 3 boss cron add lines, got %d", len(jobs))
	}
	return jobs
}

// codexDocJob applies factory.md's documented Codex substitution.
func codexDocJob(j factoryDocJob) factoryDocJob {
	j.gate = strings.ReplaceAll(j.gate, "$HOME/.claude/skills", "$HOME/.codex/skills")
	j.prompt = "$" + strings.TrimPrefix(j.prompt, "/")
	return j
}

// cronFixture is a MockDaemon-backed repo with fake installed skill trees for
// both agents, a fake service PATH, and a fully injected host environment.
type cronFixture struct {
	t           *testing.T
	daemon      *tuitest.MockDaemon
	c           client.BossClient
	repo        *pb.Repo
	repoDir     string
	configPath  string
	home        string
	serviceDir  string
	baseFiles   map[string][]byte
	env         initCronEnv
	validated   []string // node paths validate ran with
	rejectWith  error
	callerNode  string
	effectiveFn func(nodePath, module string, config []byte) (cronEffective, error)
}

func writeExecutable(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func stringPtr(s string) *string { return &s }

func defaultCronEffective() cronEffective {
	q := func(state string) cronEffectiveEntry {
		return cronEffectiveEntry{Value: &cronStageQuery{State: stringPtr(state), Selection: map[string]map[string][]string{}}}
	}
	return cronEffective{Plan: q("Backlog"), Build: q("Todo"), Verify: q("In Review"), MCPServer: cronEffectiveString{Value: stringPtr("linear")}}
}

func newCronFixture(t *testing.T, loaded ...string) *cronFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stub executables are POSIX-only")
	}
	f := &cronFixture{t: t, repoDir: t.TempDir(), home: t.TempDir(), serviceDir: t.TempDir()}
	f.daemon = tuitest.NewMockDaemon(t)
	f.c = client.NewLocal(f.daemon.SocketPath())
	agents := make([]*pb.AgentInfo, 0, len(loaded))
	for _, name := range loaded {
		agents = append(agents, &pb.AgentInfo{Name: name})
	}
	f.daemon.SetAgents(agents)
	f.repo = &pb.Repo{Id: "repo-1", LocalPath: f.repoDir, DefaultBaseBranch: "main"}
	f.daemon.AddRepo(f.repo)
	f.configPath = filepath.Join(f.repoDir, ".boss-skills.json")
	if err := os.WriteFile(f.configPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, agentDir := range []string{".claude", ".codex"} {
		for _, st := range factoryCronStages {
			root := filepath.Join(f.home, agentDir, "skills")
			for _, rel := range []string{st.gatePath(), st.modulePath()} {
				p := filepath.Join(root, filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte("// stub\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	for _, tool := range []string{"node", "claude", "codex"} {
		writeExecutable(t, f.serviceDir, tool, "exit 0\n")
	}
	f.baseFiles = map[string][]byte{
		".mcp.json":          []byte(`{"mcpServers":{"linear":{"type":"http"}}}`),
		".codex/config.toml": []byte("[mcp_servers.\"linear\"]\nurl = \"x\"\n"),
	}
	f.effectiveFn = func(string, string, []byte) (cronEffective, error) { return defaultCronEffective(), nil }
	f.env = initCronEnv{
		servicePath: func() string { return f.serviceDir },
		callerLookPath: func(name string) (string, error) {
			if name == "node" && f.callerNode != "" {
				return f.callerNode, nil
			}
			return "", errors.New("not found")
		},
		homeDir: func() (string, error) { return f.home, nil },
		skillRoot: func(agent string) (string, error) {
			return filepath.Join(f.home, "."+agent, "skills"), nil
		},
		defaultBranch: func(context.Context, string, *pb.Repo) string { return "main" },
		readRef: func(_ context.Context, _, ref, p string) ([]byte, bool, error) {
			if ref != "origin/main" {
				return nil, false, errors.New("unexpected ref " + ref)
			}
			data, ok := f.baseFiles[p]
			return data, ok, nil
		},
		validate: func(nodePath, _ string, _ []byte, _ string) error {
			f.validated = append(f.validated, nodePath)
			return f.rejectWith
		},
		effective: func(nodePath, module string, config []byte) (cronEffective, error) {
			return f.effectiveFn(nodePath, module, config)
		},
	}
	return f
}

func (f *cronFixture) run(req initCronRequest) (initCronResult, string) {
	f.t.Helper()
	if err := req.normalize(); err != nil {
		f.t.Fatal(err)
	}
	res, err := setupInitCron(context.Background(), f.c, f.repo, f.repoDir, f.configPath, req, f.env)
	if err != nil {
		f.t.Fatalf("setupInitCron: %v", err)
	}
	return res, res.report()
}

// realNode returns the host node, skipping when there is none. Only the tests
// that prove the installed skill-config module's real semantics use it.
func realNode(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	return node
}

// installRealModules copies the embedded skill-config module and its sibling
// imports into each stage's toolbox under the fixture's skill roots, and puts a
// wrapper for the real node on the fake service PATH.
func (f *cronFixture) installRealModules(node string) {
	f.t.Helper()
	writeExecutable(f.t, f.serviceDir, "node", "exec '"+node+"' \"$@\"\n")
	for _, agentDir := range []string{".claude", ".codex"} {
		for _, st := range factoryCronStages {
			toolbox := filepath.Join(f.home, agentDir, "skills", st.skill, "toolbox")
			for _, name := range append([]string{"skill-config.mjs"}, skillConfigSiblingModules...) {
				data, err := fs.ReadFile(skillinstall.SkillsFS, path.Join("skills", st.skill, "toolbox", name))
				if err != nil {
					f.t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(toolbox, name), data, 0o644); err != nil {
					f.t.Fatal(err)
				}
			}
		}
	}
	f.env.validate = validateWithInstalledModule
	f.env.effective = effectiveWithInstalledModule
}

func TestInitCronStageTablePinnedToFactoryDoc(t *testing.T) {
	doc := factoryDocJobs(t)
	if len(doc) != len(factoryCronStages) {
		t.Fatalf("stage table has %d rows, factory.md %d", len(factoryCronStages), len(doc))
	}
	for i, st := range factoryCronStages {
		got := factoryDocJob{
			name:     st.name,
			schedule: st.schedule,
			prompt:   st.prompt(cronAgentClaude),
			gate:     `node "` + renderSkillRoot("/h/.claude/skills", "/h") + "/" + st.gatePath() + `"`,
		}
		if got != doc[i] {
			t.Errorf("stage %s: table %+v, factory.md %+v", st.stage, got, doc[i])
		}
		codex := factoryDocJob{
			name: st.name, schedule: st.schedule, prompt: st.prompt(cronAgentCodex),
			gate: `node "` + renderSkillRoot("/h/.codex/skills", "/h") + "/" + st.gatePath() + `"`,
		}
		if codex != codexDocJob(doc[i]) {
			t.Errorf("stage %s codex: table %+v, factory.md %+v", st.stage, codex, codexDocJob(doc[i]))
		}
		if !strings.HasSuffix(doc[i].name, "-"+st.stage) {
			t.Errorf("stage %s: factory.md name %s", st.stage, doc[i].name)
		}
	}
}

func TestInitCronCreatesEnabledJobsByteEqualToFactoryDoc(t *testing.T) {
	doc := factoryDocJobs(t)
	for _, tc := range []struct {
		stages []string
		want   []int
	}{
		{[]string{"plan", "build"}, []int{0, 1}},
		{[]string{"verify", "plan", "build", "plan"}, []int{0, 1, 2}},
	} {
		f := newCronFixture(t, "claude", "codex")
		res, report := f.run(initCronRequest{stages: tc.stages})
		calls := f.daemon.CreateCronJobCalls()
		if len(calls) != len(tc.want) {
			t.Fatalf("want %d creates, got %d", len(tc.want), len(calls))
		}
		for i, call := range calls {
			want := doc[tc.want[i]]
			got := factoryDocJob{name: call.Name, schedule: call.Schedule, prompt: call.Prompt, gate: call.GateCommand}
			if got != want {
				t.Errorf("create %d: got %+v, want %+v", i, got, want)
			}
			if !call.IsEnabled || call.AgentName != "claude" || call.Timezone != "" || call.RepoId != "repo-1" {
				t.Errorf("create %d: enabled=%v agent=%q tz=%q repo=%q", i, call.IsEnabled, call.AgentName, call.Timezone, call.RepoId)
			}
		}
		if res.Agent != "claude" || len(res.Warnings) != 0 || !strings.Contains(report, "created, enabled") {
			t.Fatalf("unexpected result: %+v\n%s", res, report)
		}
		if strings.Contains(report, "boss skills install") {
			t.Fatalf("clean preflight printed a remedy:\n%s", report)
		}
	}
}

func TestInitCronCodexVariant(t *testing.T) {
	doc := factoryDocJobs(t)
	for _, tc := range []struct {
		name   string
		loaded []string
		agent  string
	}{{"explicit", []string{"claude", "codex"}, "codex"}, {"only codex loaded", []string{"codex"}, ""}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCronFixture(t, tc.loaded...)
			f.run(initCronRequest{stages: []string{"build"}, agent: tc.agent})
			calls := f.daemon.CreateCronJobCalls()
			if len(calls) != 1 {
				t.Fatalf("want 1 create, got %d", len(calls))
			}
			want := codexDocJob(doc[1])
			got := factoryDocJob{name: calls[0].Name, schedule: calls[0].Schedule, prompt: calls[0].Prompt, gate: calls[0].GateCommand}
			if got != want || calls[0].AgentName != "codex" || !calls[0].IsEnabled {
				t.Fatalf("got %+v agent=%q enabled=%v, want %+v", got, calls[0].AgentName, calls[0].IsEnabled, want)
			}
			if got.prompt != "$boss-build" || !strings.Contains(got.gate, "$HOME/.codex/skills/") {
				t.Fatalf("codex strings wrong: %+v", got)
			}
		})
	}
}

func TestInitCronUpdatesExistingJobOnlyWithConsent(t *testing.T) {
	doc := factoryDocJobs(t)
	oldGate := `node "$HOME/.claude/skills/boss-plan/toolbox/cron-gates/old.mjs"`
	for _, consent := range []bool{true, false} {
		f := newCronFixture(t, "claude")
		f.daemon.AddCronJob(&pb.CronJob{Id: "job-plan", RepoId: "repo-1", Name: "factory-plan", Prompt: "/boss-plan", Schedule: "0 * * * *", Timezone: "Europe/Paris", IsEnabled: true, GateCommand: oldGate})
		res, report := f.run(initCronRequest{stages: []string{"plan"}, updateConsent: consent})
		if n := len(f.daemon.CreateCronJobCalls()); n != 0 {
			t.Fatalf("consent=%v: duplicated the plan job (%d creates)", consent, n)
		}
		updates := f.daemon.UpdateCronJobCalls()
		if !consent {
			if len(updates) != 0 || res.Stages[0].Outcome != cronLeftAlone {
				t.Fatalf("updated without consent: %+v", res.Stages[0])
			}
			if !strings.Contains(report, "--update-crons") || !strings.Contains(report, oldGate) || !strings.Contains(report, doc[0].gate) {
				t.Fatalf("diff not printed:\n%s", report)
			}
			continue
		}
		if len(updates) != 1 {
			t.Fatalf("want 1 update, got %d", len(updates))
		}
		u := updates[0]
		if u.Id != "job-plan" || u.GetGateCommand() != doc[0].gate {
			t.Fatalf("update wrong: %+v", u)
		}
		if u.Schedule != nil || u.Timezone != nil || u.IsEnabled != nil || u.Name != nil || u.Prompt != nil || u.AgentName != nil {
			t.Fatalf("update touched fields other than the differing gate: %+v", u)
		}
		if res.Stages[0].Outcome != cronUpdated {
			t.Fatalf("outcome %s", res.Stages[0].Outcome)
		}
		if job := f.daemon.CronJobs()["job-plan"]; job.Schedule != "0 * * * *" || job.Timezone != "Europe/Paris" {
			t.Fatalf("schedule or timezone changed: %+v", job)
		}
	}
}

func TestInitCronUserDisabledJobStaysDisabled(t *testing.T) {
	doc := factoryDocJobs(t)
	f := newCronFixture(t, "claude")
	f.daemon.AddCronJob(&pb.CronJob{Id: "job-verify", RepoId: "repo-1", Name: "factory-verify", Prompt: "/boss-verify", Schedule: doc[2].schedule, IsEnabled: false, AgentName: "claude", GateCommand: `node "$HOME/.claude/skills/old.mjs"`})
	res, report := f.run(initCronRequest{stages: []string{"verify"}, updateConsent: true})
	updates := f.daemon.UpdateCronJobCalls()
	if len(updates) != 1 || updates[0].IsEnabled != nil {
		t.Fatalf("enabled state touched: %+v", updates)
	}
	if f.daemon.CronJobs()["job-verify"].IsEnabled || len(f.daemon.CreateCronJobCalls()) != 0 {
		t.Fatal("user-disabled job was enabled or duplicated")
	}
	if res.Stages[0].Enabled || !strings.Contains(report, "disabled") {
		t.Fatalf("report does not show the job disabled:\n%s", report)
	}
}

func TestInitCronMissingGateScriptCreatesDisabled(t *testing.T) {
	f := newCronFixture(t, "claude")
	gate := filepath.Join(f.home, ".claude", "skills", "boss-build", "toolbox", "cron-gates", "boss-build.mjs")
	if err := os.Remove(gate); err != nil {
		t.Fatal(err)
	}
	res, report := f.run(initCronRequest{stages: []string{"plan", "build"}})
	calls := f.daemon.CreateCronJobCalls()
	if len(calls) != 2 || !calls[0].IsEnabled || calls[1].IsEnabled {
		t.Fatalf("want plan enabled and build disabled: %+v", calls)
	}
	if res.Stages[1].Outcome != cronDisabledByPreflight || !strings.Contains(res.Stages[1].PreflightFailure, gate) {
		t.Fatalf("build outcome %+v", res.Stages[1])
	}
	if !strings.Contains(report, "boss skills install") || !strings.Contains(report, "created disabled") {
		t.Fatalf("remedy or failure not printed:\n%s", report)
	}
	if !strings.Contains(report, "A re-run never enables a job") {
		t.Fatalf("remedy implies a re-run enables the disabled job:\n%s", report)
	}
}

func TestInitCronNodeMissingEverywhereEnablesNothing(t *testing.T) {
	f := newCronFixture(t, "claude")
	if err := os.Remove(filepath.Join(f.serviceDir, "node")); err != nil {
		t.Fatal(err)
	}
	res, report := f.run(initCronRequest{stages: []string{"plan", "build", "verify"}})
	for _, call := range f.daemon.CreateCronJobCalls() {
		if call.IsEnabled {
			t.Fatalf("enabled %s without a resolvable node", call.Name)
		}
	}
	if len(f.validated) != 0 {
		t.Fatal("validation ran without a node")
	}
	for _, s := range res.Stages {
		if !strings.Contains(s.PreflightFailure, "node is not on the daemon's service PATH") {
			t.Fatalf("stage %s failure %q", s.Stage, s.PreflightFailure)
		}
	}
	if !strings.Contains(report, "boss skills install") {
		t.Fatalf("remedy not printed:\n%s", report)
	}
}

func TestInitCronNodeOnlyOnCallerPathUsesAbsolutePath(t *testing.T) {
	f := newCronFixture(t, "claude")
	if err := os.Remove(filepath.Join(f.serviceDir, "node")); err != nil {
		t.Fatal(err)
	}
	f.callerNode = "/opt/caller/bin/node"
	res, _ := f.run(initCronRequest{stages: []string{"plan"}})
	calls := f.daemon.CreateCronJobCalls()
	want := `/opt/caller/bin/node "$HOME/.claude/skills/boss-plan/toolbox/cron-gates/boss-plan.mjs"`
	if len(calls) != 1 || calls[0].GateCommand != want || !calls[0].IsEnabled {
		t.Fatalf("gate %q enabled=%v, want %q enabled", calls[0].GateCommand, calls[0].IsEnabled, want)
	}
	if len(f.validated) != 1 || f.validated[0] != "/opt/caller/bin/node" {
		t.Fatalf("validation ran with %v", f.validated)
	}
	if len(res.Warnings) == 0 || !strings.Contains(res.Warnings[0], "not on the daemon's service PATH") {
		t.Fatalf("warnings %v", res.Warnings)
	}
	if got := shellWord("/opt/my node/bin/node"); got != `'/opt/my node/bin/node'` {
		t.Fatalf("unsafe path not quoted: %s", got)
	}
}

func TestInitCronRejectedConfigCreatesDisabled(t *testing.T) {
	f := newCronFixture(t, "claude")
	f.rejectWith = errors.New("skill-config: trackerConfig.linear.states.planned must be a string\nstack")
	res, report := f.run(initCronRequest{stages: []string{"build"}})
	calls := f.daemon.CreateCronJobCalls()
	if len(calls) != 1 || calls[0].IsEnabled {
		t.Fatalf("rejected config enabled the job: %+v", calls)
	}
	if !strings.Contains(res.Stages[0].PreflightFailure, "rejects .boss-skills.json") || strings.Contains(res.Stages[0].PreflightFailure, "stack") {
		t.Fatalf("failure %q", res.Stages[0].PreflightFailure)
	}
	if !strings.Contains(report, "boss skills install") {
		t.Fatalf("remedy not printed:\n%s", report)
	}
}

// TestInitCronInstalledModuleValidation proves the generalised bridge runs the
// INSTALLED module path: a stub module that rejects everything fails the
// preflight, while the real module accepts the same config.
func TestInitCronInstalledModuleValidation(t *testing.T) {
	node := realNode(t)
	f := newCronFixture(t, "claude")
	f.installRealModules(node)
	stub := filepath.Join(f.home, ".claude", "skills", "boss-build", "toolbox", "skill-config.mjs")
	if err := os.WriteFile(stub, []byte(`export const DEFAULT_CONFIG = {}
export function mergeConfig(a, b) { return Object.assign({}, a, b) }
export function withTrackerDefaults(c) { return c }
export function validateConfig() { throw new Error('installed module refuses this config') }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	res, _ := f.run(initCronRequest{stages: []string{"plan", "build"}})
	calls := f.daemon.CreateCronJobCalls()
	if len(calls) != 2 || !calls[0].IsEnabled || calls[1].IsEnabled {
		t.Fatalf("want plan enabled (real module) and build disabled (stub): %+v", calls)
	}
	if !strings.Contains(res.Stages[1].PreflightFailure, "installed module refuses this config") {
		t.Fatalf("failure %q", res.Stages[1].PreflightFailure)
	}
}

func TestInitCronBaseBranchSkewWarnsAndStillEnables(t *testing.T) {
	node := realNode(t)
	for _, tc := range []struct {
		name, config, want string
		base               []byte
		planUnaffected     bool
	}{
		{
			name:   "me filter only in the working tree",
			config: `{"trackerConfig":{"linear":{"selection":{"assignees":{"include":["me"]}}}}}`,
			want:   "build workers scan all assignees",
		},
		{
			name:   "states.planned only in the working tree",
			config: `{"trackerConfig":{"linear":{"states":{"planned":"Ready"}}}}`,
			want:   "build workers query the default state `Todo`, not `Ready`",
			// plan scans the unplanned state, which this override leaves alone.
			planUnaffected: true,
		},
		{
			name:   "differs from a committed config",
			config: `{"trackerConfig":{"linear":{"states":{"planned":"Ready"}}}}`,
			base:   []byte(`{"trackerConfig":{"linear":{"states":{"planned":"Queued"}}}}`),
			want:   "build workers query the state `Queued`, not `Ready`",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCronFixture(t, "claude")
			f.installRealModules(node)
			if err := os.WriteFile(f.configPath, []byte(tc.config), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.base != nil {
				f.baseFiles[".boss-skills.json"] = tc.base
			}
			res, report := f.run(initCronRequest{stages: []string{"plan", "build"}})
			for _, call := range f.daemon.CreateCronJobCalls() {
				if !call.IsEnabled {
					t.Fatalf("skew held back %s:\n%s", call.Name, report)
				}
			}
			if !strings.Contains(strings.Join(res.Warnings, "\n"), tc.want) {
				t.Fatalf("want warning %q, got:\n%s", tc.want, report)
			}
			if !strings.Contains(strings.Join(res.LeftToDo, "\n"), "commit .boss-skills.json and merge it to main") {
				t.Fatalf("commit step missing:\n%s", report)
			}
			if tc.planUnaffected && strings.Contains(strings.Join(res.Warnings, "\n"), "plan workers") {
				t.Fatalf("plan reported skew it does not have:\n%s", report)
			}
		})
	}
}

func TestInitCronNoSkewWhenBaseMatches(t *testing.T) {
	f := newCronFixture(t, "claude")
	f.baseFiles[".boss-skills.json"] = []byte("{}\n")
	res, report := f.run(initCronRequest{stages: []string{"plan", "build", "verify"}})
	if len(res.Warnings) != 0 || len(res.LeftToDo) != 0 {
		t.Fatalf("unexpected warnings:\n%s", report)
	}
}

func TestInitCronDescribeSelectionSkew(t *testing.T) {
	working := cronEffectiveEntry{Value: &cronStageQuery{State: stringPtr("Code Review"), Selection: map[string]map[string][]string{"labels": {"exclude": {"infra"}}}, RequireLabels: []string{"agent-build"}}}
	base := cronEffectiveEntry{Value: &cronStageQuery{State: stringPtr("In Review"), Selection: map[string]map[string][]string{}, RequireLabels: []string{"agent-build"}}}
	lines := describeSelectionSkew("verify", working, base, "origin/main", true)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"verify workers look for tickets in the default state `In Review`, not `Code Review`", "verify workers exclude labels none, not `infra`"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("want %q in:\n%s", want, joined)
		}
	}
	if got := describeSelectionSkew("build", working, cronEffectiveEntry{Error: "boom"}, "origin/main", false); len(got) != 1 || !strings.Contains(got[0], "cannot run on origin/main's config") {
		t.Fatalf("base error: %v", got)
	}
}

func TestInitCronAgentEligibility(t *testing.T) {
	t.Run("codex on PATH but not loaded", func(t *testing.T) {
		f := newCronFixture(t, "claude")
		elig, err := initCronAgentEligibility(context.Background(), f.c, f.serviceDir)
		if err != nil {
			t.Fatal(err)
		}
		if !elig[0].Eligible() || elig[1].Eligible() || elig[1].CLIPath == "" {
			t.Fatalf("eligibility %+v", elig)
		}
		_, err = setupInitCron(context.Background(), f.c, f.repo, f.repoDir, f.configPath, initCronRequest{stages: []string{"plan"}, agent: "codex"}, f.env)
		if err == nil || !strings.Contains(err.Error(), "has not loaded its runner") {
			t.Fatalf("--agent codex: %v", err)
		}
		if len(f.daemon.CreateCronJobCalls()) != 0 {
			t.Fatal("created a job for an ineligible agent")
		}
	})
	t.Run("claude loaded but CLI missing", func(t *testing.T) {
		f := newCronFixture(t, "claude", "codex")
		if err := os.Remove(filepath.Join(f.serviceDir, "claude")); err != nil {
			t.Fatal(err)
		}
		_, err := setupInitCron(context.Background(), f.c, f.repo, f.repoDir, f.configPath, initCronRequest{stages: []string{"plan"}, agent: "claude"}, f.env)
		if err == nil || !strings.Contains(err.Error(), "not on the daemon's service PATH") {
			t.Fatalf("--agent claude: %v", err)
		}
		res, _ := f.run(initCronRequest{stages: []string{"plan"}})
		if res.Agent != "codex" {
			t.Fatalf("default agent %s, want codex (the only eligible one)", res.Agent)
		}
	})
	t.Run("none eligible", func(t *testing.T) {
		f := newCronFixture(t)
		_, err := setupInitCron(context.Background(), f.c, f.repo, f.repoDir, f.configPath, initCronRequest{stages: []string{"plan"}}, f.env)
		if err == nil || !strings.Contains(err.Error(), "no agent can run factory cron jobs") {
			t.Fatalf("err %v", err)
		}
	})
}

func TestInitCronLeadingSkillTokenJobIsThePlanJob(t *testing.T) {
	f := newCronFixture(t, "claude")
	f.daemon.AddCronJob(&pb.CronJob{Id: "mine", RepoId: "repo-1", Name: "my-planner", Prompt: "/boss-plan", Schedule: "0 9 * * *", IsEnabled: true, AgentName: "claude", GateCommand: `node "$HOME/.claude/skills/boss-plan/toolbox/cron-gates/boss-plan.mjs"`})
	f.daemon.AddCronJob(&pb.CronJob{Id: "other-repo", RepoId: "repo-2", Name: "factory-build", Prompt: "/boss-build"})
	res, report := f.run(initCronRequest{stages: []string{"plan", "build"}})
	calls := f.daemon.CreateCronJobCalls()
	if len(calls) != 1 || calls[0].Name != "factory-build" {
		t.Fatalf("want only factory-build created, got %+v", calls)
	}
	if res.Stages[0].Name != "my-planner" || res.Stages[0].Outcome != cronUnchanged {
		t.Fatalf("plan stage %+v\n%s", res.Stages[0], report)
	}
	if len(f.daemon.UpdateCronJobCalls()) != 0 {
		t.Fatal("updated a matching job")
	}
}

func TestInitCronGateEqualAfterHomeExpansionHasNoDiff(t *testing.T) {
	f := newCronFixture(t, "claude")
	abs := `node "` + filepath.Join(f.home, ".claude", "skills") + `/boss-plan/toolbox/cron-gates/boss-plan.mjs"`
	f.daemon.AddCronJob(&pb.CronJob{Id: "p", RepoId: "repo-1", Name: "factory-plan", Prompt: "/boss-plan", IsEnabled: true, GateCommand: abs})
	f.daemon.AddCronJob(&pb.CronJob{Id: "b", RepoId: "repo-1", Name: "factory-build", Prompt: "/boss-build", IsEnabled: true, GateCommand: `node "${HOME}/.claude/skills/boss-build/toolbox/cron-gates/boss-build.mjs"`})
	res, report := f.run(initCronRequest{stages: []string{"plan", "build"}, updateConsent: true})
	for _, s := range res.Stages {
		if s.Outcome != cronUnchanged || len(s.Diff) != 0 {
			t.Fatalf("stage %s reported a diff: %+v\n%s", s.Stage, s, report)
		}
	}
	if len(f.daemon.UpdateCronJobCalls()) != 0 || len(f.daemon.CreateCronJobCalls()) != 0 {
		t.Fatal("equal jobs were rewritten")
	}
}

func TestInitCronMissingMCPDeclarationIsLeftToDo(t *testing.T) {
	for _, tc := range []struct {
		agent, file string
		declared    []byte
	}{
		{"claude", ".mcp.json", []byte(`{"mcpServers":{"Linear":{}}}`)},
		{"codex", ".codex/config.toml", []byte("[mcp_servers.linear]\nurl = \"x\"\n")},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			f := newCronFixture(t, tc.agent)
			delete(f.baseFiles, tc.file)
			res, report := f.run(initCronRequest{stages: []string{"plan"}})
			if !strings.Contains(strings.Join(res.LeftToDo, "\n"), `declare the tracker MCP server "linear" in `+tc.file) || !strings.Contains(report, "Left to do:") {
				t.Fatalf("missing declaration not reported:\n%s", report)
			}
			f2 := newCronFixture(t, tc.agent)
			f2.baseFiles[tc.file] = tc.declared
			if res2, report2 := f2.run(initCronRequest{stages: []string{"plan"}}); len(res2.LeftToDo) != 0 {
				t.Fatalf("declared server reported missing:\n%s", report2)
			}
		})
	}
}

func TestInitCronBuildWithoutPlanNote(t *testing.T) {
	f := newCronFixture(t, "claude")
	res, _ := f.run(initCronRequest{stages: []string{"build"}})
	if !strings.Contains(strings.Join(res.LeftToDo, "\n"), "nothing reaches the planned state") {
		t.Fatalf("left to do %v", res.LeftToDo)
	}
	f2 := newCronFixture(t, "claude")
	f2.daemon.AddCronJob(&pb.CronJob{Id: "p", RepoId: "repo-1", Name: "factory-plan", Prompt: "/boss-plan", IsEnabled: true})
	if res2, _ := f2.run(initCronRequest{stages: []string{"build"}}); len(res2.LeftToDo) != 0 {
		t.Fatalf("note printed despite an enabled plan job: %v", res2.LeftToDo)
	}
}

func TestInitCronFlagValidation(t *testing.T) {
	for _, tc := range []struct {
		req  initCronRequest
		want string
	}{
		{initCronRequest{agent: "claude"}, "--agent requires --cron"},
		{initCronRequest{updateConsent: true}, "--update-crons requires --cron"},
		{initCronRequest{stages: []string{"plan", "sweep"}}, `unknown stage "sweep"`},
		{initCronRequest{stages: []string{"plan"}, agent: "hermes"}, "unsupported agent"},
	} {
		req := tc.req
		if err := req.normalize(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%+v: err %v, want %q", tc.req, err, tc.want)
		}
	}
	req := initCronRequest{stages: []string{" Verify", "plan", "plan"}, agent: "Codex"}
	if err := req.normalize(); err != nil || strings.Join(req.stages, ",") != "plan,verify" || req.agent != "codex" {
		t.Fatalf("normalize: %v %+v", err, req)
	}
}

func TestInitCronSkippedWithoutALocalRepoID(t *testing.T) {
	for _, flag := range []string{"remote", "host"} {
		t.Run(flag, func(t *testing.T) {
			var out bytes.Buffer
			c := &cobra.Command{}
			c.Flags().String(flag, "remote-destination", "")
			err := runInitCronStep(&out, initOptions{command: c, cron: initCronRequest{stages: []string{"plan"}}}, t.TempDir(), "", nil)
			if err != nil || !strings.Contains(out.String(), "Cron setup skipped") || !strings.Contains(out.String(), "--remote / --host") {
				t.Fatalf("err=%v out=%s", err, out.String())
			}
		})
	}
	t.Run("unregistered", func(t *testing.T) {
		daemon := tuitest.NewMockDaemon(t)
		t.Setenv("BOSS_SOCKET", daemon.SocketPath())
		var out bytes.Buffer
		c := &cobra.Command{}
		c.SetContext(context.Background())
		err := runInitCronStep(&out, initOptions{command: c, cron: initCronRequest{stages: []string{"plan"}}}, initGitRepo(t), "", nil)
		if err != nil || !strings.Contains(out.String(), "Cron setup skipped: no local repository id") || len(daemon.CreateCronJobCalls()) != 0 {
			t.Fatalf("err=%v out=%s", err, out.String())
		}
	})
	t.Run("not requested", func(t *testing.T) {
		var out bytes.Buffer
		if err := runInitCronStep(&out, initOptions{}, t.TempDir(), "", nil); err != nil || out.Len() != 0 {
			t.Fatalf("cron work without --cron: %v %q", err, out.String())
		}
	})
}

// TestInitCronThroughInitCommand runs `boss init --cron ...` end to end against
// a MockDaemon: the config is written first, then the jobs are created.
func TestInitCronThroughInitCommand(t *testing.T) {
	realNode(t)
	dir := initGitRepo(t)
	main, err := mainCheckout(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	f := newCronFixture(t, "claude")
	f.daemon.AddRepo(&pb.Repo{Id: "repo-git", LocalPath: main})
	t.Setenv("BOSS_SOCKET", f.daemon.SocketPath())
	t.Setenv("LINEAR_API_KEY", "")
	orig := newInitCronEnv
	newInitCronEnv = func() initCronEnv { return f.env }
	t.Cleanup(func() { newInitCronEnv = orig })

	cmd := initCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--dir", dir, "--cron", "plan,verify"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("boss init: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".boss-skills.json")); err != nil {
		t.Fatal("config not written before cron setup")
	}
	calls := f.daemon.CreateCronJobCalls()
	if len(calls) != 2 || calls[0].RepoId != "repo-git" || calls[0].Name != "factory-plan" || calls[1].Name != "factory-verify" {
		t.Fatalf("creates %+v\n%s", calls, out.String())
	}
	if !strings.Contains(out.String(), "Factory cron jobs (agent: claude)") {
		t.Fatalf("report missing:\n%s", out.String())
	}
}
