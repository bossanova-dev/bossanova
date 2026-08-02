---
name: boss
description: Complete reference for all boss CLI commands. Use to run boss operations from within a Claude Code session.
---

# Boss CLI Reference

Boss manages Claude coding sessions across git worktrees with automatic PR creation, CI fix loops, and code review handling.

The command index below is generated from the `boss` CLI by `make gen-skill`. Do not edit the region between the markers by hand — change the CLI (or the prose registry in `lib/bossalib/clidoc`) and regenerate.

<!-- BEGIN GENERATED: boss command reference — run `make gen-skill` -->

## Global Flags

### `--remote`

Connect to orchestrator URL instead of local daemon

## Command Groups

Every `boss` command is documented in one of the reference files below, grouped as `boss --help`
groups them. **Open the matching reference before using a command** — never infer a command's
syntax, arguments or flags from an index row.

| Reference                   | Read it when…                                                   |
| --------------------------- | --------------------------------------------------------------- |
| `references/session.md`     | Creating, listing, attaching to, merging or archiving a session |
| `references/chat.md`        | Starting a chat, sending it a message, or reading a transcript  |
| `references/repo.md`        | Registering, cloning, updating or removing a repository         |
| `references/cron.md`        | Creating, editing, listing or firing a scheduled job            |
| `references/callback.md`    | Arming or inspecting a one-shot GitHub PR callback              |
| `references/broadcast.md`   | Sending a broadcast or registering an outcome subscription      |
| `references/notes.md`       | Recording or harvesting durable repo-scoped notes               |
| `references/account.md`     | Adding, testing, rotating or switching a provider account       |
| `references/trash.md`       | Resurrecting an archived session or emptying the trash          |
| `references/daemon.md`      | Starting, stopping or inspecting bossd                          |
| `references/mcp.md`         | Running or configuring the MCP server                           |
| `references/skills.md`      | Installing or syncing the boss skill payload                    |
| `references/settings.md`    | Changing global settings or authenticating                      |
| `references/diagnostics.md` | Running the repair doctor, checks or other diagnostics          |
| `references/plugins.md`     | Listing or inspecting loaded bossd plugins                      |
| `references/other.md`       | Anything unclassified (e.g. `boss version`)                     |

<!-- END GENERATED -->

## Launching a session to run work unattended

Supplying a prompt when you create a session means you want that work to **run**, not sit idle. Both surfaces default to launching the agent so the work actually starts:

- **CLI:** `boss new --repo R --prompt P` runs non-interactively — `--detach` is implicit when `--repo` and `--prompt` are both given.
- **MCP:** a `create_session` call with a non-empty `prompt` and no `attended` opt-in defaults to headless (equivalent to `detach:true`). The result reports `agent_launched: true`.

Opt into an idle session (created but no agent started, awaiting a human attach) only when a human will drive it interactively: pass `attended: true` to `create_session`. That result reports `agent_launched: false` and carries a `next_action` hint.

**Post-launch verification (MCP).** After `create_session`, check `agent_launched`:

- `agent_launched: true` — an agent started; the session's `agent_session_id` is its primary chat. Address it with `send_chat_message` / `get_chat_transcript`.
- `agent_launched: false` — no agent ran. Either you passed `attended: true`, or the create was prompt-less, or the daemon attached to a pre-existing session (`attached_existing: true`, in which case your prompt was NOT run). To start work: call `start_chat` on the session, or re-create with `detach: true` (headless) or `tmux_unattended: true` (durable pane). Follow the `next_action` / `note` hint in the result.

Canonical recipe — spawn a session to run a task unattended and collect the result:

```
create_session { repo_id: R, prompt: P }          # defaults to headless; expect agent_launched: true
# then, using the returned agent_session_id:
get_chat_transcript { agent_session_id: <id> }     # read progress / final result
```

If a `create_session` result comes back with `agent_launched: false` when you expected the work to run, you (or a caller) opted into attended mode — start it with `start_chat`, or re-create with `detach: true` / `tmux_unattended: true`.

## Cron repair workflow

Cron repair example — list the stalled sessions, then link the existing PR:

```bash
boss ls --state finalizing,blocked
boss session link-pr b4764f1684e33742 477
```

## Linking a PR and titling your session

When a session was started without a PR (e.g. a cron-triggered session that committed and pushed before `bossd` finalized), use this flow — your session id is in your system prompt:

1. Create the PR yourself: `gh pr create ...` (or let `boss-finalize` open it).
2. Link it: `boss session link-pr <session-id> <pr-number-or-url>`
3. Title the session: `boss rename <session-id> <new title>`

Note that `boss rename` also best-effort renames the linked GitHub PR.

```bash
boss session link-pr <session-id> 477
boss rename <session-id> Fix flaky login test
```

## GitHub PR callbacks

