package skills

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	daemon "github.com/recurser/boss/internal/daemonlife"
	"github.com/recurser/boss/internal/domain/rpc"
	libskillinstall "github.com/recurser/bossalib/contract/plugin/skillinstall"
	"github.com/recurser/bossalib/kit/projectconfig"
)

const (
	cronAgentClaude      = "claude"
	cronAgentCodex       = "codex"
	cronAgentAntigravity = "antigravity"
)

type cronAgentHost struct {
	harness     string
	skill       libskillinstall.Agent
	trustedOnly bool
}

var cronAgentHosts = map[string]cronAgentHost{
	cronAgentClaude:      {harness: "Claude Code", skill: libskillinstall.AgentClaude},
	cronAgentCodex:       {harness: "Codex", skill: libskillinstall.AgentCodex},
	cronAgentAntigravity: {skill: libskillinstall.AgentAntigravity, trustedOnly: true},
}

type cronAgent struct {
	name, cli, harness string
	skill              libskillinstall.Agent
	trustedOnly        bool
}

// cronAgents are the agents a factory job may run under, in default-preference
// order, built from projectconfig.FactoryAgents.
var cronAgents = cliCronAgents(projectconfig.FactoryAgents())

func cliCronAgents(agents []projectconfig.FactoryAgent) []cronAgent {
	out := make([]cronAgent, len(agents))
	for i, a := range agents {
		host, ok := cronAgentHosts[a.Name]
		if !ok {
			panic("factory agent table has no CLI host metadata for " + a.Name)
		}
		out[i] = cronAgent{
			name:        a.Name,
			cli:         a.CLI,
			harness:     host.harness,
			skill:       host.skill,
			trustedOnly: host.trustedOnly,
		}
	}
	return out
}

func cronAgentByName(name string) (cronAgent, bool) {
	i := slices.IndexFunc(cronAgents, func(a cronAgent) bool { return a.name == name })
	if i < 0 {
		return cronAgent{}, false
	}
	return cronAgents[i], true
}

// cronAgentNames lists the agents for help and usage text:
// "claude, codex or antigravity".
func cronAgentNames() string {
	names := make([]string, len(cronAgents))
	for i, a := range cronAgents {
		names[i] = a.name
	}
	return joinOr(names)
}

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
func initCronAgentEligibility(ctx context.Context, c Client, servicePath string) ([]cronAgentEligibility, error) {
	loaded, err := c.ListAgents(ctx)
	if err != nil {
		return nil, errors.New("could not list the daemon's agents")
	}
	out := make([]cronAgentEligibility, 0, len(cronAgents))
	for _, a := range cronAgents {
		e := cronAgentEligibility{Name: a.name}
		e.Loaded = slices.ContainsFunc(loaded, func(info rpc.AgentInfo) bool { return info.Name == a.name })
		if path, ok := daemon.LookPathIn(servicePath, a.cli); ok {
			e.CLIPath = path
		}
		out = append(out, e)
	}
	return out, nil
}

// chooseInitCronAgent picks the requested agent when it qualifies, else the
// first eligible one in cronAgents order.
func chooseInitCronAgent(eligibility []cronAgentEligibility, requested string) (string, error) {
	if requested != "" {
		for _, e := range eligibility {
			if e.Name == requested {
				if !e.Eligible() {
					return "", fmt.Errorf("--agent %s cannot run pipeline cron jobs: %s", requested, e.reason())
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
	return "", fmt.Errorf("no agent can run pipeline cron jobs (%s)", strings.Join(why, "; "))
}
