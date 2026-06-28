-- +goose Up

ALTER TABLE cron_jobs ADD COLUMN gate_command TEXT;
ALTER TABLE cron_jobs ADD COLUMN run_setup_command INTEGER NOT NULL DEFAULT 1;

-- +goose Down

ALTER TABLE cron_jobs DROP COLUMN gate_command;
ALTER TABLE cron_jobs DROP COLUMN run_setup_command;
