package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/recurser/boss/internal/client"
	"github.com/recurser/boss/internal/daemon"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	libskillinstall "github.com/recurser/bossalib/skillinstall"
)

// cronStage is one row of the factory stage table. It mirrors the three
// `boss cron add` lines in docs/skills/factory.md exactly; a test pins the two
// together so neither can drift.
type cronStage struct {
	stage    string // plan, build, verify
	name     string // job name
	schedule string // 5-field cron expression, daemon-local timezone
	skill    string // the stage skill, and its prompt's leading token
}

// factoryCronStages is the stage table, in pipeline order.
var factoryCronStages = []cronStage{
	{stage: "plan", name: "factory-plan", schedule: "*/15 * * * *", skill: "boss-plan"},
	{stage: "build", name: "factory-build", schedule: "*/15 * * * *", skill: "boss-build"},
	{stage: "verify", name: "factory-verify", schedule: "*/10 * * * *", skill: "boss-verify"},
}

// prompt is the job prompt for agent: Claude invokes a skill as `/name`, Codex
// as `$name`.
func (s cronStage) prompt(agent string) string {
	if agent == cronAgentCodex {
		return "$" + s.skill
	}
	return "/" + s.skill
}

// gatePath is the gate script relative to an agent's skill root.
func (s cronStage) gatePath() string {
	return s.skill + "/toolbox/cron-gates/" + s.skill + ".mjs"
}

// modulePath is the installed skill-config module the stage's gate loads.
func (s cronStage) modulePath() string {
	return s.skill + "/toolbox/skill-config.mjs"
}

// matchesPrompt reports whether prompt's leading token invokes this stage's
// skill, in either agent's spelling.
func (s cronStage) matchesPrompt(prompt string) bool {
	fields := strings.Fields(prompt)
	return len(fields) > 0 && (fields[0] == "/"+s.skill || fields[0] == "$"+s.skill)
}

const (
	cronAgentClaude = "claude"
	cronAgentCodex  = "codex"
)

// cronAgents are the agents a factory job may run under, in default-preference
// order. cli is the executable the daemon's runner plugin launches.
var cronAgents = []struct {
	name, cli string
	skill     libskillinstall.Agent
}{
	{name: cronAgentClaude, cli: "claude", skill: libskillinstall.AgentClaude},
	{name: cronAgentCodex, cli: "codex", skill: libskillinstall.AgentCodex},
}

// cronSkillsInstallRemedy is printed with every preflight failure.
const cronSkillsInstallRemedy = "boss skills install"

// initCronRequest is the cron part of a boss init run: which stages, which
// agent, and whether an existing job that differs may be updated.
type initCronRequest struct {
	stages        []string
	agent         string
	updateConsent bool
}

func (r initCronRequest) requested() bool { return len(r.stages) > 0 }

// normalize validates the flags and puts the stages in pipeline order with
// duplicates dropped. It runs before any read or write, so a usage error leaves
// nothing behind.
func (r *initCronRequest) normalize() error {
	if !r.requested() {
		if r.agent != "" {
			return errors.New("--agent requires --cron")
		}
		if r.updateConsent {
			return errors.New("--update-crons requires --cron")
		}
		return nil
	}
	want := map[string]bool{}
	for _, raw := range r.stages {
		stage := strings.ToLower(strings.TrimSpace(raw))
		if _, ok := cronStageByName(stage); !ok {
			return fmt.Errorf("--cron: unknown stage %q (want plan, build or verify)", raw)
		}
		want[stage] = true
	}
	r.stages = r.stages[:0]
	for _, st := range factoryCronStages {
		if want[st.stage] {
			r.stages = append(r.stages, st.stage)
		}
	}
	r.agent = strings.ToLower(strings.TrimSpace(r.agent))
	if r.agent != "" && r.agent != cronAgentClaude && r.agent != cronAgentCodex {
		return fmt.Errorf("--agent: unsupported agent %q (want claude or codex)", r.agent)
	}
	return nil
}

func cronStageByName(stage string) (cronStage, bool) {
	for _, st := range factoryCronStages {
		if st.stage == stage {
			return st, true
		}
	}
	return cronStage{}, false
}

