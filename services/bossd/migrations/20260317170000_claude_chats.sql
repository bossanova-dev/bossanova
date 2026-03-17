-- +goose Up
CREATE TABLE claude_chats (
    id          TEXT PRIMARY KEY,
    session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    claude_id   TEXT NOT NULL,
    title       TEXT NOT NULL DEFAULT '',
    daemon_id   TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX idx_claude_chats_session_id ON claude_chats(session_id);

-- +goose Down
DROP INDEX IF EXISTS idx_claude_chats_session_id;
DROP TABLE IF EXISTS claude_chats;
