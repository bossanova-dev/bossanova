---
title: MCP Server
description: Control Bossanova sessions, repos, and cron jobs from AI agents via the Model Context Protocol.
slug: /guides/mcp
---

import CommandTabs from '@site/src/components/CommandTabs';

# MCP Server

Bossanova ships a local [Model Context Protocol (MCP)](https://modelcontextprotocol.io/) server that lets AI coding agents (Claude Code, Claude Desktop, and any MCP-capable host) drive Bossanova directly: list and create sessions, manage repositories, inspect CI, and schedule cron jobs. The tool reference below describes the supported operations.

The server exposes **79 tools** in three tiers:

| Tier        | Count | Behaviour                          |
| ----------- | ----- | ---------------------------------- |
| Read-only   | 29    | Always available                   |
| Mutating    | 35    | Non-destructive writes             |
| Destructive | 15    | Require `confirm: true` to execute |

## Install

Build the `bin/mcp` binary:

```bash
make build-mcp          # produces bin/mcp
```

That binary is all you need to wire up a stdio MCP host such as Claude Code or
Claude Desktop. Those hosts spawn `bin/mcp` themselves over stdio (see
[Connect an agent](#connect-an-agent) below), so they do **not** require the
service install below.

### Optional: run `bin/mcp` as a standalone HTTP daemon

:::note Optional: most users can skip this
Stdio MCP hosts (Claude Code, Claude Desktop) spawn `bin/mcp` themselves and
never need this. Install the HTTP daemon only if you want an always-on
`bin/mcp` reachable over HTTP: for HTTP-capable MCP clients, `curl`, or a
browser-based inspector.
:::

Install and start the local MCP HTTP daemon:

<CommandTabs
cli="boss mcp install"
/>

Print the bearer token HTTP clients must send (`--json` adds the full header and the token file path):

<CommandTabs
cli="boss mcp token"
/>

Show whether the service is installed and running, plus the instance inventory:

<CommandTabs
cli="boss mcp status"
/>

Start or restart the installed service:

<CommandTabs
cli="boss mcp start"
/>

Stop the managed service and sweep stray/orphaned `boss-mcp` processes:

<CommandTabs
cli="boss mcp stop"
/>

Stop and remove the service file:

<CommandTabs
cli="boss mcp uninstall"
/>

`boss mcp install` runs `mcp --http 127.0.0.1:<port>` (serving `/mcp`) under the
platform user service manager: launchd (`~/Library/LaunchAgents/com.bossanova.mcp.plist`)
on macOS, or systemd (`~/.config/systemd/user/bossanova-mcp.service`) on Linux.
It accepts `--port <n>` (default 8765) and `--force` (overwrite an existing
service file).

#### Authenticating HTTP clients

Every request to `/mcp` must carry `Authorization: Bearer <token>`. The 64-hex-character token
lives in `mcp-http.token` next to the bossd socket (mode 0600, created on first
start) and `boss mcp token` prints it. Add the header to your HTTP MCP client's
config, for example:

```json
{
  "mcpServers": {
    "bossanova": {
      "type": "http",
      "url": "http://127.0.0.1:8765/mcp",
      "headers": { "Authorization": "Bearer <output of boss mcp token>" }
    }
  }
}
```

Requests without the header, or with a wrong one, get `401` and run no tool.
`/healthz` and `/buildinfo` need no token. The server refuses to listen on
anything but a loopback address (`127.0.0.1`, `[::1]` or `localhost`). The token
is separate from the daemon's own socket token, so to rotate it delete
`mcp-http.token` and restart the service (`boss mcp start`), then update your
client config. Stdio hosts need none of this.

For a direct HTTP check, initialize an MCP connection with the bearer token:

```bash
curl -s -o /dev/null -w '%{http_code}\n' -X POST \
  -H "Authorization: Bearer $(boss mcp token)" \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"curl","version":"0"}}}' \
  http://127.0.0.1:8765/mcp
```

The expected response is `200`; removing the authorization header returns `401`.

#### What `boss mcp stop` owns, and what it leaves alone

`boss mcp stop` only touches the service manager when the service is actually
installed (so on a machine that never ran `boss mcp install` it does nothing
there), and its "Idempotent." guarantee is now a verified end state, not just
the service manager's exit code. Beyond the managed service, it also sweeps
every other `boss-mcp` process owned by the current user (bossd writes one
into each agent's per-chat MCP config), classifying each of them:

| class                                                            | what `stop` does                                                                                                                             |
| ---------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------- |
| the managed service                                              | stopped through the service manager only — never signalled, since its plist/unit sets `KeepAlive`/`Restart=always` and would just respawn it |
| stray HTTP daemon (`--http`, not the managed one)                | terminated                                                                                                                                   |
| orphaned session server (its MCP host died)                      | terminated                                                                                                                                   |
| live **session-owned** server (still attached to a running chat) | left running, deliberately                                                                                                                   |

In one edge case a fifth class appears: if the service is installed but the
service manager will not report its PID (a systemd unit mid-`activating`, or a
loaded launchd job between `KeepAlive` respawns), an `--http` process cannot be
distinguished from the managed instance. Those are reported as
`unattributable HTTP` and left running, rather than risk signalling a service
that is configured to respawn.

There is one known gap in the other direction, and it applies on both
platforms: if the service _file_ is deleted while the launchd job or systemd
unit is still loaded, the service reads as not installed, so `stop` neither
stops it through the service manager nor treats the managed `--http` row as
unattributable; it sweeps it as stray, and `KeepAlive` / `Restart=always`
respawns it under a new PID. Recover by re-creating the service file and
re-loading it: `boss mcp install --force` (which may report the job as already
loaded), then `boss mcp start`.

A live session-owned server is left alone on purpose: an MCP host does not
respawn a dead stdio server mid-session, so killing one would silently strip
the `mcp__boss__*` tools from a running chat. Each exits with its own chat
when that chat ends. `boss mcp status` reports this same inventory on its
`instances:` line.

## Connect an agent

stdio MCP hosts (Claude Code, Claude Desktop) spawn the binary themselves. You
do not need `boss mcp install` for this path. Point your host at the absolute
path of `bin/mcp` (run `realpath bin/mcp` after `make build-mcp`).

**Claude Code** — `.mcp.json` at your project root (team-shared):

```json
{
  "mcpServers": {
    "bossanova": {
      "command": "/absolute/path/to/bin/mcp"
    }
  }
}
```

Claude Code keeps user-scope and local-scope server configuration in `~/.claude.json`.

**Claude Desktop** — `~/Library/Application Support/Claude/claude_desktop_config.json`, same `mcpServers` block; restart Claude Desktop after saving. Verify with `/mcp` in Claude Code or the tools panel in Claude Desktop; the `bossanova` tools should appear.

> **Environment variables** in `.mcp.json`. `${VAR}` placeholders in
> `.mcp.json` (for example `Authorization: Bearer ${LINEAR_API_KEY}`) are
> resolved from the agent session's environment. Bossanova automatically
> loads a worktree's `.env` into that environment, so putting the value in
> the worktree `.env` is enough for it to resolve; see
> [Automatic `.env` loading](./setup-scripts.md#automatic-env-loading).

## Modes

- **stdio (default)** — `bin/mcp` with no flags; the MCP host spawns it and talks over stdin/stdout.
- **Streamable HTTP** — `bin/mcp --http 127.0.0.1:7474` (any free loopback port; non-loopback addresses are refused) serves `/mcp`, `/healthz` and `/buildinfo`, useful for `curl` or a browser-based MCP inspector. `/mcp` requires `Authorization: Bearer <token>` (see [Authenticating HTTP clients](#authenticating-http-clients)). Pass `--socket /path/to/bossd.sock` for a non-default bossd socket.

Pass `--read-only` to register only the 29 read-only tools; mutating and destructive tools then never appear in `tools/list`. Use `--only get_session,list_notes` to advertise a comma-separated allowlist; it composes with `--read-only`. An empty `--only` value registers the full surface allowed by the other flags.

If tools fail while `/healthz` remains healthy, check that `bossd` is running. The MCP server talks to it over a Unix socket; pass `--socket` for a non-default socket path.

## Destructive tools need confirmation

The 15 destructive tools (`remove_repo`, `remove_session`, `delete_chat`, `empty_trash`, …) refuse to run unless the caller passes `"confirm": true`:

```
remove_repo is destructive and requires {"confirm": true}; re-call with confirm set once you are sure
```

This prevents an agent from accidentally deleting a repo, session, or chat.

## Tool reference

### Read-only (29)

| Tool                           | Description                                                                                |
| ------------------------------ | ------------------------------------------------------------------------------------------ |
| `list_sessions`                | List sessions, optionally filtered by repo, states, or archived flag                       |
| `get_session`                  | Get a single session by id — `state`/`last_check_state` carry no push info, see below      |
| `list_repos`                   | List every registered repository                                                           |
| `list_repo_prs`                | List open pull requests for a repository                                                   |
| `list_tracker_issues`          | List issues from an external tracker (Linear, Sentry)                                      |
| `resolve_context`              | Resolve repo + session for a working directory                                             |
| `validate_repo_path`           | Validate a local path is a usable git repo                                                 |
| `list_chats`                   | List agent chats for a session                                                             |
| `get_chat_statuses`            | Get live chat status for a session — `last_output_at` is a floor, see below                |
| `get_session_statuses`         | Get best live status across chats for multiple sessions — aggregate only, see below        |
| `list_check_snapshots`         | List recent CI check snapshots for a session                                               |
| `repair_doctor`                | Run daemon repair-doctor diagnostics                                                       |
| `list_agents`                  | List loaded agent-runner plugins                                                           |
| `list_plugins`                 | List every plugin the daemon attempted to load                                             |
| `list_cron_jobs`               | List every scheduled cron job                                                              |
| `get_cron_job`                 | Get a single cron job by id                                                                |
| `get_chat_transcript`          | Return the conversation transcript and final assistant text for a chat                     |
| `list_accounts`                | List registry accounts and cached usage metadata; credentials are never returned           |
| `get_settings`                 | Get the daemon's global settings — the TUI-editable subset plus each agent's config        |
| `list_github_callbacks`        | List registered GitHub PR callbacks; the delivery message body is never returned           |
| `list_notes`                   | List repo-scoped notes, optionally filtered by repo, provenance, tags, or a body substring |
| `get_note`                     | Get a single note by id, including its full body and normalised tags                       |
| `list_broadcasts`              | List broadcasts and their lifecycle state; the message body is never returned              |
| `list_broadcast_subscriptions` | List standing broadcast subscriptions; the registered message body is never returned       |
| `list_organization_notes`      | List a page of an organization's cloud notes, filtered (hosted endpoint only)              |
| `get_organization_note`        | Get one organization note by id, with its body, origin and 90-day `expires_at`             |
| `get_organization_note_quota`  | Report an organization's hourly note-write quota without spending any                      |
| `export_project_config`        | Export this repo's boss setup as `.boss-project.json` content                              |
| `diff_project_config`          | Plan converging onto a `.boss-project.json` passed as an argument; writes nothing          |

### Mutating (35)

`register_repo`, `clone_and_register_repo`, `update_repo`, `create_session`, `stop_session`, `pause_session`, `resume_session`, `retry_session`, `update_session`, `link_session_pr`, `refresh_session_pr`, `start_chat`, `record_chat`, `update_chat_title`, `wake_chat`, `report_chat_status`, `create_cron_job`, `update_cron_job`, `run_cron_job_now`, `add_account`, `refresh_account`, `update_account`, `test_account`, `send_chat_message`, `switch_account`, `update_settings`, `start_repair_workflow`, `register_github_callback`, `send_broadcast`, `register_broadcast_subscription`, `create_note`, `update_note`, `create_organization_note`, `update_organization_note`, `apply_project_config`

`create_session` accepts `repo_id`, `prompt`, with `title` and
`agent` accepted too. When `title` is omitted it is auto-derived from the first
non-blank line of the prompt (truncated to 72 characters), since bossd requires
a non-empty session title. It also exposes the rest of the daemon's
`CreateSession` surface: `base_branch`, `branch_name`, `force_branch`,
`quick_chat`, `pr_number` (attach to an existing PR instead of creating one), and
the external-tracker fields `tracker_id` / `tracker_url` / `tracker_source`. The
composite `tracker_issue` field is web-only (server-side plan formatting) and is
intentionally **not** exposed here.

`create_cron_job` and `update_cron_job` accept `model`, `gate_command`,
`run_setup_command`, and `zero_output` alongside the schedule fields, matching the
TUI cron form. `run_setup_command` and `zero_output` are both tri-state (`true` /
`false` / omitted): omitting one sends no value, so on `update_cron_job` the
daemon's stored value is left untouched and on `create_cron_job` the server
applies its own default rather than an implicit `false`.

`zero_output` marks a job that runs with no worktree, branch, or PR, for jobs
that report elsewhere and change nothing in the repo. Unlike `run_setup_command`,
its default is **off**.

`concurrency_policy` decides what a fire does while the job's previous run is still
working: `skip` (the default), `cancel_in_progress`, or `allow_concurrent`. The
value is matched case-insensitively and the hyphenated CLI spelling
(`allow-concurrent`) is accepted too; anything else is rejected with an error naming
the valid values. Omitting it sends no value, so `create_cron_job` gets `skip` and
`update_cron_job` leaves the stored policy untouched. See the cron-jobs
guide's _Overlap and concurrency_ section for what each policy does.

`update_repo` accepts `linear_api_key`, `sentry_api_key`, and `sentry_org`. The
two API keys are **write-only secrets**: set them through `update_repo`
and `list_repos` and `update_repo` redact them from their
results (the non-secret `sentry_org` slug is still returned).

`clone_and_register_repo` requires both `clone_url` and `local_path` (the
destination to clone into); `name`, `default_base_branch`, and `setup_script`
are not required. `local_path` must be absolute, and `clone_url` must be an
`http(s)://`, `ssh://`, `git://` or `file://` URL, an scp-style `user@host:path`,
or an absolute local path. Option-shaped values (leading `-`) and
`<transport>::` forms such as `ext::` are rejected with `InvalidArgument`
before anything touches disk, and `git clone` always receives `--` before the
URL.

`register_repo` and `clone_and_register_repo` store the global
`settings.worktree_base_dir` as the repo's worktree base, and `update_repo`
accepts an absolute `worktree_base_dir` to change it (it cannot be cleared).

`send_chat_message` delivers a user message into a live agent chat via
`agent_session_id`. Set `wake_if_asleep: true` to wake the agent before
delivery. Returns `delivered` and `tmux_session_name` on success.

The message is submitted by default: omit `submit` and it is Enter-submitted
and the submission verified. Pass `submit: false` only to stage a message in the
target's composer without sending it. That leaves the chat idle until a human
opens it and presses Enter, so it is for deliberate staging (queueing issue or
PR context before an operator starts work), not for ordinary delivery. The same
default applies to `boss chat send`, whose `--submit=false` is the matching
opt-out.

`get_settings` / `update_settings` read and partially update the daemon's global
`settings.json`, the same TUI-editable surface the boss settings screen edits:
`worktree_base_dir`, `poll_interval_seconds`, `default_agent`, event/error
tracing, PostHog token/host, and each configured agent's `enabled` flag plus its
dynamic `config` map. `update_settings` is a partial update (only the fields you
supply change) with TUI-identical validation; an invalid field returns an error
and leaves the file untouched. Per-agent config keys upsert (an empty value
deletes the key); discover an agent's settable keys, types, and allowed values
via `list_agents`. These are **local-daemon** tools and are not served by the
hosted MCP gateway.

The callback, broadcast, and note tool families each have their own guide:
[GitHub callbacks](./github-callbacks.md) covers
`register_github_callback` / `list_github_callbacks` / `delete_github_callback`,
[Broadcasts](./broadcasts.md) covers `send_broadcast`,
`register_broadcast_subscription`, and their list/delete counterparts, and
[Notes](./notes.md) covers `create_note`,
`update_note`, `list_notes`, `get_note`, and `delete_note`. The six
organization-note tools (`list_organization_notes`, `get_organization_note`,
`get_organization_note_quota`, `create_organization_note`,
`update_organization_note`, `delete_organization_note`) reach your organization's
notes in Bossanova Cloud instead: they work only through the hosted endpoint, and
the local server answers them with `failed_precondition` pointing at
`boss notes org`. Every organization-note tool requires `organization_id`; there is no default organization. Cloud membership, plan entitlement, author-or-owner edits and the hourly write quota apply. The CLI equivalents are `boss notes org ls|show|add|edit|rm|quota`.

For example, an agent that records a finding at the end of a nightly run calls
`create_organization_note` with:

```json
{
  "organization_id": "3f9c2a7d1e4b8c06",
  "body": "The deploy fixture needs a configurable timeout.",
  "tags": ["flaky"],
  "idempotency_key": "nightly-2026-10-09"
}
```

It returns the stored note, with its `id` and its `expires_at` 90 days out. A
retry with the same `idempotency_key` returns that same note and spends no
quota. Once the organization's hourly limit is spent, the call fails instead
and the error text reports the usage and reset time:

```text
resource_exhausted: organization note quota of 1000 writes per hour is spent; retry after 2026-10-09T15:00:00Z (quota: 1000 of 1000 writes used this hour; resets at 2026-10-09T15:00:00Z)
```

[Organization notes](./notes.md#organization-notes-paid-plans) covers sync,
permissions, the hourly window and retention.

### Project config

`export_project_config`, `diff_project_config` and `apply_project_config` do what
`boss project export`, `diff` and `apply` do for a `.boss-project.json` project config,
with the file's contents passed as the `config` argument. Through the local server they
cover this daemon's repo registration, settings and crons; through the hosted endpoint they
cover your cloud triggers, the organization's session webhooks and the crons the file places
on your daemons. `apply_project_config` writes nothing unless you pass `"dry_run": false`.
It never deletes a row, resolves a `BOSS_*` secret or mints a signing secret: those changes
show up as skips to apply with the CLI. Locally, a write also needs the checkout on the
repo's default branch unless you pass `"allow_branch": true`.

Local MCP never grants repo trust: a `repo.isTrusted: true` change is a `skip` with reason
`trust-via-cli`, left for `boss project apply` with terminal confirmation or `--yes`.
Revoking trust still applies over local MCP. Export omits `repo.isTrusted`, so declaring
trust requires a deliberate config edit.

The `config` argument is a JSON object, validated before a backend call; these tools never read a config file. Local export requires `working_dir` and returns the same content as `boss project export`. For diff or apply, `working_dir` selects the checkout when no `daemons[]` entry matches this daemon; a registered repo's worktree stands for its main checkout.

Hosted export requires `repo_origin_url` and `organization_id` and omits `daemons` and `crons`. Hosted diff or apply takes `organization_id` or `cloud.organization`; conflicting values are refused. Hosted apply includes crons placed on your daemons. A factory cron without a live local row is reported as `create-via-cli` because its gate contains host paths. If writes begin and then fail, inspect the result's `error` alongside `applied: true`; refusals before writing are tool errors.

### Destructive — require `confirm: true` (15)

`remove_repo`, `remove_session`, `close_session`, `merge_session`, `archive_session`, `resurrect_session`, `delete_chat`, `empty_trash`, `delete_cron_job`, `remove_account`, `delete_github_callback`, `delete_broadcast`, `delete_broadcast_subscription`, `delete_note`, `delete_organization_note`

### `merge_session` results carry a `detail` note

On success `merge_session` returns the session object exactly as `close_session`,
`archive_session` and `resurrect_session` do (the session's own fields stay at the
top level), plus one optional sibling key, `detail`. The payload shape is otherwise
unchanged, so if you read `id` or `pr_number` off a merge result you are unaffected.

The `detail` string is the daemon's note about what it actually did, most
importantly a **merge-strategy substitution**: a rebase the repository's configured
strategy asked for, which GitHub refused, so the daemon squashed instead. Without it
you cannot tell a plain merge from a substituted one.

The key is **omitted entirely** when the note is empty, so its presence is
meaningful: do not expect a `detail` field on every successful merge, and do not
read its absence as an error.

A merge _refusal_ is not a `detail`. It comes back as an error result whose text
reaches you verbatim, including the `MERGE_STRATEGY_INCOMPATIBLE` token you can
branch on.

`detail` is **always empty** on the hosted gateway path. The orchestrator response
behind the hosted endpoint carries only the session, so the note cannot cross the
remote boundary; only the local `bin/mcp` server reports it.

### `merge_session` `match_head` pins the merged commit

On local and hosted servers, PR merges require `match_head` (a 40-character hex commit
SHA). Omitting it returns `EXPECTED_HEAD_REQUIRED` without merging. Local-only
sessions with no PR need no pin. Pass the commit you verified to merge only if
the PR head is still exactly that commit. The daemon checks it against the live PR
head first, and GitHub enforces it again atomically at merge time
(`gh pr merge --match-head-commit`), so a push that lands in between is never
merged unverified.

The pinned head must also carry a successful `boss/verify` verdict. A head without
one returns `NO_VERDICT`, naming the head and the remedies: run `/boss-verify` or
use `boss merge --approve` to record an operator approval and merge.

A moved head comes back as an error result carrying the `HEAD_MISMATCH` token,
for example `HEAD_MISMATCH: expected head <pin>, live head <sha>`. Re-verify the
new head before merging again. GitHub sometimes reports a mismatch for a few seconds
after a push that equals the pin, and the `live head` in the message tells that
lag apart from a real move. A malformed SHA is rejected as an invalid argument,
and a pin on a session with no PR is refused.

The hosted endpoint forwards `match_head` as the orchestrator merge head pin.
It enforces the same expected-head and verify-verdict requirements as the local server.

## What the status values mean (and do not)

Three values on the status tools have each been read, in a live run, as a signal
they do not carry. The tool descriptions state this too (an agent caller never
sees this page), but the reasoning only fits here.

### `get_session`: `state` and `last_check_state` carry no push information

A `state` transition, and a `last_check_state` appearing where there was none,
both fire when the daemon re-polls CI checks that already exist. Neither says a
commit reached the remote. On one epic run this fired once per child while every
branch still held only its bootstrap commit, and the driver evaluated merge rails
against an effectively empty branch.

`last_check_state=UNSPECIFIED` is also the honest answer for a stale, missing, or
non-demonstrated verdict at the current head. Inspect
`last_check_state_observed`, `last_check_state_head_sha`, and
`last_check_state_at` to see the raw cached latch and where it came from.

The push oracle is the remote itself:

```bash
git fetch --quiet origin
git rev-list --count origin/<base>..origin/<branch> 2>/dev/null || echo 0   # 0 = nothing pushed
```

Keep the guard. Before the first push there is no `origin/<branch>` at all, and
`git rev-list` then exits non-zero with empty output instead of printing `0`;
that error is also "nothing pushed", not an unreadable oracle.

### `last_output_at` is a floor, not liveness

`get_chat_statuses.last_output_at` is the last time the captured pane **changed**
— any change at all. A spinner's elapsed-time counter redrawing once a second
keeps it perpetually fresh, so an advancing `last_output_at` is nearly as
uninformative as a frozen one: it cannot tell productive work from a wedged loop,
and it cannot tell either from an agent sitting inside an awaited subagent that
emits nothing to the parent pane.

It is also **not unique per chat**. Every chat first observed in one poll tick is
seeded with that tick's single `now`, so the value can be identical to the
nanosecond across every chat in every session for a dozen cycles before diverging.
A staleness comparison written against it can therefore never pass.

Use the fields that do discriminate, all on `ChatStatusEntry`:

| Field                        | What it tells you                                                 |
| ---------------------------- | ----------------------------------------------------------------- |
| `spinner_present`            | the pane is rendering a spinner right now — the agent is mid-turn |
| `last_substantive_output_at` | last pane change that was **not** just a spinner redraw           |
| `last_output_seeded`         | this timestamp is the poller's seed value, not an observed change |

A chat is **settled** when its status is `IDLE` or `STOPPED` across two
consecutive polls with `spinner_present` false, not when `last_output_at` looks
stale.

### `get_session_statuses` is aggregate only

`SessionStatusEntry` carries `session_id`, `status` and `waiting_reason` and no
timestamps at all, and the roll-up hides which chat won. Use `get_chat_statuses`
for anything per-chat.

## Streaming limitations

- **`AttachSession`**: streaming session attachment is not yet exposed as an MCP tool.
- **`CreateSession` streaming**: the `create_session` tool drains the setup stream internally
  and returns the terminal `Session` object. Callers do not receive intermediate status events.
- **Live chat output streaming**: `get_chat_transcript` returns the current transcript snapshot
  and `send_chat_message` delivers a message; streaming live output token-by-token as it is
  generated remains deferred. Reading the final result (`get_chat_transcript`) and injecting
  a follow-up prompt (`send_chat_message`) are **shipped** in v1.

## Driving a chat headlessly

1. Call `create_session` with `repo_id`, `prompt` and optionally `agent` (for example, `codex`). The result includes `id` and the primary chat's `agent_session_id`; a separate `record_chat` call is unnecessary. A prompt defaults to unattended execution. Check `agent_launched` and `attached_existing`: attaching to an existing branch or PR does not run your supplied prompt, so send it to the returned chat. `agent_launched` alone does not prove the agent consumed the prompt; check `get_chat_statuses`.
2. Poll `get_chat_transcript` with `{ session_id, agent_session_id }` until `exists` is true and `final_assistant_text` contains the result. There is no blocking wait: choose a bounded interval and timeout, such as every three seconds for up to five minutes. On follow-up turns, compare with the prior transcript so an earlier result cannot satisfy your wait. Passing `session_id` also allows hosted reads before the chat row has propagated.
3. Use `get_chat_statuses` for per-chat progress. Confirm a settled status across consecutive polls with `spinner_present` false as described above; `CHAT_STATUS_QUESTION` requires attention rather than proving completion.
4. Continue with `send_chat_message` and `{ agent_session_id, message, wake_if_asleep: true }`. Omitted `submit` means submit and verify. Inspect `delivered` and the delivery/turn-start state in the response. A hosted call immediately after creation might briefly return `not_found`; retry after a short delay or resolve the chat through its transcript first.

## Hosted MCP

Connect to `https://mcp.bossanova.dev/mcp` using a client that supports remote
Streamable HTTP and OAuth. First sign in at [bossanova.dev](https://bossanova.dev)
once to create your Bossanova account, then connect your client:

- **Claude Code:** run the command below, then run `/mcp` in Claude Code to log in.
- **Claude.ai:** open Settings → Connectors → add a custom connector and enter
  `https://mcp.bossanova.dev/mcp` as the server URL.
- **Cursor:** add this server to your `mcp.json`, then follow its sign-in prompt.

```bash
claude mcp add --transport http bossanova https://mcp.bossanova.dev/mcp
```

```json
{
  "mcpServers": {
    "bossanova": {
      "url": "https://mcp.bossanova.dev/mcp"
    }
  }
}
```

The browser opens WorkOS AuthKit to authenticate with your existing Bossanova
identity. If you belong to multiple organizations, choose the organization your
MCP calls should act in. Reconnect the client to choose a different organization.
Clients that request `offline_access` and support refresh tokens refresh access
automatically without another browser login. A client that does not refresh may
ask you to sign in again when its access token expires.

The gateway routes tool calls to your daemon through the orchestrator. Requests require
a bearer access token. Missing or invalid credentials return `401` with a
`WWW-Authenticate` challenge. OAuth discovery is available at
`/.well-known/oauth-protected-resource` and
`/.well-known/oauth-protected-resource/mcp` for the
`https://mcp.bossanova.dev/mcp` resource.

The gateway advertises the permitted tools from a 59-tool subset of the local catalog (23 read-only,
24 mutating, 12 destructive): every session/repo/chat lifecycle tool, including the
destructive ones (which still require `confirm: true`), the cron-job mutators, and
the GitHub-callback, note, organization-note and project-config tools. Organization-note tools call the cloud API directly. `switch_account` is proxiable too: it acts on a
session's live chat, so it routes like any other session operation.

The other 20 tools stay local-only, because they have no session/daemon-routed
backing RPC: repo bootstrap (`resolve_context`, `validate_repo_path`,
`register_repo`, `clone_and_register_repo`), the six account tools
(`list_accounts`, `add_account`, `refresh_account`, `update_account`,
`remove_account`, `test_account`), whose credentials never leave your daemon;
the six broadcast tools; the daemon settings tools (`get_settings`,
`update_settings`); `start_repair_workflow`; and `refresh_session_pr`.

### Personal API tokens

Create a token under **Settings → API tokens** in Bossanova Cloud and select
its permissions. Send it on every request as `Authorization: Bearer boss_pat_…`.
The token never expires unless you set an expiry. Revoke it in the same settings
page. The advertised tool list can take up to 30 seconds to reflect a permission
change; proxied calls always check authorization immediately.

For a generic HTTP MCP client, configure the endpoint and header:

```json
{
  "mcpServers": {
    "bossanova": {
      "type": "http",
      "url": "https://mcp.bossanova.dev/mcp",
      "headers": { "Authorization": "Bearer boss_pat_REPLACE_WITH_YOUR_TOKEN" }
    }
  }
}
```

For [Hermes](./hermes.md), configure its HTTP MCP connection with this hosted
endpoint and header instead of connecting to a local `bossd`. A read-only token
shows the tools its grants allow, without inviting calls to mutating tools.

### Hosted-only tools (Bossanova cloud)

Two tool families appear **only** on the hosted gateway, on top of the proxiable
subset above: the inbound-trigger tools and the session webhook tools. Both manage
state that lives in Bossanova Cloud, which the local `bin/mcp` server cannot reach,
so it never lists them and they are not part of the local tool counts on this
page. In the gateway's read-only mode only each family's read tools appear.

#### Inbound triggers

| Tool                  | Description                                                                                         |
| --------------------- | --------------------------------------------------------------------------------------------------- |
| `get_trigger_catalog` | List the trigger types, event types and filter fields `save_trigger` accepts                        |
| `list_triggers`       | List the triggers you created, optionally in one organization; secrets are never returned           |
| `get_trigger`         | Get a trigger plus its 20 most recent invocations, each saying why it did or did not launch         |
| `save_trigger`        | Create (no `id`), update (`id`; only the fields you pass change), or rotate the HTTP signing secret |
| `test_trigger`        | Run a sample payload through a trigger's filters and policies; launches only with `should_launch`   |
| `delete_trigger`      | Delete a trigger with its secret and invocation history; requires `confirm: true`                   |

`save_trigger` returns the HTTP signing secret only when it creates a trigger or
rotates its secret (`id` plus `rotate_secret: true` and nothing else). Store it
then: it is never shown again, only replaced by another rotation. Enum fields take
the proto value names, with or without their type prefix (`CANCEL_IN_PROGRESS`
or `TRIGGER_CONCURRENCY_POLICY_CANCEL_IN_PROGRESS`). `concurrency_policy` takes
`SKIP_IF_RUNNING` (the default), `CANCEL_IN_PROGRESS` or `ALLOW_PARALLEL`. Validation errors come back exactly
as the API reports them.

There is no local equivalent yet: no `boss trigger` command exists, so the hosted
endpoint and the `OrchestratorService` trigger RPCs are the ways to manage
triggers today.

#### Session webhooks

| Tool                               | Description                                                                                  |
| ---------------------------------- | -------------------------------------------------------------------------------------------- |
| `list_session_webhook_event_types` | List the event types a webhook subscribes to, each with a sample payload                     |
| `list_session_webhooks`            | List the session webhooks visible to you (see below); secrets are never returned             |
| `list_session_webhook_deliveries`  | Page a webhook's delivery history, newest first, filtered by status, event type or test flag |
| `get_session_webhook_delivery`     | Get one delivery with its attempts, the exact request body and the request headers           |
| `save_session_webhook`             | Create (no `id`), update (`id`; only the fields you pass change), or rotate the secret       |
| `test_session_webhook`             | Send one test delivery of an event type and return it with its attempt                       |
| `delete_session_webhook`           | Delete a webhook with its delivery history; requires `confirm: true`                         |

`save_session_webhook` creates a webhook when you pass no `id`; `url` and a
non-empty `event_types` are required, and the webhook starts enabled unless you
pass `is_enabled: false`. With an `id` it updates only the fields you pass, and a
non-empty `event_types` replaces the whole set. With an `id` plus
`rotate_secret: true` and nothing else it rotates the signing secret. Combining
`rotate_secret` with other fields, or passing an `id` with nothing to change, is
refused before anything is sent. The server always generates the secret, and it
appears only in a create or rotate result. Store it then: it is never shown
again. To choose your own secret, use `boss webhook add --secret-file`.

Session webhooks are owner-only. Every tool requires the OWNER role in the
organization, and the tools add no permission check of their own: a refusal
comes back exactly as the API reports it. Every tool accepts an
`organization_id`; leave it out to act on your active organization, which is the
only one the API accepts.

Unless the organization shares member sessions, only a webhook's creator acts on
it. `list_session_webhooks` shows your own webhooks plus any whose creator is no
longer an owner, marked `is_retire_only` with an empty `url`: those accept only a disable
(`is_enabled: false` alone) or a delete, and any other tool answers
`FAILED_PRECONDITION`. Another owner's webhook is not listed, and its id
answers `NOT_FOUND`.
