---
title: Agent Plugins
description: Choose and configure the Claude Code, OpenAI Codex, or OpenCode agent plugin.
slug: /guides/agent-runners
---

# Agent Plugins

Bossanova starts coding-agent sessions through agent plugins. Each plugin owns
one CLI subprocess inside the session worktree, while `bossd` owns worktree
creation, repo setup, PR polling, and plugin dispatch.

## Bundled plugins

| Agent    | Plugin                  | CLI        | Status    |
| -------- | ----------------------- | ---------- | --------- |
| Claude   | `bossd-plugin-claude`   | `claude`   | Available |
| Codex    | `bossd-plugin-codex`    | `codex`    | Available |
| OpenCode | `bossd-plugin-opencode` | `opencode` | Available |

## Install the matching CLI

Install and authenticate the CLI for the agent you intend to use:

- Claude Code: install from [claude.ai/download](https://claude.ai/download),
  then confirm `claude` works in a terminal.
- OpenAI Codex CLI: install from the
  [OpenAI Codex CLI guide](https://help.openai.com/en/articles/11096431-openai-codex-cli-getting-started),
  then confirm `codex` works in a terminal.
- OpenCode: install from [opencode.ai](https://opencode.ai), then confirm
  `opencode` works in a terminal.

Bossanova does not log in to provider accounts for you. The plugin shells out to
the local CLI, so any provider authentication, approvals mode, model choice, or
account policy comes from that CLI's own configuration.

## Pick the default agent

Unattended sessions:

```json
{
  "default_agent": "codex"
}
```

Use `claude` to make Claude Code the default, or `codex` to make OpenAI Codex
CLI the default. The daemon refuses to start sessions when no agent plugin is
loaded.

## Verify plugins are loaded

Run:

```bash
boss repair doctor
```

If session start fails with `no AgentRunner plugin loaded`, confirm the plugin
binary sits next to `bossd` or in the Homebrew plugin directory:

```bash
which bossd
which bossd-plugin-claude
which bossd-plugin-codex
which bossd-plugin-opencode
```

Then restart `bossd` and run the doctor again.

## OpenCode

The OpenCode plugin (`bossd-plugin-opencode`) drives the
[OpenCode](https://opencode.ai) CLI. This slice targets **opencode v1.18.3**.

### Routing

`bossd` routes a session to the OpenCode plugin purely by the session's agent
name matching the plugin's `GetInfo` name, `opencode`. There is no per-agent
switch or hardcoded allow-list — the dispatcher resolves the free-form agent
name against the registry of loaded plugins keyed by their reported names. Set
`opencode` as the default:

```json
{
  "default_agent": "opencode"
}
```

or choose OpenCode per session; the dispatcher resolves it with no extra
configuration.

### Headless invocation

For unattended runs the plugin drives the CLI headlessly with
`opencode run --format json …`. The `--format json` flag makes OpenCode emit a
JSON event stream that the plugin parses; this is the non-interactive
invocation `bossd` uses (there is no TUI in the daemon-driven flow).

### Permission posture

The plugin uses **`--auto`** as its default permission posture. `--auto` grants
the autonomous approvals the orchestrator needs while retaining OpenCode's own
guardrails, so it is the documented default.

OpenCode also exposes `--dangerously-skip-permissions` as an undocumented
fuller-bypass alternative that removes those guardrails. It is intentionally
**not** the default and is mentioned here only so operators who explicitly want
it know it exists.

### Debugging session state

OpenCode persists session state in a SQLite store outside the worktree.
Operators can locate that database for debugging with:

```bash
opencode db path
```

### Upstream rename

The OpenCode project moved from `sst/opencode` to `anomalyco/opencode`. Its
documentation lives at [opencode.ai](https://opencode.ai).
