-- +goose Up
ALTER TABLE sessions ADD COLUMN account_id TEXT;
ALTER TABLE agent_chats ADD COLUMN account_id TEXT;

-- +goose Down
ALTER TABLE agent_chats DROP COLUMN account_id;
ALTER TABLE sessions DROP COLUMN account_id;
