---
title: Troubleshooting
description: 'A runbook for the most common Bossanova failures: auth, setup scripts, the agent runner, worktrees, auto-merge, and the repair loop.'
---

# Troubleshooting

This page is organized as a runbook: scan the section that matches what's
broken, follow the steps. If your problem isn't here, file a report from the
Terminal UI (TUI) by pressing `ctrl+b` (see [Reporting bugs](#reporting-bugs)).

For higher-level explanations of how the pieces fit together, see
[How It Works](../how-it-works.md).

## Auth and login

### WorkOS device-code flow times out

`boss login` requests a device code, opens the verification URL in your
browser, and polls until you complete the WorkOS flow. If the polling hits
the device-code TTL without success, it fails.

Re-run the command. A fresh code expires further in the future:

```bash
boss login
```

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

Inspect the daemon log to find the exact failure. The log path is
`$XDG_STATE_HOME/bossanova/logs/bossd.log` if `XDG_STATE_HOME` is set,
otherwise `~/.local/state/bossanova/logs/bossd.log` (on both macOS and
Linux). Use `boss daemon status` to confirm the daemon is up, then
tail its log:

```bash
tail -f ~/.local/state/bossanova/logs/bossd.log
```

The setup script runs from the new worktree's directory, so you can usually
reproduce it manually:

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

## Agent runner plugins

### Daemon healthy but `boss new` fails fast

If `bossd` is up but every session-start fails immediately with
`no AgentRunner plugin loaded; install bossd-plugin-claude (or another
agent runner) and restart`, no runner plugin loaded.

The error message itself is the signal. `bossd` discovers plugins
next to its own binary or in `../libexec/plugins/` (the Homebrew
layout), so the fix is to make sure `bossd-plugin-claude` (or another
agent runner) sits in one of those locations:

```bash
which bossd
which bossd-plugin-claude
which bossd-plugin-codex
```

If the runner binary is missing or in a different directory, build it
(`make plugins`) or install the package so it lands next to `bossd`,
then restart the daemon. Install the matching agent CLI (`claude` or
`codex`) and make sure it is on `bossd`'s `PATH`.

### Agent subprocess crashed

The selected agent plugin owns the agent CLI subprocess; if it crashes,
the session's chat stops producing output. The plugin's log line will
record the exit. Check the daemon log for entries tagged with the
plugin name (`claude` or `codex`), then archive and recreate the
session:

```bash
boss archive <session-id>
boss new
```

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
TUI settings view or the CLI:

```bash
boss settings --skip-permissions      # turn it on
boss settings --no-skip-permissions   # turn it off
```

The flag is stored at `plugins[claude].config.dangerously_skip_permissions`
in `settings.json`. Read [Security and
Permissions](../reference/security-and-permissions.md) before flipping it
on. It makes the agent more capable and more dangerous in equal measure.

## Workspace and worktree

### Worktree directory missing

Sometimes the worktree directory disappears: manual `rm -rf`, an
overzealous cleanup tool, a different machine. The session's metadata
still exists in the daemon's database but `git worktree list` no longer
shows it.

There is no `boss session repair` command today. The reliable fix is to
archive the session and start a new one against the same branch:

```bash
boss archive <session-id>
boss new
```

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
Two recovery paths:

```bash
boss daemon install   # set up automatic startup (macOS LaunchAgent)
bossd                 # run it manually in another terminal
```

`boss daemon status` reports whether the daemon is installed and
running and prints its PID. For log content, tail the `bossd.log`
file directly. See
[Setup script exits non-zero](#setup-script-exits-non-zero) above for
the path.

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
daemon, and agent runner work identically. You give up the web
app.

## Reporting bugs

Press `ctrl+b` from anywhere in the TUI to open the bug report form.
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
