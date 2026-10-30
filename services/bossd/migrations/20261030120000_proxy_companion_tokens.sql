-- Companion targets are the third proxy token shape, alongside session and chat
-- targets. account_id stores their independent Claude account binding.
-- +goose Up
ALTER TABLE proxy_tokens ADD COLUMN is_companion_shaped INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE proxy_tokens DROP COLUMN is_companion_shaped;
