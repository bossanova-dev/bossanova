-- +goose Up
-- Ownership marker for config-managed cron jobs (BOS-1489). A row created or
-- adopted by a project config carries 'boss-project:<project>'; '' means a
-- hand-made row, so every existing row backfills to today's behaviour. bossd
-- validates the marker's shape, so no CHECK freezes the slug rule here.
ALTER TABLE cron_jobs ADD COLUMN managed_by TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE cron_jobs DROP COLUMN managed_by;
