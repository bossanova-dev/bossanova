-- +goose Up
ALTER TABLE agent_accounts ADD COLUMN usage_model_windows TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE agent_accounts DROP COLUMN usage_model_windows;
