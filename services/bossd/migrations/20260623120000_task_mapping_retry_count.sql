-- +goose Up
ALTER TABLE task_mappings ADD COLUMN retry_count INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE task_mappings DROP COLUMN retry_count;
