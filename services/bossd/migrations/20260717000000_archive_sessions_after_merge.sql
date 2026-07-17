-- +goose Up
-- Whether the daemon archives a session once its PR is detected as merged.
-- Defaults on (1); proto3 bool zero-value is false, so default-on is enforced
-- here at the SQLite layer (and by the repos INSERT literal).
ALTER TABLE repos ADD COLUMN archive_sessions_after_merge INTEGER NOT NULL DEFAULT 1;

-- +goose Down
ALTER TABLE repos DROP COLUMN archive_sessions_after_merge;
