-- +goose Up
-- BOS-1429: transactional outbox for syncing local notes to Bosso.
--
-- One row per note records what the sync worker still has to send. The note
-- store writes it in the SAME transaction as the note create/update/delete, so
-- the worker can never miss a change or see one that did not commit.
--
-- source_version counts local writes (create = 1, each update or delete bumps
-- it). The worker reports an outcome against the version it sent, and the store
-- applies the outcome only while source_version still equals it, so a newer
-- local edit is never marked synced by a stale response.
--
-- No foreign key onto notes: a deleted note's row must OUTLIVE the note as its
-- tombstone (is_deleted = 1) until the delete has been propagated, which a
-- cascading FK would make impossible. The row is purged once the tombstone is
-- settled (see NoteSyncStore.PurgeSettledTombstones).
--
-- organization_id is a logical reference to a Bosso organization, which is not
-- a local entity, so it carries no FK either.
--
-- Timestamps follow the BOS-14 naming standard: TEXT ISO-8601 millisecond UTC
-- (sqlutil.TimeLayout); nullable event timestamps mean "has not happened yet".

CREATE TABLE note_sync_states (
    note_id           TEXT PRIMARY KEY, -- logical reference to notes(id); no FK
    source_version    INTEGER NOT NULL,
    is_deleted        INTEGER NOT NULL DEFAULT 0,
    sync_state        TEXT NOT NULL CHECK (sync_state IN (
                          'pending', 'synced', 'rejected', 'rate_limited', 'expired',
                          'not_entitled', 'refused', 'suppressed', 'failed')),
    synced_version    INTEGER NOT NULL DEFAULT 0,
    attempt_count     INTEGER NOT NULL DEFAULT 0,
    next_attempt_at   TEXT,
    last_attempted_at TEXT,
    synced_at         TEXT,
    organization_id   TEXT,
    last_error        TEXT,
    created_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- The sync worker's claim: due rows in a retryable state.
CREATE INDEX idx_note_sync_states_sync_state_next_attempt_at
    ON note_sync_states(sync_state, next_attempt_at);

-- Backfill: every note that predates the outbox becomes a pending version-1
-- change, so existing local notes reach the cloud once a paid daemon connects.
-- Bosso answers EXPIRED for notes older than its retention window, which is the
-- documented compatibility path for very old notes.
INSERT INTO note_sync_states (note_id, source_version, is_deleted, sync_state, created_at, updated_at)
SELECT id, 1, 0, 'pending',
       strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
FROM notes;

-- +goose Down

DROP INDEX IF EXISTS idx_note_sync_states_sync_state_next_attempt_at;
DROP TABLE IF EXISTS note_sync_states;
