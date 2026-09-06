---
title: Upgrade
description: Keep Bossanova up to date via Homebrew, the in-app upgrade prompt, or the boss upgrade CLI.
---

import CommandTabs from '@site/src/components/CommandTabs';

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

<CommandTabs
cli="boss daemon restart"
/>

Then quit and relaunch `boss` so the TUI uses the new binary. If you use the
standalone HTTP MCP server, refresh its service to use the new `boss-mcp`
binary:

<CommandTabs
cli="boss mcp install --force"
/>

Upgrade commands act on the local install (the daemon service, the MCP service
file, and the binary on disk), so none of them has an MCP tool. The **Chat** and
**MCP** tabs say so explicitly rather than leaving the gap implicit.

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
Bossanova uses day to day; see [Installation](./install.md).)

## Via the `boss upgrade` CLI

Check whether a newer release is available without installing:

<CommandTabs
cli="boss upgrade --check"
/>

Install the latest release. `--yes` is required: without it the command refuses
and exits rather than prompting.

<CommandTabs
cli="boss upgrade --yes"
/>

Useful flags:

| Flag              | What it does                                            |
| ----------------- | ------------------------------------------------------- |
| `--check`         | Check for an upgrade without installing.                |
| `--yes`           | Required to install; without it the command refuses.    |
| `--version <tag>` | Install a specific stable release tag (no prereleases). |
| `--no-restart`    | Do not restart the daemon after upgrading.              |

Upgrade checks require a stable release build; development builds report that a
stable version is required. Homebrew installs upgrade through the tap, so exact
`--version` installs are not supported there; run
`brew upgrade bossanova-dev/tap/bossanova` instead.

## Manual re-install via curl

Re-running the install script pulls the latest release binaries:

```bash
curl -fsSL https://bossanova.dev/install.sh | sh
```

## Verify the upgrade

Print the running version:

<CommandTabs
cli="boss version"
/>

Confirm the daemon and agent plugins are healthy after the upgrade:

<CommandTabs
chat='"run the repair doctor"'
cli="boss repair doctor"
mcp="repair_doctor"
/>

`boss version` reports the binary you are actually running; `boss repair doctor`
is the one check on this page an agent can run for you, so it carries a real
**Chat** and **MCP** tab.
