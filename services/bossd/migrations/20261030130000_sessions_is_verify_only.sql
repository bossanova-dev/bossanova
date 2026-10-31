-- +goose Up
-- This creation-time fact keeps sessions adopted only for verification out of automatic repair.
ALTER TABLE sessions ADD COLUMN is_verify_only INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE sessions DROP COLUMN is_verify_only;
