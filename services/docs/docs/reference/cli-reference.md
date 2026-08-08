---
sidebar_position: 2
title: CLI Reference
description: Pointers to the authoritative help text for boss and bossd.
---

import CommandTabs from '@site/src/components/CommandTabs';

# CLI Reference

The authoritative reference for every command, subcommand, and flag is
the help text built into the binary:

```bash
boss --help
boss <subcommand> --help
```

Read the help text first. The two invocations above are that reference itself
rather than an example of an operation, so they stay a plain block; every `boss`
example below carries its interface tabs. The subcommand and flag tables are
reference material rather than examples, so they stay plain too. The examples
show the same operations through the other two interfaces as well, but they are
illustrative — the binary is what is authoritative.

## Top-level commands

### `boss`

The interactive terminal UI and the CLI for non-interactive operations.

Launch the Terminal UI (TUI) on the Home screen:

<CommandTabs
cli="boss"
/>

The TUI is a terminal program on this machine, so there is no chat prompt and no
MCP tool for launching it. An agent reaches the same data through read tools
instead — `list_sessions` and `get_session_statuses` for the session state the
home screen shows, plus the tools named below. See the
[MCP guide](/guides/mcp) for the full catalog.

View global settings:

<CommandTabs
chat='"show me my boss settings"'
cli="boss settings"
mcp="get_settings"
/>

The same command _writes_ settings when you give it a flag — `boss settings --help`
lists them — and the MCP counterpart for that path is `update_settings`.

List configured repos:

<CommandTabs
chat='"list my repos"'
cli="boss repo ls"
mcp="list_repos"
/>

Change repo fields from the shell:

<CommandTabs
chat='"turn on auto-merge for the bossanova repo"'
cli="boss repo update <repo-id> ..."
mcp="update_repo"
/>

Health-check the auto-repair pipeline:

<CommandTabs
chat='"run the repair doctor"'
cli="boss repair doctor"
mcp="repair_doctor"
/>

### `boss --host` (drive a daemon on another machine)

`--host` points `boss` at a `bossd` running on another machine, over an
SSH-forwarded unix socket. Every command works the same way it does locally,
apart from a handful that act on the machine `boss` runs on.

| Flag                   | Description                                                                              |
| ---------------------- | ---------------------------------------------------------------------------------------- |
| `--host <dest>`        | SSH destination of the machine whose `bossd` to drive                                    |
| `--host-socket <path>` | Remote `bossd` socket path, skipping discovery when remote `boss` is not on the SSH PATH |

See the [Remote Daemons guide](/guides/remote-daemons) for prerequisites, a
first-connection walkthrough, how `--host` differs from `--remote`, chat-pane
attach over SSH, and the commands that stay local.

### `boss mcp`

Manage the local MCP server that exposes Bossanova to AI agents.

| Subcommand           | Description                                                                                     |
| -------------------- | ----------------------------------------------------------------------------------------------- |
| `boss mcp install`   | Install and start the local MCP service (`--port`, `--force`)                                   |
| `boss mcp status`    | Show whether the service is installed / running, plus the instance inventory                    |
| `boss mcp start`     | Start or restart the installed service                                                          |
| `boss mcp stop`      | Stop the managed service, sweep stray/orphaned instances, leave live session-owned ones running |
| `boss mcp uninstall` | Stop and remove the service file                                                                |

See the [MCP guide](/guides/mcp) for agent wiring and the tool catalog.

### `boss upgrade`

Check for and install Bossanova upgrades.

| Flag              | Description                                                           |
| ----------------- | --------------------------------------------------------------------- |
| `--check`         | check for an upgrade without installing                               |
| `--yes`           | required to install; without it the command refuses                   |
| `--version <tag>` | install a specific stable release tag (prereleases are not supported) |
| `--no-restart`    | do not restart the daemon after upgrade                               |

See [Upgrade](/upgrade) for the full upgrade guide.

### `boss new` and `boss chat` (scripted chat control)

Create a session non-interactively and drive its chat from the shell (or, with the
same reach, from an MCP agent):

<CommandTabs
chat='"start a session on the bossanova repo to fix the flaky login test"'
cli="boss new --repo <r> --prompt <p> --detach"
mcp="create_session"
/>

The CLI form creates the session, prints its session-id and chat-id, and exits.

`boss new` runs non-interactively when both `--repo` and `--prompt` are given (`--detach`
is then implicit; `--no-attach` is an alias). `--agent <a>` overrides the default agent
plugin; `--title <t>` is optional (auto-derived from the prompt when absent).

| Subcommand                                   | Description                                                                                   |
| -------------------------------------------- | --------------------------------------------------------------------------------------------- |
| `boss chat send <session-id\|chat-id> <msg>` | Deliver a follow-up message to a running chat; wakes a sleeping chat                          |
| `boss chat show <session-id\|chat-id>`       | Print the transcript (`--result-only` for just the final result, `--limit N` to cap messages) |
| `boss chat wait <session-id\|chat-id>`       | Block until the chat is idle / waiting, then print the final result (`--timeout 30m`)         |

A `<session-id>` targets that session's primary chat; a `<chat-id>` (the
`agent_session_id` printed by `boss new --detach`) targets a specific chat.

### `bossd`

The background daemon. Normally started by Homebrew's launchd plist or
your equivalent service manager. You rarely run it by hand. It takes
configuration from environment variables and `settings.json`; there
are no subcommands. It is the daemon binary rather than the `boss` CLI, so
these invocations have no chat or MCP form and stay a plain block:

```bash
bossd               # run in foreground
bossd --version     # print version info
```

## Settings overrides

Most settings can be overridden by environment variable. Precedence
(highest wins): environment variable → `settings.json` → hardcoded
default. See
[Settings → Environment overrides](./settings.md#environment-overrides)
for the table.

A hand-curated reference page is on the roadmap. If you hit something
ambiguous in the help text, open an issue at
[bossanova-dev/bossanova/issues](https://github.com/bossanova-dev/bossanova/issues).
