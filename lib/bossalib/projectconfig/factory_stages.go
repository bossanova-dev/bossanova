package projectconfig

import (
	"fmt"
	"slices"
	"strings"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// The factory stage table holds the host-independent facts about each factory
// stage: its job name, skill, default schedule, and how an agent spells its
// prompt. The CLI builds `boss init --cron` from it, and every surface that
// classifies or plans factory crons (export, the cloud fan-out, MCP) reads the
// same table, so they cannot drift apart. Host-dependent facts (the gate's
// node and skill-root paths, the gate preflight) stay with the CLI.

// FactoryStage is one row of the factory stage table: the stage, the job name
// `boss init --cron` gives it, its skill (also its prompt's leading token) and
// its default schedule (5-field cron, daemon-local timezone).
type FactoryStage struct {
	Stage           string
	JobName         string
	Skill           string
	DefaultSchedule string
}

// factoryStageTable is the table, in pipeline order. docs/skills/factory.md
// documents the same jobs as `boss cron add` lines. Retro runs weekly on a
// weekday morning, when the daemon is likely up (missed fires are not caught
// up), off the top of the hour.
var factoryStageTable = FactoryStageTable{
	{Stage: "plan", JobName: "factory-plan", Skill: "boss-plan", DefaultSchedule: "*/15 * * * *"},
	{Stage: "build", JobName: "factory-build", Skill: "boss-build", DefaultSchedule: "*/15 * * * *"},
	{Stage: "verify", JobName: "factory-verify", Skill: "boss-verify", DefaultSchedule: "*/10 * * * *"},
	{Stage: "retro", JobName: "factory-retro", Skill: "boss-retro", DefaultSchedule: "23 9 * * 1"},
}

// DefaultFactoryStages returns a copy of the factory stage table, in pipeline
// order.
func DefaultFactoryStages() FactoryStageTable { return slices.Clone(factoryStageTable) }

// Agents a factory stage knows the skill spelling of.
const (
	agentClaude = "claude"
	agentCodex  = "codex"
)

// Prompt is the stage's job prompt for agent: Codex invokes a skill as
// `$skill`, every other agent (Claude included) as `/skill`.
func (s FactoryStage) Prompt(agent string) string {
	if agent == agentCodex {
		return "$" + s.Skill
	}
	return "/" + s.Skill
}

// MatchesPrompt reports whether prompt's leading token invokes the stage's
// skill, in either agent's spelling.
func (s FactoryStage) MatchesPrompt(prompt string) bool {
	fields := strings.Fields(prompt)
	return len(fields) > 0 && (fields[0] == "/"+s.Skill || fields[0] == "$"+s.Skill)
}

// MatchesGate reports whether gate runs the stage's cron gate script
// (cron-gates/<skill>.mjs, from the installed skill tree or a repo's own
// toolbox), so a repo-local cron that serves the stage under its own prompt
// still counts as the stage.
func (s FactoryStage) MatchesGate(gate string) bool {
	return strings.Contains(gate, "cron-gates/"+s.Skill+".mjs")
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
// prompt in the agent's skill spelling (`/skill` for Claude, `$skill` for
// Codex) and the default schedule, with no gate and no preflight, since only
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
	switch agent {
	case "":
		agent = agentClaude
	case agentClaude, agentCodex:
	default:
		return FactoryCron{}, fmt.Errorf("agent %q cannot run pipeline cron jobs", agent)
	}
	return FactoryCron{Skill: st.Skill, Agent: agent, Prompt: st.Prompt(agent), DefaultSchedule: st.DefaultSchedule}, nil
}
