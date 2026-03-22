-- +goose Up

CREATE TABLE task_mappings (
    id                     TEXT PRIMARY KEY,
    external_id            TEXT NOT NULL UNIQUE,
    plugin_name            TEXT NOT NULL,
    session_id             TEXT,
    repo_id                TEXT NOT NULL,
    status                 INTEGER NOT NULL DEFAULT 0,
    pending_update_status  INTEGER,
    pending_update_details TEXT,
    created_at             TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at             TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    FOREIGN KEY (session_id) REFERENCES sessions(id),
    FOREIGN KEY (repo_id) REFERENCES repos(id)
);

CREATE INDEX idx_task_mappings_external_id ON task_mappings(external_id);
CREATE INDEX idx_task_mappings_repo_id ON task_mappings(repo_id);
CREATE INDEX idx_task_mappings_session_id ON task_mappings(session_id);

-- +goose Down

DROP TABLE IF EXISTS task_mappings;
