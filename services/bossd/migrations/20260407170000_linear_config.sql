-- +goose Up
ALTER TABLE repos ADD COLUMN linear_api_key TEXT NOT NULL DEFAULT '';
ALTER TABLE repos ADD COLUMN linear_team_key TEXT NOT NULL DEFAULT '';

-- +goose Down
-- SQLite doesn't support DROP COLUMN before 3.35; omit down migration.
