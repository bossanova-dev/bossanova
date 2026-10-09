<!-- GENERATED from the boss CLI by `make gen-skill` — do not edit by hand. Index: ../SKILL.md -->

## Triggers

### `boss trigger`

Manage inbound HTTP and GitHub triggers that start new sessions

A trigger starts a NEW SESSION when an external event happens: an authenticated HTTP request to the trigger's endpoint (`http`), or a GitHub App event on the trigger's repository (`github`). Use one when something outside Boss — a deploy hook, an alert, an opened issue or PR — should begin independent work with no agent waiting for it.

A trigger is NOT a way to watch work you already own. To be woken when a PR you are driving merges, goes green or goes red, use `boss callback add`; to wait on a running chat, use `boss chat wait`. A trigger keeps firing a fresh session on every matching event, and that session knows nothing about yours.

Triggers live in the cloud, not the local daemon: every subcommand needs `boss login` (it fails with `run 'boss login' first` otherwise) and works with or without `--remote`. A trigger is visible only to its creator; anyone else's id reads as not found. Every subcommand accepts `--json`, which writes one object to stdout and, on failure, the standard `{"error":{code,connect_code,message}}` envelope. Run `boss trigger catalog` first to see the valid types, `--event` ids and `--filter` fields.

### `boss trigger add [flags]`

Create a trigger

Create a trigger. `--type`, `--name`, `--repo-url`, one of `--prompt` / `--prompt-file`, and one of `--daemon <id>` / `--first-available` are required; `--org` defaults to your only organization. The prompt is read by a fresh agent, so write it as a complete standing instruction; `--payload-field` paths (e.g. `body.ref`) are copied into its context. Every `--filter 'field op value[,value…]'` must match for an event to launch (op: `=`, `!=`, `in`, `not-in`, `contains`, `prefix`; only `in` and `not-in` split on commas). `--concurrency` decides what an event does while the previous session is still working: `skip` (default), `cancel` (stop it, then launch), or `allow` (launch alongside). A github trigger needs at least one `--event`; `--methods`, `--idempotency-header` and `--dedup-window` are http only.

An http create prints the endpoint and the signing secret ONCE — store it now; it can never be read back, only replaced with `rotate-secret`. `--json` carries it as `secret` beside `trigger`.

**Flags:**

- `--agent` — Agent runner plugin name (empty = claude)
- `--base-branch` — Branch the session's worktree is cut from (empty = repo default)
- `--concurrency` — When the previous session is still working: skip (default), cancel (stop it, then launch), or allow (launch alongside)
- `--cooldown` — Minimum gap between two launches, e.g. 5m (0 = none) (default: 0s)
- `--daemon` — Launch only on this daemon id
- `--dedup-window` — How long a seen idempotency key suppresses a repeat, e.g. 5m (http only; 0 = 5m) (default: 0s)
- `--disabled` — Create the trigger disabled
- `--effort` — Agent reasoning-effort level (empty = plugin default)
- `--event` — GitHub event id from `boss trigger catalog`; repeat for several (github only)
- `--filter` — Payload filter 'field op value[,value…]' (op: =, !=, in, not-in, contains, prefix); repeat to AND several
- `--first-available` — Launch on the first ready daemon that manages the repo
- `--idempotency-header` — Request header carrying the idempotency key (http only; empty = Idempotency-Key)
- `--json` — Emit the trigger as a stable JSON schema
- `--methods` — Accepted HTTP methods, e.g. POST,PUT (http only; empty = POST)
- `--model` — Agent model id (empty = plugin default)
- `--name` — Trigger name (required)
- `--org` — Organization id (default: your only organization; required when you belong to several)
- `--payload-field` — Payload field copied into the prompt context, e.g. body.ref; repeat for several
- `--prompt` — Prompt sent to the agent
- `--prompt-file` — Read the prompt from a file (or '-' for stdin)
- `--repo-url` — Origin URL of the repo the launched session works in (required)
- `--skill` — Skill to invoke as a slash command, without the leading '/'
- `--type` — Trigger type: http or github (required)

```bash
# start a session whenever the deploy hook reports a failure
boss trigger add --type http --name "deploy failed" --repo-url https://github.com/acme/widget --first-available --filter 'body.status = failed' --payload-field body.log_url --prompt "Investigate the failed deploy and open a fix PR."
# run a review skill on every newly opened pull request
boss trigger add --type github --name "review new PRs" --repo-url https://github.com/acme/widget --first-available --event pull_request.opened --concurrency allow --skill review --prompt "Review this pull request."
```

### `boss trigger catalog [flags]`

List trigger types, event ids and filter fields

**Flags:**

- `--json` — Emit the catalog as a stable JSON schema

### `boss trigger disable <trigger-id> [flags]`

Disable a trigger

**Flags:**

- `--json` — Emit the trigger as a stable JSON schema

