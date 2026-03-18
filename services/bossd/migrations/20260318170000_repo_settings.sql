-- +goose Up
ALTER TABLE repos ADD COLUMN can_auto_merge INTEGER NOT NULL DEFAULT 0;
ALTER TABLE repos ADD COLUMN can_auto_merge_dependabot INTEGER NOT NULL DEFAULT 1;
ALTER TABLE repos ADD COLUMN can_auto_address_reviews INTEGER NOT NULL DEFAULT 1;
ALTER TABLE repos ADD COLUMN can_auto_resolve_conflicts INTEGER NOT NULL DEFAULT 1;

-- +goose Down
-- SQLite doesn't support DROP COLUMN before 3.35; omit down migration.
