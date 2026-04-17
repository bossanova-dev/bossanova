-- +goose Up
ALTER TABLE claude_chats ADD COLUMN tmux_session_name TEXT;

-- +goose Down
-- Recreate claude_chats table without tmux_session_name to support SQLite < 3.35
-- which does not have DROP COLUMN.
CREATE TABLE claude_chats_new (
    id          TEXT PRIMARY KEY,
    session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    claude_id   TEXT NOT NULL,
    title       TEXT NOT NULL DEFAULT '',
    daemon_id   TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

INSERT INTO claude_chats_new (id, session_id, claude_id, title, daemon_id, created_at)
SELECT id, session_id, claude_id, title, daemon_id, created_at FROM claude_chats;

DROP TABLE claude_chats;
ALTER TABLE claude_chats_new RENAME TO claude_chats;
CREATE INDEX idx_claude_chats_session_id ON claude_chats(session_id);
