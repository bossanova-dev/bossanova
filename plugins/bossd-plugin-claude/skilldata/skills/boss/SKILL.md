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

---

## Other

### `boss version`

Print version information.

```bash
boss version
```
