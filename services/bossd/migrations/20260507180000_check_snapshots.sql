-- +goose Up
-- Per-poll snapshot of the daemon's view of a session's CI checks +
-- the display status it computed from them. Lets the operator answer
-- "why does the TUI think this PR is passing when GitHub says failing?"
-- without re-running gh by hand.
--
-- raw_json holds the parsed []vcs.CheckResult slice (same shape the
-- DisplayPoller fed into ComputeDisplayStatus). computed_status is the
-- DisplayStatus enum int the poller resolved for this snapshot.
CREATE TABLE session_check_snapshots (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id      TEXT NOT NULL,
    polled_at       INTEGER NOT NULL,
    head_sha        TEXT NOT NULL DEFAULT '',
    raw_json        TEXT NOT NULL,
    computed_status INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (session_id) REFERENCES sessions(id)
);

-- Hot path: list-recent-by-session for `boss session checks <id>`.
CREATE INDEX session_check_snapshots_session_polled
    ON session_check_snapshots (session_id, polled_at DESC);

-- +goose Down
DROP INDEX IF EXISTS session_check_snapshots_session_polled;
DROP TABLE IF EXISTS session_check_snapshots;
