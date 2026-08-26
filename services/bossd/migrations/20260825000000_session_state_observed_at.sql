-- +goose Up
ALTER TABLE sessions ADD COLUMN last_check_state_head_sha TEXT;
ALTER TABLE sessions ADD COLUMN last_check_state_at TEXT;
ALTER TABLE sessions ADD COLUMN state_entered_at TEXT;

-- +goose Down
ALTER TABLE sessions DROP COLUMN state_entered_at;
ALTER TABLE sessions DROP COLUMN last_check_state_at;
ALTER TABLE sessions DROP COLUMN last_check_state_head_sha;
