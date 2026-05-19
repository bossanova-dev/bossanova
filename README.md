# <img src="https://bossanova.dev/logo.png" height="25" width="25" /> bossanova

<p>
  <a href="https://github.com/bossanova-dev/bossanova/releases"><img src="https://img.shields.io/github/v/release/bossanova-dev/bossanova" alt="Latest release"></a>
  <a href="#license"><img src="https://img.shields.io/badge/license-MIT-blue.svg" alt="License: MIT"></a>
</p>

Bossanova is an orchestrator for Claude Code and Codex.

Orchestrate AI agents across multiple machines in isolated workspaces. Repair
and respond to reviews automatically. Run scheduled tasks. Control remote Claude
Code and Codex sessions from the web (remote control requires a Bossanova Cloud
subscription).

## Getting Started

Start with one command:

```bash
boss
```

From there, the terminal UI (TUI) handles the core workflow:

- [Open the dashboard](#open-the-dashboard)
- [Add a repository](#add-a-repository)
- [Configure repo settings](#configure-repo-settings)
- [Start a session](#start-a-session)
- [Chat with the agent](#chat-with-the-agent)
- [Watch PR and CI state](#watch-pr-and-ci-state)
- [Set up scheduled jobs](#set-up-scheduled-jobs)
- [Archive finished work](#archive-finished-work)
- [Clean up old chats](#clean-up-old-chats)
- [Sign in to Bossanova Cloud](#sign-in-to-bossanova-cloud)

### Open the dashboard

The home screen shows active sessions across your repositories, grouped around
the pull requests and human decisions that need attention.

<img src="services/marketing/public/screenshots/tour/gifs/boss-open-dashboard.gif" alt="opening the Bossanova dashboard" width="900">

### Add a repository

Use the TUI to add a local checkout once. After that, Bossanova can create
isolated worktrees for future sessions in that repository.

<img src="services/marketing/public/screenshots/tour/gifs/boss-add-repo.gif" alt="adding a repository in the Bossanova TUI" width="900">

### Configure repo settings

Confirm the setup command, merge strategy, and automation toggles before
starting serious work in the repository.

<img src="services/marketing/public/screenshots/tour/gifs/boss-repo-settings.gif" alt="configuring repository settings in the Bossanova TUI" width="900">

### Start a session

Press `n` from the home screen.

Pick the repository, choose the agent runner, describe the work, and let
Bossanova create the branch and worktree for the session.

<img src="services/marketing/public/screenshots/tour/gifs/boss-new-session.gif" alt="starting a new coding-agent session in the Bossanova TUI" width="900">

### Chat with the agent

Open a session and attach to the agent chat.

Bossanova keeps the terminal focused on the active conversation while the daemon
tracks lifecycle state in the background.

<img src="services/marketing/public/screenshots/tour/gifs/boss-chat.gif" alt="chatting with an AI coding agent in Bossanova" width="900">

### Watch PR and CI state

Return to the home screen to see whether work is still running, waiting for
review, blocked on CI, or ready to merge.

<img src="services/marketing/public/screenshots/tour/gifs/boss-pr-status.gif" alt="watching pull request and CI status in Bossanova" width="900">

### Set up scheduled jobs

Open the scheduled sessions view to create recurring agent work.

Use scheduled jobs for chores that should happen on a cadence: dependency
checks, weekly cleanup, release prep, or any repeated coding task that starts
from the same prompt.

<img src="services/marketing/public/screenshots/tour/gifs/boss-cron.gif" alt="setting up a Bossanova scheduled job" width="900">

### Archive finished work

Press `a` on a completed session.

Archiving removes the local worktree while keeping the branch and pull request
history intact.

<img src="services/marketing/public/screenshots/tour/gifs/boss-archive.gif" alt="archiving a completed Bossanova session" width="900">

### Clean up old chats

Open Trash to review archived chats and permanently delete the ones you no
longer need.

Archiving keeps completed work out of the active dashboard; Trash gives you the
final cleanup step when a branch, PR, or chat history is no longer useful.

<img src="services/marketing/public/screenshots/tour/gifs/boss-trash.gif" alt="deleting old archived chats from Bossanova Trash" width="900">

### Sign in to Bossanova Cloud

Sign in from the TUI when you want browser access to the same local sessions.

Bossanova Cloud is a paid add-on to the free Bossanova client. It lets you
manage coding sessions remotely from the web and manage boss sessions on
multiple machines in one place. Sessions are securely streamed to the browser so
you can work from anywhere.

<img src="services/marketing/public/screenshots/tour/gifs/boss-cloud-sign-in.gif" alt="signing in to Bossanova Cloud from the TUI" width="900">

## Installation

Install with Homebrew:

```bash
brew install bossanova-dev/tap/bossanova
```

Then open the TUI:

```bash
boss
```

See the docs for [installation](https://docs.bossanova.dev/install),
[quick start](https://docs.bossanova.dev/quick-start), and
[CLI reference](https://docs.bossanova.dev/reference/cli-reference).

## What It Runs

- **`boss`**: the terminal UI for managing agent sessions.
- **`bossd`**: the local daemon that owns session lifecycle and git operations.
- **Agent plugins**: bundled Claude Code and Codex runners, plus automation
  plugins for repair, review, scheduled work, and integrations.
- **Boss Cloud**: optional browser access to the same local sessions, with
  real-time GitHub updates for conflicts, code review comments, and failing
  checks.

Read more in [How It Works](https://docs.bossanova.dev/how-it-works),
[Plugins](https://docs.bossanova.dev/plugins), and
[Security and Permissions](https://docs.bossanova.dev/reference/security-and-permissions).

## Build From Source

```bash
git clone https://github.com/bossanova-dev/bossanova.git
cd bossanova
make
```

## Contributing

Issues and pull requests are welcome. Before opening a PR, run:

```bash
make lint
make test
```

## License

MIT
