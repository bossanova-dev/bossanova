-- +goose Up
-- Whether the daemon deletes a session's git branch once its session is archived.
-- Defaults on (1); proto3 bool zero-value is false, so default-on is enforced
-- here at the SQLite layer (and by the repos INSERT literal).
ALTER TABLE repos ADD COLUMN can_auto_delete_branches INTEGER NOT NULL DEFAULT 1;

-- +goose Down
ALTER TABLE repos DROP COLUMN can_auto_delete_branches;
