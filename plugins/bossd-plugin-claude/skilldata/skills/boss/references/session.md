<!-- GENERATED from the boss CLI by `make gen-skill` — do not edit by hand. Index: ../SKILL.md -->

## Session Management

### `boss archive <session-id>`

Archive a session (keep branch, remove worktree)

```bash
boss archive abc123
```

### `boss attach <session-id>`

Attach to a running session

Attaches to a running session's terminal.

```bash
boss attach abc123
```

### `boss chats <session-id>`

List chats in a session

```bash
boss chats abc123
```

### `boss ls [flags]`

List sessions (non-interactive)

An extra `AGENT` column appears only when at least one listed session uses an agent that differs from the user's `Settings.DefaultAgent`. In the common single-agent case the column is hidden so the table stays compact.

**Flags:**

- `--archived` — Include archived sessions
- `--repo` — Filter by repo ID
- `--state` — Filter by state(s)

```bash
boss ls
boss ls --repo my-repo --state running,paused
boss ls --archived
```

### `boss new [flags]`

Create a new coding session

Launches the interactive session creation flow. When both --repo and --prompt are provided the command runs non-interactively: it creates the session, streams any setup output to stderr, and prints the session-id and chat-id to stdout, then exits. Combine with --detach (implicit when both flags are set) for scripting. Use --agent to override the default agent plugin.

Launching a session to run work unattended: supplying a prompt launches the agent (headless, via the implicit --detach) so the work actually runs — the CLI and the MCP `create_session` tool now share this default. `create_session` applies the same rule: a prompt-carrying call defaults to headless and reports agent_launched=true, while attended:true creates the session idle awaiting a human `boss attach` (agent_launched=false). Prefer the default for programmatic/unattended launches.

**Flags:**

- `--account` — Account id or label to run this session under (empty = system default)
- `--agent` — Override default agent plugin for this session (e.g. claude, opencode)
- `--detach` — Exit immediately after creating the session; print session-id and chat-id
- `--model` — Agent model id to run this session under (e.g. an Opus id); empty = agent default
- `--no-attach` — Alias for --detach
- `--prompt` — Initial prompt / plan for the session (enables non-interactive mode when combined with --repo)
- `--repo` — Repository id, name, or local path (enables non-interactive mode when combined with --prompt)
- `--title` — Session title (optional, auto-derived from prompt when absent)

```bash
boss new
boss new --agent opencode
# Create a session non-interactively and print its ids
boss new --repo my-repo --prompt "refactor the auth module" --detach
# Ask Codex for a second opinion; capture ids for boss chat wait
boss new --agent codex --repo my-repo --prompt "review this PR for security issues" --detach
```

### `boss rename <session-id> <new-title...>`

Rename a session (updates its title; syncs the linked PR title if any)

### `boss show <session-id>`

Show session details

```bash
boss show abc123
```
