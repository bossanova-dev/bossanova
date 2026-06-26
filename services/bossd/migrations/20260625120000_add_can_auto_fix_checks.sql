-- +goose Up
-- Per-repo toggle for the automatic-repair of failing CI checks. Previously
-- check repair was always-on (gated only by the per-session pause), unlike the
-- conflict and review-feedback repair which already had per-repo toggles.
-- Default 1 preserves that always-on behavior for existing repos.
ALTER TABLE repos ADD COLUMN can_auto_fix_checks INTEGER NOT NULL DEFAULT 1;

-- +goose Down
-- SQLite doesn't support DROP COLUMN before 3.35; omit down migration.
