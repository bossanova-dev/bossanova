package projectconfig

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// The factory stage table holds the host-independent facts about each factory
// stage: its job name, skill, default schedule, whether new repos enable it by
// default, how an agent spells its prompt, and the host-free gate shape. The
// CLI (`boss init --cron`), bossd (repo-registration seeding) and every surface
// that classifies or plans factory crons (export, the cloud fan-out, MCP) read
// the same table, so they cannot drift apart. Hosts (the CLI and bossd) supply
// node, skill-root and file facts, and this package renders and checks them.

// FactoryStage is one row of the factory stage table: the stage, the job name
// `boss init --cron` gives it, its skill (also its prompt's leading token),
// its default schedule (5-field cron, daemon-local timezone), and whether new
// repositories seed it by default.
type FactoryStage struct {
	Stage           string
	JobName         string
	Skill           string
	DefaultSchedule string
	DefaultOn       bool
}

// factoryStageTable is the table, in pipeline order. docs/skills/factory.md
// documents the same jobs as `boss cron add` lines. Retro runs weekly on a
// weekday morning, when the daemon is likely up (missed fires are not caught
// up), off the top of the hour.
var factoryStageTable = FactoryStageTable{
	{Stage: "plan", JobName: "factory-plan", Skill: "boss-plan", DefaultSchedule: "*/15 * * * *", DefaultOn: true},
	{Stage: "build", JobName: "factory-build", Skill: "boss-build", DefaultSchedule: "*/15 * * * *", DefaultOn: true},
	{Stage: "verify", JobName: "factory-verify", Skill: "boss-verify", DefaultSchedule: "*/10 * * * *", DefaultOn: true},
	{Stage: "retro", JobName: "factory-retro", Skill: "boss-retro", DefaultSchedule: "23 9 * * 1"},
}

// DefaultFactoryStages returns a copy of the factory stage table, in pipeline
// order.
func DefaultFactoryStages() FactoryStageTable { return slices.Clone(factoryStageTable) }

// Agents a factory stage knows the skill spelling of.
const (
	agentClaude      = "claude"
	agentCodex       = "codex"
	agentAntigravity = "antigravity"
)

// FactoryAgent is one coding agent a factory stage can run under: its daemon
// runner name and the CLI executable that runner launches.
type FactoryAgent struct {
	Name string
	CLI  string
}

var factoryAgents = []FactoryAgent{
	{Name: agentClaude, CLI: "claude"},
	{Name: agentCodex, CLI: "codex"},
	{Name: agentAntigravity, CLI: "agy"},
}

// FactoryAgents returns a copy of the supported factory agents in
// default-preference order.
func FactoryAgents() []FactoryAgent { return slices.Clone(factoryAgents) }

// Prompt is the stage's job prompt for agent: Codex invokes a skill as
// `$skill`, every other agent (Claude included) as `/skill`.
func (s FactoryStage) Prompt(agent string) string {
	if agent == agentCodex {
		return "$" + s.Skill
	}
	return "/" + s.Skill
}

// GatePath is the stage's gate script relative to an agent's skill root.
func (s FactoryStage) GatePath() string {
	return s.Skill + "/toolbox/cron-gates/" + s.Skill + ".mjs"
}

// ModulePath is the installed skill-config module the stage's gate loads,
// relative to an agent's skill root.
func (s FactoryStage) ModulePath() string {
	return s.Skill + "/toolbox/skill-config.mjs"
}

// GateCommand renders the POSIX shell gate command for s using nodeCommand
// (bare `node` or a shell-quoted path) and the agent's skill root.
func (s FactoryStage) GateCommand(nodeCommand, skillRoot, home string) string {
	return nodeCommand + ` "` + RenderSkillRoot(skillRoot, home) + "/" + s.GatePath() + `"`
}

// MatchesPrompt reports whether prompt's leading token invokes the stage's
// skill, in either agent's spelling.
func (s FactoryStage) MatchesPrompt(prompt string) bool {
	fields := strings.Fields(prompt)
	return len(fields) > 0 && (fields[0] == "/"+s.Skill || fields[0] == "$"+s.Skill)
}

// MatchesJob reports whether a job with the given name and prompt represents
// this stage, either by canonical job name or by leading prompt token.
func (s FactoryStage) MatchesJob(name, prompt string) bool {
	return name == s.JobName || s.MatchesPrompt(prompt)
}

// MatchesGate reports whether gate runs the stage's cron gate script
// (cron-gates/<skill>.mjs, from the installed skill tree or a repo's own
// toolbox), so a repo-local cron that serves the stage under its own prompt
// still counts as the stage.
func (s FactoryStage) MatchesGate(gate string) bool {
	return strings.Contains(gate, "cron-gates/"+s.Skill+".mjs")
}

