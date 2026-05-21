-- +goose Up
ALTER TABLE task_mappings ADD COLUMN last_error TEXT;

-- +goose Down
ALTER TABLE task_mappings DROP COLUMN last_error;
