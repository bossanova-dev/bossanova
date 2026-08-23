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

### `boss chats <session-id> [flags]`

List chats in a session with their status and last output time

Lists a session's chats with ID, TITLE, CREATED, STATUS and LAST OUTPUT (`boss show` prints the same table). STATUS is the bare chat-status name (IDLE, WORKING, QUESTION, STOPPED, LIMITED, WAITING, UNSPECIFIED); a WAITING chat also shows its reason. A chat with no cached status reads UNSPECIFIED, which means unknown, not settled; so does a status a newer daemon reports that this build has no name for, so the value is never a bare number. Use --json for one object per chat carrying agent_session_id, title, created_at, status, last_output_at and waiting_reason, with timestamps in RFC3339. No settled/not-settled boolean is emitted -- the threshold belongs to the caller. Mind what last_output_at means: while a chat is WORKING it is the fetch time, so every working chat in one read shares it, and it only freezes at the true last output once the chat is IDLE. Gate on IDLE together with a stale last_output_at; staleness alone proves nothing. If the status read fails -- a daemon too old to serve the call -- table mode prints rows with `?` plus one stderr line and exits 0, while --json exits 1 with {error:{code: CHAT_STATUS_UNAVAILABLE, ...}} and no chats array, so an unavailable status can never be misread as a settled one. (Both --remote and --host return real statuses: --remote proxies the read to the session's owning daemon, --host tunnels to a local one.)

**Flags:**

- `--json` — Emit a stable JSON envelope instead of human-readable text

```bash
boss chats abc123
# Machine-readable rows for a settled-green merge gate
boss chats abc123 --json
```

### `boss ls [flags]`

List sessions (non-interactive)

An extra `AGENT` column appears only when at least one listed session uses an agent that differs from the user's `Settings.DefaultAgent`. In the common single-agent case the column is hidden so the table stays compact.

`--json` emits `{"sessions": [...]}` — never `null`, and never the human "No sessions found." line, so a driver decodes one shape whether or not anything matched. Each row carries `id`, `title`, `state`, `repo_id`, `agent`, `pr_number`, `pr_url`, `branch`, `created_at` and `updated_at`. `state` is the enum NAME with its `SESSION_STATE_` prefix trimmed (`RUNNING`, `READY_FOR_REVIEW`, …), never the numeric value, so a caller is not coupled to proto field ordering; it is the same vocabulary `--state` accepts. `pr_number` is `null` rather than 0 for a session with no PR. Timestamps are RFC3339 in UTC. `--repo`, `--state` and `--archived` filter identically with and without `--json` — the flag changes rendering only, never the query.

**Flags:**

- `--archived` — Include archived sessions
- `--json` — Emit a stable JSON schema instead of a table
- `--repo` — Filter by repo ID
- `--state` — Filter by state(s)

```bash
boss ls
boss ls --repo my-repo --state running,paused
boss ls --archived
boss ls --json | jq -r '.sessions[] | select(.state=="READY_FOR_REVIEW") | .pr_url'
```

### `boss merge <session-id> [flags]`

Merge a session's pull request (or its local-only branch)

Merges the session's pull request through the daemon, which owns the merge gate, the per-repo merge serialization, and the merge-strategy resolution. A session with no PR takes the local-only-branch merge path. Prompts for confirmation unless -y/--yes is given; when the gate refuses, the command exits non-zero with the daemon's `merge blocked: gate=<slug>` message naming the gate that stopped it. Use --json for a machine-readable envelope: success prints {session, pr, detail}, failure prints {error:{code, connect_code, message}} on stdout with a stable `code` such as MERGE_STRATEGY_INCOMPATIBLE, FAILED_PRECONDITION or NOT_FOUND, so a driver can branch on the outcome without matching message text. Every failure still exits 1; the code, not the exit status, is the discriminator.

**Flags:**

- `--json` — Emit a stable JSON envelope instead of human-readable text (requires --yes)
- `--yes`, `-y` — Skip confirmation prompt

```bash
boss merge abc123
# Skip the confirmation prompt (unattended callers)
boss merge abc123 --yes
# Machine-readable envelope; --json requires --yes
boss merge abc123 --yes --json
```

### `boss new [flags]`

Create a new coding session

Launches the interactive session creation flow. When both --repo and --prompt are provided the command runs non-interactively: it creates the session, prints the session-id to stdout as soon as the session exists, prints chat-id later if the daemon provides one, streams setup progress to stderr until setup settles, then exits. Combine with --detach (implicit when both flags are set) for scripting. Use --agent to override the default agent plugin.

Launching a session to run work unattended: supplying a prompt launches the agent headlessly so the work actually runs — that is the path's own behaviour, not something --detach causes. The CLI and the MCP `create_session` tool now share this default. `create_session` applies the same rule: a prompt-carrying call defaults to headless and reports agent_launched=true, while attended:true creates the session idle awaiting a human `boss attach` (agent_launched=false). Prefer the default for programmatic/unattended launches.

