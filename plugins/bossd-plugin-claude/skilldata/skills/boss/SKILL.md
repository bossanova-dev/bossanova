---
name: boss
description: Complete reference for all boss CLI commands. Use to run boss operations from within a Claude Code session.
---

# Boss CLI Reference

Boss manages Claude coding sessions across git worktrees with automatic PR creation, CI fix loops, and code review handling.

## Global Flags

### `--remote <url>`

Connect to an orchestrator URL instead of the local daemon.

```bash
boss --remote https://orchestrator.example.com ls
```

---

## Session Management

### `boss ls`

List sessions (non-interactive).

**Flags:**

- `--repo <repo-id>` — Filter by repo ID
- `--archived` — Include archived sessions (default: false)
- `--state <state>[,<state>...]` — Filter by state(s)

```bash
boss ls
boss ls --repo my-repo --state running,paused
boss ls --archived
```

An extra `AGENT` column appears only when at least one listed session uses
an agent that differs from the user's `Settings.DefaultAgent`. In the
common single-agent case the column is hidden so the table stays compact.

### `boss show <session-id>`

Show detailed information about a session.

```bash
boss show abc123
```

### `boss new`

Create a new coding session. Launches the interactive session creation flow.

**Flags:**

- `--agent <name>` — Override the default agent plugin for this session (e.g. `claude`, `opencode`). When omitted, the daemon falls back to `Settings.DefaultAgent`.

```bash
boss new
boss new --agent opencode
```

### `boss attach <session-id>`

Attach to a running session's terminal.

```bash
boss attach abc123
```

### `boss chats <session-id>`

List chats (conversation turns) in a session.

```bash
boss chats abc123
```

### `boss session link-pr <session-id> <pr-number-or-url>`

Attach an existing GitHub PR to a session. Use this to repair cron sessions
where the agent already committed, pushed, and opened a PR before bossd
finalized the run.

```bash
boss session link-pr abc123 477
boss session link-pr abc123 https://github.com/owner/repo/pull/477
```

### `boss archive <session-id>`

Archive a session — keeps the branch but removes the worktree.

```bash
boss archive abc123
```

---

## Repository Management

`boss repo` is a command group; use one of its subcommands.

### `boss repo add`

Register a repository with bossd.

```bash
boss repo add
```

### `boss repo ls`

List registered repositories.

```bash
boss repo ls
```

### `boss repo remove <repo-id>`

Remove a registered repository.

```bash
boss repo remove my-repo
```

### `boss repo update <repo-id>`

Update repository settings.

**Flags:**

- `--name <name>` — Set display name
- `--setup-script <path>` — Set setup script (empty string to clear)
- `--merge-strategy <strategy>` — Set merge strategy (`merge`, `rebase`, `squash`)
- `--auto-merge` — Enable auto-merge
- `--no-auto-merge` — Disable auto-merge
- `--auto-merge-dependabot` — Enable auto-merge for Dependabot PRs
- `--no-auto-merge-dependabot` — Disable auto-merge for Dependabot PRs
- `--auto-address-reviews` — Enable auto-address review feedback
- `--no-auto-address-reviews` — Disable auto-address review feedback
- `--auto-resolve-conflicts` — Enable auto-resolve merge conflicts
- `--no-auto-resolve-conflicts` — Disable auto-resolve merge conflicts

```bash
boss repo update my-repo --name "My Repo" --merge-strategy squash
boss repo update my-repo --auto-merge-dependabot
```

---

## Trash Management

`boss trash` is a command group; use one of its subcommands to manage archived sessions.

### `boss trash ls`

List archived sessions.

```bash
boss trash ls
```

### `boss trash restore <session-id>`

Restore an archived session (recreates the worktree).

```bash
boss trash restore abc123
```

### `boss trash delete <session-id>`

Permanently delete an archived session.

**Flags:**

- `--yes`, `-y` — Skip confirmation prompt

```bash
boss trash delete abc123
boss trash delete abc123 --yes
```

### `boss trash empty`

Permanently delete all archived sessions.

**Flags:**

- `--older-than <duration>` — Only delete sessions archived longer than this duration (e.g. `30d`)

```bash
boss trash empty
boss trash empty --older-than 30d
```

---

## Daemon Management

`boss daemon` is a command group for managing the bossd background daemon.

### `boss daemon install`

Install bossd as a macOS LaunchAgent.

**Flags:**

- `--force` — Overwrite existing service file

```bash
boss daemon install
boss daemon install --force
```

### `boss daemon uninstall`

Uninstall the bossd LaunchAgent.

