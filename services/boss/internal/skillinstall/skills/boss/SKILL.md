---
name: boss
description: Complete reference for all boss CLI commands. Use to run boss operations from within a Claude Code session.
---

# Boss CLI Reference

Boss manages Claude coding sessions across git worktrees with automatic PR creation, CI fix loops, and code review handling.

The command reference below is generated from the `boss` CLI by `make gen-skill`. Do not edit the region between the markers by hand — change the CLI (or the prose registry in `lib/bossalib/clidoc`) and regenerate.

<!-- BEGIN GENERATED: boss command reference — run `make gen-skill` -->

## Global Flags

### `--remote`

Connect to orchestrator URL instead of local daemon

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

## Chat Control

### `boss chat`

Interact with session chats headlessly

### `boss chat send <session-id|chat-id> <message> [flags]`

Send a message to a chat

Delivers a follow-up message to a running chat identified by a session id or agent_session_id (the chat-id printed by `boss new --detach`). When given a session id, boss targets that session's primary chat. The daemon wakes a sleeping chat before pasting the message.

**Flags:**

- `--submit` — Submit the message (press Enter and verify); false prefills the composer without submitting (default: true)

```bash
boss chat send <session-id|chat-id> "please also add tests"
```

### `boss chat show <session-id|chat-id> [flags]`

Print a chat transcript

Prints the full conversation transcript for a chat or session's primary chat. Use --result-only to print just the final assistant response text (suitable for scripting). Use --limit to cap the number of messages.

**Flags:**

- `--limit` — Maximum number of messages to show (0 = all) (default: 0)
- `--result-only` — Print only the final assistant result text

```bash
boss chat show <session-id|chat-id>
boss chat show <session-id|chat-id> --result-only
boss chat show <session-id|chat-id> --limit 10
```

### `boss chat wait <session-id|chat-id> [flags]`

Wait for a chat to become idle, then print the result

Blocks until the chat identified by a session id or agent_session_id becomes idle or is waiting for input, then prints the final assistant result. Polls chat status every few seconds. Use --timeout to limit wait time. Typical recipe: `boss new --agent codex --repo R --prompt P --detach` then `boss chat wait <session-id|chat-id>` to collect the result.

**Flags:**

- `--timeout` — Maximum time to wait (e.g. 5m, 1h) (default: 30m0s)

```bash
boss chat wait <session-id|chat-id>
boss chat wait <session-id|chat-id> --timeout 10m
# Full cross-agent second-opinion recipe
CHAT=$(boss new --agent codex --repo my-repo --prompt "second opinion on PR #42" --detach | awk '/^chat-id/{print $2}') && boss chat wait $CHAT
```

## Repository Management

### `boss repo`

Manage repositories

### `boss repo add`

Register a repository

```bash
boss repo add
```

### `boss repo ls`

List registered repositories

```bash
boss repo ls
```

### `boss repo remove <repo-id>`

Remove a registered repository

```bash
boss repo remove my-repo
```

### `boss repo update <repo-id> [flags]`

Update repository settings

**Flags:**

- `--auto-merge` — Enable auto-merge
- `--auto-merge-dependabot` — Enable auto-merge for Dependabot PRs
- `--auto-repair` — Enable automatic repair (failing checks, conflicts, review feedback)
- `--delete-branches` — Enable deleting safe local branches after archiving
- `--keep-branches-current` — Enable proactively rebasing in-flight session branches when the base advances
- `--merge-strategy` — Set merge strategy (merge, rebase, squash)
- `--name` — Set display name
- `--no-auto-merge` — Disable auto-merge
- `--no-auto-merge-dependabot` — Disable auto-merge for Dependabot PRs
- `--no-auto-repair` — Disable automatic repair
- `--no-delete-branches` — Disable deleting local branches after archiving
- `--no-keep-branches-current` — Disable proactively rebasing in-flight session branches when the base advances
- `--setup-script` — Set setup script (empty string to clear)

```bash
boss repo update my-repo --name "My Repo" --merge-strategy squash
boss repo update my-repo --auto-merge-dependabot
```

## Cron Jobs

### `boss cron`

Manage scheduled cron jobs

### `boss cron add [flags]`

Create a cron job

**Flags:**

- `--agent` — Agent runner plugin name (empty = claude)
- `--enabled` — Whether the job is enabled (default: true)
- `--gate` — Gate command run before each fire (empty = no gate)
- `--model` — Agent model id (empty = plugin default)
- `--name` — Job name (required)
- `--prompt` — Prompt / plan for each run
- `--prompt-file` — Read the prompt from a file (or '-' for stdin)
- `--repo` — Repository ID (required)
- `--run-setup` — Run the repo setup script before the agent (default: true)
- `--schedule` — 5-field cron expression or @daily/@hourly/etc (required)
- `--tz` — IANA timezone name (empty = daemon-local)
- `--zero-output` — Run with no worktree, branch, or PR (for jobs that change nothing in this repo)

### `boss cron disable <cron-id>`

Disable a cron job

### `boss cron enable <cron-id>`

Enable a cron job

### `boss cron ls [flags]`

List cron jobs

**Flags:**

- `--json` — Emit a stable JSON schema instead of a table
- `--repo` — Filter by repo ID

### `boss cron remove <cron-id>`

Remove a cron job