--detach vs --tmux-unattended: they are NOT alternatives. The non-interactive --repo + --prompt path always detaches, so --detach is a no-op there and only affects flag parsing; it governs whether this command attaches a chat pane before it exits, never how or where the session is hosted. --tmux-unattended is the distinct durable-pane option: it hosts the session in a tmux pane that survives a daemon restart and is attach-safe, which is what a child session that must outlive the daemon needs. Both can be set at once, and the daemon carries them as independent fields.

Tracker binding: --tracker-id, --tracker-source (linear or sentry) and --tracker-url bind the session to an external issue, which is what the daemon's tracker-id dedup keys on — more robust than the `[<TICKET>] <title>` title convention, which silently duplicates a session when the title drifts. Each flag is independent and an omitted one leaves its field unset; an unrecognised --tracker-source is rejected before any session is created.

Use --json on the non-interactive path for a machine-readable envelope instead of the two-line output: success prints {session:{id, title, chat_id}} on stdout, failure prints {error:{code, connect_code, message}} with a stable `code` such as INVALID_ARGUMENT or NOT_FOUND, and every failure still exits 1. Setup output goes to stderr either way, so stdout carries exactly one JSON object. Without --json the two-line `session-id:` / `chat-id:` output is unchanged; session-id appears as soon as the session exists, and chat-id appears later if the daemon provides one.

**Flags:**

- `--account` — Account id or label to run this session under (empty = system default)
- `--agent` — Override default agent plugin for this session (e.g. claude, opencode)
- `--defer-pr` — Open no draft PR up front; a PR is opened at finalize only if the run produced commits. For runs not expected to change the repository. Pair with --tmux-unattended so a restart cannot strand commits before finalize. Non-interactive --repo + --prompt path only
- `--detach` — A no-op on the non-interactive --repo + --prompt path, which always runs headlessly, prints session-id as soon as the session exists, prints chat-id later if the daemon provides one, and streams setup progress on stderr; --tmux-unattended is the distinct durable-pane option
- `--json` — Emit the created session as a stable JSON schema instead of the two-line output
- `--model` — Agent model id to run this session under (e.g. an Opus id); empty = agent default
- `--no-attach` — Alias for --detach
- `--prompt` — Initial prompt / plan for the session (enables non-interactive mode when combined with --repo)
- `--quick-chat` — Create a session with no worktree, branch, or PR, in the repository checkout. The agent starts when you attach; unattended runs want --defer-pr. Mutually exclusive with --defer-pr. Non-interactive --repo + --prompt path only
- `--repo` — Repository id, name, or local path (enables non-interactive mode when combined with --prompt)
- `--title` — Session title (optional, auto-derived from prompt when absent)
- `--tmux-unattended` — Host the session in a durable tmux pane that survives a daemon restart and is attach-safe (independent of --detach, which only governs whether this command attaches a chat pane before it exits)
- `--tracker-id` — External issue id to bind this session to (e.g. PROJ-42)
- `--tracker-source` — External issue tracker: linear or sentry
- `--tracker-url` — URL of the external issue this session is bound to

```bash
boss new
boss new --agent opencode
# Create a session non-interactively and print its ids
boss new --repo my-repo --prompt "refactor the auth module" --detach
# Ask Codex for a second opinion; capture ids for boss chat wait
boss new --agent codex --repo my-repo --prompt "review this PR for security issues" --detach
# Launch a durable tracker-bound child and parse {session:{id, chat_id}}
boss new --repo my-repo --prompt "/boss-build PROJ-42" --tmux-unattended --tracker-id PROJ-42 --tracker-source linear --json
# Capture the chat id without parsing the human two-line output
CHAT=$(boss new --repo my-repo --prompt "/boss-build PROJ-42" --json | jq -r .session.chat_id)
```

### `boss rename <session-id> <new-title...>`

Rename a session (updates its title; syncs the linked PR title if any)

### `boss show <session-id> [flags]`

Show session details

Shows one session's details, then its chats in the same table `boss chats` prints -- ID, TITLE, CREATED, STATUS and LAST OUTPUT, with the same reading of UNSPECIFIED and of last_output_at. If the status read fails the rows still print, STATUS and LAST OUTPUT read `?`, one stderr line explains why, and the command exits 0.

`--json` emits `{"session": {...}}`: every field of a `boss ls --json` row, plus the detail the text rendering prints — `repo_display_name`, `base_branch`, `worktree_path`, `account_id`, `account_label`, `display_status`, `last_check_state`, `archived_at`, `setup_error` and `last_repair` (an object, or `null` when the repair plugin has never run for this session). `display_status` uses the same vocabulary as the TUI and `boss session checks` (`idle`, `checking`, `passing`, `failing`, …). The chats table the text rendering prints below the header is deliberately NOT in this envelope — `boss chats` owns that shape, because it has to join in per-chat status, so drive a machine gate off `boss chats --json`. An unknown session id exits 1 with the shared JSON error envelope on stdout.

**Flags:**

- `--json` — Emit a stable JSON schema instead of text

```bash
boss show abc123
boss show abc123 --json
```
