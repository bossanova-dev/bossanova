---
title: Privacy
description: An honest accounting of what bossd sends to Boss Cloud, what stays on your machine, who can see what, and how to opt out.
---

# Privacy

This page is the honest accounting of what Bossanova does with your data:
what `bossd` sends to Boss Cloud, what never leaves your machine, what
third parties get involved, and how to disconnect.

## TL;DR

- `bossd` sends **session metadata** to Boss Cloud: IDs, branch names,
  PR URLs, state, and short status labels. **bossd does not send full
  chat transcripts, code, or diffs.**
- The agent runner talks to its provider directly: Claude Code talks to
  Anthropic, and OpenAI Codex CLI talks to OpenAI. Bossanova does not
  proxy or log that traffic.
- Bug reports (the `ctrl+b` modal) are **opt-in**: they only leave your
  machine when you press _Submit_.
- Unless `BOSSD_ORCHESTRATOR_URL` is set, `bossd` is local-only. The Terminal UI (TUI)
  and plugins still work; the web app can't see your sessions.

## Identity (WorkOS)

`boss login` runs the WorkOS device-code flow. Bossanova reads exactly
two identity fields from WorkOS:

- `user.id`: the WorkOS user ID.
- `user.email`: the email address WorkOS holds for the user.

Plus an `access_token` (a WorkOS JWT) and a `refresh_token`. None of
this is hand-rolled. It's a vanilla OAuth 2.0 device-code exchange.

The tokens are stored locally via `keyringutil`: macOS Keychain on
macOS, libsecret on Linux, encrypted file backend in containers. The
refresh token never leaves your machine. The access token is sent up
the gRPC stream as a `Bearer` header so Boss Cloud can verify it
against WorkOS.

Boss Cloud receives the email on first contact and uses it to
JIT-create the user row if one doesn't already exist. The Bossanova
code never asks WorkOS for additional profile data beyond what
arrives in the device-code response.

## What `bossd` sends to Boss Cloud

`bossd` opens one long-lived bidirectional gRPC stream when paired
with Boss Cloud. The first event on every (re)connect is a snapshot;
thereafter the daemon sends event deltas.

### Snapshot (sent on every connect)

| Field       | What it is                                                                                                   |
| ----------- | ------------------------------------------------------------------------------------------------------------ |
| `daemon_id` | A stable identifier for this daemon. Defaults to the machine hostname unless `BOSSD_DAEMON_ID` overrides it. |
| `hostname`  | Machine hostname.                                                                                            |
| `repo_ids`  | The list of repo IDs this daemon manages. **No repo paths**, **no remote URLs**, **no setup-script bodies**. |
| `sessions`  | A slim per-session record; see below.                                                                        |
| `chats`     | Per-chat metadata: id, title, agent session id, timestamps. **Not the transcript.**                          |
| `statuses`  | Heartbeat-tracked chat statuses (e.g. "working", "checking", "stopped").                                     |

The session record carries: session id, repo id, title, plan text,
worktree path, branch and base branch, state, attempt count, PR
number / URL, last check state, display label, attention status,
created/updated timestamps, optional tracker id and URL (Linear /
Jira), optional tmux session name, optional `blocked_reason` string,
and optional `archived_at`.

**The plan text is sent.** That's the prompt the user typed when they
created the session. It goes up to Boss Cloud with the rest of the
session metadata and is visible in the web app. If your plan text
contains secrets, treat it as visible to Boss Cloud.

The `worktree_path` field is sent. That's the local filesystem path of
the session's worktree (e.g.
`/Users/you/.bossanova/worktrees/myrepo/feature-x`). It's a path string
only. Boss Cloud never reads from it.

### Deltas (sent as state changes)

After the snapshot, bossd forwards:

- **Session deltas:** created/updated/deleted.
- **Chat deltas:** created/updated/deleted (metadata only).
- **Chat status deltas:** coalesced to ~100 ms windows.
- **Webhook acks / command results:** replies to commands Boss Cloud
  sent down.
- **Token refresh:** a freshly-obtained WorkOS JWT (the access token
  only; refresh tokens never cross the stream).
- **Session attach chunks:** only when the user explicitly attaches
  to a running session via the web app. Carries raw stdout/stderr
  lines for that one session, for the duration of the attach.

### What is **not** sent

- **No full chat transcripts.** Full transcripts are fetched on demand
  when you explicitly attach to a session.
- **No source code.** bossd never reads files in the worktree to send up.
- **No diffs.** bossd never `git diff`s the worktree to send up.
- **No environment variables**, no shell history, no keychain contents.
- **No agent traffic.** Provider API calls go from the local agent CLI
  process directly to its provider; bossd is not in that path.

## The web terminal attach (separate stream)

When you click _Attach_ in the web app, Boss Cloud opens a second bidi
RPC to your daemon. Raw PTY output, including any text the agent
prints, flows up the wire to Boss Cloud and on to your browser.
Keystrokes typed in the browser flow back the other way. The data is
opaque to Boss Cloud (no parsing); it's the literal bytes your tmux
pane holds.

This stream is only active while you have the terminal pane open.
Closing the browser tab tears it down. The PTY itself (and the tmux
session running underneath) is unaffected.

## Bug reports (opt-in, `ctrl+b`)

The TUI's `ctrl+b` modal builds a report payload and posts it to Boss
Cloud. The payload contains:

