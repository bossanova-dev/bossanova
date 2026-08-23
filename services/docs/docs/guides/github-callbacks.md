---
title: GitHub PR Callbacks
description: 'Get notified in an agent chat the moment a pull request merges, closes, goes green, or leaves draft.'
slug: /guides/github-callbacks
---

import CommandTabs from '@site/src/components/CommandTabs';

# GitHub PR Callbacks

## What a callback is

A **GitHub callback** is a durable, one-shot notification: register it against
a pull request and a chosen trigger, and Bossanova delivers a prompt into a
chat the moment that trigger is satisfied, and then retires the callback. It
give you the ability to ask "tell me when PR #123 is merged" or "ping this chat when PR
#123 goes green" without you having to poll.

A callback is one of two chat-notification primitives:

- **Callback** — "notify this one chat when that one PR does X," then stop.
- **Broadcast** — "tell this whole audience X, right now." See
  [the broadcast MCP tools](./mcp.md#tool-reference) for the sibling primitive
  if a single PR isn't the shape of the thing you're waiting on.

## Triggers

`<trigger>` is one of six values. Pick the one that matches the PR state you
actually care about — they are evaluated independently, so nothing stops you
registering several against the same PR.

| Trigger               | Fires when                                                                                           |
| --------------------- | ---------------------------------------------------------------------------------------------------- |
| `merged`              | The PR has been merged.                                                                              |
| `closed`              | The PR is closed **without** being merged.                                                           |
| `checks_passed`       | At least one check exists, none are pending, and none failed.                                        |
| `checks_failed`       | At least one completed check failed, timed out, or was cancelled — even if others are still pending. |
| `ready_for_review`    | The PR is open and not a draft.                                                                      |
| `checks_passed_ready` | `checks_passed` **and** the PR is open and not a draft.                                              |

Two things worth knowing before you pick one:

- **Triggers match on PR state, not on transitions.** Arming a callback
  against a PR that already satisfies its trigger fires on the next
  evaluation — you don't need a fresh webhook event to land first. Registering
  it does not itself evaluate anything, though, so "the next evaluation" means
  the next webhook that lands for that PR, or the periodic reconcile described
  in [How it fires](#how-it-fires) if the PR has already gone quiet. Arm
  `checks_failed` against a PR whose checks failed an hour ago and expect it
  within a reconcile pass, not instantly.
- **`checks_passed` is not "all checks finished."** It requires that at least
  one check exists, that none is currently pending, and that none has
  failed — which can be true even while GitHub's own checks view shows more
  checks still coming, if those additional checks haven't been created yet and
  so aren't part of the set being evaluated at all. The "at least one" clause
  cuts the other way too: a PR whose CI is entirely path-filtered away, with no
  checks at all, never satisfies `checks_passed`. If you want a callback once
  the PR has passing checks and is open and not a draft, use
  `checks_passed_ready`: it adds that open-and-not-draft condition. It does not
  check mergeability, approvals, or branch-protection requirements, so check
  those separately before merging.

## Delivery is a signal, not a guarantee

The prompt a callback delivers only says the trigger evaluated true at
evaluation time — it is not a snapshot of the PR you can act on blindly. State
can move between evaluation and delivery (another push, a force-merge, a
reopened PR), and a crash after sending the prompt can cause a repeat (see
[How it fires](#how-it-fires)). Before doing anything consequential with a
delivered callback — merging, closing out a session, notifying someone else —
re-check the PR's actual current state. Treat the callback as "go look," not
as "this already happened."

## Lifecycle

A callback moves through a small state machine from creation to a terminal
state:

| State       | Meaning                                                                                                                                                                                                                                         |
| ----------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `active`    | Registered, waiting for its trigger to be satisfied.                                                                                                                                                                                            |
| `leased`    | An internal delivery claim on a still-`active` callback. The shipped worker never produces it — it leases already-`triggered` callbacks in place instead (see below) — so it stays a legal `--state` filter value you will not see in practice. |
| `triggered` | Its trigger evaluated true; queued for delivery (or waiting on retry backoff).                                                                                                                                                                  |
| `delivered` | The send call reported success to the daemon. It does not prove the target chat or agent consumed the prompt. Terminal.                                                                                                                         |
| `canceled`  | A sibling in the same daemon-local `--group` was selected for delivery first. Terminal.                                                                                                                                                         |
| `expired`   | Past its expiry without a recorded successful delivery. Terminal (swept lazily, not the instant expiry hits).                                                                                                                                   |

Creation starts a callback `active`. Once the evaluator confirms the
trigger against authoritative GitHub state it moves to `triggered` — and, if
the callback belongs to a `--group`, every sibling in that group on the same
daemon that is still waiting on its own trigger moves straight to `canceled` at
the same moment. A
`triggered` callback gets picked up by the delivery worker, which records its
claim as a lease (`lease_owner` / `lease_deadline_at`) on the row without
changing its state — it stays `triggered` while an attempt is in flight;
success moves it to `delivered`; failure schedules a retry with a backoff
timer, leaving it `triggered`. A crash after the send call returns success but
before the daemon records `delivered` can cause redelivery, not loss; consumers
dedup on callback id. A callback without a recorded successful delivery by its
expiry is swept to `expired` the next time the expiry check runs.

**Expiry** defaults to 24 hours from creation and cannot be set beyond a 30-day
ceiling — pass `--expires-in` to change it within that range (see
[Using it from the CLI](#using-it-from-the-cli)).

**`--group`** ties callbacks on one daemon together: when one member is
selected for delivery, every sibling that is still waiting on its own trigger
is canceled. Share a group only when at most one member can ever be satisfied
for the PR, such as `merged` versus `closed`; triggers that can be satisfied at
different times, or re-satisfied after a push, need separate groups. Register
`merged` and `closed` against the same PR in the same group, with target chats
owned by the same daemon. If multiple group members are satisfied in one
evaluation, the oldest registration wins (then id order), not the trigger that
became true first. Use this for "notify me either way" registrations instead of
manually tracking which one was selected. A failed delivery can still exhaust
its retries, so a group can produce no successful delivery. Register the whole
group before any member can fire: the two `add`
commands below are separate registrations, not one atomic step, and
cancellation only reaches the siblings that already exist and are still
waiting at the moment a member is selected. A member registered after another
has already been selected or delivered stays `active` and can be selected
later, so that group delivers twice. Pick a group id unique to the thing
you're waiting on: cancellation matches on the group id alone, so reusing one
across PRs cancels callbacks you didn't mean to.

## How it fires

The daemon evaluates a PR's callbacks in response to GitHub webhook events.
On the GitHub App route, that includes check activity and pull-request events
such as opening, closing, merging, review activity, and pushes. The legacy
per-repository webhook route is narrower: it evaluates immediately for a
completed check suite with a `success`, `failure`, or `timed_out` conclusion;
a completed individual check run with `failure` or `timed_out`; PR close/merge;
submitted pull-request reviews; `ready_for_review`; and a `synchronize` event
reporting a conflict. A canceled check, opened PR, or ordinary push waits for
reconciliation. Both routes read a single PR number out of a check payload, so
when GitHub associates one check suite or check run with several pull requests
only the first one listed is evaluated; a callback on any of the others waits
for reconciliation too. Those webhooks reach your daemon **through Boss Cloud**,
which is what GitHub delivers them to; a daemon running standalone never
receives one, so on a local-only setup the reconcile below is the sole
evaluation path and its cadence is the latency to plan around.

Webhook delivery isn't guaranteed either way, so the daemon also
**reconciles** — once at startup and periodically thereafter: it re-evaluates
every PR that still has an undelivered (`active`) callback against GitHub's
authoritative state, so a missed webhook self-heals on the next reconcile pass
instead of stranding the callback forever. That recovery only covers trigger
states that still hold when the pass runs: reconciliation reads the PR's
current state, not a history of events. A state that came and went in between
— checks that failed and then went green on a rerun — no longer satisfies
`checks_failed` by the time reconciliation looks, so a callback whose only
webhook was missed that way stays `active`.

The following numbers are today's defaults, set in
`services/bossd/internal/callback/worker.go` — re-check that file if you need
the current values, since they're tuning constants and not a stable contract:

- The delivery worker polls for deliverable callbacks every **15 seconds**
  (`DefaultPollInterval`).
- A failed delivery retries with exponential backoff starting at **30
  seconds** and doubling each attempt, capped at **15 minutes**
  (`baseRetryBackoff` / `maxRetryBackoff`). At the current
  `maxDeliveryAttempts` that cap never binds: the delays you can actually
  observe are 30s, 1m, 2m, and 4m.
- Delivery is abandoned after **5 attempts** (`maxDeliveryAttempts`); a
  callback that exhausts its attempts is left to expire rather than retried
  further.
- A full reconcile safety net runs every 20 worker ticks
  (`reconcileEveryTicks`) — roughly every 5 minutes at the default poll
  interval.

Delivery uses bounded best-effort retries: a crash between sending the prompt
and recording it as delivered can produce a rare duplicate, while an
unavailable target or expiry can mean no successful delivery. Every delivered
prompt carries the callback's id, so if you're scripting a downstream reaction,
use that id to recognize a repeat.

## Using it from the CLI

Three subcommands: `boss callback add`, `boss callback list`
(alias `ls`), and `boss callback remove` (alias `rm`).

### `boss callback add <pr> <trigger> --message …`

`<pr>` accepts either a bare PR number, resolved against the current
repository, or a full `https://github.com/owner/repo/pull/N` URL. `--message`
is required — it's the prompt delivered to the chat when the callback fires
(see [The message is a secret](#the-message-is-a-secret)).

Flags:

- `--chat` — target agent-session (chat) id to notify. Defaults to
  `$BOSS_AGENT_SESSION_ID`, so an agent registering a callback for itself
  usually doesn't need to pass this.
- `--repo` — repository as `owner/repo`. Defaults to the current repository's
  origin when `<pr>` is a bare number; a full PR URL carries its own
  owner/repo and doesn't need this. A URL that disagrees with the repository
  you're standing in is rejected rather than silently preferred, so run it
  from that repository or from outside a registered one. That default is
  local-daemon-only — a CLI connected with `--remote` can't resolve the
  repository you're standing in, so pass `--repo owner/repo` or a full PR URL
  there.
- `--expires-in` — expiry as a duration (`30m`, `24h`, `7d`, `2w`). Default
  `24h`, maximum `30d`.
- `--group` — optional group id; siblings on the same daemon cancel each other
  when one is selected for delivery.
- `--json` — emit the created callback as a stable JSON schema instead of the
  human-readable confirmation line.

Register a callback for a plain trigger:

<CommandTabs
chat='"tell me when PR #123 is merged"'
cli='boss callback add 123 merged --message "PR #123 merged — pull main and redeploy"'
mcp="register_github_callback"
/>

Or wait for the PR to go green and leave draft:

<CommandTabs
chat='"ping this chat when PR #123 goes green and is ready to merge"'
cli='boss callback add 123 checks_passed_ready --message "PR #123 is green and ready to merge"'
mcp="register_github_callback"
/>

A full PR URL carries its own owner/repo:

<CommandTabs
chat='"let me know if https://github.com/acme/widget/pull/123 fails its checks"'
cli={`boss callback add https://github.com/acme/widget/pull/123 checks_failed \\
  --message "PR #123 is red — investigate the failing checks"`}
mcp="register_github_callback"
/>

To be notified either way — merged or closed — register both mutually exclusive
triggers against the same daemon-local `--group`, so at most one delivers:

<CommandTabs
chat='"tell me when PR #123 merges, in callback group pr-123-outcome"'
cli='boss callback add 123 merged --group pr-123-outcome --message "PR #123 was merged"'
mcp="register_github_callback"
/>

<CommandTabs
chat='"tell me when PR #123 closes without merging, in the same callback group pr-123-outcome"'
cli='boss callback add 123 closed --group pr-123-outcome --message "PR #123 was closed without merging"'
mcp="register_github_callback"
/>

### `boss callback list`

List every registered callback:

<CommandTabs
chat='"list the registered github callbacks"'
cli="boss callback list"
mcp="list_github_callbacks"
/>

Narrow the list by repository and trigger:

<CommandTabs
chat='"list the merged callbacks registered against acme/widget"'
cli="boss callback list --repo acme/widget --trigger merged"
mcp="list_github_callbacks"
/>

Filter by lifecycle state, as machine-readable JSON:

<CommandTabs
chat='"show me the active callbacks"'
cli="boss callback list --state active --json"
mcp="list_github_callbacks"
/>

Flags: `--chat` (filter by target chat), `--repo` (filter by `owner/repo`),
`--trigger` (filter by one of the six trigger names), `--state` (filter by
`active`, `leased`, `triggered`, `delivered`, `canceled`, `expired`), `--json`.

With `--json`, the CLI emits the machine contract keys `id`, `group_id`,
`target_chat_id`, `repo_owner`, `repo_name`, `pr_number`, `trigger`, `state`,
`attempt_count`, `last_event`, `last_error`, `triggered_at`, `delivered_at`,
`expires_at`, `created_at`, and `updated_at`. The group surfaces as `group_id`
in the list output; the MCP registration input is spelled `group`, and both
names are correct in their own direction. The message body is deliberately not
emitted.

### `boss callback remove <callback-id>`

IDs are 16 hex characters; `boss callback list` prints them.

<CommandTabs
chat='"remove callback 9f2c41a7b8e30d55"'
cli="boss callback remove 9f2c41a7b8e30d55"
mcp="delete_github_callback"
/>

Takes `--chat` to route the removal to the owning daemon when going through a
remote (bosso) client; it's ignored for a local daemon and defaults to
`$BOSS_AGENT_SESSION_ID` like `add`.

## Using it from an agent (MCP)

The same three operations are exposed as MCP tools — see the
[MCP guide](./mcp.md) for how to connect an agent to Bossanova's MCP server in
the first place.

- **`register_github_callback`** — create a one-shot callback: pass the PR
  (a bare number with `repo`, or a full PR URL), the `trigger`, the
  `target_chat_id`, the `message` to deliver, and optionally `expires_in` and
  `group`.
- **`list_github_callbacks`** — list registered callbacks, optionally
  filtered by chat, repo, PR number, trigger, or state. The delivery message
  body is never included in the result.
- **`delete_github_callback`** — permanently remove a callback by id before it
  fires. It prevents future delivery claims; it cannot recall a delivery
  already in flight, so if the delivery worker has already claimed the
  callback the prompt can still arrive after the delete returns successfully.
  It's classed as a destructive tool, so the call also requires
  `confirm: true` and is rejected without it. Hosted MCP calls must also pass
  the callback's `target_chat_id` (available from `list_github_callbacks`) to
  route the delete to its owning daemon; local MCP can delete by id alone.

## The message is a secret

The `--message` / `message` body you register is delivered verbatim to the
target chat when the callback fires. The CLI, MCP tool, and hosted proxy redact
it from their list results. The daemon RPC returns the registered body, so
clients that call it directly must treat the result as secret. If you need to
know what a callback will say later, keep your own copy; don't rely on a
redacting wrapper to read it back from Bossanova.

## See also

- [PR Lifecycle](./pr-lifecycle.md) — the bigger picture of what happens
  between opening a PR and merging it, and where a callback fits if something
  stalls.
- [MCP Server](./mcp.md) — connecting an agent to drive callbacks (and
  everything else) programmatically.
