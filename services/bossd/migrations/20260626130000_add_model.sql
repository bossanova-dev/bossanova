-- +goose Up
-- Generic per-session agent model id. Empty string means "inherit the agent
-- CLI default" (today: Opus for claude). bossd never enumerates valid models;
-- the agent CLI validates the name at runtime.
ALTER TABLE cron_jobs ADD COLUMN model TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions  ADD COLUMN model TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE sessions  DROP COLUMN model;
ALTER TABLE cron_jobs DROP COLUMN model;
