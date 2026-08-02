<!-- GENERATED from the boss CLI by `make gen-skill` — do not edit by hand. Index: ../SKILL.md -->

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
