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

### `boss --host` (drive a daemon on another machine)

`--host` points `boss` at a `bossd` running on another machine, over an
SSH-forwarded unix socket. Every command works the same way it does locally,
apart from the handful noted below that act on the machine `boss` runs on:

```bash
boss --host workstation                 # TUI against the daemon on `workstation`
boss --host deploy@bastion repo ls      # one-shot CLI against a remote daemon
```

`--host` takes a standard SSH destination — `user@host`, a bare `host`, or a
`Host` alias from your `~/.ssh/config` — and hands that string to `ssh`
**unparsed**. Nothing is split, substituted, or rewritten, so `User`, `Port`,
`IdentityFile`, `ProxyJump`, and `HostName` from your SSH config all keep
working exactly as they do when you type `ssh <destination>` yourself.

| Flag                   | Description                                                                              |
| ---------------------- | ---------------------------------------------------------------------------------------- |
| `--host <dest>`        | SSH destination of the machine whose `bossd` to drive                                    |
| `--host-socket <path>` | Remote `bossd` socket path, skipping discovery when remote `boss` is not on the SSH PATH |

Things worth knowing:

- `--host` and `--remote` cannot be combined. They select different daemons —
  an SSH tunnel to another machine's `bossd` versus the cloud orchestrator — so
  `boss` refuses rather than silently preferring one.
- `--host` never starts a local daemon. You asked for the daemon on that host;
  booting one here would drive the wrong machine and hide the real failure.
- `--host-socket` is the escape hatch when discovery fails. `boss` finds the
  remote socket by running `boss env --json` over SSH, and a non-interactive
  SSH login gets a reduced `PATH` — if remote `boss` is not on it, pass the
  socket path directly.
- **Attaching to a chat pane works over `--host`.** The pane is a `tmux`
  session on the remote machine, so `boss` attaches with
  `ssh -t <destination> 'exec "$SHELL" -lc '\''tmux -u attach -t <session>'\'''`
  rather than driving the local `tmux`. The remote `tmux` runs through that
  host's **login** shell for the same reason `--host-socket` exists: a
  non-interactive SSH login gets a reduced `PATH`, and a `tmux` installed
  outside it (Homebrew's `/opt/homebrew/bin`, exported from a `~/.zprofile`
  that `zsh` does not read non-interactively) would otherwise fail every attach
  with `tmux: command not found`. Because the attach runs remotely,
  `boss --host` does **not** require `tmux` on your local machine — only `ssh`.
  `Ctrl+X` detaches back to the local TUI as usual. If the
  connection drops mid-pane you are ejected back to the local TUI — the remote
  `tmux` session survives, so attaching again returns you to the same pane, and
  `boss fix-terminal` restores a terminal left in a strange state by the drop.
- **Running `boss --host` inside `tmux` makes the prefix key ambiguous.** You
  end up with two `tmux` clients in one terminal — your local one and the remote
  one inside the pane — and both answer to the same prefix (`Ctrl+B` by
  default). The outer `tmux` wins, so the remote one needs the prefix pressed
  twice (`Ctrl+B` `Ctrl+B`, then the command). `boss` does not rebind your
  prefix to work around this; if you attach over `--host` often, set a distinct
  prefix in the outer session's own `~/.tmux.conf`.
- **The remote host needs a `terminfo` entry for your `TERM`.** `boss`
  normalizes `TERM` locally, but the `tmux` client now runs on the other
  machine and reads that machine's `terminfo` database. If an attach fails with
  a missing-terminal error, the error screen shows the reproduction command;
  install the entry with `infocmp -x $TERM | ssh <destination> tic -x -`.
- **The `[t]erminal` action is hidden under `--host`.** It opens a session's
  worktree in a new **local** terminal tab, and under `--host` that path is on
  the other machine. Chat titles are likewise not backfilled locally — the
  remote daemon does that job itself.
- `boss account add` is refused against `--host`. Registration mints
  credentials by running a **local** subprocess, so the credential would never
  belong to the remote machine. Run it in a shell on that host instead.
- The `boss daemon` subcommands are refused against `--host`. They install,
  start, stop, and uninstall **this** machine's `bossd` through local
  subprocesses, so they would manage the wrong daemon. Run
  `ssh <destination> boss daemon ...` instead.
- A dropped tunnel does not kill `boss`. The connection is supervised and
  redialled with backoff, so a blip only fails the in-flight request. While it
  is down the TUI says `Reconnecting to <destination>` and points at the SSH
  checks for that host, and the session resumes on its own once SSH is back —
  no user action, and no local daemon to restart.

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
