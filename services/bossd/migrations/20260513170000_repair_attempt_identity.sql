-- +goose Up
-- Persist the repair target identity so daemon/plugin restarts do not
-- repeatedly run the same failed agent repair on an unchanged PR head.
ALTER TABLE sessions ADD COLUMN last_repair_head_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN last_repair_display_status INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE sessions DROP COLUMN last_repair_display_status;
ALTER TABLE sessions DROP COLUMN last_repair_head_sha;
