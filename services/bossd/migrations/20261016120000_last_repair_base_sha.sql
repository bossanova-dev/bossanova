-- +goose Up
-- Persist the base branch tip a conflict repair ran against. A merge conflict
-- depends on the base as well as the head, so a base that moved since the last
-- attempt makes the same head a new repair input.
ALTER TABLE sessions ADD COLUMN last_repair_base_sha TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE sessions DROP COLUMN last_repair_base_sha;
