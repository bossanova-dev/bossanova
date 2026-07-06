-- +goose Up
-- Rotation audit trail (BOS-176): one row per rotation decision.
-- Account columns store labels/ids only, never credentials.
-- Time columns store Unix seconds (matching session_check_snapshots); reset_at
-- is nullable (0 rows carry it as NULL when the banner had no parseable reset).
CREATE TABLE rotation_events (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    chat_id TEXT NOT NULL DEFAULT '',
    provider TEXT NOT NULL,
    trigger_kind TEXT NOT NULL,
    from_account TEXT NOT NULL DEFAULT '',
    to_account TEXT NOT NULL DEFAULT '',
    reset_at INTEGER,
    outcome TEXT NOT NULL,
    detail TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

CREATE INDEX idx_rotation_events_session_created
    ON rotation_events(session_id, created_at DESC);

-- +goose Down
DROP INDEX IF EXISTS idx_rotation_events_session_created;
DROP TABLE IF EXISTS rotation_events;
