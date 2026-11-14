---
title: Web App
description: Drive your Bossanova sessions from a browser using the optional Boss Cloud web app.
---

import CommandTabs from '@site/src/components/CommandTabs';

# Web App

Bossanova ships with an optional web app at
[app.bossanova.dev](https://app.bossanova.dev) for managing your
agent sessions from a browser. It pairs with the local `bossd`
daemon over Boss Cloud, so anything you can see in the `boss` Terminal UI (TUI) you
can also see and drive from the web.

## What you can do

- **Sessions.** See every active session across every daemon you've
  paired with your account, grouped into Needs you, Ready for
  review and In progress, with live PR / CI status. Choose a
  session and the list moves aside: its chats slide in beside it, and
  its alert (what it needs from you, with buttons to act on it) heads
  the pane, above the session's history. An alert goes away when what it
  reports is fixed; the web app has no Dismiss (the TUI's `x` still
  dismisses a fix alert). Start a session from the **New session** row, one step at a
  time, without leaving the page.
- **Chat terminal.** Choose a chat to stream its live agent terminal in
  the pane and send keystrokes back. Its tools (Paste, Upload, Stop, full
  screen and Delete chat) sit on a slim strip across the top of the terminal. Useful for nudging an agent that's
  stuck or asking a follow-up while you're away from the machine running
  the daemon. **New chat** shows the agents to start one with.
- **Scheduler.** See every cron job across your daemons, run one now,
  pause or resume it, and add or edit jobs. See
  [Cron Jobs](./cron-jobs.md#the-web-scheduler).
- **Daemons list.** Manage which machines are registered to your
  account. Each `bossd` registers under a stable `daemon_id`
  (set via the `BOSSD_DAEMON_ID` env var; defaults to the machine
  hostname) so a laptop and a workstation can both appear,
  side-by-side.

Organization owners on a Bossanova Cloud plan also get
[session webhooks](./webhooks.md), which send a signed HTTPS request to
their own server whenever a session changes state.
Owners invite teammates, set their roles and remove members under
**Settings → Organization**. See
[Organizations and Members](./organizations.md).

## Personal API tokens

Open **Settings → API tokens** to create, list or revoke personal tokens for
hosted MCP and bots. Choose a name, organization and permissions. **Read-only**
selects the read permissions you hold; **Full access** selects all your available
permissions, and **Custom** lets you choose them individually. Expiry defaults to
**Never**; choose a duration or date to set an expiry.

Copy the token when it appears: its full value is shown only once. The dialog
also includes an MCP configuration snippet. After closing it, the list keeps only
a hint, permissions, dates and status. Revoke a token you no longer need; you must
confirm before it stops working. At the 50-token limit, revoke an unused token
before creating another. See [Personal API tokens](./mcp.md#personal-api-tokens)
for connecting to hosted MCP.

## Getting around

The header links the three main pages: **Sessions**, **Review** and
**Scheduler**. **Review** shows how many PRs wait for review across your
repositories. The avatar at the right shows your Google profile picture
when you signed in with Google, otherwise your Gravatar, otherwise your
initial. It opens your email, **Settings** and **Sign out**. On a phone the
header is one row: the logo, the page you are on and a menu button, marked with
a dot while PRs wait for review. The menu opens from the bottom of the screen
with the three pages, **Settings** and **Sign out**.

The Sessions page bar shows your active filters at its left; remove one with
its ×, or use **Clear**. The filter button stays at its right. With a session
open, the bar shows its title instead (hover it and click the pencil to
rename it; on a touch screen the pencil stays in view, and a phone lists
**Rename** under **⋯**) and its actions: Merge once the PR is ready to merge, the review summary,
GitHub, the tracker issue and Archive.

With a session open, drag the line after the session list or after the chats
to resize that column; double-click the line to put it back. Or focus it with
Tab and press `←` or `→` (with `Shift` for bigger steps), `Home` or `End`, and
`Enter` to reset. The pane always keeps at least 420px, and each width is
remembered in this browser.

On a tablet the list steps aside once a session is open, and the chats and the
pane share the screen, with only the chats' line to drag; **Back** in the bar returns to the list. On a phone the
page shows one thing at a time, held upright or on its side: the list, then a
session's chats with its alert above them, then a chat on its own. The session's
actions open from **⋯** at the top right, and the filters open from the bottom of
the screen. On a touch screen rename a chat from its row's **⋯**. A chat's
terminal fits the screen's width and refits when you turn the phone, and it stays
above the on-screen keyboard.

## Routes

These URL routes make up the web app, each mapping onto a feature
above. Every `/sessions` address is one page, so moving between sessions
and chats keeps the list where it was:

| Route                       | View          | What it shows                                                                         |
| --------------------------- | ------------- | ------------------------------------------------------------------------------------- |
| `/` or `/sessions`          | Sessions      | Every active session across every paired daemon, with live PR / CI status.            |
| `/sessions/:id`             | Session       | The list, then the session's chats, and its alert or the prompt to pick a chat.       |
| `/sessions/:sid/chats/:cid` | Chat terminal | Live terminal for chat `cid` in session `sid`. Add `?full=1` to give it the window.   |
| `/sessions/:id/chats/new`   | New chat      | The agents to start a new chat in session `id` with, the one used last first.         |
| `/sessions/new`             | New session   | The steps to start a session: repository, machine, agent, kind, then the details.     |
| `/review/:repo`             | Review queue  | PRs waiting for a person, confidence counts, sort controls and links to proof pages.  |
| `/scheduler`                | Scheduler     | Cron jobs across your daemons, in Scheduled and Paused sections.                      |
| `/daemons`                  | Daemons list  | Machines registered to your account. One row per `daemon_id` from your settings file. |

Bookmarkable URLs mean a teammate can paste you a link to a specific
session or chat and you'll land in the same view, auth permitting.

## Getting started

1. **Sign in.** Visit
   [app.bossanova.dev](https://app.bossanova.dev) and authenticate
   with your account (WorkOS-backed SSO).
2. **Pair your daemon.** On the machine where `bossd` runs:

   <CommandTabs
   cli="boss login"
   />

   This kicks off a WorkOS device-code flow. Approve in the browser
   and `bossd` registers itself with Boss Cloud under its
   `daemon_id` (set via the `BOSSD_DAEMON_ID` env var; defaults to
   the machine hostname). For what the command prints and how to
   approve it, see [Signing In](./login.md#cli).

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

## Keyboard shortcuts

Press `?` or `⌘K` on any screen to open the keyboard-shortcut launcher.
The launcher is a searchable command palette: type to filter, navigate with
`↑`/`↓`/`Home`/`End`, and press `Enter` to run the highlighted action.
Mouse users can hover to highlight a row and click to run it.

| Screen    | Key                    | Action                                                        |
| --------- | ---------------------- | ------------------------------------------------------------- |
| Sessions  | `J` `K` or `↑` `↓`     | Move through the sessions, or a session's chats               |
| Sessions  | `Enter` or `→`         | Open the session or chat; from the open one, go into it       |
| Sessions  | `←` on a chat          | Back to the session in the list                               |
| Sessions  | `Esc`, `←` or `Ctrl X` | Back out: leave full screen, close the chat, then the session |
| Sessions  | `N`                    | New chat while a session is open, else new session            |
| Sessions  | `C`                    | New session                                                   |
| Sessions  | `/`                    | Filters                                                       |
| Session   | `E`                    | Rename the session                                            |
| Session   | `F`                    | Open chat full screen, or back                                |
| Session   | `M`                    | Merge                                                         |
| Session   | `R`                    | Review summary                                                |
| Session   | `G`                    | Open pull request                                             |
| Session   | `I`                    | Open tracker issue                                            |
| Session   | `A`                    | Archive session                                               |
| Scheduler | `N`                    | New job                                                       |
| Scheduler | `R`                    | Run now                                                       |
| Scheduler | `E`                    | Edit job                                                      |
| Scheduler | `Space`                | Pause or resume                                               |
| Review    | `C`                    | Show or hide chats                                            |
| Review    | `N`                    | New chat                                                      |

While a session is open, moving through the sessions opens the one you land
on a moment after you stop, and moving through its chats while one is open
switches to the one you land on, keeping the keyboard in the list; neither
fills Back with every row you pass. Moving onto a folded section opens it.
`→` on the open session goes into its chats, and `Enter` or `→` on the open
chat goes into its terminal.

New chat and New session list their choices with numbers: press `1`–`9` to
pick one, `Enter` to take the highlighted one, and `Esc` to go back a step.

On the chat terminal, keys typed into the terminal go to the agent session,
so `?` and the keys above work only while focus is outside the terminal;
`Ctrl X` inside it closes the chat.

## Review screen

Open `/review/<owner>/<name>/<pr>?head=<40-hex-sha>` as a Bossanova Cloud user to review boss's stored PR record. The owner and name are the GitHub repository slug. A supplied head pins the link to that commit; a different current head disables actions and offers a link to the current record.

The screen offers Merge and Reject (send back to repair with a note), each after confirmation. Move between PRs with `J` and `K` or the arrow keys. Proof-page generators link to this route using their manifest's repository slug, PR number and head.

The PR's chats sit in a drawer beside its review summary. Toggle it with the Chats button or `C`; the choice is remembered. Choose a chat to work in its live terminal there, or start one with New chat or `N`. Esc outside the terminal returns to the list, and Open full screen opens the chat's own page. Drag the dividers, or focus one and press the arrow keys, to resize the list and the drawer. On a tablet the review queue steps aside while the drawer is open; **Show review queue** brings it back. On a phone the drawer takes the summary's place, and a chat opened from it has the screen to itself, above the on-screen keyboard.

## Encrypt the web terminal

Open **Settings → Terminal encryption**, edit the browser label and choose **Set up this browser**.
Your private key stays in this browser. Copy the fingerprint and run the displayed
`boss viewer approve <id> --fingerprint <fingerprint>` command on each of your machines.
Compare the daemon fingerprint printed by the command with the page, then confirm that they match.
If a machine identity changes, check it before forgetting the old pin and confirming the replacement.

Pairing prepares terminal encryption; encrypted terminal transport is not available yet.
A browser without IndexedDB, X25519 or Ed25519 support cannot pair.

**Forget this browser** removes the registry request, browser keys and machine pins. Run
`boss viewer revoke <id>` on each machine too: deleting the registry request cannot revoke a local
approval. Clearing browser storage loses the keys; set up the browser again and revoke its old id.
