-- +goose Up
-- BOS-706: atomically deduplicate retries of a repo-scoped note create.
--
-- The optional key is deliberately stored on notes rather than a separate
-- lease table: a successful create is the durable fact being deduplicated, and
-- SQLite's unique index lets INSERT OR IGNORE arbitrate concurrent writers.
ALTER TABLE notes ADD COLUMN idempotency_key TEXT;

CREATE UNIQUE INDEX idx_notes_repo_id_idempotency_key
    ON notes(repo_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_notes_repo_id_idempotency_key;
ALTER TABLE notes DROP COLUMN idempotency_key;
