---
sidebar_position: 2
title: CLI Reference
description: Pointers to the authoritative help text for boss and bossd.
---

# CLI Reference

The authoritative reference for every command, subcommand, and flag is
the help text built into the binary:

```bash
boss --help
boss <subcommand> --help
```

## Top-level commands

### `boss`

The interactive terminal UI and the CLI for non-interactive operations.

```bash
boss                              # launch the Terminal UI (TUI) on the Home screen
boss settings                      # view or update global settings
boss repo ls                       # list configured repos
boss repo update <repo-id> ...     # change repo fields from the shell
boss repair doctor                 # health-check the auto-repair pipeline
```

### `bossd`

The background daemon. Normally started by Homebrew's launchd plist or
your equivalent service manager. You rarely run it by hand. It takes
configuration from environment variables and `settings.json`; there
are no subcommands.

```bash
bossd               # run in foreground
bossd --version     # print version info
```

## Settings overrides

Most settings can be overridden by environment variable. Precedence
(highest wins): environment variable → `settings.json` → hardcoded
default. See
[Settings → Environment overrides](./settings.md#environment-overrides)
for the table.

A hand-curated reference page is on the roadmap. If you hit something
ambiguous in the help text, open an issue at
[bossanova-dev/bossanova/issues](https://github.com/bossanova-dev/bossanova/issues).
