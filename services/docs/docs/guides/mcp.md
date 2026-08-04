---
title: MCP Server (Agent & CLI Control)
description: Control Bossanova sessions, repos, and cron jobs from AI agents via the Model Context Protocol.
slug: /guides/mcp
---

# MCP Server

Bossanova ships a local [Model Context Protocol (MCP)](https://modelcontextprotocol.io/) server that lets AI coding agents — Claude Code, Claude Desktop, and any MCP-capable host — drive Bossanova directly: list and create sessions, manage repositories, inspect CI, and schedule cron jobs. Anything you can do from the TUI or the `boss` CLI, an agent can do through MCP.

The server exposes **69 tools** in three tiers:

| Tier        | Count | Behaviour                          |
| ----------- | ----- | ---------------------------------- |
| Read-only   | 24    | Always available                   |
| Mutating    | 31    | Non-destructive writes             |
| Destructive | 14    | Require `confirm: true` to execute |

## Install

Build the `bin/mcp` binary:

```bash
make build-mcp          # produces bin/mcp
```

That binary is all you need to wire up a stdio MCP host such as Claude Code or
Claude Desktop — those hosts spawn `bin/mcp` themselves over stdio (see
[Connect an agent](#connect-an-agent) below), so they do **not** require the
service install below.

### Optional: run `bin/mcp` as a standalone HTTP daemon

:::note Optional — most users can skip this
Stdio MCP hosts (Claude Code, Claude Desktop) spawn `bin/mcp` themselves and
never need this. Install the HTTP daemon only if you want an always-on
`bin/mcp` reachable over HTTP — for HTTP-capable MCP clients, `curl`, or a
browser-based inspector.
:::

```bash
boss mcp install        # install and start the local MCP HTTP daemon
boss mcp status         # show whether the service is installed / running, plus the instance inventory
boss mcp start          # start or restart the installed service
boss mcp stop           # stop the managed service and sweep stray/orphaned boss-mcp processes
boss mcp uninstall      # stop and remove the service file
```

`boss mcp install` runs `mcp --http 127.0.0.1:<port>` (serving `/mcp`) under the
platform user service manager — launchd (`~/Library/LaunchAgents/com.bossanova.mcp.plist`)
on macOS, or systemd (`~/.config/systemd/user/bossanova-mcp.service`) on Linux.
It accepts `--port <n>` (default 8765) and `--force` (overwrite an existing
service file).

#### What `boss mcp stop` owns, and what it leaves alone

`boss mcp stop` only touches the service manager when the service is actually
installed — so on a machine that never ran `boss mcp install` it does nothing
there — and its "Idempotent." guarantee is now a verified end state, not just
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
unattributable — it sweeps it as stray, and `KeepAlive` / `Restart=always`
respawns it under a new PID. Recover by re-creating the service file and
re-loading it: `boss mcp install --force` (which may report the job as already
loaded), then `boss mcp start`.

A live session-owned server is left alone on purpose: an MCP host does not
respawn a dead stdio server mid-session, so killing one would silently strip
the `mcp__boss__*` tools from a running chat. Each exits with its own chat
when that chat ends. `boss mcp status` reports this same inventory on its
`instances:` line.

## Connect an agent

stdio MCP hosts (Claude Code, Claude Desktop) spawn the binary themselves — you
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

**Claude Desktop** — `~/Library/Application Support/Claude/claude_desktop_config.json`, same `mcpServers` block; restart Claude Desktop after saving. Verify with `/mcp` in Claude Code or the tools panel in Claude Desktop — the `bossanova` tools should appear.

> **Environment variables in `.mcp.json`.** `${VAR}` placeholders in
> `.mcp.json` (for example `Authorization: Bearer ${LINEAR_API_KEY}`) are
> resolved from the agent session's environment. Bossanova automatically
> loads a worktree's `.env` into that environment, so putting the value in
> the worktree `.env` is enough for it to resolve — see
> [Automatic `.env` loading](./setup-scripts.md#automatic-env-loading).

## Modes

- **stdio** (default) — `bin/mcp` with no flags; the MCP host spawns it and talks over stdin/stdout.
- **Streamable HTTP** — `bin/mcp --http 127.0.0.1:7474` (any free loopback port) serves `/mcp` and `/healthz`, useful for `curl` or a browser-based MCP inspector. Pass `--socket /path/to/bossd.sock` for a non-default bossd socket.

Pass `--read-only` to register only the 24 read-only tools; mutating and destructive tools then never appear in `tools/list`.

## Destructive tools need confirmation

The 14 destructive tools (`remove_repo`, `remove_session`, `delete_chat`, `empty_trash`, …) refuse to run unless the caller passes `"confirm": true`:

```
remove_repo is destructive and requires {"confirm": true}; re-call with confirm set once you are sure
```

This prevents an agent from accidentally deleting a repo, session, or chat.

## Tool reference

### Read-only (24)

| Tool                           | Description                                                                                |
| ------------------------------ | ------------------------------------------------------------------------------------------ |
| `list_sessions`                | List sessions, optionally filtered by repo, states, or archived flag                       |
| `get_session`                  | Get a single session by id                                                                 |
| `list_repos`                   | List every registered repository                                                           |
| `list_repo_prs`                | List open pull requests for a repository                                                   |
| `list_tracker_issues`          | List issues from an external tracker (Linear, Sentry)                                      |
| `resolve_context`              | Resolve repo + session for a working directory                                             |
| `validate_repo_path`           | Validate a local path is a usable git repo                                                 |
| `list_chats`                   | List agent chats for a session                                                             |
| `get_chat_statuses`            | Get live chat status for a session                                                         |
| `get_session_statuses`         | Get best live status across chats for multiple sessions                                    |
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

### Mutating (31)

`register_repo`, `clone_and_register_repo`, `update_repo`, `create_session`, `stop_session`, `pause_session`, `resume_session`, `retry_session`, `update_session`, `link_session_pr`, `start_chat`, `record_chat`, `update_chat_title`, `wake_chat`, `report_chat_status`, `create_cron_job`, `update_cron_job`, `run_cron_job_now`, `add_account`, `refresh_account`, `update_account`, `test_account`, `send_chat_message`, `switch_account`, `update_settings`, `start_repair_workflow`, `register_github_callback`, `send_broadcast`, `register_broadcast_subscription`, `create_note`, `update_note`

`send_chat_message` delivers a follow-up message into a live agent chat via its
`agent_session_id`; set `wake_if_asleep: true` to wake the agent before delivery.

The callback, broadcast, and note tool families each have their own guide:
[GitHub callbacks](https://docs.bossanova.dev/guides/github-callbacks) covers
`register_github_callback` / `list_github_callbacks` / `delete_github_callback`,
[Broadcasts](https://docs.bossanova.dev/guides/broadcasts) covers `send_broadcast`,
`register_broadcast_subscription`, and their list/delete counterparts, and
[Notes](https://docs.bossanova.dev/guides/notes) covers `create_note`,
`update_note`, `list_notes`, `get_note`, and `delete_note`.

### Destructive — require `confirm: true` (14)

`remove_repo`, `remove_session`, `close_session`, `merge_session`, `archive_session`, `resurrect_session`, `delete_chat`, `empty_trash`, `delete_cron_job`, `remove_account`, `delete_github_callback`, `delete_broadcast`, `delete_broadcast_subscription`, `delete_note`

## Hosted MCP

A hosted endpoint at `mcp.bossanova.dev` — WorkOS-authenticated and routed to your
own daemon, so you can drive Bossanova from agents without running `bin/mcp` locally —
is **coming soon**. Until it ships, use the local `bin/mcp` server described above.

When it ships, the gateway advertises a 50-tool proxiable subset (18 read-only,
21 mutating, 11 destructive) — every session/repo/chat lifecycle tool, including the
destructive ones (which still require `confirm: true`), the cron-job mutators, and
the GitHub-callback and note tools. `switch_account` is proxiable too: it acts on a
session's live chat, so it routes like any other session operation.

The other 19 tools stay local-only, because they have no session/daemon-routed
backing RPC: repo bootstrap (`resolve_context`, `validate_repo_path`,
`register_repo`, `clone_and_register_repo`), the six account tools
(`list_accounts`, `add_account`, `refresh_account`, `update_account`,
`remove_account`, `test_account`) — whose credentials never leave your daemon —
the six broadcast tools, the daemon settings tools (`get_settings`,
`update_settings`), and `start_repair_workflow`.
