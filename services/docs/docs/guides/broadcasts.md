---
title: Broadcasts
description: Send one secret prompt to the agent chats an explicit selector resolves to.
slug: /guides/broadcasts
---

import CommandTabs from '@site/src/components/CommandTabs';

# Broadcasts

A broadcast sends one message to every agent chat an explicit selector resolves
to. Bossanova wakes each target chat and delivers the message as a prompt. Use
it when a coordinator needs to tell a known audience something now.

A broadcast is not a [GitHub PR callback](./github-callbacks.md). A callback
waits for one pull request to reach a state and notifies one chat; a broadcast
tells an audience something immediately.

## The local audience is fixed when you send

Bossanova resolves a local selector once, at send time, then stores the
resulting deliveries. A local chat created after the send is never added
retroactively. This keeps a broad message from silently expanding as new
sessions start.

This guarantee applies only to the current daemon. With `--cross-daemon`, each
receiving daemon resolves the selector when it receives the routed broadcast,
so a chat created after the origin send can be a remote target.

An empty result is valid: no routable chat happened to match. An empty
**selector** is different: it is an error, never a request to notify everyone.

## Select an audience deliberately

A selector can name six dimensions: `chat`, `session`, `repo`, `agent`,
`account`, and `daemon`. Selectors carry identifiers, not credentials, so the
selector is safe to log; the message body is not.

- Commas join terms in one clause. Different dimensions are ANDed:
  `repo:repo_123,agent:claude` selects Claude chats for one repository.
- Repeating one dimension with commas is OR within that dimension:
  `agent:claude,agent:codex` selects either agent.
- A plus sign ORs complete clauses:
  `repo:repo_123,agent:claude+account:acct_456` selects either clause.

Start with the narrowest selector you can. Every matched chat is woken, so a
broad selector has a real cost. The sending chat is excluded by default to
avoid self-wake loops; use `--include-origin` only when it should receive the
message too. One daemon refuses, rather than truncates, an audience over its
target cap.

## Keep the body secret

The body is delivered and stored verbatim, but no list or inspect surface
echoes it back. Treat it like a prompt containing operational context: do not
put it in logs or status updates. The receiver must also treat a broadcast as
a **signal, not proof**. If it says a PR merged or a deployment completed,
verify that external state before acting.

## Send a broadcast

`boss broadcast send` requires both a selector and a message:

Tell every Claude chat in one repository.

<CommandTabs
chat='"tell every claude chat in repo repo_123 the migration is complete and to rebase before their next change"'
cli={`boss broadcast send --to repo:repo_123,agent:claude \\
  --message "The migration is complete; rebase before your next change."`}
mcp="send_broadcast"
/>

Read a sensitive body from standard input instead of shell history.

<CommandTabs
chat='"send a broadcast to session session_123 saying investigate the failed integration check"'
cli={`printf '%s' 'Investigate the failed integration check.' | \\
  boss broadcast send --to session:session_123 --message -`}
mcp="send_broadcast"
/>

`--message -` is a CLI affordance: it is what keeps the body out of your shell
history. The Chat and MCP paths carry the body inline, so use the CLI when the
body itself is the thing you are protecting.

Include the sending chat when this is intentionally a self-notification.

<CommandTabs
chat='"send a broadcast to chat agent_123, including the origin chat, saying continue after the scheduled window"'
cli={`boss broadcast send --to chat:agent_123 --include-origin \\
  --message "Continue after the scheduled window."`}
mcp="send_broadcast"
/>

Use `--from` to record the originating chat and `--expires-in` to bound retry
time. Delivery retries for 24 hours by default; the maximum is 30 days.
`boss broadcast list` (`ls`) can filter by `--chat`, `--origin`, or `--state`;
`boss broadcast remove` (`rm`) permanently removes a broadcast and its
deliveries.

## Reach other daemons only when needed

Broadcasts start local to the current daemon. `--cross-daemon` asks Boss Cloud
to fan out to other live daemons, each of which resolves the selector against
its own chats:

<CommandTabs
chat='"send a cross-daemon broadcast to repo repo_123 for codex chats saying a release branch is ready for review"'
cli={`boss broadcast send --cross-daemon --to repo:repo_123,agent:codex \\
  --message "A release branch is ready for review."`}
mcp="send_broadcast"
/>

Cross-daemon delivery is off by default and best effort. An offline daemon has
no store-and-forward queue, and fan-out to more than 32 other daemons is
refused rather than truncated. Do not try to target another daemon with a
`daemon:<id>` selector term: chat rows have no daemon id in that dimension, so
such a term resolves to zero chats on every daemon.

## Subscribe to a session outcome

A subscription is a standing rule that sends one broadcast when a session
settles. Its `--on` value is `completed`, `errored`, or `settled` (either
outcome):

Tell a coordinator whether a child session reached any terminal outcome.

<CommandTabs
chat='"subscribe to session session_123 settling, and tell chat agent_456 the child session settled and to inspect its PR"'
cli={`boss broadcast subscribe --session session_123 --on settled \\
  --to chat:agent_456 --message "Child session settled; inspect its PR."`}
mcp="register_broadcast_subscription"
/>

Unlike an ordinary broadcast, a subscription resolves its audience at **fire
time**, not when registered. It resolves and delivers only on its owning
daemon: cross-daemon fan-out is available only to an explicit `broadcast send`
with `--cross-daemon`. It is one-shot: after it fires, is canceled, or expires,
it no longer stands. `--expires-in` bounds how long the rule stands (24 hours
by default, 30 days maximum), not the retry window of its fired broadcast.
`boss broadcast subscriptions` lists rules; `unsubscribe` retires one rather
than erasing it, so a cancelled subscription still appears in an unfiltered
list and can be selected by its historical state.

## MCP tools

Agents can use the same capability through MCP:

| Tool                              | Purpose                                  |
| --------------------------------- | ---------------------------------------- |
| `send_broadcast`                  | Send a message to a selector now.        |
| `list_broadcasts`                 | List broadcasts without exposing bodies. |
| `delete_broadcast`                | Remove a broadcast and its deliveries.   |
| `register_broadcast_subscription` | Register a one-shot outcome rule.        |
| `list_broadcast_subscriptions`    | List standing or terminal rules.         |
| `delete_broadcast_subscription`   | Cancel a subscription.                   |

Pass explicit IDs from the relevant list or context tool. For a broader agent
tool overview, see the [MCP guide](./mcp.md).