- `BossVersion`, `BossCommit`, `Os`, `Arch`, `Terminal` (the `TERM` env var).
- `DaemonStatuses`: a small map of daemon IDs to status strings.
- `CurrentSession` and a slim summary of every other active session (id,
  repo id, title, state, PR number, PR URL, updated_at).
- `BossLogTail`: the **last 200 lines** of the boss TUI log file.
- `BossdLogTail`: the **last 200 lines** of the bossd daemon log file.
- The free-text comment you typed in the modal.

Logs may contain anything zerolog wrote, including paths, error
messages, and the occasional pasted shell command. They do **not**
contain chat transcripts or model outputs.

When `boss login` is current, the access token is attached as a Bearer
header so Boss Cloud can record the report against your WorkOS
identity. If you're not logged in, the report goes through anonymously.

The `ctrl+b` flow is the only path that submits a bug-report payload.
Bossanova does not auto-submit reports on crash, on panic, on stream
failure, or on plugin error.

## Local data (stays on your machine)

| Path                                                    | What lives there                                                                                                                                                                                                                      |
| ------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `~/Library/Application Support/bossanova/bossd.db`      | The bossd SQLite database: repos, sessions, chats, attempts. Full chat transcripts live here, never on the wire. macOS path; Linux uses `~/.config/bossanova/bossd.db`.                                                               |
| `~/Library/Application Support/bossanova/settings.json` | Global settings; per-repo settings live in the bossd DB.                                                                                                                                                                              |
| `~/.bossanova/worktrees/<repo>/<session>/`              | Git worktrees, one per session. The agent's working tree.                                                                                                                                                                             |
| `$XDG_STATE_HOME/bossanova/logs/{boss,bossd}.log`       | Rotated log files. Defaults to `~/.local/state/bossanova/logs/` on Linux. On macOS, `$XDG_STATE_HOME` is unset by default; boss falls back to file-only logging at the same path if you set it.                                       |
| OS keychain (Keychain / libsecret / Wincred)            | WorkOS access token, refresh token, daemon session token. Linux containers without a system keychain fall back to an encrypted file backend; the passphrase is generated on first use at `~/.config/bossanova/keyring.key` mode 0600. |

The bossd SQLite file holds everything bossd needs to render the TUI
without Boss Cloud, including full chat transcripts. That data does
not sync to the cloud.

## Third parties

Bossanova doesn't proxy any third-party traffic. The agent and plugins
talk to their respective services directly:

- **Anthropic** (Claude Code): every chat message sent to the agent
  goes from your local `claude` process to `api.anthropic.com`. Boss
  Cloud is not in that path. See Anthropic's privacy policy for what
  they do with the prompts.
- **OpenAI** (Codex CLI): every chat message sent to a Codex session
  goes from your local `codex` process to OpenAI. Boss Cloud is not in
  that path. See OpenAI's privacy policy for how they handle prompts.
- **GitHub:** the bundled session tooling shells out to `gh`. Whatever
  scopes your `gh auth login` token has, the agent gets too. Webhooks
  flow GitHub → Boss Cloud → bossd; payloads are forwarded as opaque
  bytes.
- **Linear:** the `linear` plugin talks to `api.linear.app` directly,
  using the API key stored on the repo.
- **WorkOS:** the auth flow above. WorkOS sees the device-code
  request, the device-code completion, and any subsequent token
  refreshes.

If you uninstall a plugin, its third-party calls stop with it.

## Retention

bossd retains everything locally indefinitely (including archived
sessions until you delete them). Worktrees stay until you remove them
or run `boss archive <session-id>` (which removes the worktree but
keeps the branch).

For Boss Cloud:

- Daemons, sessions, and chat metadata are derived from the snapshot
  stream. The snapshot replaces in-memory state on every connect, so a
  daemon that hasn't connected in a while still has rows in Boss Cloud
  until somebody removes them.
- Bug reports persist indefinitely.
- Boss Cloud does not duplicate WorkOS profile storage; it re-validates
  JWTs against WorkOS on every connect.

## The opt-out path

By default, `bossd` runs in **local-only mode**. Homebrew installs do
not set `BOSSD_ORCHESTRATOR_URL`. To force local-only mode in an
environment that might set it, set `BOSSD_ORCHESTRATOR_URL=""` (an
explicit empty string) before launching `bossd`:

- No bidi stream is opened. No snapshots, no deltas, no terminal
  attaches.
- No registration request is sent. Boss Cloud never learns this daemon
  exists.
- The TUI keeps working. Boss talks to bossd over the local Unix
  socket.
- The bundled plugins keep working. They subscribe to bossd events
  over local gRPC.
- The web app can't see your sessions.

Pair-up is reversible. Unset the variable (or set it to a URL) and
restart bossd to reconnect.

## See also

- [Security and Permissions](./security-and-permissions.md): what an
  agent session can touch on your machine, the `dangerously_skip_permissions`
  toggle, and the kill switches.
- [Settings: Environment overrides](./settings.md#environment-overrides): every
  `BOSS_*` / `BOSSD_*` variable, including
  `BOSSD_ORCHESTRATOR_URL` and `BOSS_REPORT_URL`.
- [Web App](../guides/web.md): what Boss Cloud's UX looks like, on
  top of the data shown above.
