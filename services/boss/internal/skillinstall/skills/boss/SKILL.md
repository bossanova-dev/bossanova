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

**Flags:**

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
- `--merge-strategy` — Set merge strategy (merge, rebase, squash)
- `--name` — Set display name
- `--no-auto-merge` — Disable auto-merge
- `--no-auto-merge-dependabot` — Disable auto-merge for Dependabot PRs
- `--no-auto-repair` — Disable automatic repair
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

## Account Management

### `boss account`

Manage agent accounts (credential registry)

### `boss account add [provider] [flags]`

Register an agent account

**Flags:**

- `--credential-file` — Read the credential from a file (or '-' for stdin); preferred over --token
- `--email` — Informational account email
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

### `boss account remove <account-id>`

Remove an account and its stored credential

### `boss account test <account-id> [flags]`

Validate an account's credential and record the outcome

**Flags:**

- `--json` — Emit a stable JSON schema instead of text

### `boss account update <account-id> [flags]`

Update account metadata

**Flags:**

- `--allowed-models` — Replace the allowed-models set (comma-separated)
- `--email` — Set the account email
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

Stops the bossd daemon for the current profile via the platform service manager or profile metadata. Idempotent — quietly succeeds if the daemon is already stopped or not installed. Use `--all-standalone` only for explicit cleanup of every user-owned standalone bossd process across profiles.

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