### `boss cron run-now <cron-id>`

Fire a cron job immediately

### `boss cron show <cron-id> [flags]`

Show cron job details

**Flags:**

- `--json` — Emit a stable JSON schema instead of text

### `boss cron update <cron-id> [flags]`

Update cron job settings

**Flags:**

- `--agent` — Set the agent runner plugin name
- `--enabled` — Enable or disable the job (unset preserves current)
- `--gate` — Set the gate command (empty string clears it)
- `--model` — Set the agent model id (empty string clears it)
- `--name` — Set job name
- `--prompt` — Set the prompt / plan
- `--prompt-file` — Read a new prompt from a file (or '-' for stdin)
- `--run-setup` — Run the repo setup script before the agent (unset preserves current)
- `--schedule` — Set the cron schedule
- `--tz` — Set the IANA timezone (empty string clears it)
- `--zero-output` — Run with no worktree, branch, or PR (unset preserves current)

## GitHub Callbacks

### `boss callback`

Manage GitHub PR callbacks (durable one-shot event notifications)

A GitHub callback is a durable, one-shot notification: it fires a prompt into a chat once a pull request reaches a chosen state, then retires. Use it to answer natural-language asks like "tell me when PR #123 is merged", "ping this chat when PR #123 goes green", "let me know if PR #123's checks fail", "notify me when PR #123 is closed", "tell me when PR #123 comes out of draft", or "ping me when PR #123 is green and ready to merge". Triggers map to those phrasings: `merged`, `checks_passed` (green), `checks_failed` (red), `closed`, `ready_for_review` (the draft→ready flip), and `checks_passed_ready` (green and not a draft — the merge-eligibility moment). Triggers are evaluated on PR state, not on transitions: a callback armed on a PR that ALREADY satisfies its trigger fires on the next evaluation rather than waiting for a fresh event. Delivery only signals that the event fired — always verify the PR's actual state before acting on it. Callbacks expire after 24h by default and may not outlive 30 days.

### `boss callback add <pr> <trigger> [flags]`

Register a callback for a pull request event

Register a one-shot callback. `<pr>` is a bare PR number (resolved against the current repository) or a full `https://github.com/owner/repo/pull/N` URL. `<trigger>` is one of `merged`, `closed`, `checks_passed`, `checks_failed`, `ready_for_review` (the draft→ready flip), or `checks_passed_ready` (green and not a draft — merge-eligible). Triggers match on PR state, not on transitions, so arming one against a PR that already satisfies it fires on the next evaluation. The `--message` prompt is delivered verbatim to the target chat when the callback fires and is treated as a secret — it is never echoed back on any surface. Expiry defaults to 24h and may not exceed 30 days.

**Flags:**

