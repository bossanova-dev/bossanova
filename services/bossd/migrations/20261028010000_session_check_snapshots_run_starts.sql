-- +goose Up
ALTER TABLE session_check_snapshots ADD COLUMN status_run_started_at TEXT;
ALTER TABLE session_check_snapshots ADD COLUMN verified_run_started_at TEXT;

-- +goose Down
ALTER TABLE session_check_snapshots DROP COLUMN verified_run_started_at;
ALTER TABLE session_check_snapshots DROP COLUMN status_run_started_at;
