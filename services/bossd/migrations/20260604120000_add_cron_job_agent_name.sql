-- +goose Up

ALTER TABLE cron_jobs ADD COLUMN agent_name TEXT NOT NULL DEFAULT 'claude';

-- +goose Down

ALTER TABLE cron_jobs DROP COLUMN agent_name;
