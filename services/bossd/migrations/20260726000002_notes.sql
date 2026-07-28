-- +goose Up
-- BOS-550: durable notes primitive.
--
-- A note is a free-text write-up recorded against a repository, optionally
-- carrying the session and chat that produced it. Notes are the storage
-- foundation for a weekly sweep that harvests learnings from PAST runs.
--
-- Ownership: repo-scoped, session is provenance. A note belongs to repo_id;
-- session_id and chat_id are nullable provenance columns.
--
-- No foreign keys on repo_id/session_id/chat_id: they are logical references to
-- entities whose lifecycle is not co-owned by this table. Sessions are archived
-- and removed routinely (`boss archive`, `boss rm`), so a cascading DELETE from
-- sessions would erase exactly the notes the sweep exists to read — the note
-- must outlive the run that wrote it. github_callbacks sets the same precedent
-- and states the same reason (see 20260722000000_github_callbacks.sql).
--
-- note_tags.note_id DOES get a real FK with ON DELETE CASCADE, for the same
-- reason broadcast_deliveries.broadcast_id does: a tag row is owned by its note
-- and is meaningless without it. The connection runs with PRAGMA
-- foreign_keys=ON. Tags live in a join table rather than a JSON array column
-- because they are QUERIED (filter notes by tag), so they need an index;
-- accounts.allowed_models is a JSON array only because it is never a predicate.
--
-- Timestamps follow the BOS-14 naming standard: TEXT ISO-8601 millisecond UTC
-- (sqlutil.TimeLayout); non-null created/updated default via strftime.

CREATE TABLE notes (
    id         TEXT PRIMARY KEY,
    repo_id    TEXT NOT NULL,
    session_id TEXT,              -- nullable provenance; no FK, no cascade
    chat_id    TEXT,              -- nullable provenance; no FK, no cascade
    body       TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- Listing a repo's notes in creation order (the sweep's primary access path).
CREATE INDEX idx_notes_repo_created ON notes(repo_id, created_at);

-- Narrowing notes to the session that produced them.
CREATE INDEX idx_notes_session ON notes(session_id);

CREATE TABLE note_tags (
    note_id TEXT NOT NULL REFERENCES notes(id) ON DELETE CASCADE,
    tag     TEXT NOT NULL,
    PRIMARY KEY (note_id, tag)
);

-- Any-of tag lookup: WHERE tag IN (...) for the topic filter.
CREATE INDEX idx_note_tags_tag ON note_tags(tag);

-- +goose Down

DROP TABLE IF EXISTS note_tags;
DROP TABLE IF EXISTS notes;
