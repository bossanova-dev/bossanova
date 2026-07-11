---
title: Upgrade
description: Keep Bossanova up to date via Homebrew, the in-app upgrade prompt, or the boss upgrade CLI.
---

# Upgrade

Bossanova checks for new releases automatically and shows an in-app prompt when
one is available. You can also upgrade at any time from the command line.

## Via Homebrew (recommended)

If you installed with Homebrew, upgrade with the fully qualified tap formula:

```bash
brew upgrade bossanova-dev/tap/bossanova
```

No separate `brew tap` step is required. Restart the daemon so it picks up the
new `bossd` binary:

```bash
boss daemon restart
```

Then quit and relaunch `boss` so the TUI uses the new binary. If you use the
standalone HTTP MCP server, refresh its service to use the new `boss-mcp`
binary:

```bash
boss mcp install --force
```

## From the TUI (in-app upgrade)

When you open `boss`, it checks for a newer release in the background (results
are cached for 24 hours). If an upgrade is available, the home screen shows:

```
Upgrade available: boss v1.2.3 -> v1.3.0. [u]pgrade [d]ismiss
```

- Press **`u`** to download and install the new version.
- Press **`d`** to dismiss the prompt for this release.

After the upgrade installs, the banner changes to:

```
Upgrade installed. Quit boss after restart to use the new binary. [r]estart [esc] later
```

Press **`r`** to restart the daemon, then quit `boss` (`q`) and relaunch it to
run the new binary.

The in-app upgrade fetches public GitHub release assets and does **not** require
a GitHub login. (A `gh auth login` is still required for the PR and CI features
Bossanova uses day to day — see [Installation](./install.md).)

## Via the `boss upgrade` CLI

```bash
# Check whether a newer release is available without installing
boss upgrade --check

# Install the latest release (skips the interactive confirmation)
boss upgrade --yes
```

Running `boss upgrade` without `--yes` refuses to install non-interactively.
Useful flags:

| Flag              | What it does                                            |
| ----------------- | ------------------------------------------------------- |
| `--check`         | Check for an upgrade without installing.                |
| `--yes`           | Install without the interactive confirmation prompt.    |
| `--version <tag>` | Install a specific stable release tag (no prereleases). |
| `--no-restart`    | Do not restart the daemon after upgrading.              |

Upgrade checks require a stable release build; development builds report that a
stable version is required. Homebrew installs upgrade through the tap, so exact
`--version` installs are not supported there — run
`brew upgrade bossanova-dev/tap/bossanova` instead.

## Manual re-install via curl

Re-running the install script pulls the latest release binaries:

```bash
curl -fsSL https://bossanova.dev/install.sh | sh
```

## Verify the upgrade

```bash
boss version
boss repair doctor
```

`boss version` prints the running version; `boss repair doctor` confirms the
daemon and agent plugins are healthy after the upgrade.
