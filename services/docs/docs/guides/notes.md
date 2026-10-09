---
title: Notes
description: Keep durable, repo-scoped free-text for later runs to inspect.
slug: /guides/notes
---

import CommandTabs from '@site/src/components/CommandTabs';

# Notes

Notes are durable, repo-scoped free-text records. Use them for information a
later session needs, without forcing that information into a session, chat, or
task model. A note may record session and chat IDs as provenance, but it is not
owned by either one: it remains after that session is archived or deleted.

Notes are deliberately a small primitive. They do not impose a status, schema,
or review workflow.

Unlike broadcast and callback bodies, a note body is not a secret: it is the
payload the note exists to preserve. `show`, MCP single-note reads, and JSON
output return it in full; the human-readable `ls` table shows a preview. Do
not use notes for credentials or other values that should stay secret.

## Primary workflow: leave a record after every skill run

Have each skill record a note when it finishes. Keep the note specific enough
for a later reader to act on it:

<CommandTabs
chat='"note that the integration test failed once, its retry took 94s because the fixture waits for an external worker, and that we should make the worker timeout configurable — tag it skill, failure, improvement, analytics, timing"'
cli={`boss notes add "The integration test failed once; its retry took 94s because the fixture waits for an external worker. Make the worker timeout configurable." \\
  --tag skill \\
  --tag failure \\
  --tag improvement \\
  --tag analytics \\
  --tag timing`}
mcp="create_note"
/>

This captures what went wrong (the failure), an improvement, analytics (retry
count), and timing. The repository is inferred when the command runs in a
registered repository or session worktree, so a skill normally does not need
to look up IDs first.

Run a weekly agent sweep over the accumulated records instead of relying on the
memory of the sessions that created them. Find notes matching a set of tags
and a search term:

<CommandTabs
chat='"find notes tagged improvement or timing that mention worker"'
cli="boss notes ls --tag improvement --tag timing --search worker"
mcp="list_notes"
/>

Then read one in full:

<CommandTabs
chat='"show me note note_01J..."'
cli="boss notes show note_01J..."
mcp="get_note"
/>

It can group recurring problems, check the supporting notes, then file one
issue with the proposed change. The notes remain available even if the runs
that wrote them have been removed.

## CLI

`boss notes add <body>` records a note. A repository is required for writes;
the CLI derives it from `BOSS_REPO_ID` or the current working directory. It
derives session provenance from `BOSS_SESSION_ID` or the current context; chat
provenance comes from `BOSS_AGENT_SESSION_ID` or an explicit `--chat` value.

The working-directory part of that is local-daemon-only: a CLI connected with
`--remote` can't resolve the repository or session you're standing in, so set
`BOSS_REPO_ID` or pass `--repo` there. Without one, `add`, `show`, `edit`, and
`rm` fail, and `ls` lists every repository instead of the current one.

