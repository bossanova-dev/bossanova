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

### Detected setup commands

When you add a repository, Bossanova inspects the repository without running any
repository code and proposes the first matching setup command in this order:

1. **Makefile target**: a `Makefile` target named `setup-worktree`, `setup`, or
   `bootstrap` (in that order), including targets defined in literal relative
   `include` files.
2. **Executable setup script**: an executable regular file at `script/setup`,
   `scripts/setup.sh`, or `bin/setup` (in that order; symlinks are not adopted).
3. **Lockfile install**: `pnpm-lock.yaml` (`pnpm install --frozen-lockfile`),
   `package-lock.json` (`npm ci`), `yarn.lock` (`yarn install --immutable` when
   `.yarnrc.yml` is present, otherwise `yarn install --frozen-lockfile`),
   `bun.lock` or `bun.lockb` (`bun install`), `uv.lock` (`uv sync`), or
   `Gemfile.lock` (`bundle install`).
4. **Tool versions**: `mise.toml` or `.mise.toml` (`mise install` when `mise`
   resolves in your login shell), or `.tool-versions` (`mise install` when
   `mise` resolves, otherwise `asdf install` when `asdf` resolves).

A `.devcontainer/devcontainer.json` or `.devcontainer.json` file that declares
`postCreateCommand` is reported as evidence only and is not adopted.

In the TUI add-repository wizard's open flow, the **Setup command** field is
pre-filled with the detected command and shows a `Detected from …` note.
Accepting the pre-filled value stores the detected structured command, editing
the field stores your custom text instead, and clearing the field registers the
repository with no setup command. In the clone flow, leaving **Setup command**
empty adopts whatever setup command is detected in the repository after cloning.

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

:::tip Copy only what agents need
Every key in the worktree `.env` reaches every agent session in that
worktree. Rather than copying your whole `.env`, write only the keys
agents need into the worktree (for example, filter it with
`grep -E '^(export )?(LINEAR_API_KEY|SENTRY_ORG)=' "$REPO_DIR/.env" > "$WORKTREE_DIR/.env"`)
and leave other secrets in the main checkout.
:::

## Automatic `.env` loading

Once a worktree contains a `.env` file, Bossanova loads it into the
environment of every agent session it starts in that worktree; you do
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
`LINEAR_API_KEY` is present in the session environment; putting it in the
worktree `.env` is enough. See the [MCP guide](./mcp.md).

### Precedence and format

- **Managed values win.** The overlay is lowest-precedence: Bossanova's
  own `BOSS_*` variables and internal session values are never overridden
  by a repo `.env`, even if it defines the same key.
- **CLI-redirecting keys are ignored.** `BOSS_SETTINGS_PATH`,
  `BOSS_KEYRING_BACKEND`, `BOSS_WEB_PORT`, `BOSS_CLOUD_URL` and
  `BOSSD_ORCHESTRATOR_URL` in a worktree `.env` are never passed to a
  session, so a development daemon's settings in your `.env` cannot point
  agents at a different Bossanova instance. Values Bossanova sets itself
  are unaffected.
- **Missing is fine.** A worktree with no `.env` (or an empty/unreadable
  one) is a no-op: nothing is loaded and the session starts normally.
- **The parser is intentionally minimal.** Blank lines and `#` comment
  lines are ignored; an optional leading `export ` is stripped; one pair
  of surrounding quotes is removed from a value. There is **no** variable
  interpolation, **no** multiline values, and **no** inline-comment
  stripping (a value may legitimately contain `#`). Keep `.env` files to
  simple `KEY=value` lines.

> Values loaded from `.env` are treated as secrets and are never logged.
