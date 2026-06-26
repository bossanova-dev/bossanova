-- +goose Up
-- Collapse the three per-behavior repair toggles (can_auto_address_reviews,
-- can_auto_resolve_conflicts, can_auto_fix_checks) into a single can_auto_repair
-- flag. The three gated the removed in-process FixLoop and never reached the
-- repair plugin, so they had no effect; can_auto_repair is honored by the
-- plugin. Defaults on, preserving the prior always-on repair behavior.
--
-- We use ALTER TABLE ADD/DROP COLUMN rather than the recreate-and-DROP-TABLE
-- pattern on purpose. The connection runs with PRAGMA foreign_keys=ON, so
-- DROP TABLE repos performs an implicit DELETE of every repos row, which the
-- RESTRICT foreign keys from sessions/cron_jobs/task_mappings reject. ADD/DROP
-- COLUMN keeps the table (and its name) in place, so child FK references stay
-- valid and unrelated columns (e.g. sentry_project) are preserved untouched.
-- DROP COLUMN requires SQLite >= 3.35; modernc.org/sqlite bundles a newer
-- engine, so it is available.
ALTER TABLE repos ADD COLUMN can_auto_repair INTEGER NOT NULL DEFAULT 1;

ALTER TABLE repos DROP COLUMN can_auto_address_reviews;
ALTER TABLE repos DROP COLUMN can_auto_resolve_conflicts;
ALTER TABLE repos DROP COLUMN can_auto_fix_checks;

-- +goose Down
-- NON-REVERSIBLE: the dropped per-behavior repair flags cannot be restored;
-- they were inert before removal. Omit the down migration.
