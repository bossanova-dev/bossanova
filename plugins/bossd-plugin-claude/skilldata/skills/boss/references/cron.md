<!-- GENERATED from the boss CLI by `make gen-skill` — do not edit by hand. Index: ../SKILL.md -->

## Cron Jobs

### `boss cron`

Manage scheduled cron jobs

A cron job is a recurring schedule that starts a NEW SESSION on every fire. Use it to BEGIN work on a schedule — a nightly sweep, a backlog run, a weekly report — where each run is independent and there is nothing in flight to attend to.

A cron job is NOT a monitoring tool, and must never be registered to watch a session, chat, pull request or epic that is already running. Three things go wrong: fires OVERLAP, so a slow run is still going when the next one starts; each fire is a fresh session that begins with no memory of what the last one saw; and the job keeps firing long after the work it was watching finished, because nothing about that work can retire a schedule.

To observe work that is already running, address it directly. `boss chats <session-id>` reports whether each chat is still working. `boss chat wait <session-id|chat-id>` blocks in the foreground until one goes idle, bounded by `--timeout`. `boss tail <agent-session-id>` shows what it last said. `boss show <session-id>` and `boss session checks` give session and PR state. To be woken instead of polling, `boss callback add` fires once when a pull request reaches a chosen state, and `boss broadcast subscribe --on settled` fires when a session reaches an outcome.

### `boss cron add [flags]`

Create a cron job

Create a recurring job. Every fire starts a new session running `--prompt` against `--repo`, so write the prompt as a complete standing instruction: it is read by a fresh agent that cannot see what any previous fire did. Pass `--zero-output` for a job that changes nothing in the repository (a sweep or a report), so no worktree, branch or PR is created for it. A `--gate` command runs before each fire and skips it when it exits non-zero — use one to avoid waking an agent that would find no work to do.

Do not use this to wait for or monitor something already in flight; see `boss cron` above for why, and for the commands that do that job.

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

```bash
# "update dependencies every night" (offset off the herd minute)
boss cron add --repo <repo-id> --name "nightly deps" --schedule "17 3 * * *" --prompt "Review and update outdated dependencies."
# a job that changes nothing in the repo — no worktree, branch or PR
boss cron add --repo <repo-id> --name "backlog triage" --schedule "@weekly" --zero-output --prompt "Triage the open backlog and report."
```

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
