-- +goose Up
ALTER TABLE sessions ADD COLUMN last_repair_blocked_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN last_repair_blocked_at INTEGER;

-- +goose Down
ALTER TABLE sessions DROP COLUMN last_repair_blocked_reason;
ALTER TABLE sessions DROP COLUMN last_repair_blocked_at;
