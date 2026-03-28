---
name: boss
description: Complete reference for all boss CLI commands. Use to run boss operations from within a Claude Code session.
---

# Boss CLI Reference

Boss manages autonomous Claude coding sessions with automatic PR creation, CI fix loops, and code review handling.

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

### `boss repo add`

Register a new repository interactively.

```bash
boss repo add
```

### `boss repo ls`

List all registered repositories.

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
- `--setup-script <script>` — Set setup script (empty string to clear)
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
boss repo update my-repo --merge-strategy squash --auto-merge
boss repo update my-repo --name "My Project" --setup-script "make deps"
boss repo update my-repo --auto-address-reviews --auto-resolve-conflicts
```

---

## Trash Management

### `boss trash ls`

List archived (trashed) sessions.

```bash
boss trash ls
```

### `boss trash restore <session-id>`

Restore an archived session from the trash.

```bash
boss trash restore abc123
```

### `boss trash delete <session-id>`

Permanently delete an archived session.

**Flags:**

- `-y, --yes` — Skip confirmation prompt

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

## Autopilot (alias: `ap`)

Autopilot runs multi-leg autonomous coding workflows from a plan file.

### `boss autopilot start <plan-file>`

Start an autopilot workflow from a plan file.

**Flags:**

- `--max-legs <n>` — Maximum flight legs, 0 = use default (default: 0)
- `--confirm-land` — Pause for confirmation before landing (default: false)

```bash
boss autopilot start docs/plans/feature.md
boss ap start docs/plans/feature.md --max-legs 5 --confirm-land
```

### `boss autopilot status [workflow-id]`

Show autopilot workflow status. If no workflow ID is given, uses the most recent active workflow.

**Flags:**

- `-f, --follow` — Stream output in real-time (default: false)

```bash
boss autopilot status
boss autopilot status wf-abc123
boss ap status --follow
```

### `boss autopilot list`

List autopilot workflows. Alias: `boss autopilot ls`.

**Flags:**

- `--all` — Include completed and cancelled workflows (default: false)

```bash
boss autopilot list
boss ap ls --all
```

### `boss autopilot pause [workflow-id]`

Pause an autopilot workflow. If no ID is given, pauses the most recent active workflow.

```bash
boss autopilot pause
boss ap pause wf-abc123
```

### `boss autopilot resume [workflow-id]`

Resume a paused autopilot workflow. If no ID is given, resumes the most recent active workflow.

```bash
boss autopilot resume
boss ap resume wf-abc123
```

### `boss autopilot cancel [workflow-id]`

Cancel an autopilot workflow. If no ID is given, cancels the most recent active workflow.

```bash
boss autopilot cancel
boss ap cancel wf-abc123
```

---

## Daemon Management

### `boss daemon install`

Install bossd as a macOS LaunchAgent.

```bash
boss daemon install
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
- `--poll-interval <seconds>` — Set poll interval in seconds (0 = default)

```bash
boss settings
boss settings --skip-permissions --worktree-dir ~/worktrees
boss settings --poll-interval 30
```

### `boss login`

Log in to Bossanova cloud via Auth0 PKCE. Opens a browser for authentication.

```bash
boss login
```

### `boss logout`

Log out and remove stored credentials.

```bash
boss logout
```

### `boss auth-status`

Show authentication status (logged in, email, token expiry).

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
