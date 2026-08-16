---
title: Remote Daemons (boss --host)
description: Drive a bossd running on another machine over an SSH-forwarded unix socket.
slug: /guides/remote-daemons
---

import CommandTabs from '@site/src/components/CommandTabs';

# Remote Daemons

`boss --host` points `boss` at a `bossd` running on **another machine**, over an
SSH-forwarded unix socket. The TUI, the CLI, sessions, repos, cron jobs — every
command works the same way it does locally, apart from the handful noted in
[Commands that stay local](#commands-that-stay-local) that act on the machine
`boss` runs on.

TUI against the daemon on `workstation`:

<CommandTabs
cli="boss --host workstation"
/>

One-shot CLI against a remote daemon:

<CommandTabs
cli="boss --host deploy@bastion repo ls"
/>

Typical uses: driving a beefy build box from a laptop, or reaching a daemon
running inside a VM or on a cloud instance you already reach over SSH.

## Prerequisites

**On your local machine**, you need only `ssh`. `boss --host` does **not**
require `tmux` locally — pane attach happens on the remote host (see
[Attaching to a chat pane](#attaching-to-a-chat-pane)).

**On the remote host**, you need:

- A running `bossd`. `--host` never starts a local daemon for you, and it will
  not start a remote one either — bring the daemon up on that host first
  (`ssh <destination> boss daemon install`, or however you already manage it).
- SSH access **as the user that runs `bossd`**. The socket, its directory, and
  the auth token beside it are all owner-only, so another user's login cannot
  reach the daemon even when everything else works. Discovery reports it as
  `no bossd is reachable on <destination>`; with `--host-socket` you get
  `could not read the bossd token at … on …` instead.
- `boss` reachable on the **non-interactive** SSH `PATH`, so socket discovery
  works — or a known socket path you can pass to `--host-socket` instead. See
  [When discovery fails](#when-discovery-fails-host-socket).
- `tmux`, if you want to attach to chat panes. The pane is a `tmux` session on
  the remote machine.
- A `terminfo` entry for your `TERM`, again only for pane attach — and only if
  you want your own terminal type rather than the `xterm-256color` fallback.
  See [Terminfo on the remote host](#terminfo-on-the-remote-host).

## Connect for the first time

1. **Check plain SSH works.** `--host` hands your destination straight to
   `ssh`, so if this fails, nothing else will:

   ```bash
   ssh workstation true
   ```

2. **Check the remote `boss` is on the non-interactive `PATH`.** This is the
   exact command `boss` uses to discover the remote socket, run the same
   non-interactive way:

   ```bash
   ssh workstation boss env --json
   ```

   If that prints JSON reporting `"reachable": true` under `daemon`, discovery
   will work. If it reports `"reachable": false`, no `bossd` is running for that
   user on that host — start it there first. If the remote shell reports that
   `boss` was not found (`bash: boss: command not found`,
   `zsh: command not found: boss`), run `boss env --json` from a login shell on
   that host, note its `.daemon.socket` value, and pass that to `--host-socket`
   (below).

3. **Verify with a one-shot command before reaching for the TUI.** A CLI
   command gives you a plain error message instead of a full-screen failure,
   and `repo ls` is the cheapest end-to-end check: it dials the tunnel,
   discovers the socket, and round-trips a request through the remote daemon.
   If it lists the repos configured on **that** host — not the ones configured
   locally — the connection is good.

   <CommandTabs
   cli="boss --host workstation repo ls"
   />

4. **Launch the TUI.**

   <CommandTabs
   cli="boss --host workstation"
   />

## SSH destinations and your `~/.ssh/config`

`--host` takes a standard SSH destination — `user@host`, a bare `host`, or a
`Host` alias from your `~/.ssh/config` — and hands that string to `ssh`
**unparsed**. Nothing is split, substituted, or rewritten, so `User`, `Port`,
`IdentityFile`, `ProxyJump`, and `HostName` from your SSH config all keep
working exactly as they do when you type `ssh <destination>` yourself.

That means a jump host, a non-standard port, or a per-host key needs no
Bossanova-specific configuration at all — configure it in `~/.ssh/config` as
usual and pass the alias. `build-box` here is a `Host build-box … ProxyJump
bastion …` alias in `~/.ssh/config`:

<CommandTabs
cli="boss --host build-box"
/>

## When discovery fails: `--host-socket` {#when-discovery-fails-host-socket}

`--host-socket` is the escape hatch when discovery fails. `boss` finds the
remote socket by running `boss env --json` over SSH, and a non-interactive SSH
login gets a reduced `PATH` — if remote `boss` is not on it, pass the socket
path directly. The path must be absolute on the remote host, not this one — a
local `~` expands to the wrong home:

<CommandTabs
cli="boss --host deploy@workstation --host-socket /home/deploy/.config/bossanova/bossd.sock"
/>

## `--host` vs `--remote`

`--host` and `--remote` cannot be combined. They select different daemons — an
SSH tunnel to another machine's `bossd` versus the cloud orchestrator — so
`boss` refuses rather than silently preferring one.

| Flag             | Talks to                                | Transport                 |
| ---------------- | --------------------------------------- | ------------------------- |
| _(neither)_      | the `bossd` on this machine             | local unix socket         |
| `--host <dest>`  | a `bossd` on another machine you own    | SSH-forwarded unix socket |
| `--remote <url>` | the hosted Bossanova cloud orchestrator | HTTPS to the orchestrator |

`--host` also never starts a local daemon. You asked for the daemon on that
host; booting one here would drive the wrong machine and hide the real failure.

## Attaching to a chat pane

**Attaching to a chat pane works over `--host`.** The pane is a `tmux` session
on the remote machine, so `boss` attaches with

```bash
ssh -t <destination> 'exec "$SHELL" -lc '\''tmux -u attach -t <session>'\'''
```

rather than driving the local `tmux`. That is the trimmed form — the live attach
also passes `-e none` and ssh keepalives, none of which change which machine the
pane runs on. The remote `tmux` runs through that host's **login** shell for the
same reason `--host-socket` exists: a non-interactive SSH login gets a reduced
`PATH`, and a `tmux` installed outside it (Homebrew's `/opt/homebrew/bin`,
exported from a `~/.zprofile` that `zsh` does not read non-interactively) would
otherwise fail every attach with `tmux: command not found`. Because the attach
runs remotely, `boss --host` does **not** require `tmux` on your local machine —
only `ssh`.

`Ctrl+X` detaches back to the local TUI as usual. If the connection drops
mid-pane you are ejected back to the local TUI — the remote `tmux` session
survives, so attaching again returns you to the same pane, and
`boss fix-terminal` restores a terminal left in a strange state by the drop.

### Running `boss --host` inside a local `tmux`

**Running `boss --host` inside `tmux` makes the prefix key ambiguous.** You end
up with two `tmux` clients in one terminal — your local one and the remote one
inside the pane — and both answer to the same prefix (`Ctrl+B` by default). The
outer `tmux` wins, so the remote one needs the prefix pressed twice (`Ctrl+B`
`Ctrl+B`, then the command). `boss` does not rebind your prefix to work around
this; if you attach over `--host` often, set a distinct prefix in the outer
session's own `~/.tmux.conf`.

### Terminfo on the remote host

**The remote host is the one that needs a `terminfo` entry for your `TERM`.**
The `tmux` client runs on that machine and reads its `terminfo` database, so
under `--host` `boss` probes the **remote** host for your `TERM` and, when the
entry is missing, falls back to `xterm-256color` rather than failing. That is a
silent downgrade: the pane works, but colours and key handling may not match
your terminal. If an attach does fail with a missing-terminal error, the error
screen names the host to install on and shows the reproduction command. Install
the entry with:

```bash
infocmp -x $TERM | ssh <destination> tic -x -
```

## Pasting screenshots into a remote chat

**Pasting screenshots works under `boss --host`, and cannot work over plain ssh.**
The difference is which machine the TUI runs on, and it decides whether boss can
reach the file at all.

Your terminal never pastes an image. When you press `cmd`/`ctrl+V` with a
screenshot on the clipboard, the emulator writes the image to a temporary file on
**the machine you are sitting at** and pastes that file's **path** as text —
something like `/tmp/cmux-drop-383cd973-4bab-42c5-a2f5-20497d580fa2.png`. All boss
ever sees is that path.

- **Under `boss --host <destination>`** the TUI runs on your local machine, which
  is the machine the file is on. Boss recognises the pasted path, copies the file
  to a per-chat temporary directory on the host the agent runs on, and inserts
  **that** path into the composer instead. The agent opens a file it can actually
  read, and the temporary directory is removed when you detach.

- **Under a plain `ssh` login** — you SSH to the box and run `boss` there — the
  TUI runs on the **remote** machine and the screenshot is on your laptop. There
  is no reverse channel from the remote process back to your local filesystem, so
  boss cannot fetch the file. The path is passed through to the agent exactly as
  you pasted it, and the agent reports it cannot open it.

Boss says so rather than failing silently: a paste of an absolute image path that
does not exist on the machine running the TUI is recorded in `boss.log`, and
briefly flashes a one-line
`[boss] no such image on this machine: … · use boss --host` on the bottom row
(the file name is shortened so the line always fits one row). The log is the
durable record and the flash is
easy to miss — boss forwards the paste to the agent immediately afterwards, and
the agent's next frame redraws over that row — so check the log if you did not
catch it:

```bash
grep 'image path not claimed' "${XDG_STATE_HOME:-$HOME/.local/state}/bossanova/logs/boss.log"
```

Your keystrokes are never altered — the text still reaches the agent exactly as
typed.

To paste screenshots into a remote chat, attach with `boss --host` from your own
machine rather than running `boss` inside an SSH session:

```bash
boss --host <destination>
```

If you must work inside an SSH session, copy the file over yourself and paste the
**remote** path — for example with `scp ~/Desktop/shot.png <destination>:/tmp/`.

## Commands that stay local

A few commands act on the machine `boss` runs on, so they are refused or hidden
under `--host` rather than silently doing the wrong thing:

- **`boss account add` is refused, except for a pasted claude token.**
  Registration normally mints a credential by running the agent's own CLI as a
  **local** subprocess, which resolves against this machine's `PATH` and this
  machine's browser rather than the remote host's. The one shape with no local
  subprocess in it is allowed through:
  `boss --host <dest> account add claude --token-stdin` reads a token you already
  hold (mint one with `claude setup-token`) and stores it on the daemon you asked
  for, so the credential lands on the remote machine. Keep `--host` on that
  command — without it the token is registered on your local daemon instead.
  Every other shape — codex in any form, and claude's interactive walkthrough —
  is refused with the command that does work; run it in a shell on that host
  instead. Under `--host`, the TUI's add-account flow makes the same split:
  choosing codex is refused up front, and choosing claude skips the walkthrough
  and asks you to paste a token.

  **Known gap:** this split is `--host` only. Under `--remote <url>` the TUI is
  not gated at all, so Settings → Accounts → `[a]` still spawns a local
  `claude setup-token` / `codex login` on the machine you are sitting at while
  the credential goes to the remote daemon. Use `boss account add` in a shell on
  the remote host until that is closed.

- **The `boss daemon` subcommands are refused.** They install, start, stop, and
  uninstall **this** machine's `bossd` through local subprocesses, so they would
  manage the wrong daemon. Run `ssh <destination> boss daemon ...` instead.
- **The `[t]erminal` action is hidden.** It opens a session's worktree in a new
  **local** terminal tab, and under `--host` that path is on the other machine.
  Chat titles are likewise not backfilled locally — the remote daemon does that
  job itself.

## When the tunnel drops

A dropped tunnel does not kill `boss`. The connection is supervised and
redialled with backoff, so a blip only fails the in-flight request. While it is
down the TUI says `Reconnecting to <destination>` and points at the SSH checks
for that host, and the session resumes on its own once SSH is back — no user
action, and no local daemon to restart.

**Read that banner, though.** If the supervisor itself has stopped, nothing is
redialling, and the TUI says so instead: `Disconnected from <destination>`,
followed by `Restart boss to reconnect`. Waiting will not help in that state —
only `Reconnecting to …` promises an automatic recovery.

## Flag reference

See the [CLI Reference](/reference/cli-reference) for the `--host` /
`--host-socket` flag table.
