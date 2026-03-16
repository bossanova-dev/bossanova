-- +goose Up

CREATE TABLE repos (
    id                  TEXT PRIMARY KEY,
    display_name        TEXT NOT NULL,
    local_path          TEXT NOT NULL UNIQUE,
    origin_url          TEXT NOT NULL,
    default_base_branch TEXT NOT NULL DEFAULT 'main',
    worktree_base_dir   TEXT NOT NULL,
    setup_script        TEXT,
    created_at          TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at          TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE sessions (
    id                  TEXT PRIMARY KEY,
    repo_id             TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    title               TEXT NOT NULL,
    plan                TEXT NOT NULL DEFAULT '',
    worktree_path       TEXT NOT NULL,
    branch_name         TEXT NOT NULL,
    base_branch         TEXT NOT NULL DEFAULT 'main',
    state               INTEGER NOT NULL DEFAULT 1,
    claude_session_id   TEXT,
    pr_number           INTEGER,
    pr_url              TEXT,
    last_check_state    INTEGER NOT NULL DEFAULT 0,
    automation_enabled  INTEGER NOT NULL DEFAULT 1,
    attempt_count       INTEGER NOT NULL DEFAULT 0,
    blocked_reason      TEXT,
    archived_at         TEXT,
    created_at          TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at          TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX idx_sessions_repo_id ON sessions(repo_id);

CREATE TABLE attempts (
    id          TEXT PRIMARY KEY,
    session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    trigger     INTEGER NOT NULL DEFAULT 0,
    result      INTEGER NOT NULL DEFAULT 0,
    error       TEXT,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX idx_attempts_session_id ON attempts(session_id);

-- +goose Down

DROP TABLE IF EXISTS attempts;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS repos;
