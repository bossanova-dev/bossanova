-- +goose Up

ALTER TABLE cron_jobs ADD COLUMN last_run_agent_name TEXT;

-- +goose Down

ALTER TABLE cron_jobs DROP COLUMN last_run_agent_name;
