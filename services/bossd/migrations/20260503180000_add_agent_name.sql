-- +goose Up
ALTER TABLE sessions ADD COLUMN agent_name TEXT NOT NULL DEFAULT 'claude';
ALTER TABLE agent_chats ADD COLUMN agent_name TEXT NOT NULL DEFAULT 'claude';

-- +goose Down
ALTER TABLE agent_chats DROP COLUMN agent_name;
ALTER TABLE sessions DROP COLUMN agent_name;
