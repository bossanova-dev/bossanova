---
title: Troubleshooting
description: 'A runbook for the most common Bossanova failures: auth, setup scripts, the agent plugin, worktrees, auto-merge, and the repair loop.'
---

import CommandTabs from '@site/src/components/CommandTabs';

# Troubleshooting

This page is organized as a runbook: scan the section that matches what's
broken, follow the steps. If your problem isn't here, file a report from the
Terminal UI (TUI) by pressing `ctrl+g` (see [Reporting bugs](#reporting-bugs)).

For higher-level explanations of how the pieces fit together, see
[How It Works](../how-it-works.md).

## Auth and login

### WorkOS device-code flow times out

`boss login` requests a device code, opens the verification URL in your
browser, and polls until you complete the WorkOS flow. If the polling hits
the device-code TTL without success, it fails.

Re-run the command. A fresh code expires further in the future:

<CommandTabs
cli="boss login"
/>

`boss login` drives a browser flow on this machine, so there is no MCP tool for
it; the tabs record that rather than leaving it ambiguous.

For what a successful sign-in looks like on both surfaces, see
[Signing In](../guides/login.md#confirming-it-worked).

If it still fails, check three things:

- **Clock skew.** WorkOS rejects tokens with significant clock drift. Confirm
  `date -u` matches reality.
- **Browser opened the correct URL.** The TUI prints the verification URL; if
  the auto-open landed on a stale tab, copy-paste the URL manually.
- **Outbound HTTPS to `api.workos.com`.** Corporate proxies and VPNs often
  block device-code endpoints. Try from a different network.

### `gh` CLI is not authenticated

Bossanova shells out to `gh` for all GitHub operations (PR creation,
status checks, review state). If `gh` is missing or unauthenticated,
those operations fail with `gh: not found` or HTTP 401.

```bash
gh auth status
gh auth login          # if status shows you're logged out
```

Authenticate `gh` with the GitHub user that owns the repos Bossanova will
operate on. The daemon doesn't carry its own GitHub identity; it inherits
yours.

### Repo Add can't see my private repos

The Repo Add view lists repos `gh` can see with its current scopes. If
private repos are missing, `gh` is missing the `repo` scope:

```bash
gh auth refresh -s repo
```

If your private repos are owned by an org that requires SSO, also run
`gh auth refresh -s repo,read:org` and follow the SSO authorization link
that prints.

## Setup script failures

### Setup script exits non-zero

When a session's setup script (configured via `boss repo update <repo-id>
--setup-script "..."`) exits non-zero, the daemon marks the session failed
and the agent never starts. The setup script's stdout and stderr are
captured in the daemon log.

Inspect the daemon log to find the exact failure. Use `boss daemon status`
to confirm the daemon is up, then read its log with `boss tail`, which
finds the file for you. The failure has already happened, so read a deep
backlog rather than following:

<CommandTabs
cli="boss tail -n 200"
/>

The underlying file is `$XDG_STATE_HOME/bossanova/logs/bossd.log` if
`XDG_STATE_HOME` is set, otherwise `~/.local/state/bossanova/logs/bossd.log`
(on both macOS and Linux). See the [Logging guide](../guides/logging.md)
for sources, filters, and JSON output.

`boss tail` is the right tool for the setup-script failure text. It is not a
cost or timing report: use `boss cost <session-id>` when you need run duration,
parent-only time, token totals, model/tool calls, or subagent counts.

The setup script runs from the new worktree's directory, so you can usually
reproduce it manually. This is a multi-line shell recipe rather than a single
command, so it stays a plain block; the `boss show` inside it reads the
worktree path, and `get_session` is its MCP counterpart:

```bash
WT=$(boss show <session-id> | awk '/^  Worktree:/ {print $2}')
cd "$WT"
bash -c "$YOUR_SETUP_SCRIPT"
```

Once you've fixed the script, archive the broken session (`boss archive
<session-id>`) and create a new one. Setup scripts only run on session
creation.

### Setup script needs a secret

Setup scripts run with the same environment as `bossd`, plus
`BOSS_REPO_DIR` and `BOSS_WORKTREE_DIR`. Don't bake secrets into the
script itself. Store them in your shell's environment (or a
keyring helper) and reference them by variable name. See
[Setup Scripts](../guides/setup-scripts.md) for the full set of env vars
the daemon injects.

For credentials Bossanova itself manages (WorkOS tokens, etc.), the
daemon uses `keyringutil` and falls back to a file-backed store only when
you pass `--allow-insecure-keyring` explicitly. Don't hand-edit the
keyring file.

## Agent plugins

### Daemon healthy but `boss new` fails fast

If `bossd` is up but every session-start fails immediately with
`no AgentRunner plugin loaded; install bossd-plugin-claude (or another
agent runner) and restart`, no agent plugin loaded.

The error message itself is the signal. `bossd` discovers plugins
next to its own binary or in `../libexec/plugins/` (the Homebrew
layout), so the fix is to make sure `bossd-plugin-claude` (or another
agent plugin) sits in one of those locations:

```bash
which bossd
which bossd-plugin-claude
which bossd-plugin-codex
```

If the plugin binary is missing or in a different directory, build it
(`make plugins`) or install the package so it lands next to `bossd`,
then restart the daemon. Install the matching agent CLI (`claude` or
`codex`) and make sure it is on `bossd`'s `PATH`.

### Agent subprocess crashed

The selected agent plugin owns the agent CLI subprocess; if it crashes,
the session's chat stops producing output. The plugin's log line will
record the exit. Check the daemon log for entries tagged with the
plugin name (`claude` or `codex`), then archive and recreate the
session.

Archive the broken session:

<CommandTabs
chat='"archive session a3f2c19b7d4e0158"'
cli="boss archive <session-id>"
mcp="archive_session"
/>

`archive_session` is destructive, so the MCP call only proceeds with
`confirm: true`. The CLI archives immediately and asks nothing, so check the
session id before you run it.

Then create a fresh one:

<CommandTabs
chat='"start a session on this repo to redo the work I just archived"'
cli="boss new"
mcp="create_session"
/>

There is no in-place "restart this session" command. Sessions are tied
to a worktree and a chat history, and re-running the agent on the same
worktree is safer as a fresh session against the same branch (start a
fresh session via `boss new` and pick the same branch in the picker).

### Skills not installed in session

If the agent doesn't have the boss skills available, two things to check:

- `skills_declined` is `true` in your global settings file. Set it to
  `false` (or delete the key) and re-launch `boss`. It'll re-prompt to
  install on next start.
- The plugin couldn't write to `~/.claude/skills/`. Check directory
  permissions; the plugin extracts skills there at session boot.

If neither check resolves it, file an issue with the daemon log. Skill install is not user-configurable today.

### Agent has no permission to do X

By default, the `claude` plugin runs Claude Code without the
`--dangerously-skip-permissions` flag, so the agent prompts for any
filesystem or network operation outside its sandbox. Toggle it from the
TUI settings view, an agent, or the CLI.

Turn it on:

<CommandTabs
chat='"turn on skip-permissions for the claude plugin"'
cli="boss settings --skip-permissions"
mcp="update_settings"
/>

Turn it off again:

<CommandTabs
chat='"turn off skip-permissions for the claude plugin"'
cli="boss settings --no-skip-permissions"
mcp="update_settings"
/>

The flag is stored at `plugins[claude].config.dangerously_skip_permissions`
in `settings.json`. Read [Security and
Permissions](../reference/security-and-permissions.md) before flipping it
on. It makes the agent more capable and more dangerous in equal measure.

### macOS: archive, session creation, or repair hangs silently

If archiving a session, creating a session, or running a repair hangs without
an error, and a `git`, `tmux`, or `node` child is stuck in `getcwd`, macOS may
be waiting for a privacy permission decision. TCC keys `bossd` access by its
resolved binary path, so a Homebrew upgrade can make a previously granted path
stop matching the daemon that now runs.

First, inspect the daemon's diagnostic output:

<CommandTabs
cli="boss daemon doctor"
/>

`boss daemon doctor` reports the probe bossd ran at startup, so its verdict is
as old as the daemon. For a fresh answer, run:

<CommandTabs
chat='"run the repair doctor"'
cli="boss repair doctor"
mcp="repair_doctor"
/>

Its `protected roots readable` check probes on every invocation. Answering a
pending privacy dialog clears it immediately: no restart, because answering
unblocks the read that was already in flight. The other two routes still need
`boss daemon restart`: a System Settings grant does not reach a running
process, and a root the daemon found at startup keeps being probed until the
daemon restarts, even after you move the repository off it.

Answer any pending macOS privacy dialog. If no dialog appears, grant the
staged daemon binary access in macOS Privacy & Security settings (for example,
Full Disk Access):

```
~/Library/Application Support/bossanova/bin/bossd
```

Then restart the daemon:

<CommandTabs
cli="boss daemon restart"
/>

A Full Disk Access grant is not applied to an already-running process, so the
restart is required for that route.

After every `brew upgrade`, run the same restart. It refreshes the staged
daemon copy at that stable real path, preserving the path macOS TCC associates
with the permission.

#### On a headless Mac (no display)

Both routes above need someone at the screen: a pending privacy dialog cannot
be answered over SSH, and System Settings needs a GUI. On a host reached only
over SSH, the option that works unattended is to move the repository and the
worktree base out of the **TCC-guarded folders**: `~/Documents`, `~/Desktop`
and `~/Downloads`. A repository at, say, `~/src/...` needs no grant at all, and
cannot re-break on the next upgrade. Reinstalling the binary is not a fix: TCC
keys the grant to the installed path, not to the bytes.

If you must use the GUI route on a headless host, reach it over Screen Sharing
or push the grant with MDM.

### macOS: bossd keeps running the build you just upgraded away from

The staged daemon copy that keeps macOS privacy grants alive has a
consequence: a bare `brew upgrade` no longer deletes the binary out from under
the running daemon, so launchd's `KeepAlive` respawns the **staged** copy and
bossd keeps executing the previous build until something restarts it. The
symptom is that a fix you know shipped does not appear to take effect.

`boss` now says so on its own. Every command prints one line to stderr when
the installed build is ahead of the running daemon:

```
boss: bossd is running an older build than the one installed — run 'boss daemon restart' (details: boss daemon doctor)
```

The line appears only on Homebrew installs, never for a locally built binary
in a checkout, and never on the commands that are themselves the remedy
(`boss daemon doctor`, `boss daemon restart`, `boss daemon start`,
`boss upgrade`). Set `BOSS_DAEMON_SKIP_STALE_WARNING=1` to silence it in
scripts.

To see the detail:

<CommandTabs
cli="boss daemon doctor"
/>

The doctor answers two separate questions, and the difference matters:

- **`staged bossd: … — stale` / `— up to date`** is about the _file_. It
  compares the staged copy against the binary Homebrew installed.
- **`running executable: … (PID N) — stale` / `— up to date`** is about the
  _process_. It probes the recorded PID for liveness and compares when the
  process started against when the staged file was last written.

They can disagree. A restart re-stages the file between stopping and starting
the daemon, so after a restart that failed halfway the file is current while
the live process is not: the file line reads `up to date` and the process
line reads `stale`. Only a PID change proves a restart actually happened.

`boss daemon status` reports the same process verdict as a `running image:`
line.

#### The restart stopped the daemon and did not start it

`boss daemon restart` stops the daemon before starting it, and the start can
fail on its own. It now retries a raced `launchctl bootstrap` a few times, and
if every attempt fails it verifies what is actually running before reporting:

```
boss: restart daemon failed: launchctl bootstrap: exit status 5: Bootstrap failed: 5: Input/output error; the daemon is now stopped — run 'boss daemon start'
```

When the message names `boss daemon start`, nothing is running; run it:

<CommandTabs
cli="boss daemon start"
/>

`boss daemon doctor` reports the same state: a recorded PID that is no longer
alive is reported as `not running`, with `run 'boss daemon start'` as the
remediation rather than a restart.

## Workspace and worktree

### Worktree directory missing

Sometimes the worktree directory disappears: manual `rm -rf`, an
overzealous cleanup tool, a different machine. The session's metadata
still exists in the daemon's database but `git worktree list` no longer
shows it.

There is no `boss session repair` command today. The reliable fix is to
archive the session and start a new one against the same branch:

<CommandTabs
chat='"archive session a3f2c19b7d4e0158"'
cli="boss archive <session-id>"
mcp="archive_session"
/>

`archive_session` is destructive: the MCP call only proceeds with
`confirm: true`.

<CommandTabs
chat='"start a session on this repo against the same branch"'
cli="boss new"
mcp="create_session"
/>

If you want to recover the worktree manually first, `git worktree add
<path> <branch>` from the repo root will recreate it; the daemon will
notice the directory on next poll.

### Branch already exists upstream

When a new session's branch name collides with one that already exists
on the remote (often a leftover from a prior session), the agent's first
push will fail with `! [rejected] (non-fast-forward)`.

Two options:

- Force-push with lease: `git push --force-with-lease origin
<branch-name>` from the worktree, if you're sure the remote branch is
  abandoned.
- Rename the local branch and push to a fresh name: `git branch -m
<branch-name> <branch-name>-2 && git push -u origin <branch-name>-2`.

### Worktree dirty after session ended

`boss archive <session-id>` removes the worktree and keeps the branch.
If for some reason the directory is left behind (e.g. archive failed
mid-flight), clean up manually:

```bash
git -C <repo-dir> worktree remove --force <worktree-path>
```

The branch itself remains intact and can be re-checked-out into a new
worktree later.

## Review and merge

### Auto-merge isn't firing despite green checks

Three things have to line up:

1. The repo has `can_auto_merge` toggled on in
   your repo's automation flags (`boss repo show <name>`). Dependabot PRs
   need `can_auto_merge_dependabot` instead.
2. All required status checks listed in the GitHub branch protection
   rule are green. Add a check that passes manually but isn't in the
   required list, and Bossanova won't merge.
3. The PR is approved (if your branch protection requires review).

Confirm green-and-mergeable with `gh pr checks <pr-number>` and `gh pr
view <pr-number> --json mergeStateStatus`. If state is `BLOCKED` or
`UNSTABLE`, GitHub itself isn't ready to merge yet. Fix that first.

### Repair plugin keeps re-running on the same PR

The repair plugin tracks a per-session cooldown
(`repair.cooldown_minutes`, default `1`) and the last-attempted head
SHA. It shouldn't loop on a stable failure. If it does, two
possibilities:

- New commits keep landing on the PR (every push resets the SHA-tracked
  guard), so each new commit looks like a fresh failure.
- The repair attempt itself is what's pushing. The agent fixes
  something, CI fails for a different reason, repair fires again.

To stop repair on one PR, close it or move it to draft. The plugin only
acts on open, ready-for-review PRs. To raise the global cooldown, bump
`repair.cooldown_minutes` in [settings](../reference/settings.md).

### Conflict resolver opened a PR with conflict markers

This happens when the agent gives up part-way and commits a partially
resolved tree. Treat it as a regular merge conflict: pull the branch,
resolve manually, force-push. The repair plugin's cooldown will keep it
from immediately re-firing. If the PR is unsalvageable, close it and
let the repair plugin's `closed`-status guard hold off, then start a
fresh session.

## Repair-loop edge cases

### Repair fires on Failing but the failure is flaky

If a flaky check is in your branch-protection required-checks list,
repair will keep firing because the display status flips back to
**Failing** every time the flake re-appears. Two fixes:

- Mark the flaky check as non-required in branch protection. The repair
  plugin only treats required checks as session-failure signals.
- Stabilize the check (the better long-term fix). The repair plugin's
  cooldown buys you time but it can't fix flakes for you.

### Repair fires on Rejected but the reviewer is wrong

If a human reviewer requested changes that you, the operator, disagree
with, the right move is to **close** the PR and respond on the original
review thread manually. The repair plugin honors the PR's `closed`
status. Once closed, it won't re-fire. Re-opening reactivates the
session; address the review yourself or dismiss the requested-changes
review on GitHub, then let the daemon re-poll.

## Preflight failures

### Daemon not running

The TUI's preflight check shows
"Cannot connect to the bossd daemon" when the socket isn't reachable.
Two recovery paths. Either set up automatic startup (a macOS LaunchAgent, or a
systemd user service on Linux):

<CommandTabs
cli="boss daemon install"
/>

…or run the daemon by hand in another terminal:

```bash
bossd
```

`boss daemon status` reports whether the daemon is installed and
running and prints its PID:

<CommandTabs
cli="boss daemon status"
/>

For log content, run `boss tail`; it reads the daemon log without needing a
path:

<CommandTabs
cli="boss tail -n 100"
/>

The daemon lifecycle and log commands all act on this machine's service manager
and local log file, so none of them has an MCP tool and their tabs carry the
no-equivalent note. For `boss daemon install` an MCP tool could not have helped
in any case: every tool is served by the daemon that is not running.

See the [Logging guide](../guides/logging.md) for sources, filters, and
JSON output, or use `boss cost` for run-cost telemetry that raw log text cannot
summarize reliably. See
[Setup script exits non-zero](#setup-script-exits-non-zero) above for the
underlying file path.

### Plugins missing

If the daemon starts but a plugin you expect (e.g. `repair`,
`dependabot`) isn't reacting to events, the plugin binary likely
isn't where the daemon looked. `bossd` discovers plugins next to its
own binary or in `../libexec/plugins/` (the Homebrew layout); check
the daemon log for plugin-startup entries, and confirm the binary is
present:

```bash
make plugins                                  # from the repo root
ls /opt/homebrew/libexec/plugins/             # default Homebrew install
```

Configure plugin paths explicitly under `plugins[]` in
[settings.json](../reference/settings.md) if auto-discovery isn't
finding them.

### Orchestrator URL unreachable but cloud is on

If `bossd` logs `failed to connect to orchestrator` repeatedly but the
local TUI works fine, the cloud orchestrator URL is set but unreachable
(corporate proxy, VPN, transient outage). To run local-only, blank the
URL in `settings.json`:

```json
{
  "cloud": {
    "orchestrator_url": ""
  }
}
```

Or export `BOSSD_ORCHESTRATOR_URL=""` before starting `bossd`. The TUI,
daemon, and agent plugin work identically. You give up the web
app.

## Terminal

### Terminal spams numbers and letters when you move the mouse

After a boss session ends abnormally (an SSH connection drops, a terminal
tab is closed, the process is killed) your terminal can be left in xterm
**mouse-reporting mode**. Nothing is reading those reports any more, so every
mouse movement over the window is printed as text. Native click-drag selection
usually stops working at the same time.

It shows up in two spellings, depending on what owns the terminal when the
report arrives:

- **Escape consumed.** At a shell prompt, the line editor eats the `ESC[<`
  introducer and only the numbers reach the screen:

  ```text
  35;51;14M35;48;14M
  35;39;28M0;17;5m
  ```

- **Escape visible.** In cooked mode (for example while `ssh` is still
  connecting, before anything has taken the terminal into raw mode) the whole
  sequence is echoed:

  ```text
  ^[[<35;31;13M
  ```

A stranded **focus-reporting** mode is the same bug with a quieter tell: a
stray `I` or `O` appears every time you click away from the window and back
again (or, in cooked mode, `^[[I` and `^[[O`). The same remedies clear it.

To fix it, run this on the machine whose terminal is stuck:

<CommandTabs
cli="boss fix-terminal"
/>

If boss lives on a remote host, run it from the stuck terminal over SSH so the
escape sequences land in the local terminal:

```bash
ssh <host> boss fix-terminal
```

`boss fix-terminal` writes the mouse- and focus-reporting disable sequences and
prints nothing else, so it is safe to pipe straight into the affected terminal.

If boss isn't installed on the box, the core reset is a plain `printf` (mouse
reporting, focus reporting and bracketed paste):

```bash
printf '\033[?1000l\033[?1002l\033[?1003l\033[?1006l\033[?1004l\033[?2004l'
```

`boss fix-terminal` additionally clears some legacy mouse encodings and the
keyboard-protocol modes, so prefer it when it's available.

As a last resort, `reset` reinitializes the terminal completely; note that it
**clears your scrollback**:

```bash
reset
```

Boss also writes the same reset every time the TUI launches, so relaunching
boss on the affected terminal normally clears it on its own.

**One honest limit.** That automatic self-heal is written by the boss process
itself. When boss is on a remote host, the reset can't arrive until after SSH
has connected, authenticated, and started boss, so relaunching boss over SSH
still shows a few seconds of spam between pressing Enter and boss appearing.
`boss fix-terminal` and the raw `printf` are the remedies that don't wait on a
remote round trip: run them from a shell that's already connected, or run the
`printf` locally.

### Add-account prompt ignores everything you type

Running boss over SSH, the **Add an account** flow can reach
`Label for this account:` (cursor visible, `[enter] submit · [esc] cancel` on
the action bar) and then ignore every keystroke. Nothing appears as you type
and the form can't be submitted, even though the sign-in already succeeded and
the screen above says the token was created.

The cause is the agent CLI that boss just ran. It shares your terminal, and it
reads from it directly, even though boss deliberately hands it an empty input.
While the sign-in runs there are briefly **two programs reading your keyboard**:
the CLI and boss itself. They compete for each keystroke, which is why pasting
the sign-in code often takes several attempts, and why the prompt afterwards can
stop responding.

Boss now hands the terminal to the sign-in CLI for the duration and takes it back
cleanly when the CLI exits, so only one program is ever reading your keyboard.
While the CLI has it, the action bar reads **`claude has the terminal`** and
`esc` goes to the CLI rather than to boss; use `ctrl+c` to interrupt it.

If you hit a prompt that still ignores input:

1. Press `esc` to leave the flow. Boss re-asserts terminal state as it redraws,
   which clears most cases on its own.
2. If the terminal is still misbehaving, run the escape hatch on the machine
   whose terminal is stuck:

<CommandTabs
cli="boss fix-terminal"
/>

Over SSH, run it from the stuck session so it reaches the right terminal:

```bash
ssh <host> boss fix-terminal
```

That clears stranded **input-reporting** modes: focus reporting, bracketed
paste, `modifyOtherKeys` and the Kitty keyboard protocol.

Your sign-in token is **not silently lost**. If the flow fails at or after the
label step, boss now tells you that a token was already created, shows it in
masked `sk-ant-…` form so you can identify it, and points you at
[the Anthropic console](https://console.anthropic.com/settings/keys) to revoke
the orphaned one before registering a replacement. If you still hold a valid
token, answer **No** at the walkthrough prompt to paste it instead of minting a
new one.

### Pasted screenshot arrives as a literal `/tmp/...png` path

Pasting a screenshot into an attached chat inserts text into the composer instead
of attaching the image:

```text
/tmp/cmux-drop-383cd973-4bab-42c5-a2f5-20497d580fa2.png
```

The agent then reports it cannot open that file.

Your terminal never pastes an image. It writes the screenshot to a temporary file
on the machine you are sitting at and pastes that file's path as text. Boss
can rewrite that path only when it is running on the same machine as the file.

- If you SSH to a box and run `boss` there, the TUI is on the **remote** machine
  and the screenshot is on your laptop. There is no reverse channel back to your
  local filesystem, so boss passes the path through untouched, which is what you
  are seeing.
- If you attach with `boss --host <destination>` from your own machine, the TUI is
  local, boss copies the file to the agent's machine and inserts the remote path.

So the fix is to attach from your own machine instead of from inside an SSH
session:

```bash
boss --host <destination>
```

Boss flashes `[boss] no such image on this machine: …` on the bottom row when it
sees this, and records it in `boss.log`; the flash is transient because the agent
redraws over it, so check the log if you missed it:

```bash
grep 'image path not claimed' "${XDG_STATE_HOME:-$HOME/.local/state}/bossanova/logs/boss.log"
```

The same message appears if you paste a path that simply does not exist on this
machine, which is the other thing it can mean. Full detail, including what
`--host` cleans up afterwards, is in
[Remote Daemons](../guides/remote-daemons.md).

If you need to stay inside the SSH session, copy the file across yourself and
paste the **remote** path:

```bash
scp ~/Desktop/shot.png <destination>:/tmp/
```

## Reporting bugs

Press `ctrl+g` from anywhere in the TUI to open the bug report form. `ctrl+b`
remains a temporary deprecated alias outside tmux, but tmux consumes it as its
default prefix, so use `ctrl+g` instead.
It collects:

- `boss` version and commit
- OS and architecture
- Per-session daemon heartbeats
- The current session (if any) and a summary of all open sessions
- The last 200 lines of `boss.log` and `bossd.log`

You add a free-form comment and submit. **No source code, diffs, or
agent transcripts are included**. See
[Privacy](../reference/privacy.md) for the full inventory.

The report comes back with a short reference ID; quote that ID in any
follow-up so triage can find your submission.

## Known issues

_No active known issues at time of writing (2026-05-05). When something
chronic crops up, it'll be listed here with a link to the tracking
issue._
