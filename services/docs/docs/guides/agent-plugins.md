---
title: Agent Plugins
description: Choose and configure the Claude Code or OpenAI Codex agent plugin.
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
| OpenCode | `bossd-plugin-opencode` | `opencode` | Roadmap   |

## Install the matching CLI

Install and authenticate the CLI for the agent you intend to use:

- Claude Code: install from [claude.ai/download](https://claude.ai/download),
  then confirm `claude` works in a terminal.
- OpenAI Codex CLI: install from the
  [OpenAI Codex CLI guide](https://help.openai.com/en/articles/11096431-openai-codex-cli-getting-started),
  then confirm `codex` works in a terminal.

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
```

Then restart `bossd` and run the doctor again.
