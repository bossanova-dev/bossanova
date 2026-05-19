---
title: Quick Start
description: 'Walk through Bossanova end-to-end: install, open the TUI, add a repo, start sessions, watch PR and CI state, schedule recurring work, clean up old chats, and sign in to Bossanova Cloud.'
---

import AsciinemaDemo from '@site/src/components/AsciinemaDemo';

# Quick Start

This page walks you from a fresh install to the core Bossanova flow: add a repo,
start agent work, chat with the agent, follow pull request and CI state,
schedule recurring sessions, archive finished work, clean up old chats, and sign
in to Bossanova Cloud when you want browser access.

Skip steps you've already done; cross-links point at the relevant settings or
guide page where one exists.

## 1. Install

Install Bossanova with Homebrew:

```bash
brew install bossanova-dev/tap/bossanova
```

Then install and start the daemon as a background service:

```bash
boss daemon install
```

Homebrew installs the `boss` and `bossd` binaries, but it does not automatically
register or start the daemon. `boss daemon install` is Bossanova's service setup
wrapper. On macOS it writes `~/Library/LaunchAgents/com.bossanova.bossd.plist`
and loads it with `launchctl` as a user LaunchAgent. On Linux it creates and
enables a user systemd service.

The daemon starts at login and restarts if it exits. It owns session state,
worktree cleanup, GitHub sync, and browser access.

## 2. Open boss for the first time

Launch the terminal UI:

```bash
boss
```

The home screen is the control center for active coding-agent work. It shows
sessions across repositories, with branch, pull request, review, and CI state in
one place.

<AsciinemaDemo src="/img/screenshots/tour/boss-open-dashboard.cast" />

On a fresh install, the repo list starts empty.

## 3. Add a repo

Press `r` to load the repository list, then press `a` to add a new repository.
Provide the path to a local folder if you already have the repository checked
out. Provide a GitHub URL if you want to check out a repository that you do not
yet have locally.

<AsciinemaDemo src="/img/screenshots/tour/boss-add-repo.cast" />

You can open an existing checkout or clone from a URL.

## 4. Configure repo settings

Open the repo settings before your first serious session. Confirm the base
branch, worktree directory, agent runner, and PR behavior match how this repo
ships.

<AsciinemaDemo src="/img/screenshots/tour/boss-repo-settings.cast" />

Bossanova works best when each repo has a predictable default runner:

- **Claude Code**: install `bossd-plugin-claude`.
- **Codex**: install `bossd-plugin-codex`.
- **Custom runner**: configure the command in repo settings.

See [Agent Runners](./guides/agent-runners.md) and
[Settings](./reference/settings.md) for the full configuration surface.

## 5. Start a session

Press `n` from the home screen.

The new-session flow asks for the repo, agent runner, and task. Bossanova creates
the branch and worktree before handing the prompt to the agent.

<AsciinemaDemo src="/img/screenshots/tour/boss-new-session.cast" />

Pick the session type that matches the job:

- **PR session** for implementation work that should land through GitHub.
- **Quick Chat** for lightweight questions or repo exploration.
- **Linear** when you want to start from an issue.

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

Open the scheduled sessions view to create recurring agent work.

Scheduled jobs are useful for repeated maintenance: dependency checks, weekly
cleanup, release prep, or any coding task that starts from the same prompt.

<AsciinemaDemo src="/img/screenshots/tour/boss-cron.cast" />

See [Scheduled Sessions](./guides/scheduled-sessions.md) for schedule format and
failure behavior.

## 9. Archive finished work

Press `a` on a completed session.

Archiving removes the local worktree while keeping the branch and pull request
history available.

<AsciinemaDemo src="/img/screenshots/tour/boss-archive.cast" />

## 10. Clean up old chats

Open Trash to review archived chats and permanently delete the ones you no
longer need.

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