```bash
boss daemon uninstall
```

### `boss daemon status`

Show bossd daemon status.

```bash
boss daemon status
```

### `boss daemon start`

Start the bossd daemon. No-op if it's already running. Falls back to spawning bossd directly if it isn't installed as a LaunchAgent.

```bash
boss daemon start
```

### `boss daemon stop`

Stop the bossd daemon for the current profile via the platform service manager or profile metadata. Idempotent — quietly succeeds if the daemon is already stopped or not installed. Use `--all-standalone` only for explicit cleanup of every user-owned standalone bossd process across profiles.

```bash
boss daemon stop
boss daemon stop --all-standalone
```

### `boss daemon restart`

Restart the bossd daemon via the platform service manager. Errors out if the daemon isn't installed.

```bash
boss daemon restart
```

---

## MCP Server

`boss mcp` is a command group for managing the local MCP server, which exposes the boss operations as MCP tools over Streamable HTTP for MCP-aware hosts. It runs as an auto-starting service (launchd on macOS, systemd on Linux) and proxies through the local bossd daemon's Unix socket.

### `boss mcp install`

Install the MCP server as an auto-starting service and start it. Use `--force` to overwrite an existing service file, and `--port` to change the loopback port (default 8765). The server listens on `http://127.0.0.1:<port>/mcp`.

```bash
boss mcp install
boss mcp install --force
boss mcp install --port 8888
```

### `boss mcp uninstall`

Uninstall the MCP server service and remove its service file.

```bash
boss mcp uninstall
```

### `boss mcp status`

Show whether the MCP server is installed and running.

```bash
boss mcp status
```

### `boss mcp start`

Start (or restart) the installed MCP server.

```bash
boss mcp start
```

### `boss mcp stop`

Stop the running MCP server, leaving its service file in place. Idempotent.

```bash
boss mcp stop
```

---

## Settings & Auth

### `boss settings`

View or update global settings.

**Flags:**

- `--skip-permissions` — Enable Claude `--dangerously-skip-permissions`
- `--no-skip-permissions` — Disable Claude `--dangerously-skip-permissions`
- `--worktree-dir <path>` — Set worktree base directory
- `--default-agent <name>` — Set the default agent plugin (e.g. `claude`, `opencode`)
- `--poll-interval <seconds>` — Set poll interval in seconds (0 = use default)

```bash
boss settings
boss settings --worktree-dir ~/work/bossanova/worktrees
boss settings --skip-permissions
```

### `boss config init`

Initialize plugin configuration from a directory of plugin binaries.

**Flags:**

- `--plugin-dir <path>` — Directory containing plugin binaries (auto-detected if omitted)

```bash
boss config init
boss config init --plugin-dir ./plugins
```

### `boss login`

Log in to Bossanova cloud (WorkOS).

```bash
boss login
```

### `boss logout`

Log out and remove stored credentials.

```bash
boss logout
```

### `boss auth-status`

Show authentication status.

```bash
boss auth-status
```

---

## Diagnostics

### `boss repair doctor`

Health-check the auto-repair pipeline. Calls the daemon's `RepairDoctor` RPC
and renders a checklist (plugin loaded, `claude` on PATH, recent log files,
etc.) plus a recent-logs table — answers "is auto-repair healthy?" without
having to grep daemon stderr.

```bash
boss repair doctor
```

### `boss session checks <session-id>`

Show bossd's persisted view of a session's CI check snapshots, alongside the
`DisplayStatus` the daemon computed for each one. Useful when reconciling
"why did the TUI think this PR was passing when GitHub says failing?".

**Flags:**

- `--limit <n>` — Number of snapshots to show, newest first (default: 5)

```bash
boss session checks abc123
boss session checks abc123 --limit 10
```

Cron repair example:

```bash
boss ls --state finalizing,blocked
boss session link-pr b4764f1684e33742 477
```

---

## Plugins

### `boss plugin list`

List plugins the daemon attempted to load during this run.

Alias: `boss plugin ls`

```bash
boss plugin list
boss plugin ls
```

---

## Other

### `boss version`

Print version information.

```bash
boss version
```

### `boss upgrade`

Check for and install Bossanova upgrades.

- `--check` — Check for an upgrade without installing
- `--yes` — Install without interactive confirmation
- `--version <tag>` — Install a specific stable release tag
- `--no-restart` — Do not restart the daemon after upgrade

```bash
boss upgrade --check
boss upgrade --yes
boss upgrade --version v1.2.4 --yes
boss upgrade --yes --no-restart
```
