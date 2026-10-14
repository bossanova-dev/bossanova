-- +goose Up
-- Fix-row state that must survive a daemon restart (BOS-1537).
--
-- session_resume_attempts is the durable ledger of automatic resumes: one row
-- per attempt, so the fix sentence can name every attempt's time and the
-- failure it got. run_key identifies one run (a cron fire, or the original
-- prompt's chat), so a new run starts a fresh budget. Rows are owned by their
-- session and die with it. The unique index enforces one row per attempt
-- number within a run, and its (session_id, run_key) prefix serves the
-- per-run lookup.
CREATE TABLE session_resume_attempts (
    id             TEXT PRIMARY KEY,
    session_id     TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    run_key        TEXT NOT NULL,
    attempt_number INTEGER NOT NULL,
    attempted_at   TEXT NOT NULL,
    failure_class  TEXT NOT NULL DEFAULT 'unknown'
        CHECK (failure_class IN ('auth_expired', 'token_revoked', 'disk_full', 'crash', 'respawn_cap', 'unknown')),
    failure_detail TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE UNIQUE INDEX idx_session_resume_attempts_session_id_run_key_attempt_number
    ON session_resume_attempts(session_id, run_key, attempt_number);

-- The operator's dismissal of a session alert: the key of the latest
-- dismissed alert ('' = nothing dismissed) and when it was dismissed (NULL =
-- never). One key per session; a different alert key shows again.
ALTER TABLE sessions ADD COLUMN dismissed_alert_key TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN dismissed_alert_at TEXT;

-- +goose Down
ALTER TABLE sessions DROP COLUMN dismissed_alert_at;
ALTER TABLE sessions DROP COLUMN dismissed_alert_key;
DROP INDEX IF EXISTS idx_session_resume_attempts_session_id_run_key_attempt_number;
DROP TABLE IF EXISTS session_resume_attempts;
