-- +goose Up
-- BOS-444: bring the bossd schema into the BOS-14 naming standard.
--   * rename bare INTEGER-boolean columns to is_/has_/should_ prefixed names
--     (INTEGER stays INTEGER — only the name changes);
--   * convert INTEGER-epoch _at columns to TEXT ISO-8601 with a real data
--     conversion (strftime('%Y-%m-%dT%H:%M:%fZ', <col>, 'unixepoch'), which
--     produces exactly sqlutil.TimeLayout = 2006-01-02T15:04:05.000Z).
--
-- The connection runs with PRAGMA foreign_keys=ON, so DROP TABLE on a parent
-- (sessions, cron_jobs, repos) performs an implicit DELETE that the RESTRICT
-- foreign keys from child tables reject (see 20260626120000_collapse_repair_flags).
-- Parents are therefore altered IN PLACE with RENAME COLUMN / ADD+DROP COLUMN
-- (SQLite >= 3.25 / 3.35, bundled by modernc.org/sqlite), which keeps the table
-- and its name so child FK references stay valid. Only the childless tables
-- (session_check_snapshots, rotation_events) use the full table-rebuild pattern.

-- sessions: bare-boolean renames (in place, FK-safe).
ALTER TABLE sessions RENAME COLUMN automation_enabled TO is_automation_enabled;
ALTER TABLE sessions RENAME COLUMN display_spinner    TO has_display_spinner;
ALTER TABLE sessions RENAME COLUMN defer_pr           TO should_defer_pr;
ALTER TABLE sessions RENAME COLUMN tmux_unattended    TO is_tmux_unattended;
ALTER TABLE sessions RENAME COLUMN quick_chat         TO is_quick_chat;
ALTER TABLE sessions RENAME COLUMN detach             TO is_detached;

-- sessions: epoch -> ISO-8601 TEXT (nullable). Add a TEXT column, copy the
-- converted value, drop the INTEGER column, rename into place.
ALTER TABLE sessions ADD COLUMN last_repair_started_at_iso TEXT;
UPDATE sessions SET last_repair_started_at_iso =
    strftime('%Y-%m-%dT%H:%M:%fZ', last_repair_started_at, 'unixepoch');
ALTER TABLE sessions DROP COLUMN last_repair_started_at;
ALTER TABLE sessions RENAME COLUMN last_repair_started_at_iso TO last_repair_started_at;

ALTER TABLE sessions ADD COLUMN last_repair_blocked_at_iso TEXT;
UPDATE sessions SET last_repair_blocked_at_iso =
    strftime('%Y-%m-%dT%H:%M:%fZ', last_repair_blocked_at, 'unixepoch');
ALTER TABLE sessions DROP COLUMN last_repair_blocked_at;
ALTER TABLE sessions RENAME COLUMN last_repair_blocked_at_iso TO last_repair_blocked_at;

-- cron_jobs: bare-boolean renames (in place, FK-safe). RENAME COLUMN rewrites
-- the referencing index idx_cron_jobs_enabled_next_run automatically.
ALTER TABLE cron_jobs RENAME COLUMN enabled           TO is_enabled;
ALTER TABLE cron_jobs RENAME COLUMN run_setup_command TO should_run_setup_command;

-- repos: single-column rename (no dependent index), in place.
ALTER TABLE repos RENAME COLUMN archive_sessions_after_merge TO should_archive_sessions_after_merge;

-- session_check_snapshots: retype polled_at INTEGER NOT NULL -> TEXT NOT NULL.
-- Childless table, so a full rebuild is safe.
CREATE TABLE session_check_snapshots_new (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id      TEXT NOT NULL,
    polled_at       TEXT NOT NULL,
    head_sha        TEXT NOT NULL DEFAULT '',
    raw_json        TEXT NOT NULL,
    computed_status INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (session_id) REFERENCES sessions(id)
);
INSERT INTO session_check_snapshots_new
    (id, session_id, polled_at, head_sha, raw_json, computed_status)
SELECT id, session_id,
    strftime('%Y-%m-%dT%H:%M:%fZ', polled_at, 'unixepoch'),
    head_sha, raw_json, computed_status
FROM session_check_snapshots;
DROP TABLE session_check_snapshots;
ALTER TABLE session_check_snapshots_new RENAME TO session_check_snapshots;
CREATE INDEX session_check_snapshots_session_polled
    ON session_check_snapshots (session_id, polled_at DESC);

