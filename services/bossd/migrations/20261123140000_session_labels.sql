-- +goose Up
CREATE TABLE session_labels (
    id TEXT NOT NULL PRIMARY KEY,
    name TEXT NOT NULL UNIQUE COLLATE NOCASE,
    color TEXT NOT NULL DEFAULT '#95a2b3',
    description TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE session_label_assignments (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    label_id TEXT NOT NULL REFERENCES session_labels(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (session_id, label_id)
);

CREATE INDEX idx_session_label_assignments_label_id ON session_label_assignments(label_id);

-- +goose Down
DROP INDEX idx_session_label_assignments_label_id;
DROP TABLE session_label_assignments;
DROP TABLE session_labels;
