-- +goose Up

CREATE TABLE cron_jobs (
    id                  TEXT PRIMARY KEY,
    repo_id             TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    name                TEXT NOT NULL,
    prompt              TEXT NOT NULL,
    schedule            TEXT NOT NULL,
    timezone            TEXT,
    enabled             INTEGER NOT NULL DEFAULT 1,
    last_run_session_id TEXT REFERENCES sessions(id) ON DELETE SET NULL,
    last_run_at         TEXT,
    last_run_outcome    TEXT,
    next_run_at         TEXT,
    created_at          TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at          TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (repo_id, name)
);

CREATE INDEX idx_cron_jobs_enabled_next_run ON cron_jobs(enabled, next_run_at);

ALTER TABLE sessions ADD COLUMN cron_job_id TEXT REFERENCES cron_jobs(id) ON DELETE SET NULL;
ALTER TABLE sessions ADD COLUMN defer_pr    INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN hook_token  TEXT;

-- +goose Down

ALTER TABLE sessions DROP COLUMN hook_token;
ALTER TABLE sessions DROP COLUMN defer_pr;
ALTER TABLE sessions DROP COLUMN cron_job_id;
DROP TABLE IF EXISTS cron_jobs;
