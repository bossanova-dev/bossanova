-- +goose Up
ALTER TABLE sessions ADD COLUMN tracker_id TEXT;
ALTER TABLE sessions ADD COLUMN tracker_url TEXT;

-- +goose Down
-- Recreate sessions table without tracker_id/tracker_url to support SQLite < 3.35
-- which does not have DROP COLUMN.
CREATE TABLE sessions_new (
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

INSERT INTO sessions_new (
    id, repo_id, title, plan, worktree_path, branch_name, base_branch,
    state, claude_session_id, pr_number, pr_url, last_check_state,
    automation_enabled, attempt_count, blocked_reason, archived_at,
    created_at, updated_at
)
SELECT
    id, repo_id, title, plan, worktree_path, branch_name, base_branch,
    state, claude_session_id, pr_number, pr_url, last_check_state,
    automation_enabled, attempt_count, blocked_reason, archived_at,
    created_at, updated_at
FROM sessions;

DROP TABLE sessions;
ALTER TABLE sessions_new RENAME TO sessions;
