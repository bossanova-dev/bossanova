package agent_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/recurser/bossd/internal/agent"
)

func TestAgentRunnerNotLoaded_EmptyRegistryIsActionable(t *testing.T) {
	t.Parallel()

	err := agent.AgentRunnerNotLoaded("claude", map[string]agent.AgentRunnerClient{})
	if !errors.Is(err, agent.ErrAgentRunnerNotLoaded) {
		t.Fatal("must wrap ErrAgentRunnerNotLoaded so callers can errors.Is it")
	}
	msg := err.Error()
	if !strings.Contains(msg, "no agent plugins are loaded") || !strings.Contains(msg, "restart bossd") {
		t.Fatalf("empty-registry message not actionable: %q", msg)
	}
	if !strings.Contains(msg, "claude") {
		t.Fatalf("message should still name the requested agent: %q", msg)
	}
}

func TestAgentRunnerNotLoaded_NilRegistryIsActionable(t *testing.T) {
	t.Parallel()

	err := agent.AgentRunnerNotLoaded("claude", nil)
	if !errors.Is(err, agent.ErrAgentRunnerNotLoaded) {
		t.Fatal("nil registry must still wrap ErrAgentRunnerNotLoaded")
	}
	if msg := err.Error(); !strings.Contains(msg, "no agent plugins are loaded") {
		t.Fatalf("nil registry should be treated as empty: %q", msg)
	}
}

func TestAgentRunnerNotLoaded_NonEmptyListsLoadedAgents(t *testing.T) {
	t.Parallel()

	err := agent.AgentRunnerNotLoaded("claude", map[string]agent.AgentRunnerClient{"codex": nil})
	if !errors.Is(err, agent.ErrAgentRunnerNotLoaded) {
		t.Fatal("must wrap ErrAgentRunnerNotLoaded")
	}
	msg := err.Error()
	if !strings.Contains(msg, "codex") {
		t.Fatalf("want loaded agents listed, got %q", msg)
	}
	if !strings.Contains(msg, "claude") {
		t.Fatalf("message should still name the requested agent: %q", msg)
	}
}

func TestAgentRunnerNotLoaded_NonEmptyListsSortedAgents(t *testing.T) {
	t.Parallel()

	err := agent.AgentRunnerNotLoaded("opencode", map[string]agent.AgentRunnerClient{
		"codex":  nil,
		"claude": nil,
	})
	msg := err.Error()
	if !strings.Contains(msg, "claude, codex") {
		t.Fatalf("want deterministically sorted loaded agents, got %q", msg)
	}
}
