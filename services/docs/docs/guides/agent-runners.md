---
title: Agent Runners
description: Choose and configure the Claude Code or OpenAI Codex runner plugin.
---

# Agent Runners

Bossanova starts coding-agent sessions through runner plugins. Each runner owns
one CLI subprocess inside the session worktree, while `bossd` owns worktree
creation, repo setup, PR polling, and plugin dispatch.

## Bundled runners

| Runner   | Plugin                  | CLI        | Status    |
| -------- | ----------------------- | ---------- | --------- |
| Claude   | `bossd-plugin-claude`   | `claude`   | Available |
| Codex    | `bossd-plugin-codex`    | `codex`    | Available |
| OpenCode | `bossd-plugin-opencode` | `opencode` | Roadmap   |

## Install the matching CLI

Install and authenticate the CLI for the runner you intend to use:

- Claude Code: install from [claude.ai/download](https://claude.ai/download),
  then confirm `claude` works in a terminal.
- OpenAI Codex CLI: install from the
  [OpenAI Codex CLI guide](https://help.openai.com/en/articles/11096431-openai-codex-cli-getting-started),
  then confirm `codex` works in a terminal.

Bossanova does not log in to provider accounts for you. The runner shells out to
the local CLI, so any provider authentication, approvals mode, model choice, or
account policy comes from that CLI's own configuration.

## Pick the default runner

Unattended sessions:

```json
{
  "default_agent": "codex"
}
```

Use `claude` to make Claude Code the default, or `codex` to make OpenAI Codex
CLI the default. The daemon refuses to start sessions when no agent runner
plugin is loaded.

## Verify runners are loaded

Run:

```bash
boss repair doctor
```

If session start fails with `no AgentRunner plugin loaded`, confirm the runner
binary sits next to `bossd` or in the Homebrew plugin directory:

```bash
which bossd
which bossd-plugin-claude
which bossd-plugin-codex
```

Then restart `bossd` and run the doctor again.
