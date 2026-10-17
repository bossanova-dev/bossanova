-- +goose Up
-- Session hydration reads one PR head's snapshots, newest first, on every
-- GetSession, ListSessions and stream update of a parked or verified session.
-- session_check_snapshots_session_polled only narrows to the session, so the
-- head filter read every row's checks. This index answers the head read, and
-- carries computed_status so a run of one status is read from the index alone.
CREATE INDEX IF NOT EXISTS idx_session_check_snapshots_session_id_head_sha_polled_at_computed_status
    ON session_check_snapshots (session_id, head_sha, polled_at, computed_status);

-- +goose Down
DROP INDEX IF EXISTS idx_session_check_snapshots_session_id_head_sha_polled_at_computed_status;
