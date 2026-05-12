---
title: Web App
description: Drive your Bossanova sessions from a browser using the optional Boss Cloud web app.
---

# Web App

Bossanova ships with an optional web app at
[app.bossanova.dev](https://app.bossanova.dev) for managing your
agent sessions from a browser. It pairs with the local `bossd`
daemon over Boss Cloud, so anything you can see in the `boss` Terminal UI (TUI) you
can also see and drive from the web.

## What you can do

- **Sessions list.** See every active session across every daemon
  you've paired with your account, with live PR / CI status.
- **Session detail.** Drill into a single session: the worktree, the
  PR it produced, recent commits, plugin activity, and the agent's
  current task.
- **Chat terminal.** Stream the live agent terminal in your browser
  and send keystrokes back. Useful for nudging an agent that's stuck
  or asking a follow-up while you're away from the machine running
  the daemon.
- **Daemons list.** Manage which machines are registered to your
  account. Each `bossd` registers under a stable `daemon_id`
  (set via the `BOSSD_DAEMON_ID` env var; defaults to the machine
  hostname) so a laptop and a workstation can both appear,
  side-by-side.

## Routes

Four URL routes make up the web app, each mapping onto a feature
above:

| Route                       | View           | What it shows                                                                         |
| --------------------------- | -------------- | ------------------------------------------------------------------------------------- |
| `/`                         | Sessions list  | Every active session across every paired daemon, with live PR / CI status.            |
| `/sessions/:id`             | Session detail | A single session: worktree, PR, recent commits, plugin activity, current task.        |
| `/sessions/:sid/chats/:cid` | Chat terminal  | Live agent terminal for chat `cid` in session `sid`. Streams output, accepts keys.    |
| `/daemons`                  | Daemons list   | Machines registered to your account. One row per `daemon_id` from your settings file. |

Bookmarkable URLs mean a teammate can paste you a link to a specific
session or chat and you'll land in the same view, auth permitting.

## Getting started

1. **Sign in.** Visit
   [app.bossanova.dev](https://app.bossanova.dev) and authenticate
   with your account (WorkOS-backed SSO).
2. **Pair your daemon.** On the machine where `bossd` runs:

   ```bash
   boss login
   ```

   This kicks off a WorkOS device-code flow. Approve in the browser
   and `bossd` registers itself with Boss Cloud under its
   `daemon_id` (set via the `BOSSD_DAEMON_ID` env var; defaults to
   the machine hostname).

3. **Open the web app.** Your daemon should now appear under
   **Daemons**, and any sessions it's running should show up in
   **Sessions**.

## Local-only mode

The web app is optional. Set `BOSSD_ORCHESTRATOR_URL=""` (an
explicitly empty value) in `bossd`'s environment to opt out of cloud
sync entirely, or simply leave the daemon unauthenticated and the
web app won't see it. Every core feature, including TUI, worktrees, and plugins,
works without it.

## Authentication management

| Command            | What it does                                   |
| ------------------ | ---------------------------------------------- |
| `boss login`       | Pair this daemon with your Boss Cloud account. |
| `boss logout`      | Remove stored credentials from this machine.   |
| `boss auth-status` | Show whether this daemon is signed in.         |

To point at a non-production endpoint (e.g. a staging environment),
set the `BOSSD_ORCHESTRATOR_URL` and `BOSS_WORKOS_CLIENT_ID`
environment variables; see [Settings](../reference/settings.md) for
the full list of environment overrides.
