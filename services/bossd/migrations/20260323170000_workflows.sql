-- +goose Up

CREATE TABLE workflows (
    id               TEXT PRIMARY KEY,
    session_id       TEXT NOT NULL REFERENCES sessions(id),
    repo_id          TEXT NOT NULL REFERENCES repos(id),
    plan_path        TEXT NOT NULL,
    status           TEXT NOT NULL DEFAULT 'pending',
    current_step     TEXT NOT NULL DEFAULT 'plan',
    flight_leg       INTEGER NOT NULL DEFAULT 0,
    max_legs         INTEGER NOT NULL DEFAULT 20,
    last_error       TEXT,
    start_commit_sha TEXT,
    config_json      TEXT,
    created_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX idx_workflows_session_id ON workflows(session_id);
CREATE INDEX idx_workflows_status ON workflows(status);

-- +goose Down

DROP TABLE IF EXISTS workflows;