- `--chat` — Target agent-session (chat) id to notify (default: $BOSS_AGENT_SESSION_ID)
- `--expires-in` — Expiry as a duration (e.g. 30m, 24h, 7d, 2w); default 24h, max 30d
- `--group` — Optional group id; siblings in a group cancel each other on first fire
- `--json` — Emit the created callback as a stable JSON schema
- `--message` — Prompt delivered to the chat when the callback fires (required)
- `--repo` — Repository as owner/repo (default: the current repository's origin)

```bash
# "tell me when PR #123 is merged"
boss callback add 123 merged --message "PR #123 merged — pull main and redeploy"
# "ping this chat when PR #123 goes green"
boss callback add 123 checks_passed --message "PR #123 is green — start the release"
# "let me know if PR #123's checks fail"
boss callback add 123 checks_failed --message "PR #123 is red — investigate the failing checks"
# "notify me when PR #123 is closed" (full URL, longer expiry)
boss callback add https://github.com/acme/widget/pull/123 closed --message "PR #123 was closed" --expires-in 7d
# "tell me when PR #123 comes out of draft"
boss callback add 123 ready_for_review --message "PR #123 left draft — review it"
# "ping me when PR #123 is green and ready to merge"
boss callback add 123 checks_passed_ready --message "PR #123 is green and ready to merge"
```

### `boss callback list [flags]`

Alias: `boss callback ls`

List registered GitHub callbacks

**Flags:**

- `--chat` — Filter by target agent-session (chat) id
- `--json` — Emit a stable JSON schema instead of a table
- `--repo` — Filter by repository as owner/repo
- `--state` — Filter by state (active, leased, triggered, delivered, canceled, expired)
- `--trigger` — Filter by trigger (merged, closed, checks_passed, checks_failed, ready_for_review, checks_passed_ready)

```bash
boss callback list
boss callback list --repo acme/widget --trigger merged
boss callback list --json
```

### `boss callback remove <callback-id> [flags]`

Alias: `boss callback rm`

Remove a GitHub callback by id

**Flags:**

- `--chat` — Owning chat id for remote routing (default: $BOSS_AGENT_SESSION_ID; ignored locally)

```bash
boss callback remove cb_abc123
```

## Broadcasts

### `boss broadcast`

Send messages to the chats a selector resolves to

### `boss broadcast list [flags]`

Alias: `boss broadcast ls`

List recent broadcasts

**Flags:**

- `--chat` — Filter to broadcasts addressed to this target chat id
- `--json` — Emit a stable JSON schema instead of a table
- `--limit` — Cap the number of broadcasts returned (0 = unlimited) (default: 0)
- `--origin` — Filter by originating chat id
- `--state` — Filter by broadcast state

### `boss broadcast remove <broadcast-id>`

Alias: `boss broadcast rm`

Remove a broadcast and its deliveries by id

### `boss broadcast send [flags]`

Send a broadcast to the audience a selector resolves to

**Flags:**

- `--cross-daemon` — Ask bosso to route this broadcast to other daemons too, not just this daemon's own chats; bosso fans it out to the tenant's other live daemons and each re-resolves the selector against its own chats. Best-effort: a daemon offline at fan-out time never receives it, and past 32 other daemons the fan-out is refused rather than truncated. Pair it with a repo:/agent:/chat: selector — a daemon:<id> term matches no chats on any daemon, because chat rows carry an empty daemon id
- `--expires-in` — How long to keep retrying, as a duration (e.g. 30m, 24h, 7d); default 24h, max 30d
- `--from` — Originating chat id (default: $BOSS_AGENT_SESSION_ID; empty is allowed)
- `--include-origin` — Deliver to the origin chat too instead of excluding it from its own audience
- `--json` — Emit the sent broadcast as a stable JSON schema
- `--message` — Prompt delivered to each target chat; - reads it from stdin (required)
- `--to` — Selector resolving the audience, e.g. repo:<id>,agent:claude (required)

### `boss broadcast subscribe [flags]`

Register a standing rule that broadcasts when a session settles

**Flags:**

- `--expires-in` — How long the rule stands, as a duration (e.g. 30m, 24h, 7d); default 24h, max 30d
- `--from` — Registering chat id (default: $BOSS_AGENT_SESSION_ID; provenance only)
- `--json` — Emit the created subscription as a stable JSON schema
- `--message` — Prompt broadcast when it fires; - reads it from stdin (required)
- `--on` — Outcome to wait for: completed, errored, settled (required)
- `--session` — Session whose outcome fires it (default: $BOSS_SESSION_ID)
- `--to` — Selector resolving the audience at fire time (required)

### `boss broadcast subscriptions [flags]`

List standing broadcast subscriptions

**Flags:**

- `--json` — Emit a stable JSON schema instead of a table
- `--limit` — Cap the number of subscriptions returned (0 = unlimited) (default: 0)
- `--on` — Filter by trigger (completed, errored, settled)
- `--session` — Filter by owning session id
- `--state` — Filter by state (active, fired, canceled, expired)

### `boss broadcast unsubscribe <subscription-id>`

Retire a standing broadcast subscription by id

## Notes

### `boss notes`

Record and search repo-scoped notes

A note is durable free-text recorded against a REPOSITORY so a later sweep can harvest what a run learned — a gotcha, a decision, a piece of tech debt worth filing. Notes are repo-scoped and session and chat are provenance ONLY: they record who wrote the note, and archiving or removing that session never removes its notes. A note outlives the run that wrote it. Inside a registered repo or a session worktree the repo and session default from the working directory, so an agent can leave a note with one command and no ids to look up. A body is REQUIRED (a blank or whitespace-only one is rejected), may be up to 64 KiB, and is stored verbatim. Tags are normalised — trimmed, lowercased and de-duplicated — so `Tech-Debt` and `tech-debt` are one tag; a note may carry up to 32 tags of 64 bytes each. Notes are listed OLDEST first. `add`, `ls`, `show` and `edit` all take `--json` for machine parsing.

### `boss notes add <body> [flags]`

Record a note against a repository

Record a note. `--repo`, `--session` and `--chat` are resolved in this order: the explicit flag, then the ambient `BOSS_REPO_ID` / `BOSS_SESSION_ID` / `BOSS_AGENT_SESSION_ID`, then — for the repo and session only — the daemon's resolution of the working directory. An agent running inside its own session worktree therefore needs no ids at all. There is no working-directory fallback for the chat: a session's primary chat is not necessarily the one calling, so guessing would attribute the note to the wrong agent — export `BOSS_AGENT_SESSION_ID` or pass `--chat` if the chat matters. When the repository cannot be resolved the command FAILS naming `--repo` rather than writing the note against the wrong repo. `--tag` is repeatable (`--tag a --tag b`), not comma-separated; tags are trimmed, lowercased and de-duplicated before they are stored.

**Flags:**

- `--chat` — Chat provenance (default: $BOSS_AGENT_SESSION_ID)
- `--json` — Emit the created note as a stable JSON schema
- `--repo` — Owning repository id (default: $BOSS_REPO_ID, else the working directory's repo)
- `--session` — Session provenance (default: $BOSS_SESSION_ID, else the working directory's session)
- `--tag` — Tag to attach; repeat for several (normalised to lowercase)

```bash
# From inside a session worktree: repo and session are inferred, no ids needed
boss notes add "the flaky test is a socket-token race" --tag tech-debt
# Repeat --tag for several tags
boss notes add "auth middleware assumes a trailing slash" --tag gotcha --tag auth
# Record against an explicit repo from anywhere, and parse the result
boss notes add "release checklist step 3 is stale" --repo my-repo --json
```

### `boss notes edit <note-id> [flags]`

Change a note's body and/or tags

Change a note's body and/or tags; pass at least one of `--body` and `--tag` or the command fails with nothing to do. An omitted `--body` leaves the body alone and an omitted `--tag` leaves the tags alone. Passing `--tag` REPLACES the whole tag set with exactly what you pass — it does not merge, so re-list every tag the note should keep. `--tag ""` therefore clears every tag.

**Flags:**

- `--body` — Replacement body (omit to leave the body unchanged)
- `--json` — Emit the updated note as a stable JSON schema
- `--repo` — Owning repository id for remote routing (default: $BOSS_REPO_ID, else the working directory's repo; ignored locally)
- `--tag` — Tag for the REPLACEMENT set — the whole tag set is replaced, not merged; repeat for several, omit to leave tags unchanged

```bash
# Rewrite the body, leaving the tags untouched
boss notes edit abc123 --body "the flaky test is a socket-token race; fixed in #1712"
# REPLACES the tag set with exactly these two tags
boss notes edit abc123 --tag tech-debt --tag resolved
# Clear every tag
boss notes edit abc123 --tag ""
```

### `boss notes ls [flags]`

List notes, oldest first

List notes in the order they were recorded, OLDEST first, so `--limit N` returns the N oldest. `--repo` resolves like `add`'s: the explicit flag, then the ambient `BOSS_REPO_ID`, then the working directory's repo — so inside a repo the listing is scoped to it. To list across EVERY repo pass `--repo ""` explicitly; simply leaving the repo directory is NOT enough, because a boss-managed agent pane always exports `BOSS_REPO_ID`. `--tag` matches ANY of the tags given (a note carrying just one of them matches), not all of them; unlike on `add`/`edit`, `--tag ""` here is not a wildcard — the daemon fails closed on a tag that normalises away, so it matches nothing. `--search` matches a substring of the body, case-insensitively for ASCII only (the daemon folds case with SQLite's `LOWER()`, which does not fold non-ASCII); SQL wildcards are matched literally. `--session` filters by the session that recorded the note and does NOT default from the working directory — a session-scoped default would silently hide the repo's other notes. Bodies are flattened to one line and truncated in the table: use `boss notes show` for the full text.

**Flags:**

- `--json` — Emit a stable JSON schema instead of a table
- `--limit` — Cap the number of notes returned (0 = unlimited) (default: 0)
- `--repo` — Filter by repository id (default: $BOSS_REPO_ID, else the working directory's repo; pass --repo "" for every repo)
- `--search` — Filter to notes whose body contains this substring
- `--session` — Filter by the session that recorded the note
- `--tag` — Filter to notes carrying any of these tags; repeat for several

```bash
boss notes ls
# Notes carrying EITHER tag (any-of, not all-of)
boss notes ls --tag tech-debt --tag gotcha
# The 5 oldest notes whose body contains the term
boss notes ls --search "socket token" --limit 5
boss notes ls --repo my-repo --json
# Every repo, even from inside a session pane that exports BOSS_REPO_ID
boss notes ls --repo ""
```

### `boss notes rm <note-id> [flags]`

Remove a note by id

Remove a note by id. Removal is idempotent: removing a note that is already gone succeeds rather than erroring, so a cleanup script can be re-run safely. Removing a note is permanent — there is no trash for notes.

**Flags:**

- `--repo` — Owning repository id for remote routing (default: $BOSS_REPO_ID, else the working directory's repo; ignored locally)

```bash
boss notes rm abc123
```

### `boss notes show <note-id> [flags]`

Show one note in full

Print one note in full: its ids, provenance, tags, timestamps, and then the body verbatim and untruncated (`boss notes ls` only shows a one-line preview). `--repo` is a routing hint for a remote daemon and is ignored locally — the note is resolved by id.

**Flags:**

- `--json` — Emit the note as a stable JSON schema
- `--repo` — Owning repository id for remote routing (default: $BOSS_REPO_ID, else the working directory's repo; ignored locally)

```bash
boss notes show abc123
boss notes show abc123 --json
```

## Account Management

### `boss account`

Manage agent accounts (credential registry)

### `boss account add [provider] [flags]`

Register an agent account

**Flags:**

- `--credential-file` — Read the credential from a file (or '-' for stdin); preferred over --token
- `--label` — Human label, unique per provider (required for the non-interactive flag path)
- `--priority` — Sort order; lower = preferred (default: 0)
- `--provider` — Account provider (claude|codex) (or pass as a positional arg)
- `--timeout` — Deadline for an interactive registration walkthrough (default: 10m0s)
- `--token` — Credential token (prefer --credential-file - or stdin to keep it out of shell history)
- `--token-stdin` — claude only: read the setup token from stdin instead of running the walkthrough

### `boss account ls [flags]`

List accounts

**Flags:**

- `--json` — Emit a stable JSON schema instead of a table
- `--provider` — Filter by provider (claude|codex)
- `--refresh` — Force a live usage probe of each account before listing

### `boss account refresh <account-id> [flags]`

Replace an account's stored credential

**Flags:**

- `--credential-file` — Read the credential from a file (or '-' for stdin); preferred over --token
- `--json` — Emit a stable JSON schema instead of text
- `--test` — Validate the refreshed credential after saving
- `--token` — Credential token (prefer --credential-file - or stdin to keep it out of shell history)

### `boss account remove <account-id>`

Remove an account and its stored credential

### `boss account switch <session> <account> [flags]`

Stop a session's live chat, rebind it to the chosen account, and resume

**Flags:**

- `--chat` — Target a specific agent chat (agent session id); default: the session's primary live chat
- `--force` — Interrupt a mid-turn / WORKING chat

### `boss account test <account-id> [flags]`

Validate an account's credential and record the outcome

**Flags:**

- `--json` — Emit a stable JSON schema instead of text

### `boss account update <account-id> [flags]`

Update account metadata

**Flags:**

- `--allowed-models` — Replace the allowed-models set (comma-separated)
- `--label` — Set the label
- `--priority` — Set the priority (lower = preferred) (default: 0)
- `--status` — Set the status (active|disabled)

## Trash Management

### `boss trash`

Manage archived sessions

### `boss trash delete <session-id> [flags]`

Permanently delete an archived session

**Flags:**

- `--yes`, `-y` — Skip confirmation prompt

```bash
boss trash delete abc123
boss trash delete abc123 --yes
```

### `boss trash empty [flags]`

Permanently delete all archived sessions

**Flags:**

- `--older-than` — Only delete sessions archived longer than this duration (e.g. 30d)

```bash
boss trash empty
boss trash empty --older-than 30d
```

### `boss trash ls`

List archived sessions

```bash
boss trash ls
```

### `boss trash restore <session-id>`

Restore an archived session

Restores an archived session, recreating its worktree.

```bash
boss trash restore abc123
```

## Daemon Management

### `boss daemon`

Manage the bossd daemon

### `boss daemon install [flags]`

Install bossd as a background service (launchd on macOS, systemd on Linux)

**Flags:**

- `--force` — Overwrite existing service file

```bash
boss daemon install
boss daemon install --force
```

### `boss daemon restart`

Restart the bossd daemon

Restarts the bossd daemon via the platform service manager. Errors out if the daemon isn't installed.

```bash
boss daemon restart
```

### `boss daemon rotate-token`

Rotate the daemon socket auth token (regenerated on next daemon start)

### `boss daemon start`

Start the bossd daemon

No-op if it's already running. Falls back to spawning bossd directly if it isn't installed as a LaunchAgent.

```bash
boss daemon start
```

### `boss daemon status`

Show bossd daemon status

```bash
boss daemon status
```

### `boss daemon stop [flags]`

Stop the bossd daemon

Stops the bossd daemon for the current profile via the platform service manager or profile metadata. Idempotent — quietly succeeds if the daemon is already stopped or not installed. Normal stops leave plugin processes alone — bossd reaps its own children. Use `--all-standalone` only for explicit cleanup of every user-owned standalone bossd process and its `bossd-plugin-*` children across profiles.

**Flags:**

- `--all-standalone` — Stop all user-owned bossd processes instead of only the current profile

```bash
boss daemon stop
boss daemon stop --all-standalone
```

### `boss daemon uninstall`

Uninstall the bossd LaunchAgent

```bash
boss daemon uninstall
```

## MCP Server

### `boss mcp`

Manage the local MCP server

Manages the local MCP server, which exposes the boss operations as MCP tools over Streamable HTTP for MCP-aware hosts. It runs as an auto-starting service (launchd on macOS, systemd on Linux) and proxies through the local bossd daemon's Unix socket.

### `boss mcp install [flags]`

Install the MCP server as an auto-starting service

Installs the MCP server as an auto-starting service and starts it. Use `--force` to overwrite an existing service file, and `--port` to change the loopback port (default 8765). The server listens on `http://127.0.0.1:<port>/mcp`.

**Flags:**

- `--force` — Overwrite existing service file
- `--port` — Loopback port for the MCP HTTP server (default: 8765)

```bash
boss mcp install
boss mcp install --force
boss mcp install --port 8888
```

### `boss mcp start`

Start the MCP server

```bash
boss mcp start
```

### `boss mcp status`

Show MCP server status

```bash
boss mcp status
```

### `boss mcp stop`

Stop the MCP server

Stops the running MCP server, leaving its service file in place. Idempotent.

```bash
boss mcp stop
```

### `boss mcp uninstall`

Uninstall the MCP server service

```bash
boss mcp uninstall
```

## Skills

### `boss skills`

Manage installed boss skills

### `boss skills check [flags]`

Check installed boss skills against this binary and checkout sources

**Flags:**

- `--agent` — Restrict to one agent: claude or codex (default: all on PATH)

### `boss skills install [flags]`

Install or refresh boss skills (fresh-installs missing trees); --force reinstalls even when current

**Flags:**

- `--agent` — Restrict to one agent: claude or codex (default: all on PATH)
- `--force` — Reinstall (Extract) unconditionally, even when current

### `boss skills sync [flags]`

Refresh installed boss skills to match this binary (update-only, no prompt)

**Flags:**

- `--agent` — Restrict to one agent: claude or codex (default: all on PATH)

## Settings & Auth

### `boss auth-status`

Show authentication status

```bash
boss auth-status
```

### `boss config`

Manage configuration

### `boss config init [flags]`

Initialize plugin configuration from a directory

**Flags:**

- `--plugin-dir` — Directory containing plugin binaries (auto-detected if omitted)

```bash
boss config init
boss config init --plugin-dir ./plugins
```

### `boss login`

Log in to Bossanova cloud (WorkOS)

```bash
boss login
```

### `boss logout`

Log out and remove stored credentials

```bash
boss logout
```

### `boss settings [flags]`

View or update global settings

**Flags:**

- `--default-agent` — Set the default agent plugin (e.g. claude, opencode)
- `--managed-accounts` — Enable managed accounts (bossd credential rotation)
- `--no-managed-accounts` — Disable managed accounts (use the terminal's own login)
- `--no-skip-permissions` — Disable Claude --dangerously-skip-permissions
- `--poll-interval` — Set poll interval in seconds (0 = default) (default: 0)
- `--skip-permissions` — Enable Claude --dangerously-skip-permissions
- `--worktree-dir` — Set worktree base directory

```bash
boss settings
boss settings --worktree-dir ~/work/bossanova/worktrees
boss settings --skip-permissions
```

## Diagnostics

### `boss env [flags]`

Report this session's boss context and the full CLI + MCP capability inventory

**Flags:**

- `--json` — Emit a stable JSON schema instead of human-readable text

### `boss proof`

Provision credentials for the proof pipeline

### `boss proof set-secret [proof-anthropic-api-key|proof-cloudflare-api-token] [flags]`

Store a proof secret in the keyring, read from stdin

**Flags:**

- `--check` — Report which proof secrets are set (never prints values) and exit

### `boss repair`

Auto-repair plugin operations

### `boss repair doctor`

Health-check the auto-repair pipeline (plugin loaded, claude on PATH, recent logs, etc.)

Health-checks the auto-repair pipeline. Calls the daemon's `RepairDoctor` RPC and renders a checklist (plugin loaded, `claude` on PATH, recent log files, etc.) plus a recent-logs table — answers "is auto-repair healthy?" without having to grep daemon stderr.

```bash
boss repair doctor
```

### `boss repair start`

(Re-)arm the auto-repair workflow (e.g. after the repair plugin was stopped or restarted)

(Re-)arms the auto-repair workflow. Calls the daemon's `StartRepairWorkflow` RPC, which declares the repair plugin's desired-started state from current settings and ensures the workflow is running. A RUNNING workflow is left untouched (never restarted); a PAUSED one is left for the operator to resume. Use after the repair plugin was stopped or restarted and auto-repair is sitting disarmed — no bossd restart needed.

```bash
boss repair start
```

### `boss session`

Session diagnostics

### `boss session checks <session-id> [flags]`

Show what bossd's display poller saw for this session's CI checks

Shows bossd's persisted view of a session's CI check snapshots, alongside the `DisplayStatus` the daemon computed for each one. Useful when reconciling "why did the TUI think this PR was passing when GitHub says failing?".

**Flags:**

- `--limit` — Number of snapshots to show (newest first) (default: 5)

```bash
boss session checks abc123
boss session checks abc123 --limit 10
```

### `boss session link-pr <session-id> <pr-number-or-url>`

Attach an existing pull request to a session

Attach an existing GitHub PR to a session. Use this to repair cron sessions where the agent already committed, pushed, and opened a PR before bossd finalized the run.

```bash
boss session link-pr abc123 477
boss session link-pr abc123 https://github.com/owner/repo/pull/477
```

## Plugins

### `boss plugin`

Inspect installed plugins

### `boss plugin list`

Alias: `boss plugin ls`

List plugins the daemon attempted to load this run

```bash
boss plugin list
boss plugin ls
```

## Other

### `boss upgrade [flags]`

Check for and install Bossanova upgrades

**Flags:**

- `--check` — check for an upgrade without installing
- `--no-restart` — do not restart the daemon after upgrade
- `--version` — install a specific stable release tag (prereleases are not supported)
- `--yes` — install without interactive confirmation

```bash
boss upgrade --check
boss upgrade --yes
boss upgrade --version v1.2.4 --yes
boss upgrade --yes --no-restart
```

### `boss version`

Print version information

```bash
boss version
```

<!-- END GENERATED -->

## Launching a session to run work unattended

Supplying a prompt when you create a session means you want that work to **run**, not sit idle. Both surfaces default to launching the agent so the work actually starts:

- **CLI:** `boss new --repo R --prompt P` runs non-interactively — `--detach` is implicit when `--repo` and `--prompt` are both given.
- **MCP:** a `create_session` call with a non-empty `prompt` and no `attended` opt-in defaults to headless (equivalent to `detach:true`). The result reports `agent_launched: true`.

Opt into an idle session (created but no agent started, awaiting a human attach) only when a human will drive it interactively: pass `attended: true` to `create_session`. That result reports `agent_launched: false` and carries a `next_action` hint.

**Post-launch verification (MCP).** After `create_session`, check `agent_launched`:

- `agent_launched: true` — an agent started; the session's `agent_session_id` is its primary chat. Address it with `send_chat_message` / `get_chat_transcript`.
- `agent_launched: false` — no agent ran. Either you passed `attended: true`, or the create was prompt-less, or the daemon attached to a pre-existing session (`attached_existing: true`, in which case your prompt was NOT run). To start work: call `start_chat` on the session, or re-create with `detach: true` (headless) or `tmux_unattended: true` (durable pane). Follow the `next_action` / `note` hint in the result.

Canonical recipe — spawn a session to run a task unattended and collect the result:

```
create_session { repo_id: R, prompt: P }          # defaults to headless; expect agent_launched: true
# then, using the returned agent_session_id:
get_chat_transcript { agent_session_id: <id> }     # read progress / final result
```

If a `create_session` result comes back with `agent_launched: false` when you expected the work to run, you (or a caller) opted into attended mode — start it with `start_chat`, or re-create with `detach: true` / `tmux_unattended: true`.

## Cron repair workflow

Cron repair example — list the stalled sessions, then link the existing PR:

```bash
boss ls --state finalizing,blocked
boss session link-pr b4764f1684e33742 477
```

## Linking a PR and titling your session

When a session was started without a PR (e.g. a cron-triggered session that committed and pushed before `bossd` finalized), use this flow — your session id is in your system prompt:

1. Create the PR yourself: `gh pr create ...` (or let `boss-finalize` open it).
2. Link it: `boss session link-pr <session-id> <pr-number-or-url>`
3. Title the session: `boss rename <session-id> <new title>`

Note that `boss rename` also best-effort renames the linked GitHub PR.

```bash
boss session link-pr <session-id> 477
boss rename <session-id> Fix flaky login test
```

## GitHub PR callbacks

A GitHub callback is a durable, one-shot notification that fires a prompt into a chat when a pull request reaches a chosen state, then retires. Reach for one whenever a request maps to "let me know when this PR does X":

- "tell me when PR #123 is merged" → trigger `merged`
- "ping this chat when PR #123 goes green" → trigger `checks_passed`
- "let me know if PR #123's checks fail" → trigger `checks_failed`
- "notify me when PR #123 is closed" → trigger `closed`
- "tell me when PR #123 comes out of draft" → trigger `ready_for_review`
- "ping me when PR #123 is green and ready to merge" → trigger `checks_passed_ready`

From a shell, use the CLI (`boss callback add|list|remove`, documented above). From an MCP-aware host, the same operations are exposed as tools:

- `register_github_callback` — create a callback. Give it the PR (a bare number with repo context, or a full `https://github.com/owner/repo/pull/N` URL), a `trigger`, the `target_chat_id` to notify, and the `message` prompt to deliver. Expiry defaults to 24h and may not exceed 30 days.
- `list_github_callbacks` — list callbacks, optionally filtered by chat, repo, PR, trigger, or state.
- `delete_github_callback` — remove a callback by id (requires `confirm: true`).

The `message` you register is a secret: it is delivered verbatim to the target chat when the callback fires, and is never echoed back by any list/inspect surface.

**Delivery is a signal, not proof.** A fired callback tells you the event was observed — it does not guarantee the PR is still in that state by the time you act. Always re-verify the PR's actual state (e.g. `gh pr view <n> --json state,mergeStateStatus,statusCheckRollup`) before taking an irreversible step such as deploying or merging.

## Broadcasting to other chats

A broadcast is one message delivered to every chat a **selector** resolves to. Delivery is durable and retried until it lands or the broadcast expires, and it works by **waking** each target chat — a stopped chat is brought back up to receive the message as a prompt. Delivery is at-least-once, so write a message that is safe to act on twice. Where a callback is "notify me when this PR does X", a broadcast is "tell all of these chats X now".

### Selectors — who receives it

A selector is a set of `key:value` terms over six dimensions:

- `chat:<agent-session-id>` — one specific chat
- `session:<session-id>` — every chat in that session
- `repo:<repo-id>` — every chat on that repo's sessions
- `agent:<name>` — every chat run by that agent runner (e.g. `claude`, `codex`)
- `account:<account-id>` — every chat running under that named account (chats on the default account are not addressable this way)
- `daemon:<daemon-id>` — every chat on that daemon. Naming another daemon here does NOT reach it: chat rows carry an empty daemon id, so this term resolves to zero targets on **every** daemon, not just this one. Addressing other daemons is what `--cross-daemon` (`cross_daemon` on the MCP tool) is for: bosso fans the broadcast out to the tenant's other live daemons, and each re-resolves the selector against its own chats — so pair it with a `repo:`/`agent:`/`chat:` selector, which is what those daemons can actually match. Delivery is best-effort: a daemon offline at fan-out time never receives it (bosso holds no store-and-forward queue), and a fan-out reaching more than 32 other daemons is refused outright rather than truncated

`,` joins terms inside one clause: different dimensions are **AND**ed, repeated values of the same dimension are **OR**ed. `+` joins clauses, and clauses are **OR**ed. (Up to 16 clauses, and 64 values per dimension per clause.)

Worked examples:

```bash
# One account just hit its usage limit — tell every chat running under it.
boss broadcast send --to account:<account-id> --message "This account is exhausted; stop and hand off."

# The host resumed — wake every chat working on this repo.
boss broadcast send --to repo:<repo-id> --message "Host resumed; re-check your PR state before continuing."

# AND: only the Claude chats on one repo. OR (+): that repo, or anything on one account.
--to repo:<repo-id>,agent:claude
--to repo:<repo-id>+account:<account-id>
```

### Subscriptions — broadcast when a session settles

A subscription is a standing rule: when a session reaches an outcome, it sends a broadcast for you. Triggers are `--on completed`, `--on errored`, or `--on settled` (either). "Settles" means the **session** reached an outcome — merged, green, ready for review or closed (`completed`), or blocked or orphaned (`errored`) — not merely that an agent finished a turn. A session that is only idle between turns never fires, so do not use a subscription as a turn-completion signal.

Coordinator/child: a coordinator spawns a child session and wants to be woken when it reaches an outcome rather than polling the transcript. From inside the child (`--session` defaults to `$BOSS_SESSION_ID`):

```bash
boss broadcast subscribe --on settled --to chat:<coordinator-chat-id> --message "Child session settled — read its transcript and continue."
```

The coordinator can register the same rule itself by passing the child's `--session <session-id>` explicitly.

### Rules

- **A broadcast is a signal, not proof.** It tells you someone claimed something, not that it is still true. Verify any claimed state yourself before acting on it.
- **The message body is a secret.** It is delivered verbatim to each target chat and is never echoed back by any list or inspect surface.
- **The audience is resolved once**, when the broadcast is sent (for a subscription, when it fires). Chats that appear afterwards are not added.
- **Delivery wakes target chats**, so a broad selector has a real token cost across the fleet — and an audience over 64 chats is refused outright, not truncated. Scope to the narrowest selector that does the job.
- **An empty selector is an error, never "everyone".** Address the audience explicitly.
- **A selector that matches nobody is a success, not an error.** Unreachable chats (failed to start, or in an archived, merged, closed or orphaned session) are dropped before matching, so even `chat:<id>` naming one directly can resolve to an empty audience. Check the reported target count rather than assuming it landed.

### Both surfaces

From a shell: `boss broadcast send`, `list` (`ls`), `remove` (`rm`), `subscribe`, `subscriptions`, `unsubscribe` — flags documented above. `--from` defaults to `$BOSS_AGENT_SESSION_ID` on both `send` and `subscribe`, but means different things: `send` excludes the origin from its own audience unless you pass `--include-origin`, while on `subscribe` it is provenance only — there is no `--include-origin` there, and a fired subscription never excludes the chat that registered it.

From an MCP-aware host, the same operations are six tools (daemon-local — they are not proxied through the MCP gateway):

- `send_broadcast` — send to the audience a selector resolves to.
- `list_broadcasts` — list recent broadcasts (never their bodies).
- `delete_broadcast` — stop anything further being scheduled for a broadcast (requires `confirm: true`). Not a recall: a delivery a worker already claimed still lands.
- `register_broadcast_subscription` — register a standing rule on a session's outcome.
- `list_broadcast_subscriptions` — list standing subscriptions.
- `delete_broadcast_subscription` — cancel a standing subscription (requires `confirm: true`). It cancels rather than erases, so the row still appears in an unfiltered list.

## Notes — durable free-text a later run can harvest

A note is repo-scoped free-text one run records so a later one can read it back: what an investigation concluded, a trap the next agent should not re-discover, a decision and its reasoning. Unlike a callback (a one-shot signal) or a broadcast (a message pushed at chats now), a note is **pulled** — nobody is woken, and it simply waits until something searches for it.

**A note outlives the run that wrote it.** `session_id` and `chat_id` are **provenance, not ownership**: they record which run wrote the note and are deliberately not foreign keys, so archiving, closing or deleting that session leaves the note — and its provenance — intact. Pass your own session and chat ids when you write one, so a later sweep can attribute it; there is no way to fill them in afterwards.

The `## Notes` section above documents the `boss notes` CLI. From an MCP-aware host the operations are five tools:

- `create_note` — record a note against a repo. `repo_id` and `body` are required; `session_id`/`chat_id` are optional provenance. The body is stored verbatim (non-empty, 64 KiB cap). Tags are normalised on write — trimmed, lowercased, de-duplicated, returned in ascending order — at most 32 of at most 64 bytes each.
- `get_note` — read one note by id, body and tags included.
- `list_notes` — search notes, optionally filtered by repo, provenance, tags or a body substring. Every filter is optional and a blank one is ignored. `tags` matches notes carrying **any** of the listed tags (OR, not all-of). `search` is a literal substring match on the body — SQL wildcards match literally, and case folding is ASCII-only. `limit` zero or negative means unlimited.
- `update_note` — change a note's body and/or tags. **Supplying `tags` REPLACES the whole tag set** rather than appending to it, and supplying an empty list clears every tag; omit `tags` entirely to leave the existing ones alone.
- `delete_note` — permanently erase a note (requires `confirm: true`). Idempotent: deleting an unknown id succeeds, so a retry is safe.

### Rules

- **A note body is NOT a secret.** Unlike a callback or broadcast message — which are never echoed back — a note's body is the payload it exists to carry and is returned in full by every read tool. Do not put credentials in one.
- **`repo_id` is the daemon-local repo id** that `list_repos` and `resolve_context` return, **not** a git origin URL. An origin URL resolves to NotFound.
- **`repo_id` is required on the four id- and body-keyed tools** — `create_note`, `get_note`, `update_note`, `delete_note` — and omitting it fails schema validation before the call runs. On `list_notes` it is just another optional filter: omit it to search every repo.
- **On `get_note`/`update_note`/`delete_note`, `repo_id` routes without scoping.** It only picks which daemon to ask; that daemon then addresses the note by **id alone** and never checks the note against `repo_id`. Naming one repo while passing an id that lives in another acts on the other note — so treat the id, not the repo, as the thing to get right before confirming a delete.
- **Tag normalisation is lossy.** Display casing is not preserved, so file notes under tags you are willing to read back lowercased.