A GitHub callback is a durable, one-shot notification that fires a prompt into a chat when a pull request reaches a chosen state, then retires. Reach for one whenever a request maps to "let me know when this PR does X":

- "tell me when PR #123 is merged" → trigger `merged`
- "ping this chat when PR #123 goes green" → trigger `checks_passed`
- "let me know if PR #123's checks fail" → trigger `checks_failed`
- "notify me when PR #123 is closed" → trigger `closed`
- "tell me when PR #123 comes out of draft" → trigger `ready_for_review`
- "ping me when PR #123 is green and ready to merge" → trigger `checks_passed_ready`

From a shell, use the CLI (`boss callback add|list|remove`, documented in `references/callback.md`). From an MCP-aware host, the same operations are exposed as tools:

- `register_github_callback` — create a callback. Give it the PR (a bare number with repo context, or a full `https://github.com/owner/repo/pull/N` URL), a `trigger`, the `target_chat_id` to notify, and the `message` prompt to deliver. Expiry defaults to 24h and may not exceed 30 days.
- `list_github_callbacks` — list callbacks, optionally filtered by chat, repo, PR, trigger, or state.
- `delete_github_callback` — remove a callback by id (requires `confirm: true`).

The `message` you register is a secret: it is delivered verbatim to the target chat when the callback fires, and is never echoed back by any list/inspect surface.

**Delivery is a signal, not proof.** A fired callback tells you the event was observed — it does not guarantee the PR is still in that state by the time you act. Always re-verify the PR's actual state (e.g. `gh pr view <n> --json state,mergeStateStatus,statusCheckRollup`) before taking an irreversible step such as deploying or merging.

## Broadcasting to other chats

A broadcast is one message delivered to every chat a **selector** resolves to. Delivery is durable and retried until it lands or the broadcast expires, and it works by **waking** each target chat — a stopped chat is brought back up to receive the message as a prompt. Delivery is at-least-once, so write a message that is safe to act on twice. Where a callback is "notify me when this PR does X", a broadcast is "tell all of these chats X now".

### Selectors — who receives it

A selector is a set of `key:value` terms over six dimensions:

- `chat:<agent-session-id>` — one specific chat
- `session:<session-id>` — every chat in that session
- `repo:<repo-id>` — every chat on that repo's sessions
- `agent:<name>` — every chat run by that agent runner (e.g. `claude`, `codex`)
- `account:<account-id>` — every chat running under that named account (chats on the default account are not addressable this way)
- `daemon:<daemon-id>` — every chat on that daemon. Naming another daemon here does NOT reach it: chat rows carry an empty daemon id, so this term resolves to zero targets on **every** daemon, not just this one. Addressing other daemons is what `--cross-daemon` (`cross_daemon` on the MCP tool) is for: bosso fans the broadcast out to the tenant's other live daemons, and each re-resolves the selector against its own chats — so pair it with a `repo:`/`agent:`/`chat:` selector, which is what those daemons can actually match. Delivery is best-effort: a daemon offline at fan-out time never receives it (bosso holds no store-and-forward queue), and a fan-out reaching more than 32 other daemons is refused outright rather than truncated

`,` joins terms inside one clause: different dimensions are **AND**ed, repeated values of the same dimension are **OR**ed. `+` joins clauses, and clauses are **OR**ed. (Up to 16 clauses, and 64 values per dimension per clause.)

Worked examples:

```bash
# One account just hit its usage limit — tell every chat running under it.
boss broadcast send --to account:<account-id> --message "This account is exhausted; stop and hand off."

# The host resumed — wake every chat working on this repo.
boss broadcast send --to repo:<repo-id> --message "Host resumed; re-check your PR state before continuing."

# AND: only the Claude chats on one repo. OR (+): that repo, or anything on one account.
--to repo:<repo-id>,agent:claude
--to repo:<repo-id>+account:<account-id>
```

### Subscriptions — broadcast when a session settles

A subscription is a standing rule: when a session reaches an outcome, it sends a broadcast for you. Triggers are `--on completed`, `--on errored`, or `--on settled` (either). "Settles" means the **session** reached an outcome — merged, green, ready for review or closed (`completed`), or blocked or orphaned (`errored`) — not merely that an agent finished a turn. A session that is only idle between turns never fires, so do not use a subscription as a turn-completion signal.

Coordinator/child: a coordinator spawns a child session and wants to be woken when it reaches an outcome rather than polling the transcript. From inside the child (`--session` defaults to `$BOSS_SESSION_ID`):

```bash
boss broadcast subscribe --on settled --to chat:<coordinator-chat-id> --message "Child session settled — read its transcript and continue."
```

The coordinator can register the same rule itself by passing the child's `--session <session-id>` explicitly.

### Rules

