---
title: How It Works
description: How Bossanova orchestrates worktrees, the daemon, and plugins.
---

# How It Works

Bossanova uses git worktrees to isolate each agent session in its own
directory. The daemon (`bossd`) manages session lifecycle, monitors PR
status, and coordinates plugins. The Terminal UI (TUI), `boss`, provides a unified
view across all active sessions.

Sessions run in dedicated worktrees, allowing simultaneous work on
multiple features without conflicts. Plugins listen for events like PR
creation, CI failures, and merge conflicts, then take autonomous actions.

![Session detail view](/img/screenshots/tui-session-detail.png)

## Components

- **`boss`:** the TUI. Reads daemon state over gRPC and
  presents the session list, session detail, and configuration views.
- **`bossd`:** the background daemon. Owns worktree creation,
  bookkeeping, PR polling, and plugin dispatch. Communicates with `boss`
  and with plugins via gRPC.
- **Plugins (`bossd-plugin-*`):** out-of-process binaries that
  subscribe to bossd events. There are two flavors:
  - _Agent runner plugins_ own a coding-agent CLI subprocess for each
    session. The bundled `claude` plugin runs Claude Code, and the
    bundled `codex` plugin runs OpenAI Codex CLI. `opencode` remains
    on the roadmap. The daemon needs at least one agent runner loaded
    to start sessions.
  - _Automation plugins_ react to PR events. `dependabot`, `linear`,
    and `repair` are bundled and optional.

## Worktree lifecycle

1. You create a session in `boss`. The daemon creates a new git worktree
   under `worktree_base_dir` (configurable; defaults to
   `~/.bossanova/worktrees`).
2. If the repository has a setup script configured, the daemon runs it
   inside the new worktree.
3. The configured agent runner plugin spawns its agent CLI inside the
   worktree.
4. As the agent works, the daemon watches for PR events and notifies
   plugins.
5. When you close the session, the worktree is removed.

See [Plugins](./plugins.md) for what each plugin does.
