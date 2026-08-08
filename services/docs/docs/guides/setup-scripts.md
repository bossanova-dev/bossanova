---
title: Setup Scripts
description: Configure a per-repo setup script that runs every time Bossanova creates a new worktree.
---

import CommandTabs from '@site/src/components/CommandTabs';

# Setup Scripts

Each repository can have an optional setup script that runs
automatically whenever a new worktree is created for a session. Useful
for installing dependencies, copying configuration files, or any other
per-worktree initialization.

## Configuring

Set a setup script when adding a repo, or update it later:

<CommandTabs
chat='"set the setup script for my-repo to npm install"'
cli='boss repo update my-repo --setup-script "npm install"'
mcp="update_repo"
/>

Clear it with an empty string:

<CommandTabs
chat='"clear the setup script for my-repo"'
cli='boss repo update my-repo --setup-script ""'
mcp="update_repo"
/>

## Environment variables

The following environment variables are available to the setup script:

| Variable       | Description                                           |
| -------------- | ----------------------------------------------------- |
| `REPO_DIR`     | Path to the main git repository (the original clone). |
| `WORKTREE_DIR` | Path to the worktree being set up.                    |

These let you reference files in the main repo without hardcoding paths.
For example, to copy an `.env` file into each new worktree:

<CommandTabs
chat='"set the setup script for my-repo to copy .env into the worktree and then run npm install"'
cli={`boss repo update my-repo \\
  --setup-script 'cp "$REPO_DIR/.env" "$WORKTREE_DIR/.env" && npm install'`}
mcp="update_repo"
/>

## Automatic `.env` loading

Once a worktree contains a `.env` file, Bossanova loads it into the
environment of every agent session it starts in that worktree — you do
**not** need to `source` it or run direnv yourself. This is why the setup
script above copies `.env` into the worktree: the copy makes the file
present, and Bossanova picks it up automatically from there.

The daemon (`bossd`) runs under your OS service manager with no
interactive shell, so a normal shell's direnv/`.env` machinery never runs
for these sessions. To make repo-local variables available anyway,
Bossanova reads `<worktree>/.env` and overlays it **beneath** the
session's managed environment when it launches the agent. This applies to
interactive chats, resumed/woken chats, scheduled (cron) sessions,
headless runs, and automated repair runs alike.

A common use is resolving `${VAR}` placeholders in a project's
`.mcp.json`. For example, an MCP server configured with an
`Authorization: Bearer ${LINEAR_API_KEY}` header only authenticates if
`LINEAR_API_KEY` is present in the session environment — putting it in the
worktree `.env` is enough. See the [MCP guide](./mcp.md).

### Precedence and format

- **Managed values win.** The overlay is lowest-precedence: Bossanova's
  own `BOSS_*` variables and internal session values are never overridden
  by a repo `.env`, even if it defines the same key.
- **Missing is fine.** A worktree with no `.env` (or an empty/unreadable
  one) is a no-op — nothing is loaded and the session starts normally.
- **The parser is intentionally minimal.** Blank lines and `#` comment
  lines are ignored; an optional leading `export ` is stripped; one pair
  of surrounding quotes is removed from a value. There is **no** variable
  interpolation, **no** multiline values, and **no** inline-comment
  stripping (a value may legitimately contain `#`). Keep `.env` files to
  simple `KEY=value` lines.

> Values loaded from `.env` are treated as secrets and are never logged.