// initCronEnv is every host dependency of cron setup. Tests replace it whole
// through newInitCronEnv, so no test depends on the host's PATH, home
// directory, installed skills, git remote or node.
type initCronEnv struct {
	// servicePath is the PATH the daemon's service runs with.
	servicePath func() string
	// callerLookPath resolves an executable on the caller's own PATH.
	callerLookPath func(string) (string, error)
	homeDir        func() (string, error)
	// skillRoot is the installed global skill directory for an agent.
	skillRoot func(agent string) (string, error)
	// defaultBranch names the repository's base branch.
	defaultBranch func(ctx context.Context, repoDir string, repo *pb.Repo) string
	// readRef reads path (relative to repoDir) at ref. found is false when the
	// ref exists and the file does not; err means the ref could not be read.
	readRef func(ctx context.Context, repoDir, ref, path string) (data []byte, found bool, err error)
	// validate runs the installed module's merge-then-validate over config.
	validate func(nodePath, module string, config []byte, source string) error
	// effective evaluates the per-stage selection config resolves to under the
	// installed module.
	effective func(nodePath, module string, config []byte) (cronEffective, error)
}

// newInitCronEnv is the host environment; a package var so tests inject one.
var newInitCronEnv = defaultInitCronEnv

func defaultInitCronEnv() initCronEnv {
	return initCronEnv{
		servicePath:    daemonServicePathForResolution,
		callerLookPath: exec.LookPath,
		homeDir:        os.UserHomeDir,
		skillRoot:      defaultCronSkillRoot,
		defaultBranch:  defaultCronBaseBranch,
		readRef:        gitReadRef,
		validate:       validateWithInstalledModule,
		effective:      effectiveWithInstalledModule,
	}
}

// daemonServicePathForResolution is the PATH `boss daemon doctor` resolves
// tools against: the installed service file's when readable (what the running
// daemon has), else the one the next restart writes.
func daemonServicePathForResolution() string {
	if installed, ok := daemon.InstalledServiceEnvPath(); ok {
		return installed
	}
	return daemon.ServiceEnvPath()
}

func defaultCronSkillRoot(agent string) (string, error) {
	for _, a := range cronAgents {
		if a.name == agent {
			return libskillinstall.DirForAgent(a.skill)
		}
	}
	return "", fmt.Errorf("unsupported agent %q", agent)
}