- **A broadcast is a signal, not proof.** It tells you someone claimed something, not that it is still true. Verify any claimed state yourself before acting on it.
- **The message body is a secret.** It is delivered verbatim to each target chat and is never echoed back by any list or inspect surface.
- **The audience is resolved once**, when the broadcast is sent (for a subscription, when it fires). Chats that appear afterwards are not added.
- **Delivery wakes target chats**, so a broad selector has a real token cost across the fleet — and an audience over 64 chats is refused outright, not truncated. Scope to the narrowest selector that does the job.
- **An empty selector is an error, never "everyone".** Address the audience explicitly.
- **A selector that matches nobody is a success, not an error.** Unreachable chats (failed to start, or in an archived, merged, closed or orphaned session) are dropped before matching, so even `chat:<id>` naming one directly can resolve to an empty audience. Check the reported target count rather than assuming it landed.

### Both surfaces

From a shell: `boss broadcast send`, `list` (`ls`), `remove` (`rm`), `subscribe`, `subscriptions`, `unsubscribe` — flags documented in `references/broadcast.md`. `--from` defaults to `$BOSS_AGENT_SESSION_ID` on both `send` and `subscribe`, but means different things: `send` excludes the origin from its own audience unless you pass `--include-origin`, while on `subscribe` it is provenance only — there is no `--include-origin` there, and a fired subscription never excludes the chat that registered it.

From an MCP-aware host, the same operations are six tools (daemon-local — they are not proxied through the MCP gateway):

- `send_broadcast` — send to the audience a selector resolves to.
- `list_broadcasts` — list recent broadcasts (never their bodies).
- `delete_broadcast` — stop anything further being scheduled for a broadcast (requires `confirm: true`). Not a recall: a delivery a worker already claimed still lands.
- `register_broadcast_subscription` — register a standing rule on a session's outcome.
- `list_broadcast_subscriptions` — list standing subscriptions.
- `delete_broadcast_subscription` — cancel a standing subscription (requires `confirm: true`). It cancels rather than erases, so the row still appears in an unfiltered list.

## Notes — durable free-text a later run can harvest

A note is repo-scoped free-text one run records so a later one can read it back: what an investigation concluded, a trap the next agent should not re-discover, a decision and its reasoning. Unlike a callback (a one-shot signal) or a broadcast (a message pushed at chats now), a note is **pulled** — nobody is woken, and it simply waits until something searches for it.

**A note outlives the run that wrote it.** `session_id` and `chat_id` are **provenance, not ownership**: they record which run wrote the note and are deliberately not foreign keys, so archiving, closing or deleting that session leaves the note — and its provenance — intact. Pass your own session and chat ids when you write one, so a later sweep can attribute it; there is no way to fill them in afterwards.

`references/notes.md` documents the `boss notes` CLI. From an MCP-aware host the operations are five tools:

- `create_note` — record a note against a repo. `repo_id` and `body` are required; `session_id`/`chat_id` are optional provenance. The body is stored verbatim (non-empty, 64 KiB cap). Tags are normalised on write — trimmed, lowercased, de-duplicated, returned in ascending order — at most 32 of at most 64 bytes each.
- `get_note` — read one note by id, body and tags included.
- `list_notes` — search notes, optionally filtered by repo, provenance, tags or a body substring. Every filter is optional and a blank one is ignored. `tags` matches notes carrying **any** of the listed tags (OR, not all-of). `search` is a literal substring match on the body — SQL wildcards match literally, and case folding is ASCII-only. `limit` zero or negative means unlimited.
- `update_note` — change a note's body and/or tags. **Supplying `tags` REPLACES the whole tag set** rather than appending to it, and supplying an empty list clears every tag; omit `tags` entirely to leave the existing ones alone.
- `delete_note` — permanently erase a note (requires `confirm: true`). Idempotent: deleting an unknown id succeeds, so a retry is safe.

### Rules

- **A note body is NOT a secret.** Unlike a callback or broadcast message — which are never echoed back — a note's body is the payload it exists to carry and is returned in full by every read tool. Do not put credentials in one.
- **`repo_id` is the daemon-local repo id** that `list_repos` and `resolve_context` return, **not** a git origin URL. An origin URL resolves to NotFound.
- **`repo_id` is required on the four id- and body-keyed tools** — `create_note`, `get_note`, `update_note`, `delete_note` — and omitting it fails schema validation before the call runs. On `list_notes` it is just another optional filter: omit it to search every repo.
- **On `get_note`/`update_note`/`delete_note`, `repo_id` routes without scoping.** It only picks which daemon to ask; that daemon then addresses the note by **id alone** and never checks the note against `repo_id`. Naming one repo while passing an id that lives in another acts on the other note — so treat the id, not the repo, as the thing to get right before confirming a delete.
- **Tag normalisation is lossy.** Display casing is not preserved, so file notes under tags you are willing to read back lowercased.
