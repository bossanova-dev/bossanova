<!-- GENERATED from the boss CLI by `make gen-skill` — do not edit by hand. Index: ../SKILL.md -->

## Diagnostics

### `boss env [flags]`

Report this session's boss context and the full CLI + MCP capability inventory

**Flags:**

- `--json` — Emit a stable JSON schema instead of human-readable text

### `boss fix-terminal`

Clear stranded terminal mouse- and focus-reporting modes

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

`--json` emits `{"snapshots": [...]}` (`[]`, never `null`, when none are recorded yet), newest first and truncated by `--limit` exactly as the text rendering is. Each entry carries `polled_at` (RFC3339 UTC), `head_sha` (the FULL sha — the text rendering abbreviates for width), `computed_status` (the same `DisplayStatus` vocabulary the text rendering prints) and `raw`. `raw` is the daemon's stored payload spliced in as a NESTED JSON value, not an escaped string, so `.snapshots[0].raw.state` reads directly without decoding twice. A payload that does not parse is preserved verbatim under `raw_invalid` (with `raw` null) rather than being dropped or corrupting the envelope.

**Flags:**

- `--json` — Emit a stable JSON schema instead of text
- `--limit` — Number of snapshots to show (newest first) (default: 5)

```bash
boss session checks abc123
boss session checks abc123 --limit 10
# raw is nested JSON, so it is queryable without a second decode
boss session checks abc123 --json | jq '.snapshots[0].raw.check_runs'
```

### `boss session link-pr <session-id> <pr-number-or-url>`

Attach an existing pull request to a session

Attach an existing GitHub PR to a session. Use this to repair cron sessions where the agent already committed, pushed, and opened a PR before bossd finalized the run.

```bash
boss session link-pr abc123 477
boss session link-pr abc123 https://github.com/owner/repo/pull/477
```

### `boss tail [source] [flags]`

Tail daemon logs

Prints recent rotated service logs without needing to locate them on disk. It defaults to bossd; pass boss or bosso to select one source, or use --all to merge all three by timestamp. Use -f to follow new output. Raw non-JSON diagnostics always remain visible, including when filtering.

**Flags:**

- `--all` — merge every service log
- `--follow`, `-f` — keep reading as the log grows
- `--json` — emit one parseable JSON object per line
- `--level` — only records at this level
- `--lines`, `-n` — physical lines to read per source before filtering (default: 10)
- `--plugin` — only records from this plugin
- `--repo` — only records for this repo

```bash
boss tail
boss tail -f
boss tail --all -n 50
boss tail --plugin dependabot
boss tail --json | jq 'select(.level=="error")'
```
