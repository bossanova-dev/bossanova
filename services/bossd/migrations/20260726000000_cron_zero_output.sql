-- +goose Up
-- Whether this cron job is a "zero output" job: it fires with no worktree,
-- branch, or pull request, because the run is expected to produce no repo
-- changes (BOS-543). Opt-in per job. Defaults OFF (0), matching the proto3
-- bool zero value, so existing jobs are unaffected by this migration.
ALTER TABLE cron_jobs ADD COLUMN is_zero_output INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE cron_jobs DROP COLUMN is_zero_output;
