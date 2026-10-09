-- +goose Up
-- What a fire does while the same job's previous run is still in progress
-- (BOS-1437). Defaults to 'skip', today's overlap suppression, so existing jobs
-- are unaffected.
ALTER TABLE cron_jobs ADD COLUMN concurrency_policy TEXT NOT NULL DEFAULT 'skip'
  CHECK (concurrency_policy IN ('skip', 'cancel_in_progress', 'allow_concurrent'));

-- +goose Down
ALTER TABLE cron_jobs DROP COLUMN concurrency_policy;