func defaultCronBaseBranch(ctx context.Context, repoDir string, repo *pb.Repo) string {
	if branch := strings.TrimSpace(repo.GetDefaultBaseBranch()); branch != "" {
		return branch
	}
	out, err := exec.CommandContext(ctx, "git", "-C", repoDir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD").Output()
	if err == nil {
		if branch := strings.TrimPrefix(strings.TrimSpace(string(out)), "origin/"); branch != "" {
			return branch
		}
	}
	return "main"
}

// gitReadRef reads ./path at ref from repoDir. The `./` form resolves path
// against repoDir rather than the repository root, so a config written into a
// subdirectory is compared with the same subdirectory on the base branch.
func gitReadRef(ctx context.Context, repoDir, ref, path string) ([]byte, bool, error) {
	if err := exec.CommandContext(ctx, "git", "-C", repoDir, "rev-parse", "--verify", "--quiet", ref+"^{commit}").Run(); err != nil {
		return nil, false, fmt.Errorf("%s is not available locally (fetch it to compare)", ref)
	}
	spec := ref + ":./" + filepath.ToSlash(path)
	if err := exec.CommandContext(ctx, "git", "-C", repoDir, "cat-file", "-e", spec).Run(); err != nil {
		return nil, false, nil
	}
	data, err := exec.CommandContext(ctx, "git", "-C", repoDir, "show", spec).Output()
	if err != nil {
		return nil, false, fmt.Errorf("read %s", spec)
	}
	return data, true, nil
}

// validateWithInstalledModule runs nodeBridge.validate against the installed
// module path instead of the embedded copy.
func validateWithInstalledModule(nodePath, module string, config []byte, source string) error {
	bridge, cleanup, err := newModuleNodeBridge(module, nodePath)
	if err != nil {
		return err
	}
	defer cleanup()
	return bridge.validate(config, source)
}

// cronEffective is what one config resolves to for each stage's selection, plus
// the tracker MCP server name. Each entry carries either a value or the error
// the module threw for it.
type cronEffective struct {
	Plan      cronEffectiveEntry  `json:"plan"`
	Build     cronEffectiveEntry  `json:"build"`
	Verify    cronEffectiveEntry  `json:"verify"`
	MCPServer cronEffectiveString `json:"mcpServer"`
}

type cronEffectiveEntry struct {
	Value *cronStageQuery `json:"value"`
	Error string          `json:"error"`
}

type cronEffectiveString struct {
	Value *string `json:"value"`
	Error string  `json:"error"`
}

// cronStageQuery is stageSelectionQuery's result shape; verify carries only
// State (its review state).
type cronStageQuery struct {
	State         *string                        `json:"state"`
	Selection     map[string]map[string][]string `json:"selection"`
	RequireLabels []string                       `json:"requireLabels"`
	ExcludeLabels []string                       `json:"excludeLabels"`
}

func (e cronEffective) entry(stage string) cronEffectiveEntry {
	switch stage {
	case "plan":
		return e.Plan
	case "build":
		return e.Build
	default:
		return e.Verify
	}
}

// effectiveWithInstalledModule computes the effective per-stage selection the
// way the gates do: stageSelectionQuery for plan and build, the inReview state
// for verify, over the config merged onto the module's defaults.
func effectiveWithInstalledModule(nodePath, module string, config []byte) (cronEffective, error) {
	bridge, cleanup, err := newModuleNodeBridge(module, nodePath)
	if err != nil {
		return cronEffective{}, err
	}
	defer cleanup()
	candidate := filepath.Join(bridge.dir, "candidate.json")
	if err := os.WriteFile(candidate, config, 0o600); err != nil {
		return cronEffective{}, fmt.Errorf("write selection candidate: %w", err)
	}
	script := fmt.Sprintf(`import { readFileSync } from 'node:fs'
import { pathToFileURL } from 'node:url'
const mod = await import(pathToFileURL(%s).href)
const config = mod.withTrackerDefaults(mod.mergeConfig(mod.DEFAULT_CONFIG, JSON.parse(readFileSync(%s, 'utf8'))))
const safe = (fn) => { try { return { value: fn() } } catch (e) { return { error: String(e?.message ?? e) } } }
process.stdout.write(JSON.stringify({
  plan: safe(() => mod.stageSelectionQuery(config, 'plan')),
  build: safe(() => mod.stageSelectionQuery(config, 'build')),
  verify: safe(() => ({ state: mod.stateName(config, 'inReview') })),
  mcpServer: safe(() => mod.trackerConfigFor(config)?.mcpServer ?? null),
}))
`, jsString(bridge.module), jsString(candidate))
	stdout, err := bridge.run(script, "evaluate stage selection")
	if err != nil {
		return cronEffective{}, err
	}
	var eff cronEffective
	if err := json.Unmarshal(stdout, &eff); err != nil {
		return cronEffective{}, fmt.Errorf("%w: evaluate stage selection: %s", errNodeBadJSON, truncate(stdout))
	}
	return eff, nil
}

// --- Agent eligibility ----------------------------------------------------

// cronAgentEligibility says whether one agent can run factory jobs: the daemon
// must have loaded its runner (CreateCronJob rejects one it has not), and its
// CLI must resolve on the daemon's service PATH.
type cronAgentEligibility struct {
	Name    string
	Loaded  bool
	CLIPath string // empty when the CLI is not on the service PATH
}

func (e cronAgentEligibility) Eligible() bool { return e.Loaded && e.CLIPath != "" }

// reason names why an ineligible agent does not qualify.
func (e cronAgentEligibility) reason() string {
	var why []string
	if !e.Loaded {
		why = append(why, "the daemon has not loaded its runner plugin")
	}
	if e.CLIPath == "" {
		why = append(why, "the `"+e.Name+"` CLI is not on the daemon's service PATH")
	}
	return strings.Join(why, ", and ")
}

// initCronAgentEligibility reports every supported agent's eligibility, in
// default-preference order. The interactive installer offers only eligible ones.
func initCronAgentEligibility(ctx context.Context, c client.BossClient, servicePath string) ([]cronAgentEligibility, error) {
	loaded, err := c.ListAgents(ctx)
	if err != nil {
		return nil, errors.New("could not list the daemon's agents")
	}
	out := make([]cronAgentEligibility, 0, len(cronAgents))
	for _, a := range cronAgents {
		e := cronAgentEligibility{Name: a.name}
		e.Loaded = slices.ContainsFunc(loaded, func(info client.AgentInfo) bool { return info.Name == a.name })
		if path, ok := daemon.LookPathIn(servicePath, a.cli); ok {
			e.CLIPath = path
		}
		out = append(out, e)
	}
	return out, nil
}

// chooseInitCronAgent picks the requested agent when it qualifies, else the
// first eligible one (Claude before Codex).
func chooseInitCronAgent(eligibility []cronAgentEligibility, requested string) (string, error) {
	if requested != "" {
		for _, e := range eligibility {
			if e.Name == requested {
				if !e.Eligible() {
					return "", fmt.Errorf("--agent %s cannot run factory cron jobs: %s", requested, e.reason())
				}
				return requested, nil
			}
		}
		return "", fmt.Errorf("--agent: unsupported agent %q", requested)
	}
	var why []string
	for _, e := range eligibility {
		if e.Eligible() {
			return e.Name, nil
		}
		why = append(why, e.Name+": "+e.reason())
	}
	return "", fmt.Errorf("no agent can run factory cron jobs (%s)", strings.Join(why, "; "))
}

// --- Setup ----------------------------------------------------------------

// initCronOutcome is what cron setup did with one stage's job.
type initCronOutcome string

const (
	cronCreated             initCronOutcome = "created"
	cronDisabledByPreflight initCronOutcome = "disabled-by-preflight"
	cronUpdated             initCronOutcome = "updated"
	cronLeftAlone           initCronOutcome = "left-alone"
	cronUnchanged           initCronOutcome = "unchanged"
)

// initCronStageResult is one stage's outcome. Diff lists the fields that differ
// from the factory definition (applied when Outcome is updated, printed when it
// is left-alone).
type initCronStageResult struct {
	Stage, Name, JobID string
	Outcome            initCronOutcome
	Enabled            bool
	Diff               []string
	PreflightFailure   string
}

// initCronResult is everything cron setup did and found, for the report and
// for the interactive installer's summary.
type initCronResult struct {
	Agent    string
	Stages   []initCronStageResult
	Warnings []string
	LeftToDo []string
}

// cronNode is how a gate command invokes node.
type cronNode struct {
	path    string // resolved executable; empty when node is unresolvable
	command string // what the gate command says: `node`, or the absolute path
	note    string // set when the daemon's PATH lacks node
}

// resolveCronNode resolves node the way the daemon will: on its service PATH.
// Only when that fails is the caller's PATH consulted, and then the absolute
// path goes into the gate, because a bare `node` would not resolve for the
// daemon (BOS-880).
func resolveCronNode(servicePath string, callerLookPath func(string) (string, error)) cronNode {
	if path, ok := daemon.LookPathIn(servicePath, "node"); ok {
		return cronNode{path: path, command: "node"}
	}
	if path, err := callerLookPath("node"); err == nil {
		if abs, absErr := filepath.Abs(path); absErr == nil {
			path = abs
		}
		return cronNode{path: path, command: shellWord(path), note: "node is not on the daemon's service PATH; the gates call " + path + " directly (add its directory to daemon_path_extra and run `boss daemon restart` to use a bare `node`)"}
	}
	return cronNode{command: "node"}
}

var shellSafeWord = regexp.MustCompile(`^[A-Za-z0-9_./+=:@%-]+$`)

// shellWord renders s as one POSIX shell word.
func shellWord(s string) string {
	if shellSafeWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// renderSkillRoot writes root as factory.md does, `$HOME/.claude/skills`, when
// it is under the home directory, and as an absolute path otherwise.
func renderSkillRoot(root, home string) string {
	if home != "" {
		if rel, err := filepath.Rel(home, root); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			return "$HOME/" + filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(root)
}

// expandHome expands $HOME and ${HOME} so gates written either way compare
// equal to an absolute one.
func expandHome(s, home string) string {
	if home == "" {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(strings.NewReplacer("${HOME}", home, "$HOME", home).Replace(s))
}

// effectiveAgent treats a job's empty agent as claude, as the daemon does.
func effectiveAgent(name string) string {
	if name == "" {
		return cronAgentClaude
	}
	return name
}

// setupInitCron creates or converges the factory jobs for req.stages and
// returns what it did. It never duplicates a stage's job, never enables a job
// whose preflight failed, and never changes an existing job's schedule,
// timezone or enabled state.
func setupInitCron(ctx context.Context, c client.BossClient, repo *pb.Repo, repoDir, configPath string, req initCronRequest, env initCronEnv) (initCronResult, error) {
	servicePath := env.servicePath()
	eligibility, err := initCronAgentEligibility(ctx, c, servicePath)
	if err != nil {
		return initCronResult{}, err
	}
	agent, err := chooseInitCronAgent(eligibility, req.agent)
	if err != nil {
		return initCronResult{}, err
	}
	res := initCronResult{Agent: agent}

	home, _ := env.homeDir()
	root, rootErr := env.skillRoot(agent)
	node := resolveCronNode(servicePath, env.callerLookPath)
	if node.note != "" {
		res.Warnings = append(res.Warnings, node.note)
	}
	config, configErr := os.ReadFile(configPath)

	existing, err := c.ListCronJobs(ctx, repo.GetId())
	if err != nil {
		return res, errors.New("could not list the repository's cron jobs")
	}

	for _, stageName := range req.stages {
		st, _ := cronStageByName(stageName)
		failures := cronPreflight(st, root, rootErr, home, node, config, configErr, configPath, env)
		gate := node.command + ` "` + renderSkillRoot(root, home) + "/" + st.gatePath() + `"`
		want := &pb.CreateCronJobRequest{
			RepoId:      repo.GetId(),
			Name:        st.name,
			Prompt:      st.prompt(agent),
			Schedule:    st.schedule,
			IsEnabled:   len(failures) == 0,
			AgentName:   agent,
			GateCommand: gate,
		}
		result, err := convergeCronStage(ctx, c, st, want, existing, failures, req.updateConsent, home, &res)
		if err != nil {
			return res, err
		}
		res.Stages = append(res.Stages, result)
	}

	if configErr == nil && rootErr == nil && node.path != "" {
		appendBaseBranchWarnings(ctx, &res, repo, repoDir, configPath, config, root, node, req.stages, env)
	}
	if slices.Contains(req.stages, "build") && !slices.Contains(req.stages, "plan") && !hasEnabledStageJob(existing, factoryCronStages[0]) {
		res.LeftToDo = append(res.LeftToDo, "factory-build is enabled without factory-plan: nothing reaches the planned state unless people plan tickets (or re-run with --cron plan)")
	}
	return res, nil
}

// cronPreflight checks one stage's gate in the daemon's environment and
// returns every failure found. A missing gate script or a config the installed
// module rejects makes the gate exit 1, which reads as an ordinary `gated` skip
// forever after; that is why it is checked before a job is enabled.
func cronPreflight(st cronStage, root string, rootErr error, home string, node cronNode, config []byte, configErr error, configPath string, env initCronEnv) []string {
	var failures []string
	if node.path == "" {
		failures = append(failures, "node is not on the daemon's service PATH or on your PATH")
	}
	if rootErr != nil {
		return append(failures, "the agent's skill directory could not be resolved")
	}
	if strings.ContainsAny(renderSkillRoot(root, home), "\"`\\") || strings.Contains(strings.TrimPrefix(renderSkillRoot(root, home), "$HOME/"), "$") {
		return append(failures, "the skill directory "+root+" cannot be quoted in a gate command")
	}
	gate := filepath.Join(root, filepath.FromSlash(st.gatePath()))
	if info, err := os.Stat(gate); err != nil || info.IsDir() {
		failures = append(failures, "gate script "+gate+" is not installed")
	}
	module := filepath.Join(root, filepath.FromSlash(st.modulePath()))
	switch {
	case configErr != nil:
		failures = append(failures, "the config "+configPath+" could not be read")
	case !fileExists(module):
		failures = append(failures, "installed "+module+" is missing")
	case node.path != "":
		if err := env.validate(node.path, module, config, configPath); err != nil {
			failures = append(failures, "installed "+module+" rejects "+filepath.Base(configPath)+": "+errorSummary(err))
		}
	}
	return failures
}

var thrownErrorLine = regexp.MustCompile(`^[A-Za-z]*Error: `)

// errorSummary is one line for a node failure: the thrown error's own message
// when stderr carries one, since node prints the throwing source location
// first; otherwise the error's first line.
func errorSummary(err error) string {
	for line := range strings.SplitSeq(err.Error(), "\n") {
		if line = strings.TrimSpace(line); thrownErrorLine.MatchString(line) {
			return firstLine(line)
		}
	}
	return firstLine(err.Error())
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const limit = 300
	if len(s) > limit {
		s = s[:limit] + "…"
	}
	return s
}

// findStageJob returns the stage's existing job: the one named after it, or
// failing that the first whose prompt's leading token is the stage skill
// (enabled ones first). others lists the remaining matches.
func findStageJob(jobs []*pb.CronJob, st cronStage) (job *pb.CronJob, others []*pb.CronJob) {
	var byToken []*pb.CronJob
	for _, j := range jobs {
		if j.GetName() == st.name {
			job = j
			continue
		}
		if st.matchesPrompt(j.GetPrompt()) {
			byToken = append(byToken, j)
		}
	}
	if job == nil && len(byToken) > 0 {
		slices.SortStableFunc(byToken, func(a, b *pb.CronJob) int {
			if a.GetIsEnabled() == b.GetIsEnabled() {
				return 0
			}
			if a.GetIsEnabled() {
				return -1
			}
			return 1
		})
		job, byToken = byToken[0], byToken[1:]
	}
	return job, byToken
}

func hasEnabledStageJob(jobs []*pb.CronJob, st cronStage) bool {
	return slices.ContainsFunc(jobs, func(j *pb.CronJob) bool {
		return j.GetIsEnabled() && (j.GetName() == st.name || st.matchesPrompt(j.GetPrompt()))
	})
}

// convergeCronStage creates the stage's job when none exists, and otherwise
// reconciles prompt, gate and agent only — with consent, and only when the
// preflight passed, so an update can never point a running job at a gate that
// is known not to work.
func convergeCronStage(ctx context.Context, c client.BossClient, st cronStage, want *pb.CreateCronJobRequest, jobs []*pb.CronJob, failures []string, consent bool, home string, res *initCronResult) (initCronStageResult, error) {
	out := initCronStageResult{Stage: st.stage, Name: st.name}
	if len(failures) > 0 {
		out.PreflightFailure = strings.Join(failures, "; ")
	}
	job, others := findStageJob(jobs, st)
	if len(others) > 0 {
		names := make([]string, 0, len(others))
		for _, o := range others {
			names = append(names, o.GetName())
		}
		res.Warnings = append(res.Warnings, fmt.Sprintf("other jobs also run %s (%s); several enabled jobs for one stage break plan → build chaining", st.skill, strings.Join(names, ", ")))
	}

	if job == nil {
		created, err := c.CreateCronJob(ctx, want)
		if err != nil {
			return out, fmt.Errorf("create cron job %s failed", st.name)
		}
		out.JobID, out.Enabled = created.GetId(), created.GetIsEnabled()
		out.Outcome = cronCreated
		if len(failures) > 0 {
			out.Outcome = cronDisabledByPreflight
		}
		return out, nil
	}

	out.Name, out.JobID, out.Enabled = job.GetName(), job.GetId(), job.GetIsEnabled()
	update := &pb.UpdateCronJobRequest{Id: job.GetId()}
	if job.GetPrompt() != want.Prompt {
		out.Diff = append(out.Diff, "prompt: "+job.GetPrompt()+" -> "+want.Prompt)
		update.Prompt = &want.Prompt
	}
	if expandHome(job.GetGateCommand(), home) != expandHome(want.GateCommand, home) {
		out.Diff = append(out.Diff, "gate: "+job.GetGateCommand()+" -> "+want.GateCommand)
		update.GateCommand = &want.GateCommand
	}
	if effectiveAgent(job.GetAgentName()) != want.AgentName {
		out.Diff = append(out.Diff, "agent: "+effectiveAgent(job.GetAgentName())+" -> "+want.AgentName)
		update.AgentName = &want.AgentName
	}
	switch {
	case len(out.Diff) == 0:
		out.Outcome = cronUnchanged
	case !consent || len(failures) > 0:
		out.Outcome = cronLeftAlone
	default:
		if _, err := c.UpdateCronJob(ctx, update); err != nil {
			return out, fmt.Errorf("update cron job %s failed", job.GetName())
		}
		out.Outcome = cronUpdated
	}
	return out, nil
}

// --- Base-branch warnings -------------------------------------------------

// appendBaseBranchWarnings warns about everything the base branch does not yet
// carry. Cron sessions run in fresh worktrees off origin/<base>, so a config or
// harness declaration that exists only in the working tree is invisible to
// them. None of this ever holds a job back; it says what happens until merge.
func appendBaseBranchWarnings(ctx context.Context, res *initCronResult, repo *pb.Repo, repoDir, configPath string, config []byte, root string, node cronNode, stages []string, env initCronEnv) {
	branch := env.defaultBranch(ctx, repoDir, repo)
	ref := "origin/" + branch
	filename := filepath.Base(configPath)
	base, found, err := env.readRef(ctx, repoDir, ref, filename)
	if err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("could not compare %s with %s: %s", filename, ref, firstLine(err.Error())))
		return
	}
	if !found {
		base = []byte("{}")
	}

	skewed := false
	var mcpServer string
	for i, stageName := range stages {
		st, _ := cronStageByName(stageName)
		module := filepath.Join(root, filepath.FromSlash(st.modulePath()))
		if !fileExists(module) {
			continue
		}
		working, werr := env.effective(node.path, module, config)
		if werr != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s: could not evaluate the working-tree selection: %s", st.name, errorSummary(werr)))
			continue
		}
		if i == 0 || mcpServer == "" {
			if v := working.MCPServer.Value; v != nil {
				mcpServer = *v
			}
		}
		baseEff, berr := env.effective(node.path, module, base)
		if berr != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s: could not evaluate the selection on %s: %s", st.name, ref, errorSummary(berr)))
			skewed = true
			continue
		}
		lines := describeSelectionSkew(st.stage, working.entry(st.stage), baseEff.entry(st.stage), ref, !found)
		if len(lines) > 0 {
			skewed = true
			res.Warnings = append(res.Warnings, lines...)
		}
	}
	if skewed {
		res.LeftToDo = append(res.LeftToDo, fmt.Sprintf("commit %s and merge it to %s: cron sessions run in fresh worktrees off %s and read the config there", filename, branch, ref))
	}
	if mcpServer != "" {
		if todo := checkBaseMCPDeclaration(ctx, repoDir, ref, res.Agent, mcpServer, env); todo != "" {
			res.LeftToDo = append(res.LeftToDo, todo)
		}
	}
}