var shellSafeWord = regexp.MustCompile(`^[A-Za-z0-9_./+=:@%-]+$`)

// ShellWord renders s as one POSIX shell word.
func ShellWord(s string) string {
	if shellSafeWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// RenderSkillRoot writes root as `$HOME/<rel>` when it is under home, and as a
// slash-separated path otherwise.
func RenderSkillRoot(root, home string) string {
	if home != "" {
		if rel, err := filepath.Rel(home, root); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			return "$HOME/" + filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(root)
}

// FactoryGateHost supplies the host-dependent facts FactoryGatePreflight checks
// for a stage's gate command.
type FactoryGateHost struct {
	NodePath        string
	SkillRoot       string
	SkillRootErr    error
	Home            string
	FileExists      func(string) bool
	SkipModuleCheck bool
}

// FactoryGatePreflight checks the host-free gate requirements for st against
// host and returns every failure in canonical order: missing node, unresolvable
// skill root (stop), unquotable skill root (stop), missing gate script, and
// (unless SkipModuleCheck is set) missing installed skill-config module.
func FactoryGatePreflight(st FactoryStage, host FactoryGateHost) []string {
	var failures []string
	if host.NodePath == "" {
		failures = append(failures, "node is not on the daemon's service PATH or on your PATH")
	}
	if host.SkillRootErr != nil {
		return append(failures, "the agent's skill directory could not be resolved")
	}
	rendered := RenderSkillRoot(host.SkillRoot, host.Home)
	if strings.ContainsAny(rendered, "\"`\\") || strings.Contains(strings.TrimPrefix(rendered, "$HOME/"), "$") {
		return append(failures, "the skill directory "+host.SkillRoot+" cannot be quoted in a gate command")
	}
	gate := filepath.Join(host.SkillRoot, filepath.FromSlash(st.GatePath()))
	if host.FileExists == nil || !host.FileExists(gate) {
		failures = append(failures, "gate script "+gate+" is not installed")
	}
	if !host.SkipModuleCheck {
		module := filepath.Join(host.SkillRoot, filepath.FromSlash(st.ModulePath()))
		if host.FileExists == nil || !host.FileExists(module) {
			failures = append(failures, "installed "+module+" is missing")
		}
	}
	return failures
}

// FactoryStageTable is a factory stage table. It is a FactoryStageReader: a
// live job is a factory stage only when its prompt is exactly that stage's
// prompt for the job's agent, so a prompt with extra arguments exports as a
// plain cron.
type FactoryStageTable []FactoryStage

var _ FactoryStageReader = FactoryStageTable(nil)

// Lookup returns the row for stage.
func (t FactoryStageTable) Lookup(stage string) (FactoryStage, bool) {
	i := slices.IndexFunc(t, func(s FactoryStage) bool { return s.Stage == stage })
	if i < 0 {
		return FactoryStage{}, false
	}
	return t[i], true
}

// Classify recognises job as a factory stage.
func (t FactoryStageTable) Classify(job *pb.CronJob) (string, bool) {
	prompt := job.GetPrompt()
	for _, s := range t {
		if s.MatchesPrompt(prompt) && s.Prompt(EffectiveAgent(job.GetAgentName())) == prompt {
			return s.Stage, true
		}
	}
	return "", false
}

// DefaultSchedule is stage's default schedule; ok is false for an unknown
// stage.
func (t FactoryStageTable) DefaultSchedule(stage string) (string, bool) {
	s, ok := t.Lookup(stage)
	return s.DefaultSchedule, ok
}

// HostFreeFactory resolves factory crons from the stage table alone: the
// prompt in the agent's skill spelling (`$skill` for Codex, `/skill` for the
// others) and the default schedule, with no gate and no preflight, since only
// the host that runs the cron can resolve its own paths. An omitted agent is
// Claude.
func HostFreeFactory(stages []FactoryStage) FactoryResolver {
	return hostFreeFactory(slices.Clone(stages))
}

type hostFreeFactory []FactoryStage

func (h hostFreeFactory) Resolve(stage, agent string) (FactoryCron, error) {
	st, ok := FactoryStageTable(h).Lookup(stage)
	if !ok {
		return FactoryCron{}, ErrUnknownFactoryStage
	}
	agent = strings.ToLower(strings.TrimSpace(agent))
	if agent == "" {
		agent = agentClaude
	}
	if !slices.ContainsFunc(factoryAgents, func(a FactoryAgent) bool { return a.Name == agent }) {
		return FactoryCron{}, fmt.Errorf("agent %q cannot run pipeline cron jobs", agent)
	}
	return FactoryCron{Skill: st.Skill, Agent: agent, Prompt: st.Prompt(agent), DefaultSchedule: st.DefaultSchedule}, nil
}
