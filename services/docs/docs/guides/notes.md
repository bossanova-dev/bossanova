---
title: Notes
description: Keep durable, repo-scoped free-text for later runs to inspect.
slug: /guides/notes
---

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

```bash
boss notes add "The integration test failed once; its retry took 94s because the fixture waits for an external worker. Make the worker timeout configurable." \
  --tag skill \
  --tag failure \
  --tag improvement \
  --tag analytics \
  --tag timing
```

This captures what went wrong (the failure), an improvement, analytics (retry
count), and timing. The repository is inferred when the command runs in a
registered repository or session worktree, so a skill normally does not need
to look up IDs first.

Run a weekly agent sweep over the accumulated records instead of relying on the
memory of the sessions that created them:

```bash
boss notes ls --tag improvement --tag timing --search worker
boss notes show note_01J...
```

It can group recurring problems, check the supporting notes, then file one
issue with the proposed change. The notes remain available even if the runs
that wrote them have been removed.

## CLI

`boss notes add <body>` records a note. A repository is required for writes;
the CLI derives it from `BOSS_REPO_ID` or the current working directory. It
derives session provenance from `BOSS_SESSION_ID` or the current context; chat
provenance comes from `BOSS_AGENT_SESSION_ID` or an explicit `--chat` value.

The working-directory part of that is local-daemon-only — a CLI connected with
`--remote` can't resolve the repository or session you're standing in, so set
`BOSS_REPO_ID` or pass `--repo` there. Without one, `add`, `show`, `edit`, and
`rm` fail, and `ls` lists every repository instead of the current one.

```bash
# Add a note. Repeat --tag for several tags.
boss notes add "Review tool skipped generated files." --tag skill --tag review

# List notes for the current repository.
boss notes ls

# List every repository, including from a boss-managed pane.
boss notes ls --repo ""

# Filter by the recording session, any given tag, a body substring, or a limit.
boss notes ls --session session_01J... --tag improvement --tag timing --search retry --limit 20

# Read, edit, and permanently remove one note.
boss notes show note_01J...
boss notes edit note_01J... --body "Retry configuration is now covered." --tag resolved
boss notes rm note_01J...
```

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
`created_at`, and `updated_at` fields.

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

Note bodies are returned in full by MCP and JSON output. Do not use notes for
secrets, credentials, or other values that should not be exposed to readers of
the repository's notes.
