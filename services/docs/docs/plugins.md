---
title: Plugins
description: The bundled bossd plugins and how they're loaded.
---

# Plugins

`bossd` is extended via out-of-process **plugin binaries** named
`bossd-plugin-<name>`. Plugins subscribe to daemon events over gRPC and
take autonomous actions. They run as separate processes, so a crashing
plugin does not bring down the daemon.

There are three flavors:

- **Agent plugins** own the subprocess lifecycle for one
  coding-agent CLI. At least one agent plugin must be loaded before
  the daemon will start sessions.
- **Task source plugins** surface external issues — Linear tickets,
  Sentry errors — in the new-session picker so you can start a session
  directly from one. They are user-initiated: nothing happens until you
  pick an issue.
- **Automation plugins** react to PR / CI events and dispatch agent
  sessions to handle them.

## Bundled plugins

### Agent plugins

| Plugin     | Status      | Purpose                                                                                                                |
| ---------- | ----------- | ---------------------------------------------------------------------------------------------------------------------- |
| `claude`   | Available   | Owns the [Claude Code](https://claude.ai/download) subprocess for each session.                                        |
| `codex`    | Available   | Owns the [OpenAI Codex CLI](https://help.openai.com/en/articles/11096431-openai-codex-cli-getting-started) subprocess. |
| `opencode` | Coming soon | Will own the OpenCode CLI subprocess for each session.                                                                 |

### Task sources

| Plugin   | Purpose                                                                                                                                                                                                    |
| -------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `linear` | Lists your Linear issues in the new-session picker so you can start a session from a ticket.                                                                                                               |
| `sentry` | Lists unresolved [Sentry](https://sentry.io) issues from your organization in the new-session picker, enriched with the latest stacktrace and tags, so you can start a fix session straight from an error. |

### Automation

| Plugin       | Purpose                                                                                           |
| ------------ | ------------------------------------------------------------------------------------------------- |
| `dependabot` | Watches for Dependabot PRs and triggers an agent session to review them.                          |
| `repair`     | Watches for failing CI / merge conflicts on open PRs and dispatches an agent session to fix them. |

#### The `sentry` task source

When a repo has Sentry credentials configured, the new-session flow gains
a **"Fix a Sentry issue"** option. Picking it lists unresolved issues from
across every project in your Sentry organization (most recently-seen first,
from the last 14 days). Each issue is rendered into a prompt seed for the
agent, including:

- the culprit, level, event and user counts, and first/last-seen times;
- a permalink back to the issue in Sentry;
- the innermost frames of the latest stacktrace; and
- useful tags (`environment`, `release`, `server_name`, `transaction`).

Selecting an issue starts a session pre-seeded with that context and a
suggested `fix/<short-id>` branch name. The plugin is read-only against
Sentry — it never resolves or comments on issues — and is only consulted
when you open the picker, never on a timer.

To enable it, set the **Sentry API key** and **Sentry org** (your
organization slug) in the repo's settings under the **Integrations**
section (`boss repo` → select the repo → Settings). The API token needs
the `event:read` scope. Both values are stored per-repo in bossd's
database. Self-hosted Sentry is not supported yet; the plugin targets
`sentry.io`.

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
