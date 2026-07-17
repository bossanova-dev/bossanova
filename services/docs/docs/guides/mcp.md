---
title: MCP Server (Agent & CLI Control)
description: Control Bossanova sessions, repos, and cron jobs from AI agents via the Model Context Protocol.
slug: /guides/mcp
---

# MCP Server

Bossanova ships a local [Model Context Protocol (MCP)](https://modelcontextprotocol.io/) server that lets AI coding agents — Claude Code, Claude Desktop, and any MCP-capable host — drive Bossanova directly: list and create sessions, manage repositories, inspect CI, and schedule cron jobs. Anything you can do from the TUI or the `boss` CLI, an agent can do through MCP.

The server exposes **44 tools** in three tiers:

| Tier        | Count | Behaviour                          |
| ----------- | ----- | ---------------------------------- |
| Read-only   | 17    | Always available                   |
| Mutating    | 18    | Non-destructive writes             |
| Destructive | 9     | Require `confirm: true` to execute |

## Install

Build the `bin/mcp` binary:

```bash
make build-mcp          # produces bin/mcp
```

That binary is all you need to wire up a stdio MCP host such as Claude Code or
Claude Desktop — those hosts spawn `bin/mcp` themselves over stdio (see
[Connect an agent](#connect-an-agent) below), so they do **not** require the
service install.

Optionally, register `bin/mcp` as an always-on local **HTTP daemon** — useful
for HTTP-capable MCP clients, `curl`, or a browser-based inspector:

```bash
boss mcp install        # install and start the local MCP HTTP daemon
boss mcp status         # show whether the service is installed / running
boss mcp start          # start or restart the installed service
boss mcp stop           # stop the service (leaves the service file in place)
boss mcp uninstall      # stop and remove the service file
```

`boss mcp install` runs `mcp --http 127.0.0.1:<port>` (serving `/mcp`) under the
platform user service manager — launchd (`~/Library/LaunchAgents/com.bossanova.mcp.plist`)
on macOS, or systemd (`~/.config/systemd/user/bossanova-mcp.service`) on Linux.
It accepts `--port <n>` (default 8765) and `--force` (overwrite an existing
service file).

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

Pass `--read-only` to register only the 17 read-only tools; mutating and destructive tools then never appear in `tools/list`.

## Destructive tools need confirmation

The 9 destructive tools (`remove_repo`, `remove_session`, `delete_chat`, `empty_trash`, …) refuse to run unless the caller passes `"confirm": true`:

```
remove_repo is destructive and requires {"confirm": true}; re-call with confirm set once you are sure
```

This prevents an agent from accidentally deleting a repo, session, or chat.

## Tool reference

### Read-only (17)

| Tool                   | Description                                                            |
| ---------------------- | ---------------------------------------------------------------------- |
| `list_sessions`        | List sessions, optionally filtered by repo, states, or archived flag   |
| `get_session`          | Get a single session by id                                             |
| `list_repos`           | List every registered repository                                       |
| `list_repo_prs`        | List open pull requests for a repository                               |
| `list_tracker_issues`  | List issues from an external tracker (Linear, Sentry)                  |
| `resolve_context`      | Resolve repo + session for a working directory                         |
| `validate_repo_path`   | Validate a local path is a usable git repo                             |
| `list_chats`           | List agent chats for a session                                         |
| `get_chat_statuses`    | Get live chat status for a session                                     |
| `get_session_statuses` | Get best live status across chats for multiple sessions                |
| `list_check_snapshots` | List recent CI check snapshots for a session                           |
| `repair_doctor`        | Run daemon repair-doctor diagnostics                                   |
| `list_agents`          | List loaded agent-runner plugins                                       |
| `list_plugins`         | List every plugin the daemon attempted to load                         |
| `list_cron_jobs`       | List every scheduled cron job                                          |
| `get_cron_job`         | Get a single cron job by id                                            |
| `get_chat_transcript`  | Return the conversation transcript and final assistant text for a chat |

### Mutating (18)

`register_repo`, `clone_and_register_repo`, `update_repo`, `create_session`, `stop_session`, `pause_session`, `resume_session`, `retry_session`, `update_session`, `link_session_pr`, `record_chat`, `update_chat_title`, `wake_chat`, `report_chat_status`, `create_cron_job`, `update_cron_job`, `run_cron_job_now`, `send_chat_message`

`send_chat_message` delivers a follow-up message into a live agent chat via its
`agent_session_id`; set `wake_if_asleep: true` to wake the agent before delivery.

### Destructive — require `confirm: true` (9)

`remove_repo`, `remove_session`, `close_session`, `merge_session`, `archive_session`, `resurrect_session`, `delete_chat`, `empty_trash`, `delete_cron_job`

## Hosted MCP

A hosted endpoint at `mcp.bossanova.dev` — WorkOS-authenticated and routed to your
own daemon, so you can drive Bossanova from agents without running `bin/mcp` locally —
is **coming soon**. Until it ships, use the local `bin/mcp` server described above.

When it ships, the gateway advertises a 35-tool proxiable subset (15 read-only,
11 mutating, 9 destructive) — every session/repo/chat lifecycle tool, including the
destructive ones (which still require `confirm: true`) and the cron-job mutators.
Tools that stay local-only are the ones without a session/daemon-routed backing
RPC: repo bootstrap (`resolve_context`, `validate_repo_path`, `register_repo`,
`clone_and_register_repo`), the remaining repo/session metadata writers, and the
account tools (whose credentials never leave your daemon).