const untilMerged = " until this is merged"

// describeSelectionSkew names, per difference, what the stage's cron workers
// do instead while only the base branch's config is visible to them.
func describeSelectionSkew(stage string, working, base cronEffectiveEntry, ref string, baseMissing bool) []string {
	workers := stage + " workers"
	if working.Error != "" {
		return nil // the preflight already reported a working-tree config the module rejects
	}
	if base.Error != "" {
		return []string{fmt.Sprintf("%s cannot run on %s's config (%s)%s", workers, ref, firstLine(base.Error), untilMerged)}
	}
	if working.Value == nil || base.Value == nil {
		return nil
	}
	w, b := *working.Value, *base.Value
	var out []string
	if ws, bs := deref(w.State), deref(b.State); ws != bs {
		which := "the state"
		if baseMissing {
			which = "the default state"
		}
		if stage == "verify" {
			out = append(out, fmt.Sprintf("%s look for tickets in %s `%s`, not `%s`,%s", workers, which, bs, ws, untilMerged))
		} else {
			out = append(out, fmt.Sprintf("%s query %s `%s`, not `%s`,%s", workers, which, bs, ws, untilMerged))
		}
	}
	for _, dim := range []string{"labels", "assignees", "creators", "projects"} {
		for _, pol := range []string{"include", "exclude"} {
			wv, bv := w.Selection[dim][pol], b.Selection[dim][pol]
			if slices.Equal(wv, bv) {
				continue
			}
			switch {
			case pol == "include" && len(bv) == 0:
				out = append(out, fmt.Sprintf("%s scan all %s, not only %s,%s", workers, dim, listOrNone(wv), untilMerged))
			case pol == "include":
				out = append(out, fmt.Sprintf("%s include %s %s, not %s,%s", workers, dim, listOrNone(bv), listOrNone(wv), untilMerged))
			default:
				out = append(out, fmt.Sprintf("%s exclude %s %s, not %s,%s", workers, dim, listOrNone(bv), listOrNone(wv), untilMerged))
			}
		}
	}
	if !slices.Equal(w.RequireLabels, b.RequireLabels) {
		out = append(out, fmt.Sprintf("%s require the labels %s, not %s,%s", workers, listOrNone(b.RequireLabels), listOrNone(w.RequireLabels), untilMerged))
	}
	if !slices.Equal(w.ExcludeLabels, b.ExcludeLabels) {
		out = append(out, fmt.Sprintf("%s exclude the labels %s, not %s,%s", workers, listOrNone(b.ExcludeLabels), listOrNone(w.ExcludeLabels), untilMerged))
	}
	return out
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func listOrNone(v []string) string {
	if len(v) == 0 {
		return "none"
	}
	return "`" + strings.Join(v, "`, `") + "`"
}

// normalizeMCPName folds the spellings the skills treat as one server name.
func normalizeMCPName(s string) string {
	return strings.NewReplacer("-", "", "_", "").Replace(strings.ToLower(strings.TrimSpace(s)))
}

var codexMCPTable = regexp.MustCompile(`(?m)^\s*\[\s*mcp_servers\.("([^"]+)"|'([^']+)'|([A-Za-z0-9_-]+))\s*\]`)

// checkBaseMCPDeclaration returns a left-to-do line when the agent's harness
// file on the base branch does not declare the tracker MCP server. The file is
// looked up in supportedHarnesses, the table detectHarnesses reads.
func checkBaseMCPDeclaration(ctx context.Context, repoDir, ref, agent, server string, env initCronEnv) string {
	harnessName := "Claude Code"
	if agent == cronAgentCodex {
		harnessName = "Codex"
	}
	var h harness
	for _, candidate := range supportedHarnesses {
		if candidate.name == harnessName {
			h = candidate
		}
	}
	if h.file == "" {
		return ""
	}
	data, found, err := env.readRef(ctx, repoDir, ref, h.file)
	if err != nil {
		return ""
	}
	want := normalizeMCPName(server)
	declared := false
	if found {
		if h.file == ".mcp.json" {
			var parsed struct {
				MCPServers map[string]json.RawMessage `json:"mcpServers"`
			}
			if json.Unmarshal(data, &parsed) == nil {
				for name := range parsed.MCPServers {
					declared = declared || normalizeMCPName(name) == want
				}
			}
		} else {
			for _, m := range codexMCPTable.FindAllSubmatch(data, -1) {
				name := string(bytes.Join([][]byte{m[2], m[3], m[4]}, nil))
				declared = declared || normalizeMCPName(name) == want
			}
		}
	}
	if declared {
		return ""
	}
	return fmt.Sprintf("declare the tracker MCP server %q in %s on %s (see the declarations above): %s cron sessions read the base branch's harness file", server, h.file, ref, agent)
}

// --- Report and wiring ----------------------------------------------------

func (r initCronResult) report() string {
	var b strings.Builder
	fmt.Fprintf(&b, "\nFactory cron jobs (agent: %s):\n", r.Agent)
	failed := false
	for _, s := range r.Stages {
		state := "disabled"
		if s.Enabled {
			state = "enabled"
		}
		switch s.Outcome {
		case cronCreated:
			fmt.Fprintf(&b, "  %s: created, %s\n", s.Name, state)
		case cronDisabledByPreflight:
			fmt.Fprintf(&b, "  %s: created disabled (preflight failed)\n", s.Name)
		case cronUpdated:
			fmt.Fprintf(&b, "  %s: updated (%s, schedule unchanged)\n", s.Name, state)
		case cronUnchanged:
			fmt.Fprintf(&b, "  %s: already matches the factory definition (%s)\n", s.Name, state)
		case cronLeftAlone:
			why := "re-run with --update-crons to apply"
			if s.PreflightFailure != "" {
				why = "not updated because the preflight failed"
			}
			fmt.Fprintf(&b, "  %s: differs from the factory definition; left untouched (%s, %s):\n", s.Name, state, why)
		}
		if s.Outcome == cronUpdated || s.Outcome == cronLeftAlone {
			for _, d := range s.Diff {
				b.WriteString("      " + d + "\n")
			}
		}
		if s.PreflightFailure != "" {
			failed = true
			for _, line := range wrapText("preflight: " + s.PreflightFailure) {
				b.WriteString("    " + line + "\n")
			}
		}
	}
	if failed {
		b.WriteString("  Remedy: run `" + cronSkillsInstallRemedy + "`, then re-run this command to confirm the preflight passes. A re-run never enables a job, so enable it in Settings → Cron once the gate works.\n")
	}
	if len(r.Warnings) > 0 {
		b.WriteString("Warnings:\n")
		for _, w := range r.Warnings {
			writeWrappedItem(&b, w)
		}
	}
	if len(r.LeftToDo) > 0 {
		b.WriteString("Left to do:\n")
		for _, w := range r.LeftToDo {
			writeWrappedItem(&b, w)
		}
	}
	return b.String()
}

func writeWrappedItem(b *strings.Builder, item string) {
	for i, line := range wrapText(item) {
		prefix := "  - "
		if i > 0 {
			prefix = "    "
		}
		b.WriteString(prefix + line + "\n")
	}
}

// runInitCronStep runs cron setup after the config write. It needs a local
// daemon and the repository's id; without either it says why it skipped and
// leaves the rest of boss init's result standing.
func runInitCronStep(out io.Writer, opts initOptions, repoDir, configPath string, repo *pb.Repo) error {
	if !opts.cron.requested() {
		return nil
	}
	cmd := opts.command
	if cmd == nil {
		_, _ = io.WriteString(out, "\nCron setup skipped: no daemon connection is available.\n")
		return nil
	}
	if remoteURL(cmd) != "" || hostDestination(cmd) != "" {
		_, _ = io.WriteString(out, "\nCron setup skipped: factory cron jobs need a local daemon and a local repository id (--remote / --host has neither).\n")
		return nil
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	c, err := newClient(cmd)
	if err != nil {
		_, _ = io.WriteString(out, "\nCron setup skipped: could not connect to local bossd.\n")
		return nil
	}
	if repo == nil {
		repo, err = resolveInitRepo(ctx, c, repoDir, false, &initAppliedSteps{})
		if err != nil {
			_, _ = fmt.Fprintf(out, "\nCron setup skipped: no local repository id (%s).\n", err)
			return nil
		}
	}
	res, err := setupInitCron(ctx, c, repo, repoDir, configPath, opts.cron, newInitCronEnv())
	if opts.applied != nil {
		opts.applied.Crons = res.Stages
		opts.applied.CronWarnings, opts.applied.CronLeftToDo = res.Warnings, res.LeftToDo
	}
	if len(res.Stages) > 0 || len(res.Warnings) > 0 {
		_, _ = io.WriteString(out, res.report())
	}
	return err
}
