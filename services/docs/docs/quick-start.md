---
title: Quick Start
description: 'Walk through Bossanova end-to-end: install, open the TUI, add a repo, start sessions, watch PR and CI state, schedule recurring work, clean up old chats, and sign in to Bossanova Cloud.'
---

import AsciinemaDemo from '@site/src/components/AsciinemaDemo';
import CommandTabs from '@site/src/components/CommandTabs';
import Tabs from '@theme/Tabs';
import TabItem from '@theme/TabItem';

# Quick Start

This page walks you from a fresh install to the core Bossanova flow: add a repo,
start agent work, chat with the agent, follow pull request and CI state,
schedule recurring sessions, archive finished work, clean up old chats, and sign
in to Bossanova Cloud when you want browser access.

Skip steps you've already done; cross-links point at the relevant settings or
guide page where one exists.

## 1. Install

Bossanova ships prebuilt binaries for **macOS** (Intel and Apple Silicon) and
**Linux x86_64**. Pick your platform:

<Tabs groupId="os">
<TabItem value="macos" label="macOS" default>

Install with Homebrew:

```bash
brew install bossanova-dev/tap/bossanova
```

Then register the daemon service:

<CommandTabs
cli="boss daemon install"
/>

Registering the service touches this machine's launchd, so there is no MCP tool
for it — the tabs say so rather than hiding the gap.

</TabItem>
<TabItem value="linux" label="Linux (x86_64)">

Install with Homebrew (the tap ships a Linux x86_64 build):

```bash
brew install bossanova-dev/tap/bossanova
```

Then register the daemon service:

<CommandTabs
cli="boss daemon install"
/>

Registering the service touches this machine's service manager, so there is no
MCP tool for it — the tabs say so rather than hiding the gap. On Linux that
manager is a **systemd user service**, so `systemctl --user` must be available
(most desktop and server distros; minimal containers and some WSL setups are not
supported).

**Alternative — install script.** The install script downloads the binaries,
configures plugins, and registers the systemd daemon for you (so you can skip the
separate `boss daemon install` step):

```bash
curl -fsSL https://bossanova.dev/install.sh | sh
```

The script currently requires the [Claude Code CLI](https://claude.ai/download) to
be installed first; if you use a different agent plugin, install with Homebrew above
instead.

**Linux arm64** is not prebuilt yet — [build from source](./install.md#build-from-source).

</TabItem>
</Tabs>

Check that the daemon is running:

<CommandTabs
cli="boss daemon status"
/>

Daemon status inspects this machine's service manager and socket, so it has no
MCP tool either.

Expected output, abridged — the command also prints the settings, app-data, and
socket paths, among other daemon details (the service path is a launchd plist on
macOS and a systemd unit on Linux; PID and paths vary by machine):

```text
Daemon is running.
  PID:     11537
  service: ~/Library/LaunchAgents/com.bossanova.bossd.plist
```

The daemon owns session state, worktree cleanup, GitHub sync, and browser access.

## 2. Open boss for the first time

Launch the terminal UI:

<CommandTabs
cli="boss"
/>

The TUI is a terminal program on this machine, so there is no chat prompt and no
MCP tool for launching it — an agent reaches the same session data through read
tools such as `list_sessions` instead.

The interface tabs above are their own group: picking **Chat**, **CLI**, or
**MCP** here does not change the macOS/Linux platform choice in step 1, and
picking a platform does not change the interface.

The home screen is the control center for active coding-agent work. It shows
sessions across repositories, with branch, pull request, review, and CI state in
one place.

<AsciinemaDemo src="/img/screenshots/tour/boss-open-dashboard.cast" />

On a fresh install, the repo list starts empty.

## 3. Add a repo

Press `s` to open Settings, then `r` to load the repository list, then press
`a` to add a new repository. Provide the path to a local folder if you already
have the repository checked out. Provide a GitHub URL if you want to check out a
repository that you do not yet have locally.

<AsciinemaDemo src="/img/screenshots/tour/boss-add-repo.cast" />

You can open an existing checkout or clone from a URL.

## 4. Configure repo settings

Open the repo settings before your first serious session. Confirm the base
branch, worktree directory, agent, and PR behavior match how this repo
ships.

<AsciinemaDemo src="/img/screenshots/tour/boss-repo-settings.cast" />

See [Agent Plugins](/guides/agent-runners) and
[Settings](./reference/settings.md) for the full configuration surface.

## 5. Start a session

Press `n` from the home screen.

The new-session flow asks for the repo, agent, and task. Bossanova creates
the branch and worktree before handing the prompt to the agent.

<AsciinemaDemo src="/img/screenshots/tour/boss-new-session.cast" />

Pick the session type that matches the job:

- **PR session** for implementation work that should land through GitHub.
- **Quick Chat** for lightweight questions or repo exploration.
- **Linear** when you want to start from an issue.
- **Sentry** when you want to start from an unresolved error. (Shown once
  the repo has Sentry credentials set in its Integrations settings.)

## 6. Chat with the agent

Open a session and attach to the chat.

Use the chat pane when the agent needs direction, review, or a final decision.
Bossanova keeps the session state visible without turning every interaction into
a separate shell workflow.

Use `Ctrl-X` to detach from a session and leave it running, or use `Ctrl-C`
twice to stop the session and exit.

<AsciinemaDemo src="/img/screenshots/tour/boss-chat.cast" />

## 7. Watch PR and CI state

Return to the dashboard to see whether work is running, waiting for review,
blocked on CI, or ready to merge.

<AsciinemaDemo src="/img/screenshots/tour/boss-pr-status.cast" />

For the full pull request flow, see
[PR Lifecycle](./guides/pr-lifecycle.md).

## 8. Set up scheduled jobs

Press `s` to open Settings, then `c` to open the scheduled sessions view and
create recurring agent work.

Scheduled jobs are useful for repeated maintenance: dependency checks, weekly
cleanup, release prep, or any coding task that starts from the same prompt.

<AsciinemaDemo src="/img/screenshots/tour/boss-cron.cast" />

See [Scheduled Sessions](./guides/scheduled-sessions.md) for schedule format and
failure behavior.

## 9. Archive finished work

Select a completed session with `enter`, then press `a` from the session view
(beside the merge action).

Archiving removes the local worktree while keeping the branch and pull request
history available.

<AsciinemaDemo src="/img/screenshots/tour/boss-archive.cast" />

## 10. Clean up old chats

Press `s` to open Settings, then `t` to open Trash, where you can review
archived chats and permanently delete the ones you no longer need.

Archiving keeps completed work out of the active dashboard. Trash gives you the
final cleanup step when a branch, PR, or chat history is no longer useful.

<AsciinemaDemo src="/img/screenshots/tour/boss-trash.cast" />

## 11. Sign in to Bossanova Cloud

Sign in from the TUI when you want browser access to the same local sessions.

Bossanova Cloud is a paid add-on to the free Bossanova client. It lets you
manage coding sessions remotely from the web and manage boss sessions on
multiple machines in one place. Sessions are securely streamed to the browser so
you can work from anywhere.

<AsciinemaDemo src="/img/screenshots/tour/boss-cloud-sign-in.cast" />

See [Web App](./guides/web.md) for the full cloud setup.

## Next steps

- Learn the full pull request flow in [PR Lifecycle](./guides/pr-lifecycle.md).
- Schedule recurring work with
  [Scheduled Sessions](./guides/scheduled-sessions.md).
- Set up browser access in [Web App](./guides/web.md).
- Use the [CLI Reference](./reference/cli-reference.md) when you need exact
  command flags.
