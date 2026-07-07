-- +goose Up
-- Cached per-account rate-limit usage snapshot (BOS-297). METADATA ONLY: no
-- credential is ever stored in these columns. All columns are additive and
-- nullable/defaulted so existing rows migrate cleanly. usage_fetched_at NULL
-- means never probed.
ALTER TABLE accounts ADD COLUMN usage_util_5h REAL NOT NULL DEFAULT 0;
ALTER TABLE accounts ADD COLUMN usage_util_7d REAL NOT NULL DEFAULT 0;
ALTER TABLE accounts ADD COLUMN usage_reset_5h TEXT;
ALTER TABLE accounts ADD COLUMN usage_reset_7d TEXT;
ALTER TABLE accounts ADD COLUMN usage_status TEXT NOT NULL DEFAULT '';
ALTER TABLE accounts ADD COLUMN usage_plan_tier TEXT NOT NULL DEFAULT '';
ALTER TABLE accounts ADD COLUMN usage_fetched_at TEXT;

-- +goose Down
-- SQLite < 3.35 does not support DROP COLUMN; omit down migration
-- (matches 20260705010000_add_rotation_columns.sql).
