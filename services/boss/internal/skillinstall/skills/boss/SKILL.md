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

### `boss show <session-id>`

Show detailed information about a session.

```bash
boss show abc123
```

### `boss new`

Create a new coding session. Launches the interactive session creation flow.

```bash
boss new
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

## Other

### `boss version`

Print version information.

```bash
boss version
```
