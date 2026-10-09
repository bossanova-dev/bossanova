-- +goose Up
-- BOS-1435: a delete tombstone must still say where its note lived.
--
-- The note store hard-deletes a note, so once its row is gone the sync worker
-- can no longer read the repository (which picks the Bosso organization) or the
-- creation time (which Bosso requires to judge the 90-day TTL) for the
-- tombstone it has to send. The delete now captures both onto the outbox row
-- in the same transaction.
--
-- repo_id is a logical reference to repos(id), with no FK, exactly as
-- notes.repo_id is: the tombstone must outlive the note and is not owned by the
-- repository row. note_created_at is the deleted note's created_at (TEXT
-- ISO-8601 millisecond UTC per the BOS-14 standard). Both are NULL on a live
-- row, whose note is read directly, and on a tombstone written before this
-- migration.

ALTER TABLE note_sync_states ADD COLUMN repo_id TEXT;
ALTER TABLE note_sync_states ADD COLUMN note_created_at TEXT;

-- +goose Down

ALTER TABLE note_sync_states DROP COLUMN note_created_at;
ALTER TABLE note_sync_states DROP COLUMN repo_id;
