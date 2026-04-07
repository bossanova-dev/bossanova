-- +goose Up
ALTER TABLE repos DROP COLUMN linear_team_key;

-- +goose Down
ALTER TABLE repos ADD COLUMN linear_team_key TEXT NOT NULL DEFAULT '';
