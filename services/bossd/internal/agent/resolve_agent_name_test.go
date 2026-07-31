package agent

import (
	"testing"

	"github.com/rs/zerolog"
)

// TestDispatcher_ResolveAgentName locks the exported name-only resolution seam
// against the same rules resolveByName applies, including the empty-name
// fallbacks callers (e.g. the session lifecycle) must reproduce to decide
// agent-keyed policy before a launch.
func TestDispatcher_ResolveAgentName(t *testing.T) {
	tests := []struct {
		name         string
		runners      map[string]AgentRunner
		defaultAgent string
		agentName    string
		want         string
	}{
		{
			name: "explicit known name resolves to itself",
			runners: map[string]AgentRunner{
				"claude": newLabeledAgentRunner("claude"),
				"codex":  newLabeledAgentRunner("codex"),
			},
			defaultAgent: "claude",
			agentName:    "codex",
			want:         "codex",
		},
		{
			name: "explicit unknown name is returned verbatim",
			runners: map[string]AgentRunner{
				"claude": newLabeledAgentRunner("claude"),
			},
			defaultAgent: "claude",
			agentName:    "opencode",
			want:         "opencode",
		},
		{
			name: "empty name with a single runner picks that runner over the default",
			runners: map[string]AgentRunner{
				"codex": newLabeledAgentRunner("codex"),
			},
			defaultAgent: "claude",
			agentName:    "",
			want:         "codex",
		},
		{
			name: "empty name with two runners falls back to the default agent",
			runners: map[string]AgentRunner{
				"claude": newLabeledAgentRunner("claude"),
				"codex":  newLabeledAgentRunner("codex"),
			},
			defaultAgent: "claude",
			agentName:    "",
			want:         "claude",
		},
		{
			name:         "empty name with no runners falls back to the default agent",
			runners:      map[string]AgentRunner{},
			defaultAgent: "codex",
			agentName:    "",
			want:         "codex",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := NewDispatcher(tc.runners, func(string) (string, error) { return "", nil }, tc.defaultAgent, zerolog.Nop())
			if got := d.ResolveAgentName(tc.agentName); got != tc.want {
				t.Fatalf("ResolveAgentName(%q) = %q, want %q", tc.agentName, got, tc.want)
			}
		})
	}
}

// TestDispatcherImplementsAgentNameResolver keeps the optional seam wired to
// the concrete dispatcher so a lifecycle type assertion cannot silently start
// failing.
func TestDispatcherImplementsAgentNameResolver(t *testing.T) {
	var d any = NewDispatcher(nil, func(string) (string, error) { return "", nil }, "claude", zerolog.Nop())
	if _, ok := d.(AgentNameResolver); !ok {
		t.Fatal("*Dispatcher must implement AgentNameResolver")
	}
}
