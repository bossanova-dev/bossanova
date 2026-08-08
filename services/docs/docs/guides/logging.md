---
title: Logging
description: Read the daemon, TUI, and server logs with boss tail, and know which surface holds what.
slug: /guides/logging
---

import CommandTabs from '@site/src/components/CommandTabs';

# Logging

Three Bossanova services write structured logs: the `bossd` daemon, the `boss`
TUI, and the `bosso` server. Each one writes newline-delimited JSON to a rotated
file, and `boss tail` reads those files for you so you never have to remember
where they are.

Agent output — what Claude or Codex actually printed in a session — is a
**separate** surface that `boss tail` does not read. See
[Agent and chat logs](#agent-and-chat-logs-are-a-different-surface) below before
you go looking for it in `bossd.log`.

## The `boss tail` command

With no arguments, `boss tail` prints the last 10 lines of the daemon log:

<CommandTabs
cli="boss tail"
/>

Output is one line per record, with the time, the source service, the level, and
the message:

```
14:02:11 bossd info  session created  repo=9f1c4a7b2e6d0358
14:02:12 bossd info  worktree ready  repo=9f1c4a7b2e6d0358
14:02:19 bossd error plugin dispatch failed  plugin=repair
```

### Sources

`boss tail` takes at most one positional source. There are three:

| Source  | Contents                                                                                                                          |
| ------- | --------------------------------------------------------------------------------------------------------------------------------- |
| `bossd` | The daemon: session lifecycle, git operations, plugin dispatch. **Default.**                                                      |
| `boss`  | Every `boss` command except `boss tail` itself, TUI included. It logs to file only, so this is the only place its records appear. |
| `bosso` | The server backing the web UI.                                                                                                    |

Name a source explicitly to choose which one you read:

<CommandTabs
cli="boss tail bossd"
/>

<CommandTabs
cli="boss tail boss"
/>

<CommandTabs
cli="boss tail bosso"
/>

`boss tail` always reads the log files on the machine you run it from. It is not
a daemon call, so the global `--remote` and `--host` flags do not redirect it —
to read another machine's logs, run `boss tail` over there.

An unrecognised source is rejected rather than silently treated as a filter:

<CommandTabs
cli="boss tail daemon"
/>

```
boss: unknown log source "daemon" (want one of: bossd, boss, bosso)
```

### `--all` merges every service

`--all` reads all three logs and interleaves them by timestamp. Records that
share a timestamp are ordered `bossd`, then `boss`, then `bosso`:

<CommandTabs
cli="boss tail --all -n 50"
/>

A source and `--all` are mutually exclusive — `--all` already includes every
source, so asking for both is a mistake rather than a narrowing:

<CommandTabs
cli="boss tail bossd --all"
/>

```
boss: pass a source or --all, not both
```

Note that `-n` applies **per source**, so `boss tail --all -n 50` reads up to 50
lines from each of the three logs, not 50 lines in total.

### Flags

| Flag             | Default  | Meaning                                             |
| ---------------- | -------- | --------------------------------------------------- |
| `--all`          | `false`  | Merge every service log.                            |
| `-n`, `--lines`  | `10`     | Physical lines to read per source before filtering. |
| `-f`, `--follow` | `false`  | Keep reading as the log grows.                      |
| `--repo`         | _(none)_ | Only records for this repo.                         |
| `--plugin`       | _(none)_ | Only records from this plugin.                      |
| `--level`        | _(none)_ | Only records at this level.                         |
| `--json`         | `false`  | Emit one parseable JSON object per line.            |

### Following with `--follow`

`-f` / `--follow` prints the backlog first, then streams new records as they are
written. It keeps running until you interrupt it:

<CommandTabs
cli="boss tail -f"
/>

<CommandTabs
cli="boss tail --all --follow"
/>

Following works across a rotation: when a log rolls over, `boss tail` picks up
the replacement file without duplicating or dropping records. Piping into a
command that closes its input early is a clean exit, not an error:

<CommandTabs
cli="boss tail -f | head -20"
/>

### Two behaviours worth knowing

Both of these are in the command's own `--help` text, because both quietly
mislead if you assume otherwise.

**`-n` counts physical lines read from each source _before_ filtering.** It is
not "show me 10 matches". `boss tail -n 10 --level error` reads the last 10
lines and then discards the ones that are not errors, so it can legitimately
print nothing at all even though the log is full of errors further back. When
you are filtering, raise `-n` generously:

<CommandTabs
cli="boss tail -n 2000 --level error"
/>

**Non-JSON lines always pass filters.** Anything that reaches a log file without
being structured JSON is not parseable, so no filter can decide whether it
matches — and hiding it would be exactly the wrong outcome. Those lines are
always shown verbatim, marked with the source service:

```
bossd | unstructured line appended to the log
```

That is a safety net rather than an everyday sight, because the logger itself
only ever writes JSON to these files. A crash in particular does **not** land
here: a Go panic goes straight to the process's stderr without passing through
the logger, so `boss tail` cannot show it. Under the macOS launch agent that
`boss daemon install` writes, that stderr is
`~/Library/Logs/bossanova/bossd.stderr.log`; under the Linux systemd user unit
it goes to the journal (`journalctl --user -u bossd.service`).

## Where the logs live and how they rotate

`boss tail` exists so you do not need these paths, but they are useful for a bug
report or a `grep` across a long history.

The log directory is `$XDG_STATE_HOME/bossanova/logs` if `XDG_STATE_HOME` is
set, and `~/.local/state/bossanova/logs` otherwise (on both macOS and Linux —
`XDG_STATE_HOME` is normally unset on macOS, so the fallback is what you have).
Each service writes `<service>.log` there:

```bash
ls ~/.local/state/bossanova/logs
# boss.log  bossd.log
```

`bosso.log` appears there only if you run the server yourself; the hosted one
logs on its own machine. A log file that does not exist is treated as an empty
log rather than an error, so `boss tail bosso` simply prints nothing and
`--all` still works with only two of the three files present.

Rotation is size-based. A log rotates when it reaches **5 MB**, and **one**
backup is kept, uncompressed — so each service retains roughly 10 MB, and the
three services together are bounded at about 30 MB. Backups are written beside
the current file with a timestamp in the name, matching `<service>-*.log`:

```bash
ls ~/.local/state/bossanova/logs
# boss.log  bossd-2026-08-08T09-14-52.301.log  bossd.log
```

You do not need to name those backups yourself. When the current file holds
fewer lines than `-n` asks for, `boss tail` reads back into the rotated backups
transparently, so `boss tail -n 5000` spans the rotation boundary.

Because retention is bounded and rotation is by size, a chatty period can push
older records out within minutes. If you need to keep something, redirect it
while it is still there:

<CommandTabs
cli="boss tail --all -n 20000 --json > /tmp/bossanova-logs.ndjson"
/>

## Agent and chat logs are a different surface

The three service logs record what Bossanova did. They do **not** record what
the coding agent printed. Agent output is captured separately:

- The format depends on how the chat runs. An **interactive** chat is a raw
  terminal capture of the agent's tmux pane, mirrored with `tmux pipe-pane`: not
  JSON, no per-line timestamps, and full of terminal escape sequences. A
  **headless** run instead writes one JSON object per output line,
  `{"ts": "…", "text": "…"}`, covering the agent's stdout **and** stderr, with
  occasional `[runner]` diagnostics from the agent runner interleaved in the
  same shape. The agent's own output is the opaque string in `text` — for Claude that
  is a line of `--output-format stream-json`, so reading it means unwrapping
  twice. Neither format is the structured service record that `boss tail`
  renders.
- There is **one file per chat**, named after the chat's agent-session UUID:
  `<agent-session-id>.log`. The same directory also holds
  `repair-<session-id>.log` for the repair plugin's own runs — note that those
  are keyed by _session_ id, not agent-session id.
- The files live in an `agent-logs` directory next to your worktree base
  directory — the sibling of `worktree_base_dir` from your settings. With the
  default `worktree_base_dir` of `~/.bossanova/worktrees`, that is
  `~/.bossanova/agent-logs`.
- **`boss tail` does not read them.** `bossd`, `boss`, and `bosso` are its only
  sources, so no amount of `boss tail` will show you agent output.

So: to see why a session failed to _start_, read `bossd.log` via `boss tail`. To
see what the agent _said_, read the chat — attach to the session, or read the
transcript — and fall back to the agent log for a headless run that is still in
flight:

```bash
tail -f ~/.bossanova/agent-logs/<agent-session-id>.log | jq -r .text
```

Attaching to a chat whose headless run is still going is refused, and the
refusal prints the exact agent-log path to follow instead.

## JSON output and filtering

### Filters

`--level`, `--repo`, and `--plugin` each restrict the output to records whose
matching field equals the value you pass. Matching is case-insensitive, and
`--repo` matches either a `repo` or a `repo_id` field.

`repo_id` is always a repository ID, but the `repo` field is written by several
subsystems and may hold the repository ID, its local path, its display name, or
— from `bosso` — its origin URL. The rendered line shows `repo=…` for the `repo`
field only, so a record carrying just `repo_id` still matches `--repo` but
prints no repo token at all. Use `--json` to see which of the two a given record
actually carries before settling on a filter value.

Plugins log through the daemon's plugin host, which wraps the plugin's own JSON
record inside the outer daemon record. `boss tail` unwraps that before
filtering and rendering, so `--plugin`, `--level`, and `--repo` match the
plugin's inner fields as well as the daemon's outer ones, and the rendered
message is the plugin's message rather than an escaped blob.

Remember that filters are applied _after_ `-n` has read its lines. Every filter
example below raises `-n` for that reason.

### `--json`

`--json` emits exactly one JSON object per line, suitable for `jq`. The record
is the source record plus a few `_boss_`-prefixed keys:

| Key             | Meaning                                                                            |
| --------------- | ---------------------------------------------------------------------------------- |
| `_boss_service` | The source log the record came from (`bossd`, `boss`, or `bosso`). Always present. |
| `_boss_plugin`  | The unwrapped inner plugin record, when the line came from a plugin.               |
| `_boss_raw`     | `true` for a line that was not valid JSON; the text is in `line`.                  |

For a plugin record the top-level `level` and `message` are also **replaced**
with the plugin's own values, so `--json` reports what the rendered line shows
rather than the daemon's outer wrapper text. Every other key is the source
record's own.

### Recipes

**Every `error`-level record, across all three services.** `--level` is an exact
match, so this selects `error` and not `warn` or `fatal`. Read a deep backlog so
the filter has something to select from:

<CommandTabs
cli="boss tail --all -n 5000 --level error"
/>

**Everything one repository did.** Useful when several sessions are running and
the daemon log is interleaved:

<CommandTabs
cli="boss tail -n 5000 --repo 9f1c4a7b2e6d0358"
/>

**Watch one plugin live.** Prints the recent backlog for that plugin, then
streams its new records:

<CommandTabs
cli="boss tail --follow -n 200 --plugin repair"
/>

**Count errors by plugin.** `--json` plus `jq` handles anything the built-in
filters do not:

<CommandTabs
cli={`boss tail -n 20000 --json | jq -r 'select(.level == "error") | .plugin // "core"' | sort | uniq -c | sort -rn`}
/>

**Follow everything, merged.** The closest equivalent of watching all three logs
at once, ordered by timestamp:

<CommandTabs
cli="boss tail --all --follow -n 100"
/>

**Extract just the message text** from a merged JSON stream, tagged with the
service it came from:

<CommandTabs
cli={`boss tail --all -n 500 --json | jq -r '"\\(._boss_service)\\t\\(.message // .line)"'`}
/>

## See also

- [Troubleshooting](../help/troubleshooting.md) — the runbook that these
  commands support.
- [Privacy](../reference/privacy.md) — what a bug report includes from these
  logs, and what those logs can contain.
