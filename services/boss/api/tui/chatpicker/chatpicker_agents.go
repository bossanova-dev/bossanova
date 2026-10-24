package chatpicker

import (
	"context"

	"github.com/recurser/boss/internal/domain/rpc"

	tea "charm.land/bubbletea/v2"
)

// fetchAgents loads the daemon's installed agent runners. Errors fall
// through silently — a failed fetch leaves agents empty, which collapses
// the wizard to its single-agent shape (skip the agent-select phase).
func fetchAgents(c Client, ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		agents, err := c.ListAgents(ctx)
		return agentsMsg{agents: agents, err: err}
	}
}

func agentIndex(agents []rpc.AgentInfo, name string) int {
	if name == "" {
		return -1
	}
	for i, agent := range agents {
		if agent.Name == name {
			return i
		}
	}
	return -1
}

// agentsMsg carries the result of a ListAgents RPC call. Agents drive the
// per-session agent-select phase shown when more than one agent runner is
// loaded and the user did not pre-pick one via `--agent`.
type agentsMsg struct {
	agents []rpc.AgentInfo
	err    error
}
