---
title: Plugins
description: The bundled bossd plugins and how they're loaded.
---

# Plugins

`bossd` is extended via out-of-process **plugin binaries** named
`bossd-plugin-<name>`. Plugins subscribe to daemon events over gRPC and
take autonomous actions. They run as separate processes, so a crashing
plugin does not bring down the daemon.

There are two flavors:

- **Agent runner plugins** own the subprocess lifecycle for one
  coding-agent CLI. At least one agent runner must be loaded before
  the daemon will start sessions.
- **Automation plugins** react to PR / CI events and dispatch agent
  sessions to handle them.

## Bundled plugins

### Agent runners

| Plugin     | Status      | Purpose                                                                                                                |
| ---------- | ----------- | ---------------------------------------------------------------------------------------------------------------------- |
| `claude`   | Available   | Owns the [Claude Code](https://claude.ai/download) subprocess for each session.                                        |
| `codex`    | Available   | Owns the [OpenAI Codex CLI](https://help.openai.com/en/articles/11096431-openai-codex-cli-getting-started) subprocess. |
| `opencode` | Coming soon | Will own the OpenCode CLI subprocess for each session.                                                                 |

### Automation

| Plugin       | Purpose                                                                                           |
| ------------ | ------------------------------------------------------------------------------------------------- |
| `dependabot` | Watches for Dependabot PRs and triggers an agent session to review them.                          |
| `linear`     | Watches Linear issues and triggers an agent session for matching ones.                            |
| `repair`     | Watches for failing CI / merge conflicts on open PRs and dispatches an agent session to fix them. |

## Loading plugins

Plugins are loaded automatically when their binary is present in the
same directory as `bossd` (Homebrew installs them under
`/opt/homebrew/libexec/plugins/`). To explicitly enable, disable, or
reconfigure a plugin, edit the [settings file](./reference/settings.md) under the
top-level `plugins` array and the per-plugin config blocks (`repair`,
etc.).

## Building plugins from source

```bash
make plugins
```

Plugin binaries land in `bin/`. See the [Build from source](./install.md#build-from-source)
section for the full toolchain.
