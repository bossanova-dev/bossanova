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

- **Sessions.** One page lists every active session across every daemon
  you've paired with your account, as a board with columns for **Needs
  you**, **Working** and **Idle**, or as a list. The view tabs above it
  switch between **All sessions**, **Needs you** and **Working**, and any
  view you saved. Each card shows the repository and PR, the title, the
  PR's size, its ticket, its chats and when it last changed, with the
  session's status pill at the top right.
- **Session page.** Open a session to see it on one page: its **Proof** (the
  PR's proof page, framed in place), its **Chats** (each chat's
  conversation, with a reply box) and its **Terminal**, plus a **Details**
  panel with its repository, PR, ticket, branch, size, checks and why it
  needs you. **Merge** sits beside them once the PR can merge. The round
  buttons at the top are pinned actions; every other action is under
  **⋯**, where you can pin it too.
- **Chats.** A chat reads as a conversation: your messages on the right,
  the agent's replies as plain text, and its steps folded into one line.
  A question the agent asks docks above the reply box, with its options
  as one-tap answers. To give the agent files, use the paperclip, drop
  them on the reply box or paste them in. Each file uploads at once and
  shows its progress; **Send** waits for them, then the agent gets each
  file's path ahead of your message. The files go to your machine through
  Bossanova Cloud, as the conversation does, so they upload even when the
  terminal is end-to-end encrypted. **New chat** opens an empty composer
  with the agent, model and effort to start it with.
- **Settings.** Your own settings (Preferences, Profile, Notifications,
  Security & access, Agent accounts, Session actions, Views & favorites),
  then each organization's (General, Members, Repositories, Daemons,
  Scheduler, Triggers, Webhooks, Proof pages, Notes, Billing). The
  Scheduler lists every cron job; see
  [Cron Jobs](./cron-jobs.md#the-web-scheduler).

Organization owners on a Bossanova Cloud plan also get
[session webhooks](./webhooks.md), which send a signed HTTPS request to
their own server whenever a session changes state.
Owners invite teammates, set their roles and remove members on each
organization's **Members** settings page. See
[Organizations and Members](./organizations.md).

## Personal API tokens

Open **Settings → Security & access** to create, list or revoke personal
tokens for hosted MCP and bots. Choose a name, organization and permissions.
**Read-only** selects the read permissions you hold; **Full access** selects all
your available permissions, and **Custom** lets you choose them individually.
Expiry defaults to **Never**; choose a duration or date to set an expiry.

Copy the token when it appears: its full value is shown only once. After that,
the list keeps only a hint, permissions, dates and status. Revoke a token you no
longer need; you must confirm before it stops working. At the 50-token limit,
revoke an unused token before creating another. See
[Personal API tokens](./mcp.md#personal-api-tokens) for connecting to hosted MCP.

## Getting around

The sidebar on the left has the **Bossanova** menu (Settings, Keyboard
shortcuts and Log out), then **Sessions** with how many sessions need you,
then your **Favorites**, **Organizations**, **Daemons** and
**Repositories**. Click an organization, daemon or repository to filter
Sessions by it, and click it again to remove the filter; choices combine.
The star in the Sessions header saves the current view or filter to
Favorites.

**Filter** opens the filter menu, and the chips under the header show each
filter: click a chip's operator to flip between _is_ and _is not_, or its ×
to remove it. **Save** keeps the filters as a view of your own. **Display
options** choose the board or the list, how to group and order sessions, and
which properties the cards show.

On a phone the sidebar opens from the menu button, and the page shows one thing
at a time: Sessions, then a session with round buttons for its proof, chats and
details, then a chat on its own. The back arrow always goes up one level.

## Reviewing a PR

A session whose PR waits for you shows under **Needs you**. Open it to read the
PR's proof page in the **Proof** view, then **Merge** it, or **Send back**
under **⋯** to return it to the agent. A PR with no proof page shows the proof
note verify left on it, with **Verify again**.

The old review page is gone. A link to `/review` opens the **Needs you** view,
and a link to `/review/<owner>/<name>/<pr>` opens that PR's session on its
proof.

## Routes

These URL routes make up the web app. Every `/sessions` address is one
page, so moving between sessions and chats keeps the list where it was:

| Route                                | View        | What it shows                                                   |
| ------------------------------------ | ----------- | --------------------------------------------------------------- |
| `/` or `/sessions`                   | Sessions    | Every active session across every paired daemon.                |
| `/sessions?view=needs`               | Needs you   | The sessions waiting for you.                                   |
| `/sessions/:id`                      | Session     | The session on its proof, or on its chats when nothing applies. |
| `/sessions/:id/proof`                | Proof       | The PR's proof page, or its proof note when it has no page.     |
| `/sessions/:id/chats/:chat`          | Chat        | The chat's conversation and reply box.                          |
| `/sessions/:id/chats/:chat/terminal` | Terminal    | The chat's live terminal.                                       |
| `/sessions/:id/terminal`             | Terminal    | The live terminal of the session's default chat.                |
| `/sessions/:id/chats/new`            | New chat    | The composer for a new chat in the session.                     |
| `/sessions/new`                      | New session | The composer for a new session.                                 |
| `/settings/…`, `/:org/settings/…`    | Settings    | Your settings, and each organization's.                         |

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

Press `?` on any screen to list the keyboard shortcuts, and `⌘K` (or
`Ctrl K`) to open the command menu. The page itself never shows key hints;
the list is the one place they appear.

| Screen    | Key                    | Action                    |
| --------- | ---------------------- | ------------------------- |
| Sessions  | `J` `K` or `↑` `↓`     | Move through the sessions |
| Sessions  | `Enter` or `→`         | Open the selected session |
| Sessions  | `F`                    | Add a filter              |
| Sessions  | `⌘F` or `Ctrl F`       | Find in this view         |
| Sessions  | `N` or `C`             | New session               |
| Session   | `1` `2` `3`            | Proof, Chats, Terminal    |
| Session   | `Esc`, `←` or `Ctrl X` | Back to Sessions          |
| Session   | `N`                    | New chat                  |
| Session   | `M`                    | Merge                     |
| Session   | `S`                    | Send back                 |
| Session   | `E`                    | Rename the session        |
| Session   | `G`                    | Open on GitHub            |
| Session   | `I`                    | Open the ticket           |
| Session   | `A`                    | Archive the session       |
| Scheduler | `N`                    | New job                   |
| Scheduler | `R`                    | Run now                   |
| Scheduler | `E`                    | Edit job                  |
| Scheduler | `Space`                | Pause or resume           |

In a chat's terminal, keys typed into the terminal go to the agent, so the
keys above work only while focus is outside it; `Ctrl X` inside it leaves the
terminal.

## Encrypt the web terminal

Open **Settings → Security & access**, edit the browser label and choose **Set up this browser**.
Your private key stays in this browser. Copy the fingerprint and run the displayed
`boss viewer approve <id> --fingerprint <fingerprint>` command on each of your machines.
Compare the daemon fingerprint printed by the command with the page, then confirm that they match.
If a machine identity changes, check it before forgetting the old pin and confirming the replacement.

Pairing prepares terminal encryption; encrypted terminal transport is not available yet.
A browser without IndexedDB, X25519 or Ed25519 support cannot pair.

**Forget this browser** removes the registry request, browser keys and machine pins. Run
`boss viewer revoke <id>` on each machine too: deleting the registry request cannot revoke a local
approval. Clearing browser storage loses the keys; set up the browser again and revoke its old id.