-- rotation_events: retype reset_at (nullable) and created_at (NOT NULL) to TEXT.
-- No foreign keys and no children, so a full rebuild is safe.
CREATE TABLE rotation_events_new (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    chat_id TEXT NOT NULL DEFAULT '',
    provider TEXT NOT NULL,
    trigger_kind TEXT NOT NULL,
    from_account TEXT NOT NULL DEFAULT '',
    to_account TEXT NOT NULL DEFAULT '',
    reset_at TEXT,
    outcome TEXT NOT NULL,
    detail TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);
INSERT INTO rotation_events_new
    (id, session_id, chat_id, provider, trigger_kind, from_account,
     to_account, reset_at, outcome, detail, created_at)
SELECT id, session_id, chat_id, provider, trigger_kind, from_account,
    to_account,
    strftime('%Y-%m-%dT%H:%M:%fZ', reset_at, 'unixepoch'),
    outcome, detail,
    strftime('%Y-%m-%dT%H:%M:%fZ', created_at, 'unixepoch')
FROM rotation_events;
DROP TABLE rotation_events;
ALTER TABLE rotation_events_new RENAME TO rotation_events;
CREATE INDEX idx_rotation_events_session_created
    ON rotation_events(session_id, created_at DESC);

-- +goose Down
-- Reverse cleanly to the old names and INTEGER epoch types.

-- rotation_events: TEXT -> INTEGER epoch (full rebuild).
CREATE TABLE rotation_events_old (
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
INSERT INTO rotation_events_old
    (id, session_id, chat_id, provider, trigger_kind, from_account,
     to_account, reset_at, outcome, detail, created_at)
SELECT id, session_id, chat_id, provider, trigger_kind, from_account,
    to_account,
    CAST(strftime('%s', reset_at) AS INTEGER),
    outcome, detail,
    CAST(strftime('%s', created_at) AS INTEGER)
FROM rotation_events;
DROP TABLE rotation_events;
ALTER TABLE rotation_events_old RENAME TO rotation_events;
CREATE INDEX idx_rotation_events_session_created
    ON rotation_events(session_id, created_at DESC);

-- session_check_snapshots: TEXT -> INTEGER epoch (full rebuild).
CREATE TABLE session_check_snapshots_old (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id      TEXT NOT NULL,
    polled_at       INTEGER NOT NULL,
    head_sha        TEXT NOT NULL DEFAULT '',
    raw_json        TEXT NOT NULL,
    computed_status INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (session_id) REFERENCES sessions(id)
);
INSERT INTO session_check_snapshots_old
    (id, session_id, polled_at, head_sha, raw_json, computed_status)
SELECT id, session_id,
    CAST(strftime('%s', polled_at) AS INTEGER),
    head_sha, raw_json, computed_status
FROM session_check_snapshots;
DROP TABLE session_check_snapshots;
ALTER TABLE session_check_snapshots_old RENAME TO session_check_snapshots;
CREATE INDEX session_check_snapshots_session_polled
    ON session_check_snapshots (session_id, polled_at DESC);

-- repos: rename back.
ALTER TABLE repos RENAME COLUMN should_archive_sessions_after_merge TO archive_sessions_after_merge;

-- cron_jobs: rename back.
ALTER TABLE cron_jobs RENAME COLUMN should_run_setup_command TO run_setup_command;
ALTER TABLE cron_jobs RENAME COLUMN is_enabled               TO enabled;

-- sessions: epoch columns TEXT -> INTEGER.
ALTER TABLE sessions ADD COLUMN last_repair_blocked_at_epoch INTEGER;
UPDATE sessions SET last_repair_blocked_at_epoch =
    CAST(strftime('%s', last_repair_blocked_at) AS INTEGER);
ALTER TABLE sessions DROP COLUMN last_repair_blocked_at;
ALTER TABLE sessions RENAME COLUMN last_repair_blocked_at_epoch TO last_repair_blocked_at;

ALTER TABLE sessions ADD COLUMN last_repair_started_at_epoch INTEGER;
UPDATE sessions SET last_repair_started_at_epoch =
    CAST(strftime('%s', last_repair_started_at) AS INTEGER);
ALTER TABLE sessions DROP COLUMN last_repair_started_at;
ALTER TABLE sessions RENAME COLUMN last_repair_started_at_epoch TO last_repair_started_at;

-- sessions: bare-boolean renames back.
ALTER TABLE sessions RENAME COLUMN is_detached           TO detach;
ALTER TABLE sessions RENAME COLUMN is_quick_chat         TO quick_chat;
ALTER TABLE sessions RENAME COLUMN is_tmux_unattended    TO tmux_unattended;
ALTER TABLE sessions RENAME COLUMN should_defer_pr       TO defer_pr;
ALTER TABLE sessions RENAME COLUMN has_display_spinner   TO display_spinner;
ALTER TABLE sessions RENAME COLUMN is_automation_enabled TO automation_enabled;
