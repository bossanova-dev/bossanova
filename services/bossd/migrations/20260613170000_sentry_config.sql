-- +goose Up
-- Issues are listed org-wide (across every project), so only the auth token and
-- organization slug are stored — no per-project slug.
ALTER TABLE repos ADD COLUMN sentry_api_key TEXT NOT NULL DEFAULT '';
ALTER TABLE repos ADD COLUMN sentry_org TEXT NOT NULL DEFAULT '';

-- +goose Down
-- SQLite doesn't support DROP COLUMN before 3.35; omit down migration.
