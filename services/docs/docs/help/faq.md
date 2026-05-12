---
title: FAQ
description: Common questions about how Bossanova runs agent sessions, what it sends to the cloud, when automation fires, and how it differs from similar tools.
---

# FAQ

The questions below cover the things people ask most often before and after
adopting Bossanova. If your question isn't here, the
[Troubleshooting](./troubleshooting.md) runbook covers concrete failure modes,
and [How It Works](../how-it-works.md) covers the system, and [Settings](../reference/settings.md) covers configuration.

## Basics

### What is Bossanova and who is it for?

Bossanova lets you run several coding-agent sessions at once. Each session gets
its own git worktree, and you watch them through a single Terminal UI (TUI). You kick off a task, the
agent works, a PR opens, CI runs, and (optionally) Bossanova reacts to failures
and review comments without you having to babysit it.

It's pitched at engineers who fan out across many repos or many small tasks at
once: bug triage queues, dependabot churn, multi-repo refactors, and the kind
of work where the bottleneck is "how many things can I keep in my head at
once" rather than "how fast can I type."

### What's the difference between `boss`, `bossd`, Boss Cloud, and the plugins?

`boss` is the TUI you spend most of your time in. `bossd` is the daemon that
holds session state, drives git worktrees, and dispatches plugin events. Boss
Cloud is an optional managed service that pairs with your daemons so you can
manage the same sessions from a browser. The plugins (`bossd-plugin-claude`,
`bossd-plugin-repair`, `bossd-plugin-dependabot`, `bossd-plugin-linear`) are
out-of-process binaries that subscribe to daemon events over gRPC and take
specific actions: running the agent, repairing failing PRs, classifying
Dependabot updates, syncing with Linear.

See [How It Works](../how-it-works.md) for the components and the worktree lifecycle.

### Do I need a separate Claude Code subscription or API key?

Yes. Bring your own. The bundled `claude` plugin manages a Claude Code
subprocess, and the bundled `codex` plugin manages an OpenAI Codex CLI
subprocess. They do not log in for you. If you can run `claude` or
`codex` from a terminal, the matching plugin can drive it. See
[Plugins](../plugins.md) for the contract.

The same model will hold for the future OpenCode runner plugin: each
plugin shells out to the agent CLI you already have authenticated.

## Automation

### Is it safe to enable auto-merge? When does the daemon merge things on my behalf?

The daemon merges only when **both** of the following are true: the repo has
`can_auto_merge` (or `can_auto_merge_dependabot` for Dependabot PRs)
explicitly toggled on, and the PR's required checks are passing and the PR is
in a mergeable state. Without those flags set, no plugin merges anything. The
default for new repos is off.

Toggle the flags from the Repo Settings screen (`boss repo show <name>`).

### When does the repair plugin fire? Can it loop?

The [plugins overview](../plugins.md#automation) describes the repair plugin, which fires when a session's
display status flips to **Failing** (CI red), **Conflict** (merge conflict
on the PR), or **Rejected** (review changes requested). It also runs a
periodic sweep that catches sessions stuck in those states across daemon
restarts.

A per-session cooldown (default `repair.cooldown_minutes = 1`) stops it from
hammering the same PR. The plugin also tracks the head SHA. If a repair
attempt completes and the SHA hasn't moved, it doesn't re-fire on the same
commit. So in practice it can attempt a fix, see the same failure, and back
off rather than spin.

### Why does the daemon need an agent runner plugin to start sessions?

Bossd itself doesn't know how to talk to Claude, Codex, or OpenCode. That
job lives in a runner plugin: `bossd-plugin-claude` for Claude Code,
`bossd-plugin-codex` for OpenAI Codex CLI, and `bossd-plugin-opencode`
on the roadmap. The plugin
satisfies the `AgentRunnerService` gRPC contract and owns the subprocess
lifecycle for its agent.

If no runner plugin loads, `bossd` stays healthy but every session-start
attempt fails fast with `no AgentRunner plugin loaded; install
bossd-plugin-claude (or another agent runner) and restart`.
Bossanova ships the `claude` and `codex` runners today; install the
matching agent CLI first.

## Cloud, privacy, and local-only

### What does Bossanova send to the cloud?

`bossd` does not register with Boss Cloud unless you create an account
and sign in with `boss login`. After you sign in, it connects to
`https://orchestrator.bossanova.dev` and streams **session metadata**:
session IDs, titles, states, PR numbers, repo IDs, and chat status
events. It also sends the WorkOS auth token established by `boss login`
so Boss Cloud knows which account a daemon belongs to.

It does **not** send the contents of your worktrees: no source code, no
diffs, no commit messages, no agent transcripts. The full inventory and the
opt-out paths are in [Privacy](../reference/privacy.md).

### Can I run Bossanova fully local with no cloud?

Yes. Set `cloud.orchestrator_url` to the empty string in your
[settings file](../reference/settings.md) (or export
`BOSSD_ORCHESTRATOR_URL=""`) and don't run `boss login`. The TUI, daemon, and
runner plugins all work without any upstream connection. You give up the web
app and any future cross-machine features that depend on it.

The full local-only posture, including what the daemon binds to and what it
won't try to dial out to, is documented in
[Security and Permissions](../reference/security-and-permissions.md).

## Worktrees, sessions, and chats

### Does my IDE see what's happening in a session?

The session lives as a normal git worktree on disk under `worktree_base_dir`.
Open it in any editor: VS Code, Vim, JetBrains, whatever. It works the way
you'd open any other directory. The agent doesn't need your IDE, and your IDE
doesn't need to know the agent is there.

The TUI is read-only on the agent's side: you watch its output, you reply via
chat, but Bossanova doesn't try to drive your editor. If you want to take over
manually, `cd` in and start typing. The agent is just a tmux pane.

### Can I transfer a chat between sessions?

No. Each session owns its chats and its worktree; chats can't be moved between
sessions. The session itself is the unit of work. When one is done, archive
it (`a` from the home view, or `boss archive <session-id>`). The session
lands in Trash, where you can restore or permanently delete it. Archiving
keeps the branch and removes the worktree, which is usually what you want.

## Skills

### What's a "skill" and where do they come from?

A skill is a markdown prompt-bundle (a directory containing `SKILL.md` plus
any helper files) that the [Plugins](../plugins.md)
auto-installs into a session so the agent has consistent instructions for
recurring jobs. The bundled set is **`boss`**, **`boss-repair`**,
**`boss-verify`**, and **`boss-finalize`**, embedded in the plugin binary.

### What if I don't want skills installed?

Decline at the one-time prompt that appears on first run; this sets
`skills_declined: true` in your settings file and the prompt won't appear again.
You can clear that flag manually in `settings.json` if you change your mind.

## Reporting and roadmap

### How do I report a bug?

Press `ctrl+b` from anywhere in the TUI to open the bug report form. It
collects boss/bossd version, OS/arch, per-session daemon heartbeats, the
current session, your session list, and the tail of `boss.log` and
`bossd.log`. You add a comment and submit; no source code, diffs, or agent
transcripts are included.

For step-by-step reproduction guidance, see
[Reporting bugs](./troubleshooting.md#reporting-bugs).

### What agents does Bossanova support?

Bossanova supports Claude Code through `bossd-plugin-claude` and OpenAI Codex
CLI through `bossd-plugin-codex`. OpenCode support is coming soon through
`bossd-plugin-opencode`.
