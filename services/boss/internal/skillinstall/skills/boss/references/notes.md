<!-- GENERATED from the boss CLI by `make gen-skill` — do not edit by hand. Index: ../SKILL.md -->

## Notes

### `boss notes`

Record and search repo-scoped notes

A note is durable free-text recorded against a REPOSITORY so a later sweep can harvest what a run learned — a gotcha, a decision, a piece of tech debt worth filing. Notes are repo-scoped and session and chat are provenance ONLY: they record who wrote the note, and archiving or removing that session never removes its notes. A note outlives the run that wrote it. Inside a registered repo or a session worktree the repo and session default from the working directory, so an agent can leave a note with one command and no ids to look up. A body is REQUIRED (a blank or whitespace-only one is rejected), may be up to 64 KiB, and is stored verbatim. Tags are normalised — trimmed, lowercased and de-duplicated — so `Tech-Debt` and `tech-debt` are one tag; a note may carry up to 32 tags of 64 bytes each. Notes are listed OLDEST first. `add`, `ls`, `show` and `edit` all take `--json` for machine parsing.

### `boss notes add <body> [flags]`

Record a note against a repository

Record a note. `--repo`, `--session` and `--chat` are resolved in this order: the explicit flag, then the ambient `BOSS_REPO_ID` / `BOSS_SESSION_ID` / `BOSS_AGENT_SESSION_ID`, then — for the repo and session only — the daemon's resolution of the working directory. An agent running inside its own session worktree therefore needs no ids at all. There is no working-directory fallback for the chat: a session's primary chat is not necessarily the one calling, so guessing would attribute the note to the wrong agent — export `BOSS_AGENT_SESSION_ID` or pass `--chat` if the chat matters. When the repository cannot be resolved the command FAILS naming `--repo` rather than writing the note against the wrong repo. `--tag` is repeatable (`--tag a --tag b`), not comma-separated; tags are trimmed, lowercased and de-duplicated before they are stored.

**Flags:**

- `--chat` — Chat provenance (default: $BOSS_AGENT_SESSION_ID)
- `--idempotency-key` — Atomically return an existing note with this repo-scoped key instead of creating a duplicate
- `--json` — Emit the created note as a stable JSON schema
- `--repo` — Owning repository id (default: $BOSS_REPO_ID, else the working directory's repo)
- `--session` — Session provenance (default: $BOSS_SESSION_ID, else the working directory's session)
- `--tag` — Tag to attach; repeat for several (normalised to lowercase)

```bash
# From inside a session worktree: repo and session are inferred, no ids needed
boss notes add "the flaky test is a socket-token race" --tag tech-debt
# Repeat --tag for several tags
boss notes add "auth middleware assumes a trailing slash" --tag gotcha --tag auth
# Record against an explicit repo from anywhere, and parse the result
boss notes add "release checklist step 3 is stale" --repo my-repo --json
```

### `boss notes edit <note-id> [flags]`

Change a note's body and/or tags

Change a note's body and/or tags; pass at least one of `--body` and `--tag` or the command fails with nothing to do. An omitted `--body` leaves the body alone and an omitted `--tag` leaves the tags alone. Passing `--tag` REPLACES the whole tag set with exactly what you pass — it does not merge, so re-list every tag the note should keep. `--tag ""` therefore clears every tag.

**Flags:**

- `--body` — Replacement body (omit to leave the body unchanged)
- `--json` — Emit the updated note as a stable JSON schema
- `--repo` — Owning repository id for remote routing (default: $BOSS_REPO_ID, else the working directory's repo; ignored locally)
- `--tag` — Tag for the REPLACEMENT set — the whole tag set is replaced, not merged; repeat for several, omit to leave tags unchanged

```bash
# Rewrite the body, leaving the tags untouched
boss notes edit abc123 --body "the flaky test is a socket-token race; fixed in #1712"
# REPLACES the tag set with exactly these two tags
boss notes edit abc123 --tag tech-debt --tag resolved
# Clear every tag
boss notes edit abc123 --tag ""
```

### `boss notes ls [flags]`

List notes, oldest first

List notes in the order they were recorded, OLDEST first, so `--limit N` returns the N oldest. `--repo` resolves like `add`'s: the explicit flag, then the ambient `BOSS_REPO_ID`, then the working directory's repo — so inside a repo the listing is scoped to it. To list across EVERY repo pass `--repo ""` explicitly; simply leaving the repo directory is NOT enough, because a boss-managed agent pane always exports `BOSS_REPO_ID`. `--tag` matches ANY of the tags given (a note carrying just one of them matches), not all of them; unlike on `add`/`edit`, `--tag ""` here is not a wildcard — the daemon fails closed on a tag that normalises away, so it matches nothing. `--search` matches a substring of the body, case-insensitively for ASCII only (the daemon folds case with SQLite's `LOWER()`, which does not fold non-ASCII); SQL wildcards are matched literally. `--session` filters by the session that recorded the note and does NOT default from the working directory — a session-scoped default would silently hide the repo's other notes. Bodies are flattened to one line and truncated in the table: use `boss notes show` for the full text.

**Flags:**

- `--json` — Emit a stable JSON schema instead of a table
- `--limit` — Cap the number of notes returned (0 = unlimited) (default: 0)
- `--repo` — Filter by repository id (default: $BOSS_REPO_ID, else the working directory's repo; pass --repo "" for every repo)
- `--search` — Filter to notes whose body contains this substring
- `--session` — Filter by the session that recorded the note
- `--tag` — Filter to notes carrying any of these tags; repeat for several

```bash
boss notes ls
# Notes carrying EITHER tag (any-of, not all-of)
boss notes ls --tag tech-debt --tag gotcha
# The 5 oldest notes whose body contains the term
boss notes ls --search "socket token" --limit 5
boss notes ls --repo my-repo --json
# Every repo, even from inside a session pane that exports BOSS_REPO_ID
boss notes ls --repo ""
```

### `boss notes rm <note-id> [flags]`

Remove a note by id

Remove a note by id. Removal is idempotent: removing a note that is already gone succeeds rather than erroring, so a cleanup script can be re-run safely. Removing a note is permanent — there is no trash for notes.

**Flags:**

- `--repo` — Owning repository id for remote routing (default: $BOSS_REPO_ID, else the working directory's repo; ignored locally)

```bash
boss notes rm abc123
```

### `boss notes show <note-id> [flags]`

Show one note in full

Print one note in full: its ids, provenance, tags, timestamps, and then the body verbatim and untruncated (`boss notes ls` only shows a one-line preview). `--repo` is a routing hint for a remote daemon and is ignored locally — the note is resolved by id.

**Flags:**

- `--json` — Emit the note as a stable JSON schema
- `--repo` — Owning repository id for remote routing (default: $BOSS_REPO_ID, else the working directory's repo; ignored locally)

```bash
boss notes show abc123
boss notes show abc123 --json
```
