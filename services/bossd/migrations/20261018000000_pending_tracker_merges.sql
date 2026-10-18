-- +goose Up
-- A merged session whose post-merge tracker write did not land and is retried.
CREATE TABLE pending_tracker_merges (
    session_id      TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    attempt_count   INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TEXT NOT NULL,
    last_reason     TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX idx_pending_tracker_merges_next_attempt_at ON pending_tracker_merges(next_attempt_at);

-- +goose Down
DROP TABLE pending_tracker_merges;