Listing notes is one operation you can reach three ways, though only a
local-daemon CLI defaults to the current repository, for the reason just above.
`list_notes` with no `repo_id` searches
every repository the agent can reach, as [MCP reference](#mcp-reference) covers
below:

<CommandTabs
chat='"list the notes for this repo"'
cli="boss notes ls"
mcp="list_notes"
/>

The other operations, and the `ls` filters:

Add a note. Repeat `--tag` for several tags.

<CommandTabs
chat='"note that the review tool skipped generated files, and tag it skill and review"'
cli='boss notes add "Review tool skipped generated files." --tag skill --tag review'
mcp="create_note"
/>

List every repository, including from a boss-managed pane.

<CommandTabs
chat='"list notes across every repository"'
cli='boss notes ls --repo ""'
mcp="list_notes"
/>

Filter by the recording session, any given tag, a body substring, or a limit.

<CommandTabs
chat='"find notes from session session_01J... tagged improvement or timing that mention retry, limited to 20"'
cli="boss notes ls --session session_01J... --tag improvement --tag timing --search retry --limit 20"
mcp="list_notes"
/>

Read, edit, and permanently remove one note.

<CommandTabs
chat='"pull up note note_01J..."'
cli="boss notes show note_01J..."
mcp="get_note"
/>

<CommandTabs
chat='"update note note_01J... to say retry configuration is now covered, and tag it resolved"'
cli='boss notes edit note_01J... --body "Retry configuration is now covered." --tag resolved'
mcp="update_note"
/>

<CommandTabs
chat='"delete note note_01J..."'
cli="boss notes rm note_01J..."
mcp="delete_note"
/>

`ls` defaults to the current repository. `--repo ""` is the explicit
cross-repository form. Repeating `--tag` on `ls` matches notes with _any_ of
the supplied tags. The table lists a one-line body preview; `show` returns the
full body.

`--search` performs a literal substring search of the body. `%` and `_` are
ordinary characters, not SQL wildcards, and matching is case-insensitive for
ASCII only.

On `edit`, omitting `--body` leaves the body unchanged and omitting `--tag`
leaves the tags unchanged. Supplying `--tag` replaces the complete tag set,
rather than adding to it. Pass an empty `--tag` value to clear the tags.
`rm` is permanent.

Add `--json` to `add`, `ls`, `show`, or `edit` for stable machine-readable
output. A note has `id`, `repo_id`, `session_id`, `chat_id`, `body`, `tags`,
`created_at`, `updated_at`, `sync_state`, and `synced_at` fields. `sync_state`
is the note's cloud-sync state (for example `pending` or `synced`) and
`synced_at` is when the cloud last accepted it; both are empty when the daemon
predates note sync or the note has never synced.
[Sync states](#sync-states) lists every value.

## MCP reference

Agents using MCP have the same operations:

| Tool          | Required input                   | Optional input                                                | Result                                                |
| ------------- | -------------------------------- | ------------------------------------------------------------- | ----------------------------------------------------- |
| `create_note` | `repo_id`, `body`                | `session_id`, `chat_id`, `tags`                               | Creates a note.                                       |
| `list_notes`  | None                             | `repo_id`, `session_id`, `chat_id`, `tags`, `search`, `limit` | Lists matching notes. Tags match any supplied tag.    |
| `get_note`    | `repo_id`, `id`                  | None                                                          | Returns one note.                                     |
| `update_note` | `repo_id`, `id`                  | `body`, `tags`                                                | Updates supplied fields. `tags` replaces the tag set. |
| `delete_note` | `repo_id`, `id`, `confirm: true` | None                                                          | Permanently removes a note.                           |

Use the daemon-local repository ID returned by `list_repos` or
`resolve_context`, not a Git remote URL. In hosted mode, `get_note`,
`update_note`, and `delete_note` require the owning daemon-local `repo_id` for
routing. That `repo_id` does not scope or authorize the note: `id` selects it,
and a mismatched `repo_id` is not checked; the local adapter ignores `repo_id`.
For `list_notes`, omit `repo_id` to search all accessible repositories. The
hosted gateway fans out only to reachable daemons and skips offline or slow
ones, so an empty or short result does not prove no other notes exist. MCP does
not infer a repository or provenance from the agent's current directory, so
pass those fields when they matter.

## Limits and handling

The body must be non-empty and no larger than 64 KiB. Tags are trimmed,
lowercased, deduplicated, and returned in ascending order. A note can have up
to 32 tags, each at most 64 bytes long.

Notes are not kept forever. Each time a note is written, bossd deletes that
repo's notes older than 180 days and then trims the repo to its newest 10,000
notes, oldest first. The note just written is always kept. To change either
limit, set `notes.retention_days` or `notes.max_per_repo` in `settings.json`
(`0` means unlimited) and restart the daemon. See
[`notes` fields](../reference/settings.md#notes-fields).

Note bodies are returned in full by MCP and JSON output. Do not use notes for
secrets, credentials, or other values that should not be exposed to readers of
the repository's notes.

## Organization notes (paid plans)

Organizations on a paid Bossanova Cloud plan also get organization notes: one
shared store in Bossanova Cloud that holds the notes every member's daemon
records, plus notes written straight to the cloud API. It gives a weekly sweep
one place to search across every machine instead of one daemon at a time.

An organization note is a copy. Each daemon keeps its own local notes, with the
local retention described above, whether or not they sync. Without a paid plan
nothing changes: local notes work exactly as the rest of this page describes.

### Who gets it

The paid entitlement is judged per organization. Only an organization that
holds it stores notes; paying through one organization grants nothing in
another.

Notes sync only from a daemon connected to Bossanova Cloud (see
[Signing In](./login.md)). A local-only daemon runs no sync worker, so its notes
stay `pending`. Reading and writing organization notes yourself needs
`boss login`; the `boss notes org` commands call the cloud directly, so they work
without `--remote`.

### Which organization a synced note lands in

The daemon sends each note with its repository's origin URL, and Bossanova
Cloud routes it with the same rule it uses for sessions:

- A repository mapped to an organization you belong to sends its notes to that
  organization.
- A repository with no organization mapping sends its notes to your personal
  organization.
- A repository mapped to an organization you do not belong to is not synced.
  Its notes become `refused`, and the daemon asks again every hour.

A note whose repository has no origin URL is never sent and is marked
`rejected` locally. The daemon's owner, the account the daemon signed in as, is
the author of every note it syncs.

### Permissions

Every member of an organization lists, searches and reads every unexpired note
in it. Editing or deleting a note is limited to its author and the
organization's owners; anyone else gets `permission_denied`. A non-member gets `permission_denied`
before anything else is checked.

### How sync works

Sync is automatic and asynchronous. Writing a local note never waits on the
network: the daemon records the change in its sync outbox in the same
transaction as the note, and a background worker sends it. The worker drains the
outbox when the daemon starts, every 30 seconds, and right after every local
write. It sends up to 50 notes per request and at most 10 requests per pass, so
a large backlog drains over several passes.

Any local edit or delete makes the note `pending` again, whatever state it was
in. A failed request is retried after a delay that starts at 30 seconds and
doubles with each attempt, up to 30 minutes. Each delay is randomised between
half and all of that value so daemons do not retry in step.

`boss notes show` prints the sync state on its `Sync:` line, with the time the
cloud last accepted the note and the last error. To sync now and see how many
notes, including deletes not yet sent, are in each state:

<CommandTabs
cli="boss notes sync"
/>

The counts are read as the request goes out, so run it again to see them move.
On a daemon that is not connected to Bossanova Cloud it reports that sync is not
running. Add `--json` for `worker_configured` and a `counts` list that always
carries every state.

### Sync states

| State          | Meaning                                                            | What happens next                                     |
| -------------- | ------------------------------------------------------------------ | ----------------------------------------------------- |
| `pending`      | This version has not reached the cloud yet.                        | Sent on the next pass.                                |
| `synced`       | The cloud holds this version.                                      | Nothing, until the note changes.                      |
| `failed`       | The last attempt hit a transient error, such as a network failure. | Retried with backoff.                                 |
| `rate_limited` | The organization's hourly limit is spent.                          | Retried when the hour resets, plus up to 30 seconds.  |
| `not_entitled` | The organization does not have the paid entitlement.               | Checked again every hour, so an upgrade is picked up. |
| `refused`      | The repository is mapped to an organization you do not belong to.  | Checked again every hour.                             |
| `rejected`     | The cloud refused the note as invalid, or it has no origin URL.    | Not retried until the note changes.                   |
| `expired`      | The note is older than the 90-day retention window.                | Never synced; it stays local.                         |
| `suppressed`   | Someone deleted the cloud copy.                                    | Never synced again; the local note is kept.           |

### Conflicts and deletes

The daemon that recorded a note is the authority for it:

- Edit a synced note on its daemon. The cloud refuses edits to synced notes
  with `failed_precondition`; it edits only notes written through the cloud
  API.
- A newer local version replaces the cloud copy. An older or repeated version
  is ignored, so resending is safe.
- Deleting a local note deletes its cloud copy.
- Deleting a synced note in the cloud removes it from the organization but not
  from its daemon. That note is never accepted again, and its local state
  becomes `suppressed`.
- Deleting a note written through the cloud API removes it permanently.

### Reading and writing organization notes

`boss notes org` reads and writes the organization's notes in Bossanova Cloud
with `ls`, `show`, `add`, `edit`, `rm` and `quota`. When you belong to one
organization it is the default; with several, pass `--org <id>`. Agents use the
six organization-note tools on the hosted MCP endpoint (see
[MCP](./mcp.md)). In the web app, **Settings → Notes** lists the same notes with
the organization's quota and each note's expiry.

<CommandTabs
chat='"add an organization note that the deploy fixture needs a configurable timeout, tagged flaky, using idempotency key nightly-2026-10-09"'
cli='boss notes org add "The deploy fixture needs a configurable timeout." --tag flaky --idempotency-key nightly-2026-10-09'
mcp="create_organization_note"
/>

With an idempotency key (at most 255 bytes), a retry returns the original note
instead of creating a duplicate and spends no quota. Reusing the key with a
different body, tags, repository, session or chat fails with `already_exists`.

<CommandTabs
chat='"list our organization notes tagged flaky that mention timeout"'
cli="boss notes org ls --tag flaky --search timeout"
mcp="list_organization_notes"
/>

`ls` returns one page, newest first: 50 notes by default and at most 200 with
`--page-size`. Pass the page token it prints to `--page-token`, with the same
filters, for the next page. It filters by `--author`, `--repo`, `--session`,
any of several `--tag` values, and `--search`, a case-insensitive body
substring.

### Limits and the hourly window

Organization notes have the same content limits as local notes: a non-empty
body of at most 64 KiB, and at most 32 tags of at most 64 bytes each.

Each organization gets 1,000 note writes an hour by default. A write is
creating a note or changing its body or tags, whether through the API or by
sync. Reads, deletes, idempotent retries, edits that change nothing and syncs
of unchanged content are free. Whoever runs the Bosso server sets a
different limit with the `BOSSO_ORGANIZATION_NOTE_HOURLY_LIMIT` environment
variable; a value that is not a positive integer falls back to 1,000.

The hour is a fixed UTC clock hour, such as 14:00 to 15:00 UTC, not a rolling 60
minutes. Usage resets to zero at the top of each hour. A write over the limit
fails with `resource_exhausted`, and the error reports the usage and the reset
time. Check the current window without spending any of it:

<CommandTabs
chat='"how much of our organization note quota is used this hour?"'
cli="boss notes org quota"
mcp="get_organization_note_quota"
/>

It prints a line such as
`12 of 1000 writes used this hour; resets at 2026-10-09T15:00:00Z`.

### Retention

Every organization note expires 90 days after it was created. For a synced note
that is when it was written on its daemon, not when it synced. Edits never
extend it. An expired note disappears from every read at once, and a background
job deletes expired notes and their tags every 10 minutes. Expiry in the cloud
does not touch the daemon's local copy.

### Existing local notes

A daemon upgraded to a version with note sync marks every existing local note
`pending`, so it syncs once the daemon connects. A note already older than 90
days comes back `expired` and stays local.

### Privacy

Bosso and the sync worker log ids, counts and outcomes, never a note's body or
tags. Every note in an organization is readable by all of its members, so the advice
about secrets above applies to the whole organization. Deleting a user account
deletes every organization note that user authored, and deleting an
organization deletes all of its notes.
