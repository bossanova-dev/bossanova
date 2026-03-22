-- +goose Up
ALTER TABLE repos ADD COLUMN merge_strategy TEXT NOT NULL DEFAULT 'rebase';

-- +goose Down
-- SQLite doesn't support DROP COLUMN before 3.35; omit down migration.
