-- +goose Up
ALTER TABLE agent_chats ADD COLUMN provider_session_id TEXT;
CREATE INDEX idx_agent_chats_provider_session_id ON agent_chats(provider_session_id);

-- +goose Down
DROP INDEX IF EXISTS idx_agent_chats_provider_session_id;
ALTER TABLE agent_chats DROP COLUMN provider_session_id;
