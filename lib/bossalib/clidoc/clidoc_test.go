package clidoc

import (
	"reflect"
	"testing"
)

func TestGroupOrderIsStableAndTitled(t *testing.T) {
	if len(GroupOrder) == 0 {
		t.Fatal("GroupOrder must define at least one group")
	}
	seen := map[string]bool{}
	for _, g := range GroupOrder {
		if g.ID == "" || g.Title == "" {
			t.Errorf("group spec has empty field: %+v", g)
		}
		if seen[g.ID] {
			t.Errorf("duplicate group id %q", g.ID)
		}
		seen[g.ID] = true
		title, ok := GroupTitle(g.ID)
		if !ok || title != g.Title {
			t.Errorf("GroupTitle(%q) = %q,%v; want %q,true", g.ID, title, ok, g.Title)
		}
	}
}

func TestGroupTitleUnknownID(t *testing.T) {
	if title, ok := GroupTitle("does-not-exist"); ok || title != "" {
		t.Errorf("GroupTitle(unknown) = %q,%v; want \"\",false", title, ok)
	}
}

func TestRegistryKeysAreCommandPaths(t *testing.T) {
	for path := range Registry {
		if len(path) < len("boss ") || path[:5] != "boss " {
			t.Errorf("registry key %q is not a 'boss ...' command path", path)
		}
	}
}

func TestRegistryBuilderPreservesSessionAndChatDocumentation(t *testing.T) {
	registry := newRegistry()
	tests := map[string]Prose{
		"boss ls": {
			Long: "An extra `AGENT` column appears only when at least one listed session uses an agent that differs from the user's `Settings.DefaultAgent`. In the common single-agent case the column is hidden so the table stays compact.",
			Examples: []Example{
				{Command: "boss ls"},
				{Command: "boss ls --repo my-repo --state running,paused"},
				{Command: "boss ls --archived"},
			},
		},
		"boss new": {
			Long: "Launches the interactive session creation flow. When both --repo and --prompt are provided the command runs non-interactively: it creates the session, streams any setup output to stderr, and prints the session-id and chat-id to stdout, then exits. Combine with --detach (implicit when both flags are set) for scripting. Use --agent to override the default agent plugin.\n\nLaunching a session to run work unattended: supplying a prompt launches the agent (headless, via the implicit --detach) so the work actually runs — the CLI and the MCP `create_session` tool now share this default. `create_session` applies the same rule: a prompt-carrying call defaults to headless and reports agent_launched=true, while attended:true creates the session idle awaiting a human `boss attach` (agent_launched=false). Prefer the default for programmatic/unattended launches.",
			Examples: []Example{
				{Command: "boss new"},
				{Command: "boss new --agent opencode"},
				{Command: "boss new --repo my-repo --prompt \"refactor the auth module\" --detach", Explanation: "Create a session non-interactively and print its ids"},
				{Command: "boss new --agent codex --repo my-repo --prompt \"review this PR for security issues\" --detach", Explanation: "Ask Codex for a second opinion; capture ids for boss chat wait"},
			},
		},
		"boss chat send": {
			Long: "Delivers a follow-up message to a running chat identified by a session id or agent_session_id (the chat-id printed by `boss new --detach`). When given a session id, boss targets that session's primary chat. The daemon wakes a sleeping chat before pasting the message.",
		},
		"boss chat show": {
			Long: "Prints the full conversation transcript for a chat or session's primary chat. Use --result-only to print just the final assistant response text (suitable for scripting). Use --limit to cap the number of messages.",
		},
		"boss chat wait": {
			Long: "Blocks until the chat identified by a session id or agent_session_id becomes idle or is waiting for input, then prints the final assistant result. Polls chat status every few seconds. Use --timeout to limit wait time. Typical recipe: `boss new --agent codex --repo R --prompt P --detach` then `boss chat wait <session-id|chat-id>` to collect the result.",
			Examples: []Example{
				{Command: "boss chat wait <session-id|chat-id>"},
				{Command: "boss chat wait <session-id|chat-id> --timeout 10m"},
				{Command: "CHAT=$(boss new --agent codex --repo my-repo --prompt \"second opinion on PR #42\" --detach | awk '/^chat-id/{print $2}') && boss chat wait $CHAT", Explanation: "Full cross-agent second-opinion recipe"},
			},
		},
	}

	for command, want := range tests {
		got, ok := registry[command]
		if !ok {
			t.Errorf("newRegistry() missing %q", command)
			continue
		}
		if got.Long != want.Long {
			t.Errorf("newRegistry()[%q].Long = %q; want %q", command, got.Long, want.Long)
		}
		if want.Examples != nil && !reflect.DeepEqual(got.Examples, want.Examples) {
			t.Errorf("newRegistry()[%q].Examples = %#v; want %#v", command, got.Examples, want.Examples)
		}
	}
}
