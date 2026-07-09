package agent

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrAgentRunnerNotLoaded is the sentinel for a failed lookup in an agent-runner
// registry keyed by agent name. Callers use errors.Is to treat "the plugin isn't
// loaded" distinctly from a real failure (e.g. account verification can degrade
// to "unavailable" instead of "failed").
var ErrAgentRunnerNotLoaded = errors.New("agent runner not loaded")

// AgentRunnerNotLoaded builds an actionable error for a registry miss. An empty
// (or nil) registry means NO agent plugin is loaded at all — most often a package
// upgrade moved the plugin binaries out from under a still-running daemon, whose
// captured registry then came up empty — so the fix is to restart bossd. A
// non-empty registry means the specific agent is missing; the message lists what
// IS loaded (sorted for determinism).
func AgentRunnerNotLoaded(name string, registry map[string]AgentRunnerClient) error {
	if len(registry) == 0 {
		return fmt.Errorf("%w for agent %q: no agent plugins are loaded — restart bossd (a recent upgrade may have moved the plugin binaries)", ErrAgentRunnerNotLoaded, name)
	}
	loaded := make([]string, 0, len(registry))
	for k := range registry {
		loaded = append(loaded, k)
	}
	sort.Strings(loaded)
	return fmt.Errorf("%w for agent %q; loaded agents: %s", ErrAgentRunnerNotLoaded, name, strings.Join(loaded, ", "))
}
