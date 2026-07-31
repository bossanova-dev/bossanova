---
sidebar_position: 2
title: CLI Reference
description: Pointers to the authoritative help text for boss and bossd.
---

# CLI Reference

The authoritative reference for every command, subcommand, and flag is
the help text built into the binary:

```bash
boss --help
boss <subcommand> --help
```

## Top-level commands

### `boss`

The interactive terminal UI and the CLI for non-interactive operations.

```bash
boss                              # launch the Terminal UI (TUI) on the Home screen
boss settings                      # view or update global settings
boss repo ls                       # list configured repos
boss repo update <repo-id> ...     # change repo fields from the shell
boss repair doctor                 # health-check the auto-repair pipeline
```

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
| `--yes`           | install without interactive confirmation                              |
| `--version <tag>` | install a specific stable release tag (prereleases are not supported) |
| `--no-restart`    | do not restart the daemon after upgrade                               |

See [Upgrade](/upgrade) for the full upgrade guide.

### `boss new` and `boss chat` (scripted chat control)

Create a session non-interactively and drive its chat from the shell (or, with the
same reach, from an MCP agent):

```bash
boss new --repo <r> --prompt <p> --detach   # create a session, print session-id + chat-id, exit
```

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
are no subcommands.

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
