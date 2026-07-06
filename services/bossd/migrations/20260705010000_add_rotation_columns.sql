-- +goose Up
ALTER TABLE repos ADD COLUMN can_auto_rotate INTEGER NOT NULL DEFAULT 1;
ALTER TABLE sessions ADD COLUMN rotation_attempt_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN rotation_resume_at TEXT;

-- +goose Down
-- SQLite < 3.35 doesn't support DROP COLUMN; omit down migration.
