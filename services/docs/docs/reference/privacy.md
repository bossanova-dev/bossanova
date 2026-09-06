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
- The agent plugin talks to its provider directly: Claude Code talks to
  Anthropic, and OpenAI Codex CLI talks to OpenAI. Bossanova does not
  proxy or log that traffic.
- Bug reports (the `ctrl+g` modal) are **opt-in**: they only leave your
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
Sentry / Jira), optional tmux session name, optional `blocked_reason` string,
and optional `archived_at`.

The **plan text is sent**. That's the prompt the user typed when they
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

## Bug reports (opt-in, `ctrl+g`)

The TUI's `ctrl+g` modal builds a report payload and posts it to Boss Cloud.
`ctrl+b` remains a temporary deprecated alias outside tmux. The payload
contains:

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

The bug-report modal flow is the only path that submits a bug-report payload.
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

By default Bossanova doesn't proxy most third-party traffic (the one
exception is the local failover proxy described below, which is **on**
by default for Claude sessions). The agent and plugins talk to their
respective services directly:

- **Anthropic** (Claude Code): every chat message sent to the agent
  goes from your local `claude` process to `api.anthropic.com`. Boss
  Cloud is not in that path. See Anthropic's privacy policy for what
  they do with the prompts. (By default, this traffic first passes
  through a loopback proxy on your own machine unless you've disabled
  the local failover proxy or managed accounts entirely; see below.)
- **OpenAI** (Codex CLI): every chat message sent to a Codex session
  goes from your local `codex` process to OpenAI. Boss Cloud is not in
  that path. See OpenAI's privacy policy for how they handle prompts.
- **GitHub:** the bundled session tooling shells out to `gh`. Whatever
  scopes your `gh auth login` token has, the agent gets too. Webhooks
  flow GitHub → Boss Cloud → bossd; payloads are forwarded as opaque
  bytes.
- **Linear:** the `linear` plugin talks to `api.linear.app` directly,
  using the API key stored on the repo.
- **Sentry:** the `sentry` plugin talks to `sentry.io` directly, using
  the API token and organization slug stored on the repo. It only reads
  issues (when you open the new-session picker); it never writes back to
  Sentry. Boss Cloud is not in that path.
- **WorkOS:** the auth flow above. WorkOS sees the device-code
  request, the device-code completion, and any subsequent token
  refreshes.

If you uninstall a plugin, its third-party calls stop with it.

## Local failover proxy (on by default)

Bossanova can transparently switch a Claude session to another of your
accounts when Anthropic returns a rate-limit (`429`) or auth (`401`)
response, without visibly restarting the session. This is powered by a
local reverse proxy (config key `managed_accounts.failover_proxy_enabled`,
**default on**). It runs entirely on your own machine, bound to
`127.0.0.1` only: Boss Cloud is never in the path, and no traffic leaves
your machine that wouldn't already go to `api.anthropic.com`.

It is on whenever `managed_accounts.enabled` is true (itself the default):
`bossd` points the `claude` subprocess at the loopback proxy via
`ANTHROPIC_BASE_URL` (**overriding** any custom `ANTHROPIC_BASE_URL` you set
yourself for Claude sessions) and the proxy forwards to
`api.anthropic.com`. If you point Claude at a custom endpoint (a corporate
gateway or Bedrock), turn the proxy off (see below); left on, it would
redirect that traffic to `api.anthropic.com` instead. The proxy forwards
your requests over TLS; on a `429`/`401` it swaps the `Authorization`
bearer to your next account and replays the request.

**Interactive sessions and the sentinel API key.** The interactive Claude
REPL authenticates only from your login keychain (it ignores the account
credential `bossd` injects), so managed accounts and rotation would
otherwise never take effect for an interactive chat. To make an interactive
chat actually run on the account `bossd` bound it to, `bossd` injects a
**fixed, non-secret sentinel** `ANTHROPIC_API_KEY` for proxied Claude
sessions. That key overrides the keychain and routes every request through
the loopback proxy as an `x-api-key` request; the proxy **strips** the
sentinel key and substitutes the bound account's OAuth bearer before
forwarding; the sentinel key never reaches `api.anthropic.com`. Because the
credential upstream receives is the OAuth bearer, **billing follows your
subscription**, not pay-per-use API/console billing. `bossd` also pre-seeds
a one-time approval for that key in `~/.claude.json`
(`customApiKeyResponses.approved`, merged non-destructively) so sessions
don't prompt to approve it. Because the proxy resolves the bound account's
OAuth bearer server-side, `bossd` does **not** also inject
`CLAUDE_CODE_OAUTH_TOKEN` into a proxied subprocess; that would be dead
weight and would trip Claude Code's "Both `CLAUDE_CODE_OAUTH_TOKEN` and
`ANTHROPIC_API_KEY` set" warning without changing the auth actually used.

The status header labelled **"API Usage Billing"** is **cosmetic**: it
reflects the client's belief about `ANTHROPIC_API_KEY`, not the auth actually
used. The real billing is your subscription, via the OAuth bearer the proxy
substitutes. (If the machine also has a `claude.ai` keychain login, Claude
Code may still show a "Both claude.ai and `ANTHROPIC_API_KEY` set" warning;
that too is cosmetic; the proxy resolves the auth.)

To opt out of the sentinel-key behavior, disable
`managed_accounts.failover_proxy_enabled` or `managed_accounts.enabled`
(below); interactive Claude then runs under your keychain login exactly as
before.

How to **opt out**:

- `managed_accounts.failover_proxy_enabled: false` turns the proxy off
  while leaving account rotation itself on: a capped session still moves
  to another account, it just does so by respawning rather than by a
  transparent mid-request swap.
- `managed_accounts.enabled: false` turns off all managed-account
  behavior (rotation and the proxy both) and Claude sessions run under
  your terminal's own login exactly as before this feature existed.

Because it's on by default, it's worth understanding the trade-off it
introduces: a deliberate auth-MITM on your own credentials, and a single
point of failure in the request path.

- **Auth-MITM.** The proxy terminates your own Claude auth traffic
  in-process and re-signs it with a different account's OAuth bearer. That
  is a deliberate machine-in-the-middle on your own credentials. Tokens are
  held only in memory for the duration of a request and are never written
  to logs, but the proxy does see every request your `claude` process makes.
- **Single point of failure.** The proxy sits in the hot request path, so a
  bug or crash in it can degrade or break Claude requests for the session.
  Injection is gated on both `managed_accounts.enabled` and
  `failover_proxy_enabled`, and on top of that it's liveness-gated (the
  `ANTHROPIC_BASE_URL` override is only injected once the proxy has actually
  bound its loopback port); a construction/bind failure is non-fatal to
  `bossd` and just leaves every session on the direct `api.anthropic.com`
  path. But once a session's subprocess has picked up the override, the
  proxy is a new dependency in that path for the life of the session.
  Known limitation: the proxy binds an ephemeral loopback port that is baked
  into the running `claude` subprocess at spawn. If `bossd` restarts (or the
  proxy dies) mid-session, an already-running subprocess still points at the
  old port and its requests fail until the session is restarted; it does not
  transparently re-home to a new port or to the direct path on its own.
  Restarting the session (or opting out via the flags above, which drops the
  override) restores the direct `api.anthropic.com` path. Relatedly, after a
  failover the swapped account's bearer is held in memory for the rest of the
  session; if that token later expires it surfaces as an auth (`401`)
  failover rather than a silent token refresh, which rotates the session to a
  further account.

With the proxy off (or managed accounts off entirely), Claude talks to
`api.anthropic.com` directly exactly as described above and none of this
applies.

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
