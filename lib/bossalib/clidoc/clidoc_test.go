package clidoc

import (
	"reflect"
	"regexp"
	"testing"
)

var groupIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

func TestGroupOrderIsStableAndTitled(t *testing.T) {
	if len(GroupOrder) == 0 {
		t.Fatal("GroupOrder must define at least one group")
	}
	seen := map[string]bool{}
	for _, g := range GroupOrder {
		if g.ID == "" || g.Title == "" {
			t.Errorf("group spec has empty field: %+v", g)
		}
		if g.ReadWhen == "" {
			t.Errorf("group spec %q has empty ReadWhen (needed for the skill routing index)", g.ID)
		}
		if !groupIDPattern.MatchString(g.ID) {
			t.Errorf("group id %q must match %s (it becomes a references/<id>.md filename)", g.ID, groupIDPattern.String())
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

// TestGroupOrderReadWhenHintsArePinned pins every routing hint literally.
//
// The generator-side tests derive their expectation from GroupOrder itself, so
// they prove the string is threaded through Extract and renderIndex unmutated
// but cannot notice a wrong hint — a copy-pasted line passes a non-empty check
// and a derived-expectation check alike. These hints are the only thing routing
// an agent to the right references/<id>.md, so a silently mis-copied one sends
// it to the wrong file with no gate firing. Pinning them here makes a reword a
// deliberate two-place edit.
func TestGroupOrderReadWhenHintsArePinned(t *testing.T) {
	want := map[string]string{
		"session":     "Creating, listing, attaching to, merging or archiving a session",
		"chat":        "Starting a chat, sending it a message, or reading a transcript",
		"repo":        "Registering, cloning, updating or removing a repository",
		"cron":        "Creating, editing, listing or firing a scheduled job",
		"callback":    "Arming or inspecting a one-shot GitHub PR callback",
		"broadcast":   "Sending a broadcast or registering an outcome subscription",
		"notes":       "Recording or harvesting durable repo-scoped notes",
		"account":     "Adding, testing, rotating or switching a provider account",
		"trash":       "Resurrecting an archived session or emptying the trash",
		"daemon":      "Starting, stopping or inspecting bossd",
		"mcp":         "Running or configuring the MCP server",
		"skills":      "Installing or syncing the boss skill payload",
		"settings":    "Changing global settings or authenticating",
		"diagnostics": "Running the repair doctor, checks or other diagnostics",
		"plugins":     "Listing or inspecting loaded bossd plugins",
		"other":       "Anything unclassified (e.g. `boss version`)",
	}
	if len(GroupOrder) != len(want) {
		t.Errorf("GroupOrder has %d groups, pinned hints cover %d — add the new group's hint here", len(GroupOrder), len(want))
	}
	for _, g := range GroupOrder {
		hint, ok := want[g.ID]
		if !ok {
			t.Errorf("group %q has no pinned ReadWhen hint — add one here", g.ID)
			continue
		}
		if g.ReadWhen != hint {
			t.Errorf("group %q ReadWhen = %q, want %q", g.ID, g.ReadWhen, hint)
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
			Long: "An extra `AGENT` column appears only when at least one listed session uses an agent that differs from the user's `Settings.DefaultAgent`. In the common single-agent case the column is hidden so the table stays compact.\n\n" +
				"`--json` emits `{\"sessions\": [...]}` — never `null`, and never the human \"No sessions found.\" line, so a driver decodes one shape whether or not anything matched. Each row carries `id`, `title`, `state`, `repo_id`, `agent`, `pr_number`, `pr_url`, `branch`, `created_at` and `updated_at`. `state` is the enum NAME with its `SESSION_STATE_` prefix trimmed (`RUNNING`, `READY_FOR_REVIEW`, …), never the numeric value, so a caller is not coupled to proto field ordering; it is the same vocabulary `--state` accepts. `pr_number` is `null` rather than 0 for a session with no PR. Timestamps are RFC3339 in UTC. `--repo`, `--state` and `--archived` filter identically with and without `--json` — the flag changes rendering only, never the query.",
			Examples: []Example{
				{Command: "boss ls"},
				{Command: "boss ls --repo my-repo --state running,paused"},
				{Command: "boss ls --archived"},
				{Command: "boss ls --json | jq -r '.sessions[] | select(.state==\"READY_FOR_REVIEW\") | .pr_url'"},
			},
		},
		// The first two paragraphs are the pre-existing prose this test was
		// written to preserve; the three that follow document the BOS-821
		// flags. The assertion stays exact equality over the whole string so
		// that dropping or rewording any paragraph still fails here.
		"boss new": {
			Long: "Launches the interactive session creation flow. " +
				"When both --repo and --prompt are provided the command runs " +
				"non-interactively: it creates the session, prints the session-id " +
				"to stdout as soon as the session exists, prints chat-id later if the " +
				"daemon provides one, streams setup progress to stderr until setup " +
				"settles, then exits. Combine with --detach (implicit when both flags are set) for " +
				"scripting. Use --agent to override the default agent plugin.\n\n" +
				"Launching a session to run work unattended: supplying a prompt " +
				"launches the agent headlessly so the work actually runs — that is " +
				"the path's own behaviour, not something --detach causes. " +
				"The CLI and the MCP `create_session` tool now share " +
				"this default. `create_session` applies the same rule: a " +
				"prompt-carrying call defaults to headless and reports " +
				"agent_launched=true, while attended:true creates the session idle " +
				"awaiting a human `boss attach` (agent_launched=false). Prefer the " +
				"default for programmatic/unattended launches.\n\n" +
				"--detach vs --tmux-unattended: they are NOT alternatives. The " +
				"non-interactive --repo + --prompt path always detaches, so --detach " +
				"is a no-op there and only affects flag parsing; it governs whether " +
				"this command attaches a chat pane before it exits, never how or " +
				"where the session is hosted. --tmux-unattended is the distinct " +
				"durable-pane option: it hosts the session in a tmux pane that " +
				"survives a daemon restart and is attach-safe, which is what a child " +
				"session that must outlive the daemon needs. Both can be set at once, " +
				"and the daemon carries them as independent fields.\n\n" +
				"Tracker binding: --tracker-id, --tracker-source (linear or sentry) " +
				"and --tracker-url bind the session to an external issue, which is " +
				"what the daemon's tracker-id dedup keys on — more robust than the " +
				"`[<TICKET>] <title>` title convention, which silently duplicates a " +
				"session when the title drifts. Each flag is independent and an " +
				"omitted one leaves its field unset; an unrecognised --tracker-source " +
				"is rejected before any session is created.\n\n" +
				"Use --json on the non-interactive path for a machine-readable " +
				"envelope instead of the two-line output: success prints " +
				"{session:{id, title, chat_id}} on stdout, failure prints " +
				"{error:{code, connect_code, message}} with a stable `code` such as " +
				"INVALID_ARGUMENT or NOT_FOUND, and every failure still exits 1. " +
				"Setup output goes to stderr either way, so stdout carries exactly " +
				"one JSON object. Without --json the two-line `session-id:` / " +
				"`chat-id:` output is unchanged; session-id appears as soon as the " +
				"session exists, and chat-id appears later if the daemon provides one.",
			Examples: []Example{
				{Command: "boss new"},
				{Command: "boss new --agent opencode"},
				{Command: "boss new --repo my-repo --prompt \"refactor the auth module\" --detach", Explanation: "Create a session non-interactively and print its ids"},
				{Command: "boss new --agent codex --repo my-repo --prompt \"review this PR for security issues\" --detach", Explanation: "Ask Codex for a second opinion; capture ids for boss chat wait"},
				// Deliberately a generic placeholder, not a real BOS-<id>: this
				// prose ships in the globally-installed `boss` core, which
				// TestPublishedCoresAreProjectAgnostic forbids from carrying
				// Bossanova backlog identity.
				{
					Command: "boss new --repo my-repo --prompt \"/boss-build PROJ-42\" " +
						"--tmux-unattended --tracker-id PROJ-42 --tracker-source linear --json",
					Explanation: "Launch a durable tracker-bound child and parse {session:{id, chat_id}}",
				},
				{
					Command: "CHAT=$(boss new --repo my-repo --prompt \"/boss-build PROJ-42\" " +
						"--json | jq -r .session.chat_id)",
					Explanation: "Capture the chat id without parsing the human two-line output",
				},
			},
		},
		"boss chat new": {
			Long: "Starts a brand-new live chat inside an existing session, reusing that session's worktree, branch and PR with a clean context. This is the CLI counterpart of the MCP start_chat tool. `boss new` is not a substitute: on a session that is already live the daemon attaches to it instead, and the supplied prompt is never run. The agent_session_id is minted for you; the command fails rather than reporting success if the daemon could not spawn a live agent behind the chat. An omitted --agent inherits the session's own agent; --title names the chat. Print the id with --json and feed it straight to `boss chat send`.",
			Examples: []Example{
				{Command: "boss chat new <session-id>"},
				{Command: "boss chat new <session-id> --title \"repair round\" --json", Explanation: "Capture chat.agent_session_id, then `boss chat send <chat-id> ... --submit`"},
			},
		},
		"boss chat send": {
			Long: "Delivers a follow-up message to a running chat identified by a session id or agent_session_id (the chat-id printed by `boss new --detach` or `boss chat new --json`). When given a session id, boss targets that session's primary chat. The daemon wakes a sleeping chat before pasting the message; --wake-if-asleep defaults to true and exists so a caller can pass --wake-if-asleep=false to leave a deliberately stopped chat stopped.",
			Examples: []Example{
				{Command: "boss chat send <session-id|chat-id> \"please also add tests\""},
				{Command: "boss chat send <chat-id> \"status?\" --wake-if-asleep=false", Explanation: "Do not wake a stopped chat just to deliver this message"},
			},
		},
		"boss chat show": {
			Long: "Prints the full conversation transcript for a chat or session's primary chat. Use --result-only to print just the final assistant response text (suitable for scripting). Use --limit to cap the number of messages.",
		},
		"boss chat wait": {
			Long: "Blocks until the chat identified by a session id or agent_session_id becomes idle or is waiting for input, then prints the final assistant result. Polls chat status every few seconds. Use --timeout to limit wait time. Typical recipe: `boss new --agent codex --repo R --prompt P --detach` then `boss chat wait <session-id|chat-id>` to collect the result.",
			Examples: []Example{
				{Command: "boss chat wait <session-id|chat-id>"},
				{Command: "boss chat wait <session-id|chat-id> --timeout 10m"},
				{Command: "CHAT=$(boss new --agent codex --repo my-repo --prompt \"second opinion on PR #42\" --detach | sed -n 's/^chat-id:[[:space:]]*//p') && boss chat wait $CHAT", Explanation: "Full cross-agent second-opinion recipe"},
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