### `boss trigger enable <trigger-id> [flags]`

Enable a trigger

**Flags:**

- `--json` — Emit the trigger as a stable JSON schema

### `boss trigger history <trigger-id> [flags]`

Show a trigger's invocation history, newest first

Show invocations newest first. `status` is the outcome — `launched` (with its session), `filtered`, `skipped`, `deduplicated`, `launching`, `failed` or `accepted` — and `decision_reason` says why: `filter_mismatch` (a filter did not match), `cooldown`, `prior_session_running` (concurrency `skip`), `prior_session_stop_failed` (concurrency `cancel` could not stop the previous session; it retries), `duplicate_delivery`, `placement_unavailable` (the daemon is offline or does not manage the repo) or `daemon_unsupported`. Treat an unknown reason as opaque. Failed and skipped rows also print an actionable detail line.

**Flags:**

- `--json` — Emit {invocations} as a stable JSON schema
- `--limit` — Maximum invocations to show (max 200) (default: 20)

### `boss trigger ls [flags]`

List your triggers

**Flags:**

- `--json` — Emit a stable JSON schema ({triggers}) instead of a table
- `--org` — Restrict to one organization id (default: every organization you belong to)

### `boss trigger rm <trigger-id> [flags]`

Delete a trigger and its history

Delete a trigger, its secret and its history; the endpoint stops accepting requests at once. It prompts on a terminal; scripts and `--json` must pass `--yes`, or it refuses with `CONFIRMATION_REQUIRED`.

**Flags:**

- `--json` — Emit the deleted trigger id as a stable JSON schema
- `--yes`, `-y` — Skip the confirmation prompt

### `boss trigger rotate-secret <trigger-id> [flags]`

Replace an HTTP trigger's signing secret

**Flags:**

- `--json` — Emit {trigger, secret} as a stable JSON schema

### `boss trigger show <trigger-id> [flags]`

Show a trigger's configuration and recent invocations

**Flags:**

- `--json` — Emit {trigger, invocations} as a stable JSON schema

### `boss trigger test <trigger-id> [flags]`

Run a sample payload through a trigger (dry run by default)

Run a sample payload (`--payload-file`, `-` for stdin, default `{}`) through the trigger's filters and policies and record the result as an invocation with source `test`. It is a dry run unless `--launch` is set, which starts a real session when the sample passes. github triggers need `--event <id>`.

**Flags:**

- `--event` — Event id the sample is treated as (required for github triggers)
- `--json` — Emit the recorded invocation as a stable JSON schema
- `--launch` — Launch a real session when the sample passes every filter and policy
- `--payload-file` — Sample JSON payload file (or '-' for stdin; default {})

```bash
echo '{"status":"failed"}' | boss trigger test tr_123 --payload-file -
```

### `boss trigger update <trigger-id> [flags]`

Change a trigger's settings

Change only the settings whose flags are given; everything else is kept, including the rest of the launch settings when only `--prompt` changes. `--filter` and `--payload-field` REPLACE the whole list; `--clear-filters` / `--clear-payload-fields` empty it. The type cannot change. Use `enable` / `disable` to switch a trigger on or off.

**Flags:**

- `--agent` — Agent runner plugin name (empty = claude)
- `--base-branch` — Branch the session's worktree is cut from (empty = repo default)
- `--clear-filters` — Remove every filter
- `--clear-payload-fields` — Remove every payload field
- `--concurrency` — When the previous session is still working: skip (default), cancel (stop it, then launch), or allow (launch alongside)
- `--cooldown` — Minimum gap between two launches, e.g. 5m (0 = none) (default: 0s)
- `--daemon` — Launch only on this daemon id
- `--dedup-window` — How long a seen idempotency key suppresses a repeat, e.g. 5m (http only; 0 = 5m) (default: 0s)
- `--effort` — Agent reasoning-effort level (empty = plugin default)
- `--event` — GitHub event id from `boss trigger catalog`; repeat for several (github only)
- `--filter` — Payload filter 'field op value[,value…]' (op: =, !=, in, not-in, contains, prefix); repeat to AND several
- `--first-available` — Launch on the first ready daemon that manages the repo
- `--idempotency-header` — Request header carrying the idempotency key (http only; empty = Idempotency-Key)
- `--json` — Emit the trigger as a stable JSON schema
- `--methods` — Accepted HTTP methods, e.g. POST,PUT (http only; empty = POST)
- `--model` — Agent model id (empty = plugin default)
- `--name` — Trigger name
- `--payload-field` — Payload field copied into the prompt context, e.g. body.ref; repeat for several
- `--prompt` — Prompt sent to the agent
- `--prompt-file` — Read the prompt from a file (or '-' for stdin)
- `--repo-url` — Origin URL of the repo the launched session works in
- `--skill` — Skill to invoke as a slash command, without the leading '/'
